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

// Each newly refused magic gives the same 78, naming the filesystem.
func TestOpenRefusesAStateDirOnOtherNetworkFS(t *testing.T) {
	defer func(f func(string, *unix.Statfs_t) error) { statfs = f }(statfs)
	for typ, name := range map[int64]string{unix.CIFS_SUPER_MAGIC: "cifs", unix.SMB2_SUPER_MAGIC: "smb2", unix.FUSE_SUPER_MAGIC: "fuse"} {
		statfs = func(_ string, sfs *unix.Statfs_t) error { sfs.Type = typ; return nil }
		root := filepath.Join(t.TempDir(), "state")
		_, err := Open(root)
		if exit.Code(err) != exit.Config || !strings.Contains(err.Error(), "state dir "+root+" is on "+name+"; bitc needs a local filesystem for flock") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// The state dir refusal names the fix on one line; the accounting dir, which BITC_DIR cannot move, gets none.
func TestNetworkFSRefusalHint(t *testing.T) {
	defer func(f func(string, *unix.Statfs_t) error) { statfs = f }(statfs)
	statfs = func(_ string, sfs *unix.Statfs_t) error { sfs.Type = unix.FUSE_SUPER_MAGIC; return nil }
	err := checkFS("state dir", "/x")
	want := "state dir /x is on fuse; bitc needs a local filesystem for flock; set BITC_DIR (or --state-dir) to a local path (in a container, a tmpfs or a named volume)"
	if err == nil || err.Error() != "bitc: "+want {
		t.Errorf("%v, want %q", err, want)
	}
	err = checkFS("accounting dir", "/x")
	want = "accounting dir /x is on fuse; bitc needs a local filesystem for flock"
	if err == nil || err.Error() != "bitc: "+want {
		t.Errorf("%v, want %q", err, want)
	}
}
