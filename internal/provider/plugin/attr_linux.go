//go:build linux

package plugin

import "syscall"

// pluginAttr: the plugin's own group, and SIGKILL if bitc dies first.
func pluginAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
}
