package agent_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Exploded-views/bitc/internal/agent"
	"github.com/Exploded-views/bitc/internal/agent/agenttest"
)

// recSystem is a PlainSystem that records the installs and mounts asked of it.
type recSystem struct {
	agent.PlainSystem
	pkgErr   error
	packages [][]string
	mounts   []string
	unmounts int
}

func (r *recSystem) InstallPackages(n []string) error {
	r.packages = append(r.packages, n)
	return r.pkgErr
}

func (r *recSystem) MountTmpfs(dir string) (func() error, error) {
	r.mounts = append(r.mounts, dir)
	return func() error { r.unmounts++; return nil }, nil
}

// toolServer serves a one-file tool archive and counts the downloads.
func toolServer(t *testing.T) (tool agent.Tool, hits *atomic.Int32, client *http.Client) {
	t.Helper()
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	tw := tar.NewWriter(zw)
	body := "#!/bin/sh\necho toolout\n"
	_ = tw.WriteHeader(&tar.Header{Name: "tl/bin/hello", Typeflag: tar.TypeReg, Mode: 0o755, Size: int64(len(body))})
	_, _ = tw.Write([]byte(body))
	_ = tw.Close()
	_ = zw.Close()
	hits = new(atomic.Int32)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { hits.Add(1); _, _ = w.Write(gz.Bytes()) }))
	t.Cleanup(srv.Close)
	sum := sha256.Sum256(gz.Bytes())
	return agent.Tool{Name: "tl", URL: srv.URL + "/tl.tgz", SHA256: hex.EncodeToString(sum[:])}, hits, srv.Client()
}

type provEnv struct {
	root, tools, state string
	sys                *recSystem
	out                *bytes.Buffer
	runs               int
}

func newProvEnv(t *testing.T) *provEnv {
	t.Helper()
	root := t.TempDir()
	return &provEnv{root: root, tools: filepath.Join(root, "tools"), state: filepath.Join(root, "var", "lib", "bitc"), sys: &recSystem{}, out: new(bytes.Buffer)}
}

func (e *provEnv) opts(c *http.Client) agent.ProvisionOptions {
	return agent.ProvisionOptions{Stdout: e.out, System: e.sys, ToolsRoot: e.tools, StateRoot: e.state, HTTP: c,
		SelfSHA: func() (string, error) { return strings.Repeat("a", 64), nil }}
}

func TestProvisionInstallsAndWritesTheMarker(t *testing.T) {
	tool, hits, client := toolServer(t)
	e := newProvEnv(t)
	code := agent.Provision(e.opts(client), agent.ProvisionSpec{Packages: []string{"p1", "p2"}, Tools: []agent.Tool{tool}})
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, e.out)
	}
	if len(e.sys.packages) != 1 || strings.Join(e.sys.packages[0], ",") != "p1,p2" || hits.Load() != 1 {
		t.Fatalf("installs: packages %v, downloads %d", e.sys.packages, hits.Load())
	}
	b, err := os.ReadFile(filepath.Join(e.state, agent.ProvisionMarkerName))
	if err != nil {
		t.Fatal(err)
	}
	var m agent.ProvisionMarker
	if err := json.Unmarshal(b, &m); err != nil || m.AgentSHA256 != strings.Repeat("a", 64) || strings.Join(m.Packages, ",") != "p1,p2" || len(m.Tools) != 1 || m.Tools[0] != tool {
		t.Fatalf("marker %s (%v)", b, err)
	}
	if st, _ := os.Stat(filepath.Join(e.state, agent.ProvisionMarkerName)); st.Mode().Perm() != 0o600 {
		t.Fatalf("marker mode %v", st.Mode().Perm())
	}
	if st, _ := os.Stat(e.state); st.Mode().Perm() != 0o700 {
		t.Fatalf("state root mode %v", st.Mode().Perm())
	}
	if !strings.Contains(e.out.String(), "provision: ") {
		t.Fatalf("no progress line:\n%s", e.out)
	}
}

