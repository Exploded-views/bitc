//go:build linux

package state

import "golang.org/x/sys/unix"

// networkFS names the network filesystem sfs is on, or "" for a local one, by its statfs magic.
// Refused: filesystems whose flock is local to the client or unreliable (NFS, CIFS, SMB2, SMB,
// 9p, AFS) and every FUSE mount (sshfs, rclone, s3fs; a local FUSE filesystem too, since the
// magic cannot tell them apart, and bind mounts in Docker Desktop or another VM-backed container
// runtime on a Mac, which report FUSE). CephFS and OCFS2 stay allowed: their flock is cluster-wide, except under the
// localflocks mount option, which this check cannot see.
func networkFS(sfs *unix.Statfs_t) string {
	switch uint32(sfs.Type) {
	case unix.NFS_SUPER_MAGIC:
		return "NFS"
	case unix.CIFS_SUPER_MAGIC:
		return "cifs"
	case unix.SMB2_SUPER_MAGIC:
		return "smb2"
	case unix.SMB_SUPER_MAGIC:
		return "smb"
	case unix.FUSE_SUPER_MAGIC:
		return "fuse"
	case unix.V9FS_MAGIC:
		return "9p"
	case unix.AFS_SUPER_MAGIC, unix.AFS_FS_MAGIC:
		return "afs"
	}
	return ""
}
