//go:build !darwin

package lock

import "os"

// inspectNoProcLocks is Inspect without /proc/locks: Linux cannot see a flock through F_GETLK,
// and a try-lock would make a free lock look busy to a concurrent admission, so it does not look
// at all (#24). The file exists (Inspect checked); the answer is unknown. inProbe runs here so a
// test can prove that nothing is locked at this point.
func inspectNoProcLocks(string) State {
	if inProbe != nil {
		inProbe()
	}
	return StateUnknown
}

// HoldersVisible reports whether Inspect can tell held from free: on Linux, whether
// /proc/locks is readable.
func HoldersVisible() bool {
	f, err := os.Open(procLocksPath)
	if err != nil {
		return false
	}
	_ = f.Close()
	return true
}
