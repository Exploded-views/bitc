//go:build !linux

package lock

import "golang.org/x/sys/unix"

// devMajorMinor splits st's device number (an int32 on darwin).
func devMajorMinor(st *unix.Stat_t) (uint32, uint32) {
	return unix.Major(uint64(st.Dev)), unix.Minor(uint64(st.Dev))
}
