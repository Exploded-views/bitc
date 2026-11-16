package harness

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// The fake docker CLI (internal/container/testdata/fakedocker) is built once per test binary,
// the first time a test asks for it, into a temp dir that CleanupFakeDocker removes.
var (
	fakeOnce sync.Once
	fakeDir  string
	fakeErr  error
)

// BuildFakeDockerBinary builds the fake docker into a temp dir and returns the binary's path.
// The module root is found from the test's working directory (conformance/ or an internal package).
func BuildFakeDockerBinary(moduleRoot string) (string, error) {
	fakeOnce.Do(func() {
		fakeDir, fakeErr = os.MkdirTemp("", "bitc-fakedocker-")
		if fakeErr != nil {
			return
		}
		build := exec.Command("go", "build", "-trimpath", "-o", filepath.Join(fakeDir, "docker"), "./internal/container/testdata/fakedocker")
		build.Dir = moduleRoot
		build.Env = append(os.Environ(), "CGO_ENABLED=0")
		if out, err := build.CombinedOutput(); err != nil {
			fakeErr = fmt.Errorf("build fakedocker: %w\n%s", err, out)
		}
	})
	return filepath.Join(fakeDir, "docker"), fakeErr
}

// CleanupFakeDocker removes the built fake; TestMain calls it before exiting.
func CleanupFakeDocker() {
	if fakeDir != "" {
		_ = os.RemoveAll(fakeDir)
	}
}

// FakeDocker is one test's fake docker: a `docker` on PATH for every bitc the test starts, and
// the state directory it records into.
type FakeDocker struct {
	h     *H
	State string // $FAKE_DOCKER_DIR
}

// FakeDocker puts the fake docker first on PATH for bitc and its children in this sandbox.
func (h *H) FakeDocker() *FakeDocker {
	h.T.Helper()
	bin, err := BuildFakeDockerBinary(ModuleRoot(h.T))
	if err != nil {
		h.T.Fatal(err)
	}
	b, err := os.ReadFile(bin)
	if err != nil {
		h.T.Fatal(err)
	}
	binDir := filepath.Join(h.Root, "fakebin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		h.T.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "docker"), b, 0o755); err != nil {
		h.T.Fatal(err)
	}
	f := &FakeDocker{h: h, State: filepath.Join(h.Root, "fakedocker")}
	h.Env = MergeEnv(h.Env, []string{"PATH=" + binDir + string(os.PathListSeparator) + os.Getenv("PATH"), "FAKE_DOCKER_DIR=" + f.State})
	return f
}

// ModuleRoot is the module root, found from the working directory (a test runs in its package).
func ModuleRoot(t interface{ Fatal(...any) }) string {
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for d := dir; ; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, "go.mod")); err == nil {
			return d
		}
		if d == filepath.Dir(d) {
			t.Fatal("no go.mod above " + dir)
		}
	}
}

// Calls is every docker call so far, argv without the program name, oldest first.
func (f *FakeDocker) Calls() [][]string {
	var out [][]string
	file, err := os.Open(filepath.Join(f.State, "argv.log"))
	if err != nil {
		return nil
	}
	defer file.Close()
	sc := bufio.NewScanner(file)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		var a []string
		if json.Unmarshal(sc.Bytes(), &a) == nil {
			out = append(out, a)
		}
	}
	return out
}

// CallsOf is the calls whose first argument is verb ("run", "rm", "ps").
func (f *FakeDocker) CallsOf(verb string) [][]string {
	var out [][]string
	for _, c := range f.Calls() {
		if len(c) > 0 && c[0] == verb {
			out = append(out, c)
		}
	}
	return out
}

// Removed reports whether a `docker rm` removed the container of that name (by whatever it was given).
func (f *FakeDocker) Removed(name string) bool {
	for _, r := range f.removals() {
		if r["name"] == name {
			return true
		}
	}
	return false
}

// Created reports whether the fake ever created a container of that name (a run killed before the
// CLI got that far creates none).
func (f *FakeDocker) Created(name string) bool {
	b, err := os.ReadFile(filepath.Join(f.State, "created.log"))
	if err != nil {
		return false
	}
	for _, l := range strings.Split(string(b), "\n") {
		if l == name {
			return true
		}
	}
	return false
}

// DockerHosts is the DOCKER_HOST each docker call ran with, oldest first ("" when unset).
func (f *FakeDocker) DockerHosts() []string {
	b, err := os.ReadFile(filepath.Join(f.State, "hostenv.log"))
	if err != nil {
		return nil
	}
	return strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
}

// RemovedBy is what each `docker rm` that removed a container was given (an id, or a name).
func (f *FakeDocker) RemovedBy() []string {
	var out []string
	for _, r := range f.removals() {
		out = append(out, r["arg"])
	}
	return out
}

func (f *FakeDocker) removals() []map[string]string {
	var out []map[string]string
	file, err := os.Open(filepath.Join(f.State, "removed.log"))
	if err != nil {
		return nil
	}
	defer file.Close()
	sc := bufio.NewScanner(file)
	for sc.Scan() {
		var m map[string]string
		if json.Unmarshal(sc.Bytes(), &m) == nil {
			out = append(out, m)
		}
	}
	return out
}

// EnvFile is what the fake found in the --env-file of the run that named its container name: the
// file's path, its mode ("0600") and its body, taken when docker run started.
func (f *FakeDocker) EnvFile(name string) (path, mode, body string, ok bool) {
	var s struct{ Path, Mode, Body string }
	b, err := os.ReadFile(filepath.Join(f.State, "envfiles", name+".json"))
	if err != nil || json.Unmarshal(b, &s) != nil {
		return "", "", "", false
	}
	return s.Path, s.Mode, s.Body, true
}

type fakeRec struct {
	ID     string            `json:"id"`
	Name   string            `json:"name"`
	Labels map[string]string `json:"labels"`
	Pgid   int               `json:"pgid"`
}

// Container returns the fake container name and whether it exists.
func (f *FakeDocker) Container(name string) (fakeRec, bool) {
	var r fakeRec
	b, err := os.ReadFile(filepath.Join(f.State, "c", name+".json"))
	if err != nil || json.Unmarshal(b, &r) != nil {
		return r, false
	}
	return r, true
}

// Exists reports whether the container exists (created, running or exited without --rm).
func (f *FakeDocker) Exists(name string) bool { _, ok := f.Container(name); return ok }

// Running reports whether the container's command has started (its process group is recorded).
func (f *FakeDocker) Running(name string) bool { r, ok := f.Container(name); return ok && r.Pgid > 0 }

// Plant adds a container that nothing runs, as a leftover of a bitc that died: an orphan to sweep.
// It returns the container's id.
func (f *FakeDocker) Plant(name string, labels map[string]string) string {
	f.h.T.Helper()
	if err := os.MkdirAll(filepath.Join(f.State, "c"), 0o755); err != nil {
		f.h.T.Fatal(err)
	}
	sum := sha256.Sum256([]byte("planted " + name))
	id := hex.EncodeToString(sum[:]) // 64 hex digits, as docker's
	b, _ := json.Marshal(fakeRec{ID: id, Name: name, Labels: labels})
	if err := os.WriteFile(filepath.Join(f.State, "c", name+".json"), b, 0o644); err != nil {
		f.h.T.Fatal(err)
	}
	return id
}
