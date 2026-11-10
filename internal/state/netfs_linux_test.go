package state

import (
	"testing"

	"golang.org/x/sys/unix"
)

// Linux classifies by the statfs magic: NFS, CIFS, SMB2, SMB, FUSE, 9p and AFS (whose flock
// is local to the client) are refused and named; local ones stay.
func TestNetworkFSLinuxMagic(t *testing.T) {
	for typ, want := range map[int64]string{
		unix.NFS_SUPER_MAGIC:  "NFS",
		unix.CIFS_SUPER_MAGIC: "cifs", unix.SMB2_SUPER_MAGIC: "smb2", unix.SMB_SUPER_MAGIC: "smb", unix.FUSE_SUPER_MAGIC: "fuse",
		unix.V9FS_MAGIC: "9p", unix.AFS_SUPER_MAGIC: "afs", unix.AFS_FS_MAGIC: "afs",
		// cluster filesystems with a cluster-wide flock (CephFS, OCFS2) stay allowed.
		unix.CEPH_SUPER_MAGIC: "", unix.OCFS2_SUPER_MAGIC: "", unix.EXT4_SUPER_MAGIC: "", unix.TMPFS_MAGIC: "", unix.XFS_SUPER_MAGIC: "",
		unix.BTRFS_SUPER_MAGIC: "", unix.OVERLAYFS_SUPER_MAGIC: "",
	} {
		sfs := unix.Statfs_t{Type: typ}
		if got := networkFS(&sfs); got != want {
			t.Errorf("type %#x: %q, want %q", typ, got, want)
		}
	}
}

// A magic with the top bit set may arrive sign-extended from a 32-bit f_type; it still matches.
func TestNetworkFSLinuxSignedMagic(t *testing.T) {
	for _, m := range []uint32{unix.CIFS_SUPER_MAGIC, unix.SMB2_SUPER_MAGIC} {
		sfs := unix.Statfs_t{Type: int64(int32(m))}
		if got := networkFS(&sfs); got == "" {
			t.Errorf("negative int32 form of %#x is not refused", m)
		}
	}
}
