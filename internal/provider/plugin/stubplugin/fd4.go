package main

import "golang.org/x/sys/unix"

// fd4AtStart is whether descriptor 4 was open when the process started, read in package
// initialisation before this program opens any file. bitc's lifeline (darwin) and the pinned
// plugin (Linux) both arrive on fd 4 of the shim and must not reach the plugin.
var fd4AtStart = func() bool { _, err := unix.FcntlInt(4, unix.F_GETFD, 0); return err == nil }()
