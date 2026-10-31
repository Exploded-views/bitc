//go:build darwin

package state

import (
	"unsafe"

	"golang.org/x/sys/unix"
)

func fakeNFS(sfs *unix.Statfs_t) error {
	b := unsafe.Slice((*byte)(unsafe.Pointer(&sfs.Fstypename[0])), len(sfs.Fstypename))
	copy(b[:len(b)-1], "nfs")
	return nil
}
