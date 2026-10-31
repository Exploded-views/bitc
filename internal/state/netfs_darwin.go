//go:build darwin

package state

import "golang.org/x/sys/unix"

// networkFS names the network filesystem sfs is on, or "" for a local one. macOS has no statfs
// magic: the known network names come from f_fstypename, and any filesystem without MNT_LOCAL in
// f_flags (FUSE, which the name list misses) is a network one, named by its type.
func networkFS(sfs *unix.Statfs_t) string {
	b := make([]byte, 0, len(sfs.Fstypename))
	for _, c := range sfs.Fstypename {
		if c == 0 {
			break
		}
		b = append(b, byte(c))
	}
	name := string(b)
	if l := networkFSLabel(name); l != "" {
		return l
	}
	if sfs.Flags&unix.MNT_LOCAL == 0 {
		if name == "" {
			return "a network filesystem"
		}
		return name
	}
	return ""
}
