package conformance

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Exploded-views/bitc/conformance/harness"
	"github.com/Exploded-views/bitc/internal/config"
	"github.com/Exploded-views/bitc/internal/provider"
	"github.com/Exploded-views/bitc/internal/remote"
)

// archCheck runs `config check` for a repo file body under a host with pool heavy.
func archCheck(t *testing.T, repoBody string) harness.Result {
	t.Helper()
	h := harness.New(t)
	h.HostConfig("[pools.heavy]\n")
	h.Cwd = h.Repo("app", map[string]string{".bitc.toml": "version = 1\n" + repoBody})
	return h.Run("config", "check")
}

func TestAR1_PoolArchIsValidatedAndDefaultsToAmd64(t *testing.T) {
	for _, bad := range []string{`["riscv64"]`, `["amd64", "amd64"]`, `[""]`} {
		r := archCheck(t, "[pools.heavy.remote]\narch = "+bad+"\n")
		if r.Code != 78 || !strings.Contains(r.Stderr, "pools.heavy.remote.arch: invalid: ") {
			t.Errorf("%s: %+v", bad, r)
		}
	}
	for _, ok := range []string{"", "arch = [\"amd64\"]\n", "arch = [\"amd64\", \"arm64\"]\n"} {
		if r := archCheck(t, "[pools.heavy.remote]\n"+ok); r.Code != 0 {
			t.Errorf("%q: %+v", ok, r)
		}
	}
}

func TestAR2_ToolArchIsValidatedAndNameArchIsUnique(t *testing.T) {
	sum := strings.Repeat("a", 64)
	tool := func(arch string) string {
		return `{ name = "node", ` + arch + `url = "https://x.invalid/n.tgz", sha256 = "` + sum + `" }`
	}
	for want, tools := range map[string]string{
		"remote.tools: node/sparc: invalid arch": tool(`arch = "sparc", `),
		"remote.tools: node/amd64: duplicate":    tool("") + ", " + tool(`arch = "amd64", `),
	} {
		if r := archCheck(t, "[remote]\ntools = ["+tools+"]\n"); r.Code != 78 || !strings.Contains(r.Stderr, want) {
			t.Errorf("%s: %+v", want, r)
		}
	}
	if r := archCheck(t, "[remote]\ntools = ["+tool("")+", "+tool(`arch = "arm64", `)+"]\n"); r.Code != 0 {
		t.Errorf("one tool for each arch: %+v", r)
	}
}

// archFake sets up pool heavy on a remote-only fake provider that runs on providerArch, with the
// given pool arch line and extra repo-file text; it returns the harness and the fake's state dir.
func archFake(t *testing.T, providerArch, poolArch, repoExtra string) (*harness.H, string) {
	t.Helper()
	h := harness.New(t)
	st := t.TempDir()
	h.HostConfig("[remote_limits]\nworktrees = [\"**/app\"]\n" +
		"[pools.heavy]\nproviders = [\"fake\"]\nremote_limits = { size = { fake = \"small\" } }\nwait = \"5s\"\n" +
		"[providers.fake]\ndaily_usd = 1\nmode = \"run\"\narch = " + harness.Q(providerArch) + "\nstate_dir = " + harness.Q(st) + "\n")
	h.Cwd = h.Repo("app", map[string]string{".bitc.toml": "version = 1\n" + repoExtra +
		"[pools.heavy.remote]\nallow = [[\"echo\", \"*\"]]\n" + poolArch})
	return h, st
}

func TestAR3_PoolArchMustIntersectTheProvidersArch(t *testing.T) {
	h, st := archFake(t, "arm64", "", "")
	r := h.Run("run", "heavy", "--", "echo", "x")
	if r.Code != 69 || r.Stdout != "" || r.Stderr != "bitc: fake: heavy: not eligible for remote: arch arm64 not allowed by pool (arch = [\"amd64\"])\n" {
		t.Fatalf("pool default: code %d stdout %q stderr %q", r.Code, r.Stdout, r.Stderr)
	}
	if got := lines(t, filepath.Join(st, "launches.log")); len(got) != 0 {
		t.Fatalf("an ineligible job reached Launch: %v", got)
	}
	for _, pool := range []string{"arch = [\"arm64\"]\n", "arch = [\"amd64\", \"arm64\"]\n"} {
		h, st := archFake(t, "arm64", pool, "")
		if r := h.Run("run", "heavy", "--", "echo", "x"); r.Code != 0 || r.Stdout != "x\n" {
			t.Errorf("%s: %+v", pool, r)
		}
		if got := lines(t, filepath.Join(st, "arch.log")); len(got) != 1 || !strings.HasPrefix(got[0], "arm64") {
			t.Errorf("%s: job arch %v", pool, got)
		}
	}
	h, _ = archFake(t, "amd64", "arch = [\"arm64\"]\n", "")
	if r := h.Run("run", "heavy", "--", "echo", "x"); r.Code != 69 || !strings.Contains(r.Stderr, "arch amd64 not allowed by pool (arch = [\"arm64\"])") {
		t.Fatalf("amd64 provider, arm64 pool: %+v", r)
	}
}

