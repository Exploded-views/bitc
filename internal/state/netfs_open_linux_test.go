package state

import (
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/Exploded-views/bitc/internal/exit"
)

// Open refuses a state dir that statfs places on NFS (78), with the message it always had.
func TestOpenRefusesAStateDirOnNFS(t *testing.T) {
	defer func(f func(string, *unix.Statfs_t) error) { statfs = f }(statfs)
	statfs = func(_ string, sfs *unix.Statfs_t) error { sfs.Type = unix.NFS_SUPER_MAGIC; return nil }
	root := filepath.Join(t.TempDir(), "state")
	_, err := Open(root)
	if exit.Code(err) != exit.Config || !strings.Contains(err.Error(), "state dir "+root+" is on NFS; bitc needs a local filesystem for flock") {
		t.Fatalf("%v", err)
	}
}
