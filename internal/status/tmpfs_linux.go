//go:build linux

package status

import "golang.org/x/sys/unix"

func onTmpfs(st *unix.Statfs_t) bool { return st.Type == unix.TMPFS_MAGIC }
