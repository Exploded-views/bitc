//go:build linux

package main

import (
	"os"
	"strings"
)

// noNewPrivs is the NoNewPrivs line of /proc/self/status.
func noNewPrivs() string {
	st, _ := os.ReadFile("/proc/self/status")
	for _, l := range strings.Split(string(st), "\n") {
		if v, ok := strings.CutPrefix(l, "NoNewPrivs:"); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// openFds lists "<fd> <target>" from /proc/self/fd.
func openFds() []string {
	ents, _ := os.ReadDir("/proc/self/fd")
	var fds []string
	for _, e := range ents {
		l, _ := os.Readlink("/proc/self/fd/" + e.Name())
		fds = append(fds, e.Name()+" "+l)
	}
	return fds
}

// exePath is the running binary's path, from /proc/self/exe.
func exePath() string {
	p, _ := os.Readlink("/proc/self/exe")
	if p == "" {
		return "/proc/self/exe"
	}
	return p
}
