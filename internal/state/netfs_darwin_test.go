package state

import (
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"
)

func fakeStatfs(name string) *unix.Statfs_t {
	var sfs unix.Statfs_t
	b := unsafe.Slice((*byte)(unsafe.Pointer(&sfs.Fstypename[0])), len(sfs.Fstypename))
	copy(b[:len(b)-1], name) // keeps the NUL whatever the element type ([16]byte or [16]int8)
	return &sfs
}

// macOS classifies by f_fstypename: nfs, smbfs, afpfs and webdav are refused.
func TestDarwinNetworkFSFstypename(t *testing.T) {
	for name, want := range map[string]string{"nfs": "NFS", "smbfs": "smbfs", "afpfs": "afpfs", "webdav": "webdav",
		"apfs": "", "hfs": "", "devfs": ""} {
		if got := networkFS(fakeStatfs(name)); got != want {
			t.Errorf("%q: %q, want %q", name, got, want)
		}
	}
}