// runProvisioned runs the fixture job in no-listener mode against a marker left by Provision.
func runProvisioned(t *testing.T, e *provEnv, client *http.Client, sys agent.System, edit func(f *agenttest.Fixture)) (*agenttest.Fixture, string, int) {
	t.Helper()
	f := agenttest.NewFixture(t, "#!/bin/sh\necho \"$PATH\"\n")
	f.NoListener, f.HTTP, f.System = true, client, sys
	f.StateRoot = e.state
	f.SelfSHA = func() (string, error) { return strings.Repeat("a", 64), nil }
	f.Root = filepath.Dir(e.tools) // the fixture's tools root is <Root>/tools
	e.runs++
	f.Spec.Job = fmt.Sprintf("run%d", e.runs) // one results dir per run: a second boot of a job is LOST
	if edit != nil {
		edit(f)
	}
	log, code := f.Run(t)
	return f, log, code
}

func TestProvisionedRunSkipsPackageAndToolInstalls(t *testing.T) {
	tool, hits, client := toolServer(t)
	e := newProvEnv(t)
	ps := agent.ProvisionSpec{Packages: []string{"p1"}, Tools: []agent.Tool{tool}}
	if code := agent.Provision(e.opts(client), ps); code != 0 {
		t.Fatalf("provision %d\n%s", code, e.out)
	}
	run := &recSystem{}
	f, log, code := runProvisioned(t, e, client, run, func(f *agenttest.Fixture) { f.Spec.Packages, f.Spec.Tools = ps.Packages, ps.Tools })
	if code != 0 || len(run.packages) != 0 || hits.Load() != 1 {
		t.Fatalf("exit %d, run installed %v, downloads %d\n%s", code, run.packages, hits.Load(), log)
	}
	out, _ := os.ReadFile(filepath.Join(f.Root, f.Spec.Job, "out.log"))
	if !strings.Contains(string(out), filepath.Join(e.tools, "tl", "bin")) {
		t.Fatalf("the tool is not on the job's PATH: %q", out)
	}
	if !strings.Contains(log, "provisioned: skipping package and tool installs") {
		t.Fatalf("no skip line:\n%s", log)
	}
}

func TestRunInstallsAgainWhenTheMarkerDoesNotMatch(t *testing.T) {
	tool, hits, client := toolServer(t)
	e := newProvEnv(t)
	if code := agent.Provision(e.opts(client), agent.ProvisionSpec{Packages: []string{"p1"}, Tools: []agent.Tool{tool}}); code != 0 {
		t.Fatalf("provision %d", code)
	}
	for name, edit := range map[string]func(f *agenttest.Fixture){
		"packages": func(f *agenttest.Fixture) { f.Spec.Packages, f.Spec.Tools = []string{"p1", "p9"}, []agent.Tool{tool} },
		"tools":    func(f *agenttest.Fixture) { f.Spec.Packages = []string{"p1"} },
		"agent": func(f *agenttest.Fixture) {
			f.Spec.Packages, f.Spec.Tools = []string{"p1"}, []agent.Tool{tool}
			f.SelfSHA = func() (string, error) { return strings.Repeat("b", 64), nil }
		},
	} {
		run := &recSystem{}
		before := hits.Load()
		_, log, code := runProvisioned(t, e, client, run, edit)
		if code != 0 || len(run.packages) != 1 {
			t.Errorf("%s: exit %d, run installed %v\n%s", name, code, run.packages, log)
		}
		if name != "tools" && hits.Load() == before {
			t.Errorf("%s: the tool was not downloaded again", name)
		}
		if strings.Contains(log, "provisioned: skipping") {
			t.Errorf("%s: skipped the installs although the marker does not match", name)
		}
	}
}

