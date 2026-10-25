package conformance

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"context"
	"time"

	"github.com/Exploded-views/bitc/conformance/harness"
	"github.com/Exploded-views/bitc/internal/agent"
	"github.com/Exploded-views/bitc/internal/config"
	"github.com/Exploded-views/bitc/internal/provider"
	"github.com/Exploded-views/bitc/internal/provider/runpod"
	"github.com/Exploded-views/bitc/internal/provider/runpod/runpodfake"
	"github.com/Exploded-views/bitc/internal/remote"
)

const (
	tbCommit = "0123456789abcdef0123456789abcdef01234567"
	tbRef    = "refs/bitc/agent/0123456789abcdef/heavy/0123456789ab-1700000000-deadbeef"
	tbClone  = "git@github.example:org/repo.git"
)

// runStubBuild runs the generated build stub under sh with git, go and ssh shims on PATH. The
// shims log their argv and what they see of the key file to <dir>/log.
func runStubBuild(t *testing.T, sum string) (out, log, dir string, code int) {
	t.Helper()
	return runStubBuildPayload(t, "#!/bin/sh\necho agent-ran \"$@\"\n", sum)
}

// runStubBuildPayload is runStubBuild with the "built" agent being payload.
func runStubBuildPayload(t *testing.T, payload, sum string) (out, log, dir string, code int) {
	t.Helper()
	dir = t.TempDir()
	bin := filepath.Join(dir, "bin")
	_ = os.MkdirAll(bin, 0o755)
	shim := func(name, body string) {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+body), 0o755); err != nil { //nolint:gosec // a test shim
			t.Fatal(err)
		}
	}
	shim("git", `echo "git $*" >> "$LOG"
case "$*" in *" fetch "*) k=$(ls "$BITC_STUB_DIR"/*/key 2>/dev/null | head -1); echo "keymode $(stat -c %a "$k") dirmode $(stat -c %a "$(dirname "$k")")" >> "$LOG";; esac
case "$*" in *rev-parse*) echo `+tbCommit+`;; esac
exit 0
`)
	shim("go", `echo "go $*" >> "$LOG"
echo "env CGO_ENABLED=$CGO_ENABLED GOFLAGS=$GOFLAGS GOTOOLCHAIN=$GOTOOLCHAIN" >> "$LOG"
echo "key $(ls "$BITC_STUB_DIR"/*/key 2>/dev/null | wc -l) cwd $(pwd | sed 's|.*/||')" >> "$LOG"
while [ $# -gt 0 ]; do [ "$1" = -o ] && o=$2; shift; done
printf '%s' '`+payload+`' > "$o"
`)
	shim("ssh", "exit 1\n")
	cmd := exec.Command("sh", "-c", agent.StubBuild(tbClone, tbRef, tbCommit, sum))
	cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin", "BITC_STUB_DIR=" + filepath.Join(dir, "stub"), "LOG=" + filepath.Join(dir, "log"),
		"BITC_DEPLOY_KEY=SECRET-KEY-BODY", "BITC_KNOWN_HOSTS=github.example ssh-ed25519 AAAA"}
	b, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code = ee.ExitCode()
	}
	l, _ := os.ReadFile(filepath.Join(dir, "log"))
	return string(b), string(l), dir, code
}

