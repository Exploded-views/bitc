package admit

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Exploded-views/bitc/internal/config"
	"github.com/Exploded-views/bitc/internal/lock"
)

// #24: a legacy lock whose holder cannot be read (no /proc/locks) is not listed, held or not;
// with /proc/locks readable a held one is listed and a free one is not.
func TestLegacyHoldersSkipsUnknownAndListsHeld(t *testing.T) {
	c := &config.Compat{LegacyDir: t.TempDir()}
	if err := os.MkdirAll(filepath.Join(c.LegacyDir, "locks"), 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := lock.Open(filepath.Join(c.LegacyDir, "locks", "lint.1.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if ok, _ := f.TryLock(); !ok {
		t.Fatal("cannot hold the legacy lock")
	}
	free, err := lock.Open(filepath.Join(c.LegacyDir, "locks", "lint.2.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer free.Close()

	restore := lock.SetProcLocksPath(filepath.Join(t.TempDir(), "no-proc-locks"))
	if got := LegacyHolders(c); len(got) != 0 {
		t.Fatalf("unknown holders were listed: %v", got)
	}
	restore()
	if !lock.HoldersVisible() {
		t.Skip("this host cannot read /proc/locks")
	}
	got := LegacyHolders(c)
	if len(got) != 1 || got[0] != "lint.1 held" {
		t.Fatalf("holders = %v, want [lint.1 held]", got)
	}
}
