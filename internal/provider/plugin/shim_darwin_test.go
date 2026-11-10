//go:build darwin

// darwin counterparts of the Linux spawn guarantees: the shim runs a private, content-addressed
// copy of the pinned plugin and stays the plugin's parent.

package plugin

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/Exploded-views/bitc/internal/config"
	"github.com/Exploded-views/bitc/internal/exit"
	"github.com/Exploded-views/bitc/internal/provider/plugin/pin"
	"github.com/Exploded-views/bitc/internal/remote"
)

// The copy the shim execs lives at <state>/plugins/<type>/bin/<sha256 of the pinned bytes>.
func (r *rig) copyPath() string { return filepath.Join(r.own, "bin", r.core.Plugin.SHA256) }

func evalSame(t *testing.T, a, b string) bool {
	t.Helper()
	ea, err1 := filepath.EvalSymlinks(a)
	eb, err2 := filepath.EvalSymlinks(b)
	return err1 == nil && err2 == nil && ea == eb
}

func TestDarwinShimRunsAPrivateContentAddressedCopyOfTheHashedBytes(t *testing.T) {
	r := newRig(t)
	r.start()
	d := r.diag()
	if !evalSame(t, d.Exe, r.copyPath()) {
		t.Errorf("exe %q, want the private copy %q", d.Exe, r.copyPath())
	}
	if len(d.Args) == 0 || d.Args[0] != r.bin {
		t.Errorf("argv[0] %q, want the pinned path %q", d.Args, r.bin)
	}
	got, err := pin.SumFile(r.copyPath())
	if err != nil || got != r.core.Plugin.SHA256 {
		t.Errorf("the copy hashes to %q (%v), want the pinned %q", got, err, r.core.Plugin.SHA256)
	}
}

func TestDarwinCopyIsMode0500InADirOfMode0700WithoutSetuid(t *testing.T) {
	r := newRig(t)
	r.start()
	st, err := os.Stat(r.copyPath())
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o500 || st.Mode()&(os.ModeSetuid|os.ModeSetgid) != 0 {
		t.Errorf("copy mode %v, want -r-x------ and no setuid or setgid bit", st.Mode())
	}
	ds, err := os.Stat(filepath.Dir(r.copyPath()))
	if err != nil || ds.Mode().Perm() != 0o700 || !ds.IsDir() {
		t.Errorf("copy dir %v %v, want a directory of mode 0700", ds, err)
	}
}

// The copy is never modified in place: a second spawn leaves the same inode.
func TestDarwinCopyIsNotRewrittenBySecondSpawn(t *testing.T) {
	r := newRig(t)
	r.start()
	before, err := os.Stat(r.copyPath())
	if err != nil {
		t.Fatal(err)
	}
	killPlugin(t, r)
	if err := r.p.Preflight(context.Background(), r.job()); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(r.copyPath())
	if err != nil || !os.SameFile(before, after) {
		t.Errorf("the copy was replaced on respawn: %v", err)
	}
}

// A binary swapped in after hashing must not run: the shim re-hashes the copy just before exec.
func TestDarwinShimRefusesACopySwappedAfterHashing(t *testing.T) {
	r := newRig(t)
	marker := filepath.Join(r.dir, "tampered-ran")
	if err := os.MkdirAll(filepath.Dir(r.copyPath()), 0o700); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\ntouch " + marker + "\n"
	if err := os.WriteFile(r.copyPath(), []byte(script), 0o500); err != nil {
		t.Fatal(err)
	}
	wantUnavailableLocal(t, r.startErr(), "changed")
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the swapped copy ran")
	}
	if r.exists("diag.json") {
		t.Error("a plugin ran from a swapped copy")
	}
}

// The shim stays the plugin's parent, in the plugin's own group (not bitc's), so killing the
// group takes both.
func TestDarwinShimStaysTheParentInThePluginsGroup(t *testing.T) {
	r := newRig(t)
	r.start()
	d := r.diag()
	if d.Ppid == os.Getpid() || d.Ppid <= 1 {
		t.Errorf("plugin ppid %d: want the shim, not bitc (%d) or init", d.Ppid, os.Getpid())
	}
	if d.Pgid != d.Ppid || d.Pgid == d.Pid || d.Pgid == syscall.Getpgrp() {
		t.Errorf("pgid %d ppid %d pid %d: the shim leads the group and the plugin is in it", d.Pgid, d.Ppid, d.Pid)
	}
}

// The shim catches TERM, forwards it to the plugin, waits, and exits: the session ends.
func TestDarwinShimForwardsTermToThePluginAndExits(t *testing.T) {
	r := newRig(t)
	r.start()
	d := r.diag()
	r.p.mu.Lock()
	s := r.p.sess
	r.p.mu.Unlock()
	if err := syscall.Kill(d.Ppid, syscall.SIGTERM); err != nil { // the shim only, not the group
		t.Fatal(err)
	}
	r.awaitGone(d.Pid)
	r.until("the shim exited and the adapter saw it", s.isDead)
}

// The shim passes on the plugin's exit status.
func TestDarwinShimPassesOnThePluginsExitStatus(t *testing.T) {
	t.Setenv("STUB_HELLO_FAULT", "exit-before-hello") // the stub exits 3
	r := newRig(t)
	wantUnavailableLocal(t, r.startErr(), "exit status 3")
}

// A plugin that carries com.apple.quarantine (Gatekeeper would stop it) is refused with 78 and a
// message on how to clear it.
func TestDarwinQuarantinedPluginIsRefusedAtSpawn(t *testing.T) {
	r := newRig(t)
	quarantine(t, r.bin)
	wantUnavailableLocal(t, r.startErr(), "com.apple.quarantine")
}

func TestDarwinQuarantinedPluginIsExit78ByConfigCheck(t *testing.T) {
	r := newRig(t)
	quarantine(t, r.bin)
	r.eff.HostFile = "/etc/bitc.toml"
	r.eff.Providers = map[string]*config.Provider{"stub": r.core}
	err := remote.CheckPlugins(context.Background(), r.eff, io.Discard)
	var ee *exit.Error
	if !errors.As(err, &ee) || ee.Code != 78 || !strings.Contains(ee.Line, "xattr -d com.apple.quarantine") {
		t.Fatalf("got %v, want exit 78 telling how to clear the quarantine", err)
	}
}

func quarantine(t *testing.T, path string) {
	t.Helper()
	if err := unix.Setxattr(path, "com.apple.quarantine", []byte("0083;00000000;Safari;"), 0); err != nil {
		t.Fatalf("cannot set com.apple.quarantine: %v", err)
	}
}

// The no_new_privs decision on darwin goes through the one seam darwinSetuidPolicy: set to
// refuse, no plugin runs and the config check says 78.
func TestDarwinSetuidPolicyRefuseRefusesPluginsWith78(t *testing.T) {
	old := darwinSetuidPolicy
	darwinSetuidPolicy = darwinRefuse
	defer func() { darwinSetuidPolicy = old }()
	r := newRig(t)
	r.eff.HostFile = "/etc/bitc.toml"
	r.eff.Providers = map[string]*config.Provider{"stub": r.core}
	err := remote.CheckPlugins(context.Background(), r.eff, io.Discard)
	var ee *exit.Error
	if !errors.As(err, &ee) || ee.Code != 78 {
		t.Fatalf("got %v, want exit 78", err)
	}
	if r.exists("diag.json") {
		t.Error("a plugin ran under the refuse policy")
	}
}