func TestAG4_BuildStubFetchesThePinnedRefAndShredsTheKey(t *testing.T) {
	out, log, dir, code := runStubBuild(t, sha256Hex("#!/bin/sh\necho agent-ran \"$@\"\n"))
	if code != 0 || !strings.Contains(out, "agent-ran __agent") {
		t.Fatalf("code %d: %q\n%s", code, out, log)
	}
	if !strings.Contains(log, "fetch -q --depth=1 "+tbClone+" "+tbRef) {
		t.Fatalf("fetch is not shallow on the ref:\n%s", log)
	}
	if !strings.Contains(log, "IdentitiesOnly=yes") || !strings.Contains(log, "StrictHostKeyChecking=yes") || !strings.Contains(log, "UserKnownHostsFile=") {
		t.Fatalf("ssh options:\n%s", log)
	}
	if !strings.Contains(log, "keymode 600 dirmode 700") {
		t.Fatalf("key file mode at fetch:\n%s", log)
	}
	if !strings.Contains(log, "key 0 ") {
		t.Fatalf("the key file still existed when the build ran:\n%s", log)
	}
	if !strings.Contains(log, "rev-parse HEAD") {
		t.Fatalf("the checked-out commit was not verified:\n%s", log)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "stub", "*")); len(left) != 1 || filepath.Base(left[0]) != "bitc" {
		t.Fatalf("the build tree or caches were left behind: %v", left)
	}
}

func TestAG13_BuildStubExecsTheAgentWithUmask022(t *testing.T) {
	payload := "#!/bin/sh\necho agent-umask $(umask)\n"
	out, log, _, code := runStubBuildPayload(t, payload, sha256Hex(payload))
	if code != 0 || !strings.Contains(out, "agent-umask 0022") {
		t.Fatalf("the agent must run under umask 022 (job dirs the job user traverses), code %d: %q\n%s", code, out, log)
	}
}

func TestAG5_BuildStubUsesExactlyTheReproducibleRecipe(t *testing.T) {
	_, log, _, _ := runStubBuild(t, strings.Repeat("0", 64))
	c12 := tbCommit[:12]
	want := "go build -trimpath -buildvcs=false -ldflags -s -w -X github.com/Exploded-views/bitc/internal/version.Version=build-" + c12 +
		" -X github.com/Exploded-views/bitc/internal/version.Commit=" + c12 + " -o "
	if !strings.Contains(log, want) || !strings.Contains(log, " ./cmd/bitc\n") {
		t.Fatalf("build command:\n%s", log)
	}
	if !strings.Contains(log, "env CGO_ENABLED=0 GOFLAGS=-mod=readonly GOTOOLCHAIN=local\n") {
		t.Fatalf("build env:\n%s", log)
	}
}

func TestAG6_BuildStubRefusesAShaMismatchAndNeverExecs(t *testing.T) {
	out, _, _, code := runStubBuild(t, strings.Repeat("0", 64))
	if code != 1 || !strings.Contains(out, "bitc agent: build sha256 mismatch") || strings.Contains(out, "agent-ran") {
		t.Fatalf("code %d: %q", code, out)
	}
}

func TestAG7_BuildRecipeModulePathMatchesGoMod(t *testing.T) {
	mod, err := os.ReadFile("../go.mod")
	if err != nil {
		t.Fatal(err)
	}
	recipe, err := os.ReadFile("../internal/agent/buildrecipe.sh")
	if err != nil {
		t.Fatal(err)
	}
	if first := strings.SplitN(string(mod), "\n", 2)[0]; !strings.Contains(string(recipe), "\nbitc_pkg="+strings.TrimPrefix(first, "module ")+"\n") {
		t.Fatalf("buildrecipe.sh names another module than go.mod: %q", first)
	}
}

func TestAG8_BuildSourceResolvesToTheBuildVariant(t *testing.T) {
	c, img, sum := strings.Repeat("a", 40), "golang:1.27.2-bookworm@sha256:"+strings.Repeat("b", 64), strings.Repeat("c", 64)
	core := &config.Provider{Agent: config.Agent{Source: "build", Commit: c, Image: img, SHA256: sum}}
	src, err := remote.ResolveAgentSource(&config.Effective{}, core)
	if err != nil || src.Build == nil || src.URL != "" || src.SHA256 != sum || src.Build.Commit != c || src.Build.Image != img {
		t.Fatalf("%+v %v", src, err)
	}
	if got, want := src.String(), "build aaaaaaaaaaaa on "+img; got != want {
		t.Fatalf("doctor line %q, want %q", got, want)
	}
	if u, s, err := remote.ResolveAgent(&config.Effective{}, core); err != nil || u != "" || s != sum {
		t.Fatalf("ResolveAgent %q %q %v", u, s, err)
	}
}

