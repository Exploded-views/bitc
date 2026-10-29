package agent

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func tarBytes(t *testing.T, add func(w *tar.Writer)) []byte {
	t.Helper()
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	add(tw)
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// serveInstall serves body at /name and runs installTool on it; it returns the tool dir and error.
func serveInstall(t *testing.T, name string, body []byte) (string, string, error) {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(body) }))
	t.Cleanup(srv.Close)
	sum := sha256.Sum256(body)
	root := t.TempDir()
	dir, err := installTool(srv.Client(), root, Tool{Name: "tl", URL: srv.URL + "/" + name, SHA256: hex.EncodeToString(sum[:])})
	return dir, root, err
}

func wantRepoErr(t *testing.T, what string, err error) {
	t.Helper()
	var se *stepError
	if !errors.As(err, &se) || !se.Repo {
		t.Errorf("%s: want a repo-class stepError, got %v", what, err)
	}
}

func regTar(w *tar.Writer, name string, mode int64) {
	_ = w.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: mode, Size: 1})
	_, _ = w.Write([]byte("x"))
}

func TestInstallToolMasksSetuidAndWriteBits(t *testing.T) {
	body := tarBytes(t, func(w *tar.Writer) {
		regTar(w, "t/bin/su", 0o4755)
		regTar(w, "t/bin/gw", 0o2777)
		regTar(w, "t/bin/sticky", 0o1666)
	})
	dir, _, err := serveInstall(t, "a.tar", body)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"su", "gw", "sticky"} {
		st, err := os.Stat(filepath.Join(dir, n))
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 || st.Mode().Perm()&^0o755 != 0 {
			t.Errorf("%s: mode %v keeps special or write bits", n, st.Mode())
		}
	}
}

func TestInstallToolRefusesSpecialEntries(t *testing.T) {
	for name, h := range map[string]*tar.Header{
		"char":     {Name: "t/dev", Typeflag: tar.TypeChar, Mode: 0o666, Devmajor: 1, Devminor: 3},
		"block":    {Name: "t/dev", Typeflag: tar.TypeBlock, Mode: 0o666, Devmajor: 7, Devminor: 0},
		"fifo":     {Name: "t/pipe", Typeflag: tar.TypeFifo, Mode: 0o666},
		"hardlink": {Name: "t/l", Typeflag: tar.TypeLink, Linkname: "t/x"},
	} {
		body := tarBytes(t, func(w *tar.Writer) {
			regTar(w, "t/x", 0o644)
			_ = w.WriteHeader(h)
		})
		_, _, err := serveInstall(t, "a.tar", body)
		if err == nil {
			t.Errorf("%s: archive accepted", name)
			continue
		}
		wantRepoErr(t, name, err)
	}
}

func TestInstallToolUnknownSuffixIsRepoError(t *testing.T) {
	body := tarBytes(t, func(w *tar.Writer) { regTar(w, "t/x", 0o644) })
	_, _, err := serveInstall(t, "a.zip", body)
	if err == nil {
		t.Fatal("unsupported suffix accepted")
	}
	wantRepoErr(t, "a.zip", err)
}

