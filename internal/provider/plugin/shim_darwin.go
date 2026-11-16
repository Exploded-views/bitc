//go:build darwin

package plugin

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/Exploded-views/bitc/internal/provider"
	"github.com/Exploded-views/bitc/internal/provider/plugin/pin"
)

// lifelineFD is the descriptor the shim finds the read end of bitc's lifeline pipe on; fd 3 is the
// secrets pipe (A.3.3).
const lifelineFD = 4

// shimLaunch makes (or finds) the private copy of the hashed plugin, and starts bitc's own binary
// (os.Executable: macOS has no /proc/self/exe) as the shim that runs the copy.
func (p *Provider) shimLaunch(f, secR *os.File, d dirs) (shimSpec, error) {
	local := func(why string) error { return &provider.ErrUnavailable{Why: why, Local: true} }
	copyPath, err := privateCopy(f, filepath.Join(d.state, "bin"), p.core.Plugin.SHA256)
	if err != nil {
		var ch *pin.Changed
		if errors.As(err, &ch) {
			return shimSpec{}, local(ch.Error())
		}
		return shimSpec{}, local("plugin " + p.core.Plugin.Path + ": " + err.Error())
	}
	self, err := os.Executable()
	if err != nil {
		return shimSpec{}, local("cannot find the bitc binary: " + err.Error())
	}
	lifeR, lifeW, err := os.Pipe()
	if err != nil {
		return shimSpec{}, local(err.Error())
	}
	return shimSpec{exe: self, args: []string{"bitc", "__plugin-exec", p.core.Plugin.Path, copyPath, p.core.Plugin.SHA256},
		extra: []*os.File{secR, lifeR}, child: lifeR, lifeline: lifeW}, nil
}

// privateCopy returns <dir>/<sha256>, a 0500 copy of the open, hashed plugin f in a 0700
// directory. The name is the hash of the bytes, so an existing file is the same plugin and is
// never rewritten; one that no longer hashes to its name was tampered with and is refused.
func privateCopy(f *os.File, dir, sum string) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return "", err
	}
	dst := filepath.Join(dir, sum)
	if _, err := os.Lstat(dst); err == nil {
		return dst, verifyCopy(dst, sum)
	}
	tmp, err := os.CreateTemp(dir, ".copy-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	if _, err := io.Copy(tmp, f); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Chmod(0o500); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	// link, not rename: a copy another bitc made first stays the same inode
	if err := os.Link(tmp.Name(), dst); err != nil && !errors.Is(err, os.ErrExist) {
		return "", err
	}
	return dst, verifyCopy(dst, sum)
}

// verifyCopy refuses a copy that is not a plain 0500 file of the pinned bytes.
func verifyCopy(path, sum string) error {
	st, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() || st.Mode().Perm() != 0o500 || st.Mode()&(os.ModeSetuid|os.ModeSetgid) != 0 {
		return fmt.Errorf("the private copy %s is not a plain mode 0500 file", path)
	}
	got, err := pin.SumFile(path)
	if err != nil {
		return err
	}
	if got != sum {
		return &pin.Changed{Path: path, Want: sum, Got: got}
	}
	return nil
}

// ExecShim is the body of `bitc __plugin-exec <pinned path> <copy> <sha256>` on macOS. macOS has
// no execveat and no PR_SET_NO_NEW_PRIVS, so the shim stays the plugin's parent: it re-hashes the
// private copy, starts it with argv[0] the pinned path and the secrets pipe as fd 3, forwards
// TERM, INT and HUP, and exits with the plugin's status. fd 4 is bitc's lifeline: when it reads
// EOF bitc is gone (even by SIGKILL) and the shim kills the whole group, itself included.
func ExecShim(args []string) int {
	if len(args) < 3 {
		fmt.Fprintln(os.Stderr, "bitc: __plugin-exec: usage: __plugin-exec <path> <copy> <sha256>")
		return 64
	}
	pinned, copyPath, want := args[0], args[1], args[2]
	unix.CloseOnExec(lifelineFD)
	life := os.NewFile(lifelineFD, "lifeline")
	if err := verifyCopy(copyPath, want); err != nil {
		fmt.Fprintln(os.Stderr, "bitc: __plugin-exec:", err)
		return 69
	}
	cmd := exec.Command(copyPath)
	cmd.Args = []string{pinned}
	cmd.Env = os.Environ()
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.ExtraFiles = []*os.File{os.NewFile(3, "secrets")}
	sigs := make(chan os.Signal, 8)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "bitc: __plugin-exec: start:", err)
		return 69
	}
	go func() {
		for sg := range sigs {
			_ = cmd.Process.Signal(sg)
		}
	}()
	if life != nil {
		go func() {
			_, _ = io.Copy(io.Discard, life) // returns at EOF: bitc closed or died
			if syscall.Getpgrp() == os.Getpid() {
				_ = syscall.Kill(-os.Getpid(), syscall.SIGKILL)
			}
			_ = cmd.Process.Kill()
			os.Exit(137)
		}()
	}
	err := cmd.Wait()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &ee):
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return 128 + int(ws.Signal())
		}
		return ee.ExitCode()
	}
	fmt.Fprintln(os.Stderr, "bitc: __plugin-exec: wait:", err)
	return 69
}