func TestProvisionedRunRehashesTheInstalledTool(t *testing.T) {
	tool, _, client := toolServer(t)
	e := newProvEnv(t)
	ps := agent.ProvisionSpec{Tools: []agent.Tool{tool}}
	if code := agent.Provision(e.opts(client), ps); code != 0 {
		t.Fatalf("provision %d", code)
	}
	if err := os.WriteFile(filepath.Join(e.tools, ".dl", "tl"), []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, log, code := runProvisioned(t, e, client, &recSystem{}, func(f *agenttest.Fixture) { f.Spec.Tools = ps.Tools })
	if !strings.Contains(log, "__BITC_SETUP_FAILED__ n0nce tl infra ") || strings.Contains(log, "BEGIN") || code == 0 {
		t.Fatalf("exit %d\n%s", code, log)
	}
}

func TestProvisionFailureClasses(t *testing.T) {
	tool, _, client := toolServer(t)
	bad := tool
	bad.SHA256 = strings.Repeat("0", 64)
	down := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(500) }))
	defer down.Close()
	for _, c := range []struct {
		name string
		ps   agent.ProvisionSpec
		sys  *recSystem
		cl   *http.Client
		want int
		line string
	}{
		{"package install fails", agent.ProvisionSpec{Packages: []string{"p"}}, &recSystem{pkgErr: errors.New("apt timeout")}, client, agent.ProvisionExitInfra, "setup step packages failed: "},
		{"tool pin mismatch", agent.ProvisionSpec{Tools: []agent.Tool{bad}}, &recSystem{}, client, agent.ProvisionExitRepo, "setup step tl failed: "},
		{"tool download fails", agent.ProvisionSpec{Tools: []agent.Tool{{Name: "tl", URL: down.URL + "/tl.tgz", SHA256: tool.SHA256}}}, &recSystem{}, down.Client(), agent.ProvisionExitInfra, "setup step tl failed: "},
		{"unsafe tool name", agent.ProvisionSpec{Tools: []agent.Tool{{Name: "../x", URL: tool.URL, SHA256: tool.SHA256}}}, &recSystem{}, client, agent.ProvisionExitRepo, "setup step ../x failed: "},
	} {
		e := newProvEnv(t)
		e.sys = c.sys
		code := agent.Provision(e.opts(c.cl), c.ps)
		lines := strings.Split(strings.TrimSpace(e.out.String()), "\n")
		last := lines[len(lines)-1]
		if code != c.want || !strings.HasPrefix(last, c.line) {
			t.Errorf("%s: exit %d (want %d), last line %q\n%s", c.name, code, c.want, last, e.out)
		}
		if _, err := os.Stat(filepath.Join(e.state, agent.ProvisionMarkerName)); err == nil {
			t.Errorf("%s: a marker was written for a failed provisioning", c.name)
		}
	}
}

func TestProvisionSpecFrameRoundTripAndTruncation(t *testing.T) {
	ps := agent.ProvisionSpec{Packages: []string{"a"}, Tools: []agent.Tool{{Name: "t", URL: "https://x/t.tgz", SHA256: strings.Repeat("c", 64)}}}
	fr, err := ps.Frame()
	if err != nil || len(fr) < 9 {
		t.Fatalf("frame %v %v", fr, err)
	}
	got, err := agent.ReadProvisionSpec(bytes.NewReader(fr))
	if err != nil || len(got.Packages) != 1 || got.Tools[0].Name != "t" {
		t.Fatalf("round trip: %+v %v", got, err)
	}
	if _, err := agent.ReadProvisionSpec(bytes.NewReader(fr[:len(fr)-3])); !errors.Is(err, agent.ErrTruncated) {
		t.Fatalf("truncated frame: %v", err)
	}
	if _, err := agent.ReadProvisionSpec(bytes.NewReader(nil)); !errors.Is(err, agent.ErrTruncated) {
		t.Fatalf("empty input: %v", err)
	}
}

func TestProvisionVerbRefusesBadInputWithNothingStarted(t *testing.T) {
	var out bytes.Buffer
	code := agent.MainIO([]string{"provision"}, strings.NewReader("not a frame"), &out)
	if code != 64 || !strings.HasPrefix(out.String(), "refused: ") {
		t.Fatalf("exit %d, out %q", code, out.String())
	}
}

func TestProvisionVerbRunsAnUnprivilegedSpecUnderItsTestRoot(t *testing.T) {
	root := t.TempDir()
	fr, err := agent.ProvisionSpec{Unprivileged: true, TestRoot: root}.Frame()
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if code := agent.MainIO([]string{"provision"}, bytes.NewReader(fr), &out); code != 0 {
		t.Fatalf("exit %d\n%s", code, out.String())
	}
	if _, err := os.Stat(filepath.Join(root, "var", "lib", "bitc", agent.ProvisionMarkerName)); err != nil {
		t.Fatalf("no marker under the test root: %v\n%s", err, out.String())
	}
}
