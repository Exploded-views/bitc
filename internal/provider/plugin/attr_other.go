//go:build !linux

package plugin

import "syscall"

// pluginAttr off Linux has no parent-death signal. Unreached there: a non-Linux build refuses
// every [providers.*] table at config load.
func pluginAttr() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setpgid: true} }
