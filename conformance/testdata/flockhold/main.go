// Command flockhold is the conformance stand-in for the shell callers of a one-lock-file pool
// (the flock(1) of util-linux, which macOS does not ship). It takes real flock(2) locks, so what
// bitc meets is what a shell holder makes it meet.
//
//	flockhold hold READY SPEC...   take each lock in order, blocking like flock(1); then create
//	                               the file READY and keep them until stdin closes
//	flockhold try SPEC...          take each lock without blocking, as flock -n does; exit 0 if
//	                               every one was free, 1 if any was busy
//
// A SPEC is s=PATH (shared, flock -s) or x=PATH (exclusive, flock -x).
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"
)

func take(spec string, nonblock bool) (*os.File, error) {
	mode, path, ok := strings.Cut(spec, "=")
	if !ok || (mode != "s" && mode != "x") {
		return nil, fmt.Errorf("bad spec %q", spec)
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	how := syscall.LOCK_EX
	if mode == "s" {
		how = syscall.LOCK_SH
	}
	if nonblock {
		how |= syscall.LOCK_NB
	}
	if err := syscall.Flock(int(f.Fd()), how); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: flockhold hold READY SPEC... | try SPEC...")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "hold":
		for _, spec := range os.Args[3:] {
			if _, err := take(spec, false); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(2)
			}
		}
		if err := os.WriteFile(os.Args[2], nil, 0o600); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		_, _ = io.Copy(io.Discard, os.Stdin)
	case "try":
		for _, spec := range os.Args[2:] {
			if _, err := take(spec, true); err != nil {
				if errors.Is(err, syscall.EWOULDBLOCK) {
					os.Exit(1)
				}
				fmt.Fprintln(os.Stderr, err)
				os.Exit(2)
			}
		}
	default:
		os.Exit(2)
	}
}
