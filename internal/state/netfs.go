package state

// networkFSLabel maps a macOS f_fstypename to the label the state-dir refusal prints, or ""
// for a local filesystem. flock is not reliable on a network filesystem (spec 9); Linux refuses
// NFS by its statfs magic, macOS by name: NFS and its other network filesystems.
func networkFSLabel(fstypename string) string {
	switch fstypename {
	case "nfs":
		return "NFS"
	case "smbfs", "afpfs", "webdav":
		return fstypename
	}
	return ""
}
