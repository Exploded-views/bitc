package state

import (
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// StubNetworkFS makes statfs of a path under `under` (every path if "") report NFS until the
// returned function is called. For tests in other packages; the seam itself is unexported. It
// swaps a global without a lock, so a test using it must not run in parallel.
func StubNetworkFS(under string) (restore func()) {
	if !testing.Testing() {
		panic("state.StubNetworkFS is test-only")
	}
	old := statfs
	statfs = func(p string, sfs *unix.Statfs_t) error {
		if strings.HasPrefix(p, under) {
			return fakeNFS(sfs)
		}
		return old(p, sfs)
	}
	return func() { statfs = old }
}