func TestInstallToolFormatsExtractTheSame(t *testing.T) {
	plain := tarBytes(t, func(w *tar.Writer) {
		regTar(w, "t/bin/tool", 0o755)
		regTar(w, "t/share/doc", 0o644)
	})
	var gzb bytes.Buffer
	gz := gzip.NewWriter(&gzb)
	_, _ = gz.Write(plain)
	_ = gz.Close()
	cases := map[string][]byte{"a.tar": plain, "a.tar.gz": gzb.Bytes(), "a.tgz": gzb.Bytes()}
	if _, err := exec.LookPath("xz"); err == nil {
		cases["a.tar.xz"] = pipeThrough(t, plain, "xz", "-zc")
		cases["a.txz"] = cases["a.tar.xz"]
	}
	if _, err := exec.LookPath("zstd"); err == nil {
		cases["a.tar.zst"] = pipeThrough(t, plain, "zstd", "-c")
	}
	for name, body := range cases {
		dir, _, err := serveInstall(t, name, body)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if filepath.Base(dir) != "bin" {
			t.Errorf("%s: dir %s", name, dir)
		}
		if st, err := os.Stat(filepath.Join(dir, "tool")); err != nil || st.Mode().Perm() != 0o755 {
			t.Errorf("%s: tool %v %v", name, st, err)
		}
		if _, err := os.Stat(filepath.Join(dir, "..", "share", "doc")); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := exec.LookPath("xz"); err != nil {
		t.Log("xz not installed in this environment: .tar.xz/.txz cases skipped")
	}
}

func pipeThrough(t *testing.T, in []byte, name string, args ...string) []byte {
	t.Helper()
	c := exec.Command(name, args...)
	c.Stdin = bytes.NewReader(in)
	out, err := c.Output()
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// A symlink may point at another of the archive's links: the in-tree check must follow them, not
// clean the path lexically. sub/s -> .. is dest itself; sub/u -> s/../.. reads as dest when
// cleaned, but resolves to dest's parent.
func TestExtractTarRefusesAChainedLinkThatLeaves(t *testing.T) {
	body := tarBytes(t, func(w *tar.Writer) {
		_ = w.WriteHeader(&tar.Header{Name: "t/sub/", Typeflag: tar.TypeDir, Mode: 0o755})
		_ = w.WriteHeader(&tar.Header{Name: "t/sub/s", Typeflag: tar.TypeSymlink, Linkname: ".."})
		_ = w.WriteHeader(&tar.Header{Name: "t/sub/u", Typeflag: tar.TypeSymlink, Linkname: "s/../.."})
	})
	dest := filepath.Join(t.TempDir(), "tool")
	if err := os.Mkdir(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	err := extractTar(bytes.NewReader(body), dest)
	if !errors.Is(err, errUnsafeArchive) {
		real, _ := filepath.EvalSymlinks(filepath.Join(dest, "sub", "u"))
		t.Fatalf("got %v; sub/u resolves to %q, outside %q", err, real, dest)
	}
}

// Links between the archive's own entries that stay inside still extract.
func TestExtractTarKeepsChainedLinksThatStayInside(t *testing.T) {
	body := tarBytes(t, func(w *tar.Writer) {
		_ = w.WriteHeader(&tar.Header{Name: "t/lib/a/", Typeflag: tar.TypeDir, Mode: 0o755})
		regTar(w, "t/lib/a/tool", 0o755)
		_ = w.WriteHeader(&tar.Header{Name: "t/cur", Typeflag: tar.TypeSymlink, Linkname: "lib/a"})
		_ = w.WriteHeader(&tar.Header{Name: "t/bin/tool", Typeflag: tar.TypeSymlink, Linkname: "../cur/tool"})
	})
	dest := filepath.Join(t.TempDir(), "tool")
	if err := os.Mkdir(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := extractTar(bytes.NewReader(body), dest); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(dest, "bin", "tool")); err != nil || string(b) != "x" {
		t.Fatalf("bin/tool: %q %v", b, err)
	}
}

// A .. after a name could climb out of a link that a later entry puts at that name, so .. is
// allowed only at the start of a link target.
func TestExtractTarRefusesDotDotAfterAName(t *testing.T) {
	for _, link := range []string{"a/../b", "../cur/../bin", "./x/.."} {
		body := tarBytes(t, func(w *tar.Writer) {
			_ = w.WriteHeader(&tar.Header{Name: "t/lib/l", Typeflag: tar.TypeSymlink, Linkname: link})
		})
		dest := filepath.Join(t.TempDir(), "tool")
		if err := os.Mkdir(dest, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := extractTar(bytes.NewReader(body), dest); !errors.Is(err, errUnsafeArchive) {
			t.Errorf("%s: got %v", link, err)
		}
	}
}

// A symlink entry that cannot be created fails the extract: a second entry at the same name
// (EEXIST), an empty target, and a parent that is a file must not leave the archive half-extracted
// without an error.
func TestExtractTarFailsWhenASymlinkCannotBeCreated(t *testing.T) {
	cases := map[string]func(w *tar.Writer){
		"duplicate": func(w *tar.Writer) {
			_ = w.WriteHeader(&tar.Header{Name: "t/l", Typeflag: tar.TypeSymlink, Linkname: "x"})
			_ = w.WriteHeader(&tar.Header{Name: "t/l", Typeflag: tar.TypeSymlink, Linkname: "y"})
		},
		"empty target": func(w *tar.Writer) {
			_ = w.WriteHeader(&tar.Header{Name: "t/l", Typeflag: tar.TypeSymlink})
		},
		"parent is a file": func(w *tar.Writer) {
			regTar(w, "t/f", 0o644)
			_ = w.WriteHeader(&tar.Header{Name: "t/f/l", Typeflag: tar.TypeSymlink, Linkname: "x"})
		},
	}
	for name, add := range cases {
		dest := filepath.Join(t.TempDir(), "tool")
		if err := os.Mkdir(dest, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := extractTar(bytes.NewReader(tarBytes(t, add)), dest); err == nil {
			t.Errorf("%s: extract reported success without creating the link", name)
		}
	}
}

// A link entry may not be created under an earlier link: a real dir never turns into a link, so a
// leading .. in a link target always climbs real dirs.
func TestExtractTarRefusesALinkCreatedUnderALink(t *testing.T) {
	body := tarBytes(t, func(w *tar.Writer) {
		_ = w.WriteHeader(&tar.Header{Name: "t/sub/", Typeflag: tar.TypeDir, Mode: 0o755})
		_ = w.WriteHeader(&tar.Header{Name: "t/a", Typeflag: tar.TypeSymlink, Linkname: "sub"})
		_ = w.WriteHeader(&tar.Header{Name: "t/a/b", Typeflag: tar.TypeSymlink, Linkname: "x"})
	})
	dest := filepath.Join(t.TempDir(), "tool")
	if err := os.Mkdir(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := extractTar(bytes.NewReader(body), dest); !errors.Is(err, errUnsafeArchive) {
		t.Fatalf("got %v", err)
	}
}
