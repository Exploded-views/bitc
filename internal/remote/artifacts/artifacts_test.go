package artifacts

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Exploded-views/bitc/internal/config"
)

type entry struct {
	name, body string
	typ        byte
	link       string
}

func archive(t *testing.T, entries ...entry) string {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		h := &tar.Header{Name: e.name, Typeflag: e.typ, Mode: 0o644, Size: int64(len(e.body)), Linkname: e.link}
		if e.typ == tar.TypeDir || e.typ == tar.TypeSymlink {
			h.Size = 0
		}
		_ = tw.WriteHeader(h)
		if e.typ == tar.TypeReg {
			_, _ = tw.Write([]byte(e.body))
		}
	}
	tw.Close()
	gz.Close()
	p := filepath.Join(t.TempDir(), "a.tar.gz")
	_ = os.WriteFile(p, buf.Bytes(), 0o600)
	return p
}

var lim = config.ArtifactLimits{MaxCompressed: 1 << 20, MaxUnpacked: 1 << 20, MaxEntries: 100}
var cov = []config.Artifact{{Path: "coverage", Replace: true}}

func TestValidate(t *testing.T) {
	ok := archive(t, entry{"coverage/", "", tar.TypeDir, ""}, entry{"coverage/a.txt", "a", tar.TypeReg, ""})
	if err := Validate(ok, cov, lim); err != nil {
		t.Fatal(err)
	}
	bad := map[string]string{
		archive(t, entry{"other.txt", "x", tar.TypeReg, ""}):                           "outside the declared paths",
		archive(t, entry{"/coverage/a", "x", tar.TypeReg, ""}):                         "absolute",
		archive(t, entry{"coverage/../x", "x", tar.TypeReg, ""}):                       "..",
		archive(t, entry{"coverage/l", "", tar.TypeSymlink, "/etc/passwd"}):            "not a regular file or dir",
		archive(t, entry{"coverage/big", strings.Repeat("x", 2<<20), tar.TypeReg, ""}): "unpacked",
	}
	for p, frag := range bad {
		if err := Validate(p, cov, lim); err == nil || !strings.Contains(err.Error(), frag) {
			t.Errorf("%s: %v", frag, err)
		}
	}
	many := []entry{}
	for i := 0; i < 101; i++ {
		many = append(many, entry{"coverage/f" + strings.Repeat("x", i%5) + string(rune('a'+i%26)) + "-" + string(rune('a'+i/26)), "", tar.TypeReg, ""})
	}
	if err := Validate(archive(t, many...), cov, lim); err == nil || !strings.Contains(err.Error(), "entries") {
		t.Fatal(err)
	}
}

func TestApplyReplacesEvenWhenTheArchiveLacksThePath(t *testing.T) {
	wt, acct := t.TempDir(), t.TempDir()
	_ = os.MkdirAll(filepath.Join(wt, "coverage"), 0o755)
	_ = os.WriteFile(filepath.Join(wt, "coverage", "stale.txt"), []byte("old"), 0o644)
	empty := archive(t)
	if err := Apply(acct, wt, "j1", empty, cov); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(wt, "coverage")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("replace = true kept a path the archive lacks")
	}
	if b, _ := os.ReadFile(filepath.Join(wt, ".bitc", ".gitignore")); string(b) != "*\n" {
		t.Fatalf("%q", b)
	}
	if _, err := os.Stat(filepath.Join(wt, ".bitc", "staging", "j1")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("staging left behind")
	}
}

func TestApplyRefusesASymlinkThatAppearedDuringTheJob(t *testing.T) {
	wt, acct, outside := t.TempDir(), t.TempDir(), t.TempDir()
	_ = os.WriteFile(filepath.Join(outside, "precious"), []byte("keep"), 0o644)
	_ = os.Symlink(outside, filepath.Join(wt, "coverage"))
	p := archive(t, entry{"coverage/", "", tar.TypeDir, ""}, entry{"coverage/a", "a", tar.TypeReg, ""})
	if err := Apply(acct, wt, "j1", p, cov); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(outside, "precious")); string(b) != "keep" {
		t.Fatal("deleted through a symlink")
	}
}

