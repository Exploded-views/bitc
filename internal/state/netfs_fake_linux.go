//go:build linux

package state

import "golang.org/x/sys/unix"

func fakeNFS(sfs *unix.Statfs_t) error { sfs.Type = unix.NFS_SUPER_MAGIC; return nil }
