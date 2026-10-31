package lock

import (
	"bufio"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// TestHelperHoldLock is the child half of the tests below, not a test of its own: it takes the
// lock named by BITC_HOLD_PATH (BITC_HOLD_MODE "sh" or "ex"), prints "ready", and holds it
// until stdin closes. F_GETLK skips the caller's own POSIX (fcntl) locks, but a flock lock
// belongs to the open file, so darwin reports it in-process too. The separate holder process
// adds coverage of cross-process LOCK_EX and LOCK_SH and of the F_WRLCK choice.
func TestHelperHoldLock(t *testing.T) {
	p := os.Getenv("BITC_HOLD_PATH")
	if p == "" {
		t.Skip("helper process only")
	}
	fd, err := unix.Open(p, unix.O_RDWR|unix.O_CREAT|unix.O_CLOEXEC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	how := unix.LOCK_EX
	if os.Getenv("BITC_HOLD_MODE") == "sh" {
		how = unix.LOCK_SH
	}
	if err := unix.Flock(fd, how|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	os.Stdout.WriteString("ready\n")
	_, _ = io.Copy(io.Discard, os.Stdin)
}

// holdInChild starts the helper holding path and returns once it reports ready; release closes
// its stdin and waits for it to exit.
func holdInChild(t *testing.T, path, mode string) (release func()) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperHoldLock$")
	cmd.Env = append(os.Environ(), "BITC_HOLD_PATH="+path, "BITC_HOLD_MODE="+mode)
	cmd.Stderr = os.Stderr
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := false
	release = func() {
		if !done {
			done = true
			in.Close()
			_ = cmd.Wait()
		}
	}
	t.Cleanup(release)
	if line, _ := bufio.NewReader(out).ReadString('\n'); line != "ready\n" {
		release()
		t.Fatalf("helper did not take the lock: %q", line)
	}
	return release
}

func TestHeldROSeesAnotherProcessHolding(t *testing.T) {
	for _, mode := range []string{"ex", "sh"} {
		t.Run(mode, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "unit.1.lock")
			release := holdInChild(t, p, mode)
			if !HeldRO(p) {
				t.Fatal("a lock held by another process reads as free")
			}
			b, err := Open(p)
			if err != nil {
				t.Fatal(err)
			}
			defer b.Close()
			if ok, _ := b.TryLock(); ok {
				t.Fatal("TryLock got a lock another process holds")
			}
			// the probe took nothing: once the holder is gone the lock is free and stays takeable
			release()
			ran, got := false, false
			inProbe = func() {
				ran = true
				got, _ = b.TryLock()
				if got {
					_ = b.Unlock()
				}
			}
			t.Cleanup(func() { inProbe = nil })
			if HeldRO(p) {
				t.Fatal("a released lock reads as held")
			}
			if !ran || !got {
				t.Fatalf("probe seam ran=%v, TryLock during probe ok=%v", ran, got)
			}
		})
	}
}
