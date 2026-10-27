package plugin

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Exploded-views/bitc/internal/proc"
	"github.com/Exploded-views/bitc/internal/provider"
)

func wantUnavailableLocal(t *testing.T, err error, frag string) {
	t.Helper()
	var ua *provider.ErrUnavailable
	if !errors.As(err, &ua) || !ua.Local || !strings.Contains(ua.Why, frag) {
		t.Fatalf("got %T %v, want ErrUnavailable{Local} containing %q", err, err, frag)
	}
}

func envMap(env []string) map[string]string {
	m := map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		m[k] = v
	}
	return m
}

func TestSpawnEnvironmentIsBuiltFromEmpty(t *testing.T) {
	t.Setenv("LEAKY_VARIABLE", "must-not-pass")
	t.Setenv("STUB_FALLBACK", "attach")
	r := newRig(t)
	r.start()
	d := r.diag()
	env := envMap(d.Env)
	home := os.Getenv("HOME")
	if u, err := user.Current(); err == nil && u.HomeDir != "" {
		home = u.HomeDir
	}
	want := map[string]string{"PATH": "/usr/sbin:/usr/bin:/sbin:/bin", "HOME": home, "LANG": "C.UTF-8", "BITC_PLUGIN_STATE": r.own,
		"BITC_PLUGIN_SHARED": filepath.Join(r.acct, "remote", "stub", "plugin"), "BITC_PLUGIN_PROTOCOL": "1", "BITC_SECRETS_FD": "3", "STUB_FALLBACK": "attach"}
	for k, v := range want {
		if env[k] != v {
			t.Errorf("%s = %q, want %q", k, env[k], v)
		}
	}
	if !strings.HasPrefix(env["TMPDIR"], filepath.Join(r.own, "tmp")+"/") {
		t.Errorf("TMPDIR = %q", env["TMPDIR"])
	}
	for k := range env {
		if _, ok := want[k]; !ok && k != "TMPDIR" {
			t.Errorf("unexpected variable %s", k)
		}
	}
	if st, err := os.Stat(want["BITC_PLUGIN_SHARED"]); err != nil || st.Mode().Perm() != 0o700 {
		t.Errorf("BITC_PLUGIN_SHARED %v %v", st, err)
	}
}

func TestSpawnRunsThePinnedFdUnderNoNewPrivsInItsOwnProcessGroup(t *testing.T) {
	r := newRig(t)
	r.start()
	d := r.diag()
	if d.NoNewPrivs != "1" {
		t.Errorf("NoNewPrivs = %q", d.NoNewPrivs)
	}
	if d.Exe != r.bin || len(d.Args) == 0 || d.Args[0] != r.bin {
		t.Errorf("exe %q args %q, want the pinned path %q", d.Exe, d.Args, r.bin)
	}
	if d.Pgid != d.Pid || d.Pgid == syscall.Getpgrp() {
		t.Errorf("pgid %d pid %d (ours %d): the plugin must lead its own process group", d.Pgid, d.Pid, syscall.Getpgrp())
	}
	for _, fd := range d.Fds {
		if strings.HasSuffix(fd, "bitc-provider-stub") {
			t.Errorf("the pin fd leaked into the plugin: %q", fd)
		}
	}
}

func TestSecretsArriveOnFd3AndNowhereElse(t *testing.T) {
	r := newRig(t)
	sf := filepath.Join(r.dir, "tok")
	_ = os.WriteFile(sf, []byte("tok-bytes-777\n"), 0o600)
	r.core.Plugin.SecretFiles["token_secret"] = sf
	r.start()
	d := r.diag()
	if want := `{"api_key":"sk-test-key-1","token_secret":"tok-bytes-777\n"}`; d.Secrets != want {
		t.Errorf("fd 3 = %q, want %q", d.Secrets, want)
	}
	for _, s := range []string{"sk-test-key-1", "tok-bytes-777"} {
		if strings.Contains(d.Cmdline, s) || strings.Contains(strings.Join(d.Env, "\n"), s) {
			t.Errorf("secret %q is in argv or env", s)
		}
	}
	if !r.red.Contains("sk-test-key-1") || !r.red.Contains("tok-bytes-777") {
		t.Error("spawn-time secrets did not join the redactor")
	}
}

