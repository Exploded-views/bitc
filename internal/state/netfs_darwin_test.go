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
		sfs := fakeStatfs(name)
		sfs.Flags = unix.MNT_LOCAL // a local mount; the name list still refuses the network ones
		if got := networkFS(sfs); got != want {
			t.Errorf("%q: %q, want %q", name, got, want)
		}
	}
}

// macOS also asks statfs f_flags: a filesystem without MNT_LOCAL is a network one even when its
// type name is not in the list (FUSE: macfuse, osxfuse, fuse-t), and a local one stays local.
func TestDarwinNetworkFSMntLocal(t *testing.T) {
	for _, c := range []struct {
		name  string
		flags uint32
		want  string
	}{
		{"macfuse", 0, "macfuse"}, {"fuse-t", 0, "fuse-t"}, {"", 0, "a network filesystem"},
		{"apfs", unix.MNT_LOCAL, ""}, {"macfuse", unix.MNT_LOCAL, ""}, {"nfs", unix.MNT_LOCAL, "NFS"},
	} {
		sfs := fakeStatfs(c.name)
		sfs.Flags = c.flags
		if got := networkFS(sfs); got != c.want {
			t.Errorf("%q flags %#x: %q, want %q", c.name, c.flags, got, c.want)
		}
	}
}