func TestAR4_ToolWithoutAnEntryForTheJobsArchIsIneligible(t *testing.T) {
	sum := strings.Repeat("a", 64)
	amd := `{ name = "node", url = "https://x.invalid/amd.tgz", sha256 = "` + sum + `" }`
	arm := `{ name = "node", arch = "arm64", url = "https://x.invalid/arm.tgz", sha256 = "` + sum + `" }`
	h, _ := archFake(t, "arm64", "arch = [\"arm64\"]\n", "[remote]\ntools = ["+amd+"]\n")
	r := h.Run("run", "heavy", "--", "echo", "x")
	if r.Code != 69 || r.Stderr != "bitc: fake: heavy: not eligible for remote: tool node has no arm64 entry\n" {
		t.Fatalf("code %d stderr %q", r.Code, r.Stderr)
	}
	// both entries present: the job carries only the entry for its own arch
	h, st := archFake(t, "arm64", "arch = [\"arm64\"]\n", "[remote]\ntools = ["+amd+", "+arm+"]\n")
	if r := h.Run("run", "heavy", "--", "echo", "x"); r.Code != 0 {
		t.Fatalf("%+v", r)
	}
	if got := lines(t, filepath.Join(st, "arch.log")); len(got) != 1 || got[0] != "arm64 node/arm64" {
		t.Fatalf("tools sent to the provider: %v", got)
	}
	h, st = archFake(t, "amd64", "", "[remote]\ntools = ["+amd+", "+arm+"]\n")
	if r := h.Run("run", "heavy", "--", "echo", "x"); r.Code != 0 {
		t.Fatalf("%+v", r)
	}
	if got := lines(t, filepath.Join(st, "arch.log")); len(got) != 1 || got[0] != "amd64 node/amd64" {
		t.Fatalf("tools sent to the provider: %v", got)
	}
}

const arHostHead = "[providers.fake]\ndaily_usd = 25\nmax_live = 6\nstart_timeout = \"20m\"\nusd_per_hour = 0.24\n"

