package admit

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"

	"github.com/Exploded-views/bitc/internal/config"
	"github.com/Exploded-views/bitc/internal/state"
)

func legacy(t *testing.T) (*config.Compat, string) {
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, "locks"), 0o700)
	return &config.Compat{LegacyDir: dir, HoldEnv: "APP_POOL_BIGMEM", NamePrefix: "legacy-pool-", Groups: map[string]string{"bigmem": "bigmem"}}, dir
}

func holdLegacy(t *testing.T, path string, pid int) *os.File {
	f, _ := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(strconv.Itoa(pid) + "\n")
	t.Cleanup(func() { f.Close() })
	return f
}

func TestTrySlotAndTryUnit(t *testing.T) {
	c, dir := legacy(t)
	cl := NewCompat(c, state.Layout{Root: t.TempDir()})
	holdLegacy(t, filepath.Join(dir, "locks", "bigmem.1.lock"), 1)
	if _, ok, err := cl.TryUnit("bigmem", 1, os.Getpid()); ok || err != nil {
		t.Fatal("got a held legacy unit", err)
	}
	rel, ok, _ := cl.TrySlot("db", 2)
	if !ok || !lockHeldPath(filepath.Join(dir, "locks", "db.2.lock")) {
		t.Fatal("db.2 not taken")
	}
	rel()
	if lockHeldPath(filepath.Join(dir, "locks", "db.2.lock")) {
		t.Fatal("not released")
	}
	if _, ok, _ := cl.TryUnit("other", 1, 1); !ok {
		t.Fatal("a group without a legacy stem must need nothing")
	}
}

func TestVerifiedClaim(t *testing.T) {
	c, dir := legacy(t)
	cl := NewCompat(c, state.Layout{Root: t.TempDir()})
	// The holder is this process: IsAncestor counts the caller itself, and os.Getppid() is not
	// used because under some container recipes the test binary's parent is PID 1.
	holdLegacy(t, filepath.Join(dir, "locks", "bigmem.1.lock"), os.Getpid())
	if g, ok := cl.VerifiedClaim([]string{"APP_POOL_BIGMEM=bigmem.1"}); !ok || g != "bigmem" {
		t.Fatal(g, ok)
	}
	other := exec.Command("sleep", "60") // alive, but not an ancestor
	if err := other.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = other.Process.Kill(); _ = other.Wait() })
	holdLegacy(t, filepath.Join(dir, "locks", "bigmem.3.lock"), other.Process.Pid)
	holdLegacy(t, filepath.Join(dir, "locks", "bigmem.4.lock"), 1)
	for _, bad := range []string{"APP_POOL_BIGMEM=bigmem.2", "APP_POOL_BIGMEM=bigmem.3", "APP_POOL_BIGMEM=bigmem.4",
		"APP_POOL_BIGMEM=bigmem.1/../x", "APP_POOL_BIGMEM=x", "OTHER=bigmem.1"} {
		if _, ok := cl.VerifiedClaim([]string{bad}); ok {
			t.Errorf("%s believed", bad)
		}
	}
}

func lockHeldPath(p string) bool {
	f, err := os.Open(p)
	if err != nil {
		return false
	}
	defer f.Close()
	if syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		return true
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return false
}