func TestSecretFileThatIsLooseOrMissingIsUnavailableLocal(t *testing.T) {
	r := newRig(t)
	sf := filepath.Join(r.dir, "tok")
	_ = os.WriteFile(sf, []byte("x"), 0o644)
	_ = os.Chmod(sf, 0o644)
	r.core.Plugin.SecretFiles["token_secret"] = sf
	wantUnavailableLocal(t, r.startErr(), "token_secret")
	r.core.Plugin.SecretFiles["token_secret"] = filepath.Join(r.dir, "absent")
	wantUnavailableLocal(t, r.startErr(), "token_secret")
}

func TestTmpdirIsPerBitcProcessAndStalePeersArePruned(t *testing.T) {
	r := newRig(t)
	tmp := filepath.Join(r.own, "tmp")
	_ = os.MkdirAll(tmp, 0o700)
	self, _ := proc.ReadStat(os.Getpid())
	init1, _ := proc.ReadStat(1)
	stale := []string{"999999999-1", fmt.Sprintf("%d-%d", os.Getpid(), self.Start+1), "garbage"}
	live := fmt.Sprintf("1-%d", init1.Start)
	for _, n := range append(stale, live) {
		_ = os.MkdirAll(filepath.Join(tmp, n), 0o700)
	}
	r.start()
	d := r.diag()
	if want := filepath.Join(tmp, fmt.Sprintf("%d-%d", os.Getpid(), self.Start)); d.Tmpdir != want {
		t.Errorf("TMPDIR = %q, want %q", d.Tmpdir, want)
	}
	if st, err := os.Stat(d.Tmpdir); err != nil || st.Mode().Perm() != 0o700 {
		t.Errorf("TMPDIR %v %v", st, err)
	}
	for _, n := range stale {
		if _, err := os.Stat(filepath.Join(tmp, n)); err == nil {
			t.Errorf("stale dir %s was not pruned", n)
		}
	}
	if _, err := os.Stat(filepath.Join(tmp, live)); err != nil {
		t.Errorf("a live peer's dir was removed: %v", err)
	}
	r.p.Close()
	if _, err := os.Stat(d.Tmpdir); err == nil {
		t.Error("TMPDIR still exists after the plugin exited")
	}
}

func TestSpawnRefusesAPluginThatChangedSinceLoad(t *testing.T) {
	r := newRig(t)
	f, _ := os.OpenFile(r.bin, os.O_WRONLY|os.O_APPEND, 0)
	_, _ = f.Write([]byte{0})
	f.Close()
	err := r.startErr()
	want := fmt.Sprintf("plugin %s changed since load (sha256 %s, now ", r.bin, r.core.Plugin.SHA256[:12])
	wantUnavailableLocal(t, err, want)
	if r.exists("diag.json") {
		t.Error("the changed plugin ran")
	}
}

func TestSpawnRefusesBitsSetBetweenLoadAndSpawn(t *testing.T) {
	r := newRig(t)
	if err := os.Chmod(r.bin, os.ModeSetuid|0o755); err != nil {
		t.Fatal(err)
	}
	wantUnavailableLocal(t, r.startErr(), "plugin "+r.bin+": insecure: setuid/setgid/capabilities")
	if r.exists("diag.json") {
		t.Error("the setuid plugin ran")
	}
}

func killPlugin(t *testing.T, r *rig) {
	t.Helper()
	r.p.mu.Lock()
	s := r.p.sess
	r.p.mu.Unlock()
	if err := syscall.Kill(r.diag().Pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	r.until("the adapter saw the plugin die", s.isDead)
}

func TestADeadPluginIsRespawnedOnceThenUnavailableForTheProcess(t *testing.T) {
	r := newRig(t)
	r.start()
	ctx := context.Background()
	if err := r.p.Preflight(ctx, r.job()); err != nil {
		t.Fatal(err)
	}
	first := r.diag().Pid
	killPlugin(t, r)
	if err := r.p.Preflight(ctx, r.job()); err != nil {
		t.Fatalf("after the first death the next call respawns: %v", err)
	}
	second := r.diag().Pid
	if second == first {
		t.Fatal("not respawned")
	}
	killPlugin(t, r)
	wantUnavailableLocal(t, r.p.Preflight(ctx, r.job()), "plugin")
	wantUnavailableLocal(t, r.p.Teardown(ctx, "x"), "plugin")
}

func TestClosingStopsThePluginWithinItsGrace(t *testing.T) {
	r := newRig(t)
	r.start()
	pid := r.diag().Pid
	r.p.Close()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := proc.ReadStat(pid); err != nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("plugin still running after Close")
}
