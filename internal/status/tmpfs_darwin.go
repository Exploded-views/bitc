//go:build darwin

package status

import "golang.org/x/sys/unix"

// onTmpfs reads macOS's f_fstypename (there is no statfs magic).
func onTmpfs(st *unix.Statfs_t) bool {
	var b []byte
	for _, c := range st.Fstypename {
		if c == 0 {
			break
		}
		b = append(b, byte(c))
	}
	return string(b) == "tmpfs"
}
