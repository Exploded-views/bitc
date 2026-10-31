package lock

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
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
	stop, done := make(chan struct{}), make(chan struct{})
	// the observer must exit before the test returns: it reads package state (inProbe) that
	// later tests write
	defer func() { close(stop); <-done }()
	go func() {
		defer close(done)
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

// #55: a non-blocking TryLock that runs while HeldRO inspects a free lock still gets it. The
// seam runs the competing TryLock inside HeldRO's probe, so the collision is forced, not hoped for.
func TestHeldRONeverMakesAFreeLockBusy(t *testing.T) {
	p := filepath.Join(t.TempDir(), "unit.1.lock")
	b, err := Open(p) // the admission's open file description; creates the file
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	ran, got := false, false
	inProbe = func() {
		ran = true
		got, _ = b.TryLock() // the same call admission makes
		if got {
			_ = b.Unlock()
		}
	}
	t.Cleanup(func() { inProbe = nil })
	if HeldRO(p) {
		t.Fatal("a free lock reads as held")
	}
	if !ran {
		t.Fatal("HeldRO never reached its probe seam")
	}
	if !got {
		t.Fatal("a TryLock during HeldRO's probe found a free lock busy (#55)")
	}
	// safe direction through the same path: a held lock still reads as held
	if ok, _ := b.TryLock(); !ok {
		t.Fatal("cannot lock")
	}
	inProbe = nil
	if !HeldRO(p) {
		t.Fatal("a held lock reads as free")
	}
}

// Where /proc/locks cannot be read (darwin, or a Linux without procfs), HeldRO falls back to
// heldNoProcLocks. A free lock reads as free and a held lock as held. On darwin (F_GETLK) the free
// lock also stays takeable during the probe; Linux's fallback is a brief try-lock, so there a
// concurrent TryLock can find it busy for that moment, as before #55.
func TestHeldROWithoutProcLocks(t *testing.T) {
	defer func(p string) { procLocksPath = p }(procLocksPath)
	procLocksPath = filepath.Join(t.TempDir(), "no-proc-locks")
	p := filepath.Join(t.TempDir(), "unit.1.lock")
	b, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if HeldRO(filepath.Join(t.TempDir(), "missing.lock")) {
		t.Fatal("a missing lock file reads as held")
	}
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
		t.Fatal("a free lock reads as held")
	}
	if !ran {
		t.Fatal("HeldRO never reached its probe seam")
	}
	if runtime.GOOS == "darwin" && !got {
		t.Fatal("a TryLock during the F_GETLK probe found a free lock busy (#55)")
	}
	inProbe = nil
	if ok, _ := b.TryLock(); !ok {
		t.Fatal("cannot lock")
	}
	if !HeldRO(p) {
		t.Fatal("a held lock reads as free")
	}
}

// The probe's safe direction, on every OS (and on the try-lock fallback): a held
// lock never reads as free, and a TryLock that loses to a probe gets the normal busy answer, never
// an error. The holder count proves flock exclusion between the contenders only; a probe's own
// brief hold is not counted. Goroutines race through a barrier; no sleeps.
func TestHeldROSafeDirection(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.lock")
	a, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if ok, _ := a.TryLock(); !ok {
		t.Fatal("cannot lock")
	}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ { // a held lock reads as held, whatever the probes race with
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				if !HeldRO(p) {
					t.Error("a held lock read as free")
					return
				}
			}
		}()
	}
	wg.Wait()
	_ = a.Unlock()

	const contenders, rounds = 4, 200
	var holders atomic.Int32
	start := make(chan struct{})
	files := make([]*File, contenders) // opened before any goroutine waits on start
	for i := range files {
		f, err := Open(p)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		files[i] = f
	}
	for _, f := range files {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for j := 0; j < rounds; j++ {
				ok, err := f.TryLock()
				if err != nil {
					t.Errorf("TryLock lost to a probe with an error, not busy: %v", err)
					return
				}
				if !ok {
					continue
				}
				if n := holders.Add(1); n != 1 {
					t.Errorf("%d holders at once", n)
				}
				if !HeldRO(p) {
					t.Error("a lock this goroutine holds read as free")
				}
				holders.Add(-1)
				_ = f.Unlock()
			}
		}()
	}
	for i := 0; i < 2; i++ { // probes racing the contenders
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for j := 0; j < rounds; j++ {
				HeldRO(p)
			}
		}()
	}
	close(start)
	wg.Wait()
}
