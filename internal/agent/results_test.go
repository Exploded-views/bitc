package agent

import (
	"archive/tar"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

func TestPackArtifactsOnlyDeclaredExistingRegular(t *testing.T) {
	wt := t.TempDir()
	_ = os.MkdirAll(filepath.Join(wt, "coverage", "sub"), 0o755)
	_ = os.WriteFile(filepath.Join(wt, "coverage", "sub", "a.txt"), []byte("a"), 0o644)
	_ = os.Symlink("/etc/passwd", filepath.Join(wt, "coverage", "link"))
	_ = os.WriteFile(filepath.Join(wt, "other.txt"), []byte("x"), 0o644)
	out := filepath.Join(t.TempDir(), "a.tar.gz")
	f, _ := os.Create(out)
	if err := PackArtifacts(wt, []string{"coverage", "missing"}, f); err != nil {
		t.Fatal(err)
	}
	f.Close()
	got := listTar(t, out)
	want := []string{"coverage/", "coverage/sub/", "coverage/sub/a.txt"}
	if len(got) != len(want) {
		t.Fatalf("%v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%v", got)
		}
	}
	f, _ = os.Create(out)
	_ = PackArtifacts(wt, []string{"missing"}, f)
	f.Close()
	if len(listTar(t, out)) != 0 {
		t.Fatal("not an empty archive")
	}
}

func listTar(t *testing.T, p string) []string {
	f, _ := os.Open(p)
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	var names []string
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, h.Name)
	}
	sort.Strings(names)
	return names
}

func TestFinishWritesVerifiableResult(t *testing.T) {
	k := []byte("0123456789abcdef0123456789abcdef")
	r, _ := NewResults(t.TempDir(), "j1")
	o, _ := r.Create("out.log")
	if _, err := o.WriteString("hello\n"); err != nil {
		t.Fatal(err)
	}
	o.Close()
	e, _ := r.Create("err.log")
	e.Close()
	rf, err := r.Finish(k, 4, t.TempDir(), nil)
	if err != nil || rf.Exit != 4 || rf.OutBytes != 6 {
		t.Fatal(rf, err)
	}
	raw, _ := os.ReadFile(filepath.Join(r.Dir, "result.json"))
	if _, err := VerifyResult(k, raw); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(r.Dir, "out.log.part")); err == nil {
		t.Fatal(".part left behind")
	}
}

func TestWriteStateSignsWithMonotonicSeq(t *testing.T) {
	k := []byte("0123456789abcdef0123456789abcdef")
	r, _ := NewResults(t.TempDir(), "j1")
	if err := r.WriteState("begun", nil); err == nil {
		t.Fatal("an unbound Results wrote an unsigned state")
	}
	r.Bind(k, "j1", "aa")
	code := 3
	var last int64
	for _, c := range []struct {
		phase string
		exit  *int
	}{{"setup", nil}, {"begun", nil}, {"ended", &code}} {
		if err := r.WriteState(c.phase, c.exit); err != nil {
			t.Fatal(err)
		}
		raw, _ := os.ReadFile(filepath.Join(r.Dir, "state.json"))
		st, err := VerifyState(k, raw, "j1", "aa")
		if err != nil || st.Phase != c.phase || st.Seq <= last {
			t.Fatalf("%s: %+v %v last=%d", c.phase, st, err, last)
		}
		last = st.Seq
	}
	fi, _ := os.Stat(filepath.Join(r.Dir, "state.json"))
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode())
	}
}
