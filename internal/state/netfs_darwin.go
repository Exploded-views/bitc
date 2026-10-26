//go:build darwin

package state

import "golang.org/x/sys/unix"

// networkFS names the network filesystem sfs is on, or "" for a local one. macOS has no statfs
// magic: it is read from f_fstypename.
func networkFS(sfs *unix.Statfs_t) string {
	b := make([]byte, 0, len(sfs.Fstypename))
	for _, c := range sfs.Fstypename {
		if c == 0 {
			break
		}
		b = append(b, byte(c))
	}
	return networkFSLabel(string(b))
}
