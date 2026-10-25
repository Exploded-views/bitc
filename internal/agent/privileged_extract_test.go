//go:build privileged

package agent

import (
	"archive/tar"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// TestPrivilegedAgentToolArchive runs installTool as root (the real agent's identity) on a
// repo-controlled .tar holding setuid/setgid files owned by the job user. Non-root runs cannot
// show this: GNU tar as root keeps setuid bits and owners, as non-root it already drops them.
func TestPrivilegedAgentToolArchive(t *testing.T) {
	if os.Geteuid() != 0 || os.Getenv("BITC_PRIVILEGED_TEST") != "1" {
		t.Skip("runs only as root in a throwaway container: sh scripts/privileged.sh")
	}
	body := tarBytes(t, func(w *tar.Writer) {
		for name, mode := range map[string]int64{"t/su": 0o4755, "t/sg": 0o2777} {
			_ = w.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: mode, Size: 1, Uid: 10001, Gid: 10001})
			_, _ = w.Write([]byte("x"))
		}
	})
	dir, _, err := serveInstall(t, "a.tar", body)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"su", "sg"} {
		st, err := os.Stat(filepath.Join(dir, n))
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 || st.Mode().Perm()&^0o755 != 0 {
			t.Errorf("%s: mode %v keeps special or write bits", n, st.Mode())
		}
		if uid := st.Sys().(*syscall.Stat_t).Uid; uid != 0 {
			t.Errorf("%s: owned by uid %d, want 0", n, uid)
		}
	}
}
