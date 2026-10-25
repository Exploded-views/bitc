package agent

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"
)

func tgz(t *testing.T, add func(w *tar.Writer)) string {
	t.Helper()
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	tw := tar.NewWriter(gz)
	add(tw)
	tw.Close()
	gz.Close()
	p := filepath.Join(t.TempDir(), "a.tgz")
	if err := os.WriteFile(p, b.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestUntarGzNeverWritesThroughSymlinks(t *testing.T) {
	for _, link := range []string{"ABS", "../outside", ".."} {
		root := t.TempDir()
		dest, outside := filepath.Join(root, "dest"), filepath.Join(root, "outside")
		_ = os.MkdirAll(dest, 0o755)
		_ = os.MkdirAll(outside, 0o755)
		target := link
		if link == "ABS" {
			target = outside
		}
		arc := tgz(t, func(w *tar.Writer) {
			_ = w.WriteHeader(&tar.Header{Name: "t/", Typeflag: tar.TypeDir, Mode: 0o755})
			_ = w.WriteHeader(&tar.Header{Name: "t/x", Typeflag: tar.TypeSymlink, Linkname: target})
			_ = w.WriteHeader(&tar.Header{Name: "t/x/evil", Typeflag: tar.TypeReg, Mode: 0o644, Size: 1})
			_, _ = w.Write([]byte("x"))
			_ = w.WriteHeader(&tar.Header{Name: "t/x/sub/", Typeflag: tar.TypeDir, Mode: 0o755})
		})
		_ = extractArchive(arc, "x.tgz", dest) // may refuse or skip; what matters is where bytes land
		for _, bad := range []string{filepath.Join(outside, "evil"), filepath.Join(outside, "sub"), filepath.Join(root, "evil"), filepath.Join(root, "sub")} {
			if _, err := os.Lstat(bad); err == nil {
				t.Errorf("link %q: wrote %s outside the tools dir", link, bad)
			}
		}
	}
}

func TestUntarGzKeepsInsideRelativeSymlinks(t *testing.T) {
	dest := t.TempDir()
	arc := tgz(t, func(w *tar.Writer) {
		_ = w.WriteHeader(&tar.Header{Name: "t/bin/tool-1", Typeflag: tar.TypeReg, Mode: 0o755, Size: 1})
		_, _ = w.Write([]byte("x"))
		_ = w.WriteHeader(&tar.Header{Name: "t/bin/tool", Typeflag: tar.TypeSymlink, Linkname: "tool-1"})
	})
	if err := extractArchive(arc, "x.tgz", dest); err != nil {
		t.Fatal(err)
	}
	if got, err := os.Readlink(filepath.Join(dest, "bin", "tool")); err != nil || got != "tool-1" {
		t.Fatal(got, err)
	}
}

func TestHomeFromPasswd(t *testing.T) {
	p := filepath.Join(t.TempDir(), "passwd")
	_ = os.WriteFile(p, []byte("root:x:0:0:root:/root:/bin/sh\nsvc:x:123:123::/var/lib/svc:/bin/false\n"), 0o600)
	if h, ok := homeFromPasswd(p, 123); !ok || h != "/var/lib/svc" {
		t.Fatal(h, ok)
	}
	if h, ok := homeFromPasswd(p, 0); !ok || h != "/root" {
		t.Fatal(h, ok)
	}
	if _, ok := homeFromPasswd(p, 7); ok {
		t.Fatal("found a uid that is not there")
	}
}

func TestRemoveSSHRemovesTheGivenHomesDotSSH(t *testing.T) {
	h := t.TempDir()
	_ = os.MkdirAll(filepath.Join(h, ".ssh"), 0o700)
	_ = os.WriteFile(filepath.Join(h, ".ssh", "id_ed25519"), []byte("k"), 0o600)
	removeSSH(h)
	if _, err := os.Stat(filepath.Join(h, ".ssh")); err == nil {
		t.Fatal(".ssh survived")
	}
}
