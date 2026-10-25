package conformance

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Exploded-views/bitc/conformance/harness"
	"github.com/Exploded-views/bitc/internal/config"
	"github.com/Exploded-views/bitc/internal/remote/artifacts"
)

func TestA1_DeclaredAndSubsetSelection(t *testing.T) {
	h := harness.New(t)
	h.HostConfig("[pools.heavy]\n")
	h.Cwd = h.Repo("app", map[string]string{".bitc.toml": "version = 1\n[pools.heavy.remote]\nallow = [[\"true\"]]\nartifacts = [{ path = \"coverage\" }, { path = \"report/\" }]\n"})
	if r := h.Run("run", "--artifacts", "nope", "heavy", "--", "true"); r.Code != 64 || !strings.Contains(r.Stderr, `artifact "nope" is not declared`) {
		t.Fatalf("%+v", r)
	}
	if r := h.RunWith([]string{"BITC_ARTIFACTS=report/"}, "run", "heavy", "--", "true"); r.Code != 0 {
		t.Fatalf("a declared path with a trailing slash was refused: %+v", r)
	}
	if r := h.RunWith([]string{"BITC_ARTIFACTS=other"}, "run", "heavy", "--", "true"); r.Code != 64 {
		t.Fatalf("BITC_ARTIFACTS widened the list: %+v", r)
	}
}

func TestA2_SizeCappedResumableDownload(t *testing.T) {
	data := bytes.Repeat([]byte("z"), 5000)
	attempts := 0
	fetch := func(off int64, w io.Writer) error {
		attempts++
		end := off + 2000
		if end > int64(len(data)) {
			end = int64(len(data))
		}
		_, _ = w.Write(data[off:end])
		if end < int64(len(data)) {
			return errors.New("reset")
		}
		return nil
	}
	dst := filepath.Join(t.TempDir(), "a.tar.gz")
	if err := artifacts.Download(context.Background(), fetch, dst, 1<<20); err != nil || attempts != 3 {
		t.Fatal(err, attempts)
	}
	big := func(off int64, w io.Writer) error { _, _ = w.Write(make([]byte, 4096)); return errors.New("more") }
	if err := artifacts.Download(context.Background(), big, filepath.Join(t.TempDir(), "b"), 1000); err == nil {
		t.Fatal("over max_compressed accepted")
	}
}

func TestA3_WholeArchiveValidatedFirst(t *testing.T) {
	wt, acct := t.TempDir(), t.TempDir()
	_ = os.MkdirAll(filepath.Join(wt, "coverage"), 0o755)
	_ = os.WriteFile(filepath.Join(wt, "coverage", "old"), []byte("old"), 0o644)
	p := tgz(t, map[string]string{"coverage/a": "a", "../escape": "x"})
	lim := config.ArtifactLimits{MaxCompressed: 1 << 20, MaxUnpacked: 1 << 20, MaxEntries: 10}
	decl := []config.Artifact{{Path: "coverage", Replace: true}}
	if err := artifacts.Validate(p, decl, lim); err == nil {
		t.Fatal("an escaping entry passed")
	}
	// Apply checks the whole archive before it writes or removes anything: replace = true must
	// not remove the old tree, the good entry is not applied, and the bad one lands nowhere.
	if err := artifacts.Apply(acct, wt, "j", p, decl); err == nil {
		t.Fatal("Apply took an archive with an escaping entry")
	}
	if b, _ := os.ReadFile(filepath.Join(wt, "coverage", "old")); string(b) != "old" {
		t.Fatal("the declared path was touched")
	}
	for _, p := range []string{filepath.Join(wt, "coverage", "a"), filepath.Join(wt, ".bitc", "staging", "escape"), filepath.Join(wt, ".bitc", "staging", "j")} {
		if _, err := os.Lstat(p); err == nil {
			t.Fatalf("%s exists", p)
		}
	}
}

func TestA4_LockedSymlinkCheckedReplaceAndSwap(t *testing.T) {
	wt, acct := t.TempDir(), t.TempDir()
	_ = os.MkdirAll(filepath.Join(wt, "coverage"), 0o755)
	_ = os.WriteFile(filepath.Join(wt, "coverage", "stale"), []byte("s"), 0o644)
	p := tgz(t, map[string]string{"coverage/new": "n"})
	if err := artifacts.Apply(acct, wt, "j", p, []config.Artifact{{Path: "coverage", Replace: true}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(wt, "coverage", "stale")); err == nil {
		t.Fatal("replace kept a stale file")
	}
	if b, _ := os.ReadFile(filepath.Join(wt, "coverage", "new")); string(b) != "n" {
		t.Fatal("not swapped in")
	}
	_ = os.WriteFile(filepath.Join(wt, "coverage", "keep"), []byte("k"), 0o644)
	if err := artifacts.Apply(acct, wt, "j2", tgz(t, map[string]string{"coverage/more": "m"}), []config.Artifact{{Path: "coverage", Replace: false}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(wt, "coverage", "keep")); err != nil {
		t.Fatal("replace = false removed a file")
	}
	if b, _ := os.ReadFile(filepath.Join(wt, "coverage", "more")); string(b) != "m" {
		t.Fatal("replace = false did not merge the archive file")
	}
}

func TestAF1_StagingUnderBitcDirIsIgnored(t *testing.T) {
	h := harness.New(t)
	wt := h.Repo("app", map[string]string{"README.md": "x", ".gitignore": "out/\n"})
	if err := artifacts.Apply(t.TempDir(), wt, "j1", tgz(t, map[string]string{"out/f": "1"}), []config.Artifact{{Path: "out", Replace: true}}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(wt, "out", "f")); string(b) != "1" {
		t.Fatalf("not applied: %q", b)
	}
	out, err := exec.Command("git", "-C", wt, "status", "--porcelain").Output()
	if err != nil || len(out) != 0 {
		t.Fatalf("the tree is not clean: %v %q", err, out)
	}
	if _, err := os.Stat(filepath.Join(wt, ".bitc", "staging", "j1")); !os.IsNotExist(err) {
		t.Fatal("staging left behind")
	}
}

func tgz(t *testing.T, files map[string]string) string {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		_ = tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body))})
		_, _ = tw.Write([]byte(body))
	}
	tw.Close()
	gz.Close()
	p := filepath.Join(t.TempDir(), "a.tar.gz")
	_ = os.WriteFile(p, buf.Bytes(), 0o600)
	return p
}
