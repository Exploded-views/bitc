package lock

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestTryLockExcludesAnotherOpenFileDescription(t *testing.T) {
	p := filepath.Join(t.TempDir(), "a", "slot.1.lock")
	a, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if ok, err := a.TryLock(); !ok || err != nil {
		t.Fatal(ok, err)
	}
	b, _ := Open(p)
	defer b.Close()
	if ok, _ := b.TryLock(); ok {
		t.Fatal("second open file description got the lock")
	}
	if !Held(p) {
		t.Fatal("Held = false while locked")
	}
	_ = a.Unlock()
	if Held(p) {
		t.Fatal("Held = true after unlock")
	}
	if ok, _ := b.TryLock(); !ok {
		t.Fatal("not free after unlock")
	}
}

func TestCloexecAndMode(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.lock")
	f, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	flags, err := fcntlGetfd(f.Fd())
	if err != nil || flags&syscall.FD_CLOEXEC == 0 {
		t.Fatalf("FD_CLOEXEC not set: %v", err)
	}
	st, _ := os.Stat(p)
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %o", st.Mode().Perm())
	}
}

func TestLockWaitIsBounded(t *testing.T) {
	p := filepath.Join(t.TempDir(), "admit.lock")
	a, _ := Open(p)
	defer a.Close()
	_, _ = a.TryLock()
	b, _ := Open(p)
	defer b.Close()
	t0 := time.Now()
	if err := b.LockWait(context.Background(), 150*time.Millisecond); err == nil {
		t.Fatal("got a held lock")
	}
	if time.Since(t0) > time.Second {
		t.Fatal("not bounded")
	}
}

func TestRewrite(t *testing.T) {
	p := filepath.Join(t.TempDir(), "t.json")
	f, _ := Open(p)
	defer f.Close()
	_ = f.Rewrite([]byte("long content here"))
	_ = f.Rewrite([]byte("short"))
	b, _ := os.ReadFile(p)
	if string(b) != "short" {
		t.Fatalf("%q", b)
	}
}

func TestHeldROSeesLocksWithoutTakingThem(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.lock")
	if HeldRO(p) {
		t.Fatal("a missing file is held")
	}
	a, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if HeldRO(p) {
		t.Fatal("a free lock is held")
	}
	if ok, _ := a.TryLock(); !ok {
		t.Fatal("cannot lock")
	}
	if !HeldRO(p) {
		t.Fatal("a held lock is not seen")
	}
	_ = a.Unlock()
	// an observer never makes a free lock busy for a concurrent non-blocking caller
	b, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
				HeldRO(p)
			}
		}
	}()
	for i := 0; i < 2000; i++ {
		if ok, _ := b.TryLock(); !ok {
			t.Fatal("HeldRO made a free lock busy")
		}
		_ = b.Unlock()
	}
}
