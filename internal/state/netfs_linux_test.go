package state

import (
	"testing"

	"golang.org/x/sys/unix"
)

// Linux classifies by the statfs magic: only NFS is refused (unchanged from before the macOS build).
func TestNetworkFSLinuxMagic(t *testing.T) {
	for typ, want := range map[int64]string{
		unix.NFS_SUPER_MAGIC: "NFS", unix.EXT4_SUPER_MAGIC: "", unix.TMPFS_MAGIC: "", unix.XFS_SUPER_MAGIC: "",
		unix.BTRFS_SUPER_MAGIC: "", unix.OVERLAYFS_SUPER_MAGIC: "",
	} {
		sfs := unix.Statfs_t{Type: typ}
		if got := networkFS(&sfs); got != want {
			t.Errorf("type %#x: %q, want %q", typ, got, want)
		}
	}
}
