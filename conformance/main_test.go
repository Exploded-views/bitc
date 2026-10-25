package conformance

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/Exploded-views/bitc/conformance/harness"
)

var (
	binFlag   = flag.String("bin", "", "bitc binary under test; relative paths are from the module root (default: build ./cmd/bitc)")
	milestone = flag.String("milestone", "", "with TestMetaNoTodo: fail on todo rows of this milestone or earlier (M1, M2, M3)")
)

// buildStubPlugin builds the stub provider plugin the plugin rows run (providers delta P.1).
func buildStubPlugin() (dir string, err error) {
	if dir, err = os.MkdirTemp("", "bitc-stubplugin-"); err != nil {
		return "", err
	}
	harness.StubPlugin = filepath.Join(dir, "stubplugin")
	build := exec.Command("go", "build", "-trimpath", "-o", harness.StubPlugin, "../internal/provider/plugin/stubplugin")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		return dir, fmt.Errorf("build stubplugin: %w\n%s", err, out)
	}
	return dir, nil
}

func TestMain(m *testing.M) {
	flag.Parse()
	stubDir, err := buildStubPlugin()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	sshDir, err := harness.BuildSSHFake()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if *binFlag != "" {
		p := *binFlag
		if !filepath.IsAbs(p) {
			p = filepath.Join("..", p)
		}
		abs, err := filepath.Abs(p)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		harness.Bin = abs
		code := m.Run()
		os.RemoveAll(stubDir)
		os.RemoveAll(sshDir)
		os.Exit(code)
	}
	dir, err := os.MkdirTemp("", "bitc-conformance-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	harness.Bin = filepath.Join(dir, "bitc")
	build := exec.Command("go", "build", "-trimpath", "-o", harness.Bin, "../cmd/bitc")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "build bitc: %v\n%s", err, out)
		os.Exit(2)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.RemoveAll(stubDir)
	os.RemoveAll(sshDir)
	os.Exit(code)
}

// must returns b or panics; the shared helper of every family file.
func must(b []byte, err error) []byte {
	if err != nil {
		panic(err)
	}
	return b
}

// lines returns the non-empty lines of a file ("" if it does not exist).
func lines(t *testing.T, path string) []string {
	t.Helper()
	b, _ := os.ReadFile(path)
	var out []string
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}

// tickets lists a pool's ticket files, sorted.
func tickets(h *harness.H, pool string) []string {
	ents, _ := os.ReadDir(filepath.Join(h.State, "pools", pool, "tickets"))
	var out []string
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".json") {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

// Nothing calls of these helpers yet; this keeps the `unused` linter quiet until the
// family files of later tasks use them.
var _, _, _ = must, lines, tickets
