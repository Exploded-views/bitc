//go:build linux

package lock

import "golang.org/x/sys/unix"

// devMajorMinor splits st's device number as /proc/locks prints it.
func devMajorMinor(st *unix.Stat_t) (uint32, uint32) { return unix.Major(st.Dev), unix.Minor(st.Dev) }