// modDir is a directory with this module's go.mod, a stand-in for a bitc source tree.
func modDir(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	mod := string(must(os.ReadFile("../go.mod")))
	if err := os.WriteFile(filepath.Join(d, "go.mod"), []byte(strings.SplitN(mod, "\n", 2)[0]+"\n\ngo 1.26\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestAR5_AgentSourceKeysPerArchAreValidatedAtLoad(t *testing.T) {
	sum, good := strings.Repeat("c", 64), modDir(t)
	img, c := "golang@sha256:"+strings.Repeat("b", 64), strings.Repeat("a", 40)
	for want, line := range map[string]string{
		`agent.url: {arch} is required to serve paths.arm64`: `agent = { source = "url", url = "https://h.invalid/bitc", paths = { amd64 = "/a", arm64 = "/b" } }`,
		`agent.paths: invalid arch "sparc"`:                  `agent = { source = "url", url = "https://h.invalid/bitc-{arch}", paths = { sparc = "/a" } }`,
		`agent.path and agent.paths.amd64 differ`:            `agent = { source = "url", url = "https://h.invalid/bitc-{arch}", path = "/a", paths = { amd64 = "/b" } }`,
		`agent.module_dir: not an absolute path`:             `agent = { source = "build", module_dir = "src/bitc" }`,
		`agent.module_dir: no go.mod`:                        `agent = { source = "build", module_dir = ` + harness.Q(t.TempDir()) + ` }`,
		`agent.module_dir: cannot be combined`:               `agent = { source = "build", module_dir = ` + harness.Q(good) + `, commit = "` + c + `", image = "` + img + `", sha256 = "` + sum + `" }`,
		`agent.module_dir: only source = "build"`:            `agent = { source = "url", url = "https://h.invalid/bitc", path = "/a", module_dir = ` + harness.Q(good) + ` }`,
	} {
		h := harness.New(t)
		h.HostConfig(arHostHead + line + "\n")
		if r := h.Run("config", "check"); r.Code != 78 || !strings.Contains(r.Stderr, want) {
			t.Errorf("%s: %+v", want, r)
		}
	}
	for _, ok := range []string{
		`agent = { source = "url", url = "https://h.invalid/bitc-{arch}", paths = { amd64 = "/a", arm64 = "/b" } }`,
		`agent = { source = "url", url = "https://h.invalid/bitc", path = "/a" }`,
		`agent = { source = "build", module_dir = ` + harness.Q(good) + ` }`,
		`agent = { source = "build", commit = "` + c + `", image = "` + img + `", sha256 = "` + sum + `" }`,
	} {
		h := harness.New(t)
		h.HostConfig(arHostHead + ok + "\n")
		if r := h.Run("config", "check"); r.Code != 0 {
			t.Errorf("%s: %+v", ok, r)
		}
	}
}

func TestAR6_UrlSourceServesEachArchFromItsOwnFile(t *testing.T) {
	d := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(d, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	amd, arm := write("amd", "amd64 bytes"), write("arm", "arm64 bytes")
	core := &config.Provider{Agent: config.Agent{Source: "url", URL: "https://h.invalid/bitc-{arch}", Paths: map[string]string{"amd64": amd, "arm64": arm}}}
	for arch, p := range map[string]string{"amd64": amd, "arm64": arm} {
		src, err := remote.ResolveAgentSourceFor(&config.Effective{}, core, arch)
		want := sha256Hex(string(must(os.ReadFile(p))))
		if err != nil || src.URL != "https://h.invalid/bitc-"+arch || src.SHA256 != want || src.Path != p {
			t.Errorf("%s: %+v %v", arch, src, err)
		}
	}
	// the old single path is the amd64 file, and a url without {arch} has no arm64 form
	old := &config.Provider{Agent: config.Agent{Source: "url", URL: "https://h.invalid/bitc", Path: amd, Paths: map[string]string{"amd64": amd}}}
	if src, err := remote.ResolveAgentSourceFor(&config.Effective{}, old, "amd64"); err != nil || src.URL != "https://h.invalid/bitc" || src.Path != amd {
		t.Errorf("amd64 via path: %+v %v", src, err)
	}
	_, err := remote.ResolveAgentSourceFor(&config.Effective{}, old, "arm64")
	var inel *provider.ErrIneligible
	if !errors.As(err, &inel) || inel.Why != "agent paths has no arm64 entry" {
		t.Errorf("arm64 via path: %v", err)
	}
}

func TestAR7_ReleaseSourceTakesTheArchsShaFromChecksums(t *testing.T) {
	sums := strings.Repeat("1", 64) + "  bitc_1.2.3_linux_amd64\n" + strings.Repeat("2", 64) + "  bitc_1.2.3_linux_arm64\n" +
		strings.Repeat("3", 64) + "  bitc_1.2.3_linux_arm64.tar.gz\n"
	var asked []string
	fetch := func(url string) ([]byte, error) { asked = append(asked, url); return []byte(sums), nil }
	url, sha, err := remote.ResolveRelease("1.2.3", "arm64", fetch)
	base := "https://github.com/Exploded-views/bitc/releases/download/v1.2.3/"
	if err != nil || url != base+"bitc_1.2.3_linux_arm64" || sha != strings.Repeat("2", 64) {
		t.Fatalf("%q %q %v", url, sha, err)
	}
	if len(asked) != 1 || asked[0] != base+"checksums.txt" {
		t.Fatalf("checksums fetched from %v", asked)
	}
	if _, sha, _ := remote.ResolveRelease("1.2.3", "amd64", fetch); sha != strings.Repeat("1", 64) {
		t.Fatalf("amd64 sha %q", sha)
	}
	_, _, err = remote.ResolveRelease("9.9.9", "arm64", fetch)
	var inel *provider.ErrIneligible
	if !errors.As(err, &inel) || !strings.Contains(inel.Why, "checksums.txt has no bitc_9.9.9_linux_arm64") {
		t.Fatalf("%v", err)
	}
	_, _, err = remote.ResolveRelease("1.2.3", "arm64", func(string) ([]byte, error) { return nil, errors.New("offline") })
	var tr *provider.ErrTransient
	if !errors.As(err, &tr) {
		t.Fatalf("a failed checksums fetch is transient: %v", err)
	}
}

// srcTree is a git work tree with this module's go.mod, one commit.
func srcTree(t *testing.T) (dir, head string) {
	t.Helper()
	dir = modDir(t)
	for _, a := range [][]string{{"init", "-q"}, {"add", "-A"}, {"-c", "user.name=t", "-c", "user.email=t@x", "commit", "-q", "-m", "i"}} {
		c := exec.Command("git", a...)
		c.Dir = dir
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("%v %s", err, out)
		}
	}
	out, _ := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	return dir, strings.TrimSpace(string(out))
}

func TestAR8_BuildSourceCrossCompilesOncePerArchAndTreeState(t *testing.T) {
	dir, head := srcTree(t)
	acct := t.TempDir()
	_ = os.Chmod(acct, 0o700)
	var calls [][]string
	run := func(wd string, env []string, argv ...string) ([]byte, error) {
		calls = append(calls, append(append([]string{wd}, env...), argv...))
		for i, a := range argv {
			if a == "-o" {
				return nil, os.WriteFile(argv[i+1], []byte("agent for "+strings.Join(env, " ")), 0o700)
			}
		}
		return nil, errors.New("no -o")
	}
	core := &config.Provider{Agent: config.Agent{Source: "build", ModuleDir: dir}}
	e := &config.Effective{Test: &config.Test{AccountingDir: acct}}
	got, err := remote.BuildAgent(e, core, "arm64", run)
	if err != nil {
		t.Fatal(err)
	}
	body := "agent for CGO_ENABLED=0 GOOS=linux GOARCH=arm64"
	if got.Path != filepath.Join(acct, "agents", "arm64", got.SHA256) || got.SHA256 != sha256Hex(body) || got.Commit != head {
		t.Fatalf("built agent: path %q sha %q commit %q, want sha %s commit %s", got.Path, got.SHA256, got.Commit, sha256Hex(body), head)
	}
	if b := must(os.ReadFile(got.Path)); string(b) != body {
		t.Fatalf("built file %q", b)
	}
	want := []string{dir, "CGO_ENABLED=0", "GOOS=linux", "GOARCH=arm64", "go", "build", "-trimpath", "-ldflags=-buildid=", "-o"}
	if len(calls) != 1 || !reflect.DeepEqual(calls[0][:len(want)], want) || calls[0][len(calls[0])-1] != "./cmd/bitc" {
		t.Fatalf("go invocation %q", calls)
	}
	again, err := remote.BuildAgent(e, core, "arm64", run)
	if err != nil || again.SHA256 != got.SHA256 || len(calls) != 1 {
		t.Fatalf("same tree state rebuilt: %+v %v calls=%d", again, err, len(calls))
	}
	if other, err := remote.BuildAgent(e, core, "amd64", run); err != nil || other.Path == got.Path || len(calls) != 2 {
		t.Fatalf("amd64 shares arm64's build: %+v %v", other, err)
	}
}

func TestAR9_BuildSourceNeedsACleanGitTree(t *testing.T) {
	dir, _ := srcTree(t)
	acct := t.TempDir()
	_ = os.Chmod(acct, 0o700)
	e := &config.Effective{Test: &config.Test{AccountingDir: acct}}
	core := &config.Provider{Agent: config.Agent{Source: "build", ModuleDir: dir}}
	never := func(string, []string, ...string) ([]byte, error) { t.Fatal("built from a dirty tree"); return nil, nil }
	var inel *provider.ErrIneligible
	if err := os.WriteFile(filepath.Join(dir, "stray.go"), []byte("package x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := remote.BuildAgent(e, core, "arm64", never); !errors.As(err, &inel) || !strings.Contains(inel.Why, "agent.module_dir: work tree has uncommitted or untracked changes") {
		t.Fatalf("dirty tree: %v", err)
	}
	if _, err := remote.CheckModuleDir(dir); err == nil {
		t.Fatal("CheckModuleDir accepted a dirty tree")
	}
	bare := &config.Provider{Agent: config.Agent{Source: "build", ModuleDir: modDir(t)}}
	if _, err := remote.BuildAgent(e, bare, "arm64", never); !errors.As(err, &inel) || !strings.Contains(inel.Why, "agent.module_dir: not a git work tree") {
		t.Fatalf("not a repo: %v", err)
	}
	// a container provider that fetches by url cannot use a source that is delivered by bytes
	if _, _, err := remote.ResolveAgent(&config.Effective{}, core); !errors.As(err, &inel) || !strings.Contains(inel.Why, "module_dir") {
		t.Fatalf("url-based callers: %v", err)
	}
}

func TestAR10_BuildRecipeCrossCompilesForTheGivenArch(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	_ = os.MkdirAll(bin, 0o755)
	shim := "#!/bin/sh\necho \"CGO_ENABLED=$CGO_ENABLED GOOS=${GOOS-unset} GOARCH=${GOARCH-unset}\" >> \"$LOG\"\n"
	if err := os.WriteFile(filepath.Join(bin, "go"), []byte(shim), 0o755); err != nil { //nolint:gosec // a test shim
		t.Fatal(err)
	}
	build := func(args string) string {
		log := filepath.Join(dir, "log")
		_ = os.Remove(log)
		c := exec.Command("sh", "-c", `. ../internal/agent/buildrecipe.sh && bitc_build 0123456789abcdef /dev/null `+args)
		c.Env = []string{"PATH=" + bin + ":/usr/bin:/bin", "LOG=" + log}
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("%v %s", err, out)
		}
		return strings.TrimSpace(string(must(os.ReadFile(log))))
	}
	if got := build("arm64"); got != "CGO_ENABLED=0 GOOS=linux GOARCH=arm64" {
		t.Errorf("arm64: %q", got)
	}
	if got := build("amd64"); got != "CGO_ENABLED=0 GOOS=linux GOARCH=amd64" {
		t.Errorf("amd64: %q", got)
	}
	// no arch: a native build, as the pod's stub does
	if got := build(""); got != "CGO_ENABLED=0 GOOS=unset GOARCH=unset" {
		t.Errorf("native: %q", got)
	}
}

func TestAR11_BuildScriptsTakeAnArchAndRefuseOthers(t *testing.T) {
	for _, sc := range []string{"agent-build-sha.sh", "repro-check.sh"} {
		for _, bad := range []string{"sparc", "linux/amd64", "AMD64", ""} {
			c := exec.Command("sh", "../scripts/"+sc, "HEAD", bad)
			c.Env = []string{"PATH=/nonexistent", "HOME=" + t.TempDir()} // refuses before git or docker run
			out, err := c.CombinedOutput()
			var ee *exec.ExitError
			if !errors.As(err, &ee) || ee.ExitCode() != 64 || !strings.Contains(string(out), "arch must be amd64 or arm64") {
				t.Errorf("%s %q: %v %s", sc, bad, err, out)
			}
		}
	}
}

func TestAR12_ReleaseConfigBuildsBothArchsStaticallyWithRawBinaries(t *testing.T) {
	y := string(must(os.ReadFile("../.goreleaser.yaml")))
	for _, want := range []string{"goos: [linux, darwin]", "goarch: [amd64, arm64]", "env: [CGO_ENABLED=0]", "flags: [-trimpath]",
		"formats: [binary]", `name_template: "bitc_{{ .Version }}_{{ .Os }}_{{ .Arch }}"`, "name_template: checksums.txt", "algorithm: sha256"} {
		if !strings.Contains(y, want) {
			t.Errorf(".goreleaser.yaml lacks %q", want)
		}
	}
	if n := strings.Count(y, "formats: ["); n != 2 {
		t.Errorf("want a tar.gz archive and a raw binary archive, found %d archive formats", n)
	}
}

func TestAR13_DoctorRemoteExits78ForAnUnusableModuleDir(t *testing.T) {
	dir, _ := srcTree(t)
	h := harness.New(t)
	st := t.TempDir()
	h.HostConfig("[providers.fake]\ndaily_usd = 1\nmode = \"run\"\nstate_dir = " + harness.Q(st) + "\n" +
		"agent = { source = \"build\", module_dir = " + harness.Q(dir) + " }\n")
	if r := h.Run("doctor", "--remote"); r.Code != 0 || !strings.Contains(r.Stdout, "ok fake agent: build ") {
		t.Fatalf("clean tree: %+v", r)
	}
	if err := os.WriteFile(filepath.Join(dir, "stray.go"), []byte("package x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := h.Run("doctor", "--remote")
	if r.Code != 78 || !strings.Contains(r.Stdout, "fail fake agent: agent.module_dir: work tree has uncommitted or untracked changes") {
		t.Fatalf("dirty tree: %+v", r)
	}
}