func TestAG9_CreateBodyForABuildAgent(t *testing.T) {
	f := runpodfake.New(t, runpodfake.Options{Bin: harness.Bin, Secrets: map[string]string{"app_key": "PRIVATE KEY"}})
	p := rpProvider(t, f)
	runpod.AgentSpec = func(j *provider.JobSpec) agent.Spec {
		return agent.Spec{Job: j.Job, Nonce: "nn", Unprivileged: true, TestRoot: t.TempDir(), CloneURL: "/srv/o/r.git", KnownHosts: "h ssh-ed25519 AAAA"}
	}
	img, sum := "golang:1.27.2-bookworm@sha256:"+strings.Repeat("b", 64), strings.Repeat("c", 64)
	j := &provider.JobSpec{Job: "j1", Pool: "heavy", SizeKey: "cpu8", Name: "bitc-x", Owner: "tag", DeploySecret: "app_key", AgentSHA256: sum,
		AgentBuild: &provider.AgentBuild{Commit: strings.Repeat("a", 40), Ref: "refs/bitc/agent/x/j1", Image: img, SHA256: sum}, Deadline: time.Now().Add(time.Hour)}
	if _, err := p.Launch(context.Background(), j); err != nil {
		t.Fatal(err)
	}
	b := f.Bodies()[0]
	env := b["env"].(map[string]any)
	if b["image"] != img || env["BITC_KNOWN_HOSTS"] != "h ssh-ed25519 AAAA" || env["BITC_AGENT_SHA256"] != sum || env["BITC_DEPLOY_KEY"] != "{{ RUNPOD_SECRET_app_key }}" {
		t.Fatalf("%v %v", b["image"], env)
	}
	if _, ok := env["BITC_AGENT_URL"]; ok {
		t.Fatal("a build agent has no download URL")
	}
	want := agent.StubBuild("/srv/o/r.git", "refs/bitc/agent/x/j1", strings.Repeat("a", 40), sum)
	if cmd := b["cmd"].([]any); len(cmd) != 1 || cmd[0] != want {
		t.Fatal("the start command is not the build stub")
	}
}

const agHostHead = "[providers.fake]\ndaily_usd = 25\nmax_live = 6\nstart_timeout = \"20m\"\nusd_per_hour = 0.24\n"

func TestAG3_BuildAgentIsValidatedAtLoadAndHostOnly(t *testing.T) {
	c, img, sum := strings.Repeat("a", 40), "golang@sha256:"+strings.Repeat("b", 64), strings.Repeat("c", 64)
	line := func(commit, image, sha string) string {
		return `agent = { source = "build", commit = "` + commit + `", image = "` + image + `", sha256 = "` + sha + `" }` + "\n"
	}
	for _, bad := range []string{line(c[:39], img, sum), line(strings.ToUpper(c), img, sum), line(c, "golang:1.27", sum),
		line(c, img, sum[:63]), line(c, img, ""), line("", img, sum), line(c, "", sum)} {
		h := harness.New(t)
		h.HostConfig(agHostHead + bad)
		if r := h.Run("config", "check"); r.Code != 78 || !strings.Contains(r.Stderr, "agent") {
			t.Fatalf("%q: %+v", bad, r)
		}
	}
	h := harness.New(t)
	h.HostConfig(agHostHead + line(c, img, sum))
	if r := h.Run("config", "check"); r.Code != 0 {
		t.Fatalf("%+v", r)
	}
	h.Cwd = h.Repo("app", map[string]string{".bitc.toml": "version = 1\n[providers.fake]\n" + line(c, img, sum)})
	if r := h.Run("config", "check"); r.Code != 78 {
		t.Fatalf("a repo file set the agent: %+v", r)
	}
}
