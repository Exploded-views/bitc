package lock

import (
	"path/filepath"
	"testing"
)

// #24: where /proc/locks cannot be read, Linux has no way to see a flock without taking it, so
// Inspect says unknown and never takes any lock. The seam runs a competing TryLock inside the
// probe: it must win.
func TestInspectWithoutProcLocksIsUnknownAndTakesNoLock(t *testing.T) {
	defer func(p string) { procLocksPath = p }(procLocksPath)
	procLocksPath = filepath.Join(t.TempDir(), "no-proc-locks")
	p := filepath.Join(t.TempDir(), "unit.1.lock")
	b, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	ran, got := false, false
	inProbe = func() {
		ran = true
		got, _ = b.TryLock()
		if got {
			_ = b.Unlock()
		}
	}
	t.Cleanup(func() { inProbe = nil })
	if s := Inspect(p); s != StateUnknown {
		t.Fatalf("a free lock without /proc/locks is %v, want unknown", s)
	}
	if !ran {
		t.Fatal("Inspect never reached its probe seam")
	}
	if !got {
		t.Fatal("a TryLock during the probe found the lock busy: the probe took a lock (#24)")
	}
	inProbe = nil
	if ok, _ := b.TryLock(); !ok {
		t.Fatal("cannot lock")
	}
	if s := Inspect(p); s != StateUnknown {
		t.Fatalf("a held lock without /proc/locks is %v, want unknown (never guessed)", s)
	}
	if HeldRO(p) {
		t.Fatal("HeldRO reports an unknown lock as held")
	}
	if s := Inspect(filepath.Join(t.TempDir(), "missing.lock")); s != StateFree {
		t.Fatalf("a missing lock file is %v, want free", s)
	}
}

func TestSetProcLocksPathAndVisible(t *testing.T) {
	restore := SetProcLocksPath(filepath.Join(t.TempDir(), "no-proc-locks"))
	if HoldersVisible() {
		t.Fatal("holders are visible without a readable /proc/locks")
	}
	restore()
	if !HoldersVisible() {
		t.Skip("this host cannot read /proc/locks")
	}
}

// A directory opens but cannot be read: Inspect says unknown, so HoldersVisible must say false.
func TestHoldersVisibleAgreesWithInspectWhenProcLocksOpensButCannotBeRead(t *testing.T) {
	defer func(p string) { procLocksPath = p }(procLocksPath)
	procLocksPath = t.TempDir()
	p := filepath.Join(t.TempDir(), "unit.1.lock")
	b, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if s := Inspect(p); s != StateUnknown {
		t.Fatalf("Inspect = %v, want unknown", s)
	}
	if HoldersVisible() {
		t.Fatal("HoldersVisible is true though Inspect cannot read /proc/locks")
	}
}

func TestSetProcLocksPathPanicsOutsideTests(t *testing.T) {
	defer func(f func() bool) { underTest = f }(underTest)
	underTest = func() bool { return false }
	defer func() {
		if recover() == nil {
			t.Fatal("SetProcLocksPath did not panic outside tests")
		}
	}()
	SetProcLocksPath("/nonexistent")
}
