package state

import "testing"

// The macOS network-filesystem names (statfs f_fstypename) the state dir refuses, as Linux
// refuses NFS: flock is not reliable there (spec 9).
func TestNetworkFSLabel(t *testing.T) {
	for name, want := range map[string]string{
		"nfs": "NFS", "smbfs": "smbfs", "afpfs": "afpfs", "webdav": "webdav",
		"apfs": "", "hfs": "", "msdos": "", "exfat": "", "devfs": "", "tmpfs": "", "": "", "NFS": "", "nfsx": "",
	} {
		if got := networkFSLabel(name); got != want {
			t.Errorf("networkFSLabel(%q) = %q, want %q", name, got, want)
		}
	}
}