func TestApplyMergeRefusesASymlinkInsideTheDeclaredTree(t *testing.T) {
	wt, acct, outside := t.TempDir(), t.TempDir(), t.TempDir()
	_ = os.MkdirAll(filepath.Join(wt, "coverage"), 0o755)
	_ = os.Symlink(outside, filepath.Join(wt, "coverage", "sub"))
	p := archive(t, entry{"coverage/sub/", "", tar.TypeDir, ""}, entry{"coverage/sub/a", "a", tar.TypeReg, ""})
	merge := []config.Artifact{{Path: "coverage", Replace: false}}
	if err := Apply(acct, wt, "j1", p, merge); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(outside, "a")); err == nil {
		t.Fatal("wrote through a symlink")
	}
}

func TestDownloadResumes(t *testing.T) {
	data := []byte(strings.Repeat("0123456789", 1000))
	calls := 0
	fetch := func(off int64, w io.Writer) error {
		calls++
		end := off + 3000
		if end > int64(len(data)) {
			end = int64(len(data))
		}
		_, _ = w.Write(data[off:end])
		if end < int64(len(data)) {
			return errors.New("connection reset")
		}
		return nil
	}
	dst := filepath.Join(t.TempDir(), "a")
	if err := Download(context.Background(), fetch, dst, 1<<20); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(dst); !bytes.Equal(b, data) || calls != 4 {
		t.Fatalf("calls %d", calls)
	}
}

func TestDownloadStopsAtTheCapInsideOneAttempt(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "a")
	calls := 0
	flood := func(off int64, w io.Writer) error {
		calls++
		_, err := w.Write(make([]byte, 1<<20))
		return err
	}
	if err := Download(context.Background(), flood, dst, 1000); err == nil || !strings.Contains(err.Error(), "max_compressed") {
		t.Fatal(err)
	}
	if st, _ := os.Stat(dst); st == nil || st.Size() > 1000 || calls != 1 {
		t.Fatalf("wrote past the cap or retried: calls %d", calls)
	}
}

// FuzzValidate: any bytes either fail Validate or are a gzip tar whose every entry lies under
// a declared path; Validate never panics.
func FuzzValidate(f *testing.F) {
	for _, files := range []map[string]string{{"coverage/a": "a"}, {"../x": "x"}, {"coverage/../../x": "x"}, {"/abs": "x"}} {
		f.Add(tgzBytes(files))
	}
	f.Add([]byte("not a gzip"))
	lim := config.ArtifactLimits{MaxCompressed: 1 << 20, MaxUnpacked: 1 << 20, MaxEntries: 100}
	decl := []config.Artifact{{Path: "coverage"}}
	f.Fuzz(func(t *testing.T, b []byte) {
		p := filepath.Join(t.TempDir(), "a.tar.gz")
		if err := os.WriteFile(p, b, 0o600); err != nil {
			t.Fatal(err)
		}
		if Validate(p, decl, lim) != nil {
			return
		}
		zr, err := gzip.NewReader(bytes.NewReader(b))
		if err != nil {
			t.Fatal("accepted a non-gzip archive")
		}
		tr := tar.NewReader(zr)
		for {
			hdr, err := tr.Next()
			if err == io.EOF {
				return
			}
			if err != nil {
				t.Fatalf("accepted a corrupt archive: %v", err)
			}
			if n := path.Clean(hdr.Name); n != "coverage" && !strings.HasPrefix(n, "coverage/") {
				t.Fatalf("accepted entry %q", hdr.Name)
			}
		}
	})
}

func tgzBytes(files map[string]string) []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		_ = tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body))})
		_, _ = tw.Write([]byte(body))
	}
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

func TestApplyDoesNotFollowASymlinkedGitignore(t *testing.T) {
	wt, acct, outside := t.TempDir(), t.TempDir(), t.TempDir()
	target := filepath.Join(outside, "precious")
	_ = os.WriteFile(target, []byte("keep"), 0o644)
	_ = os.MkdirAll(filepath.Join(wt, ".bitc"), 0o755)
	_ = os.Symlink(target, filepath.Join(wt, ".bitc", ".gitignore"))
	p := archive(t, entry{"coverage/", "", tar.TypeDir, ""}, entry{"coverage/a", "a", tar.TypeReg, ""})
	_ = Apply(acct, wt, "j1", p, cov)
	if b, _ := os.ReadFile(target); string(b) != "keep" {
		t.Fatalf("the file behind a symlinked .gitignore was overwritten: %q", b)
	}
}
