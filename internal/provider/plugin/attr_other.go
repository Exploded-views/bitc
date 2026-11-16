//go:build !linux

package plugin

import "syscall"

// pluginAttr off Linux has no parent-death signal: on darwin the shim's lifeline (shim_darwin.go) stands in.
func pluginAttr() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setpgid: true} }
