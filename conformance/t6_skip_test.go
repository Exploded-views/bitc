package conformance

import (
	"path/filepath"
	"testing"

	"github.com/Exploded-views/bitc/internal/lock"
)

// T6 reads admit.lock through lock.Inspect. Where lock holders cannot be read, every read is
// "unknown", the stop loop always accepts its first stop and the admit check can never fail, so T6
// must skip there rather than pass without testing anything.
func TestT6_SkipsWhereLockHoldersCannotBeRead(t *testing.T) {
	if !lock.HoldersVisible() {
		t.Skip("this host cannot read lock holders either")
	}
	restore := lock.SetProcLocksPath(filepath.Join(t.TempDir(), "no-proc-locks"))
	defer restore()
	if lock.HoldersVisible() {
		t.Skip("lock holders are always visible on this OS")
	}
	var sub *testing.T
	t.Run("blind", func(st *testing.T) {
		sub = st
		t6NeedHolders(st)
	})
	if !sub.Skipped() {
		t.Fatal("t6NeedHolders did not skip on a host that cannot read lock holders")
	}
}
