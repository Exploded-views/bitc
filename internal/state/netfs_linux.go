//go:build linux

package state

import "golang.org/x/sys/unix"

// networkFS names the network filesystem sfs is on, or "" for a local one: NFS, by its magic.
func networkFS(sfs *unix.Statfs_t) string {
	if sfs.Type == unix.NFS_SUPER_MAGIC {
		return "NFS"
	}
	return ""
}
