package conformance

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Exploded-views/bitc/conformance/harness"
	"github.com/Exploded-views/bitc/internal/config"
)

func gitIn(t *testing.T, h *harness.H, dir string, args ...string) string {
	t.Helper()
	c := exec.Command("git", args...)
	c.Dir, c.Env = dir, h.Env
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// originRepo: capRepo's work tree and the bare origin harness.Repo gave it.
func originRepo(t *testing.T, h *harness.H) (wt, bare, st string) {
	st = capRepo(t, h, 0)
	return h.Cwd, h.Origin("app"), st
}

var refRE = regexp.MustCompile(`^refs/bitc/jobs/[0-9a-f]{16}/heavy/[0-9a-f]{12}-[0-9]+-[0-9a-f]{8}$`)

func TestK1_PushesExactlyHEADToAFreshRef(t *testing.T) {
	h := harness.New(t)
	wt, bare, st := originRepo(t, h)
	head := gitIn(t, h, wt, "rev-parse", "HEAD")
	p := h.Start("run", "heavy", "--", "sh", "-c", "sleep 1")
	h.Until(func() bool { b, _ := os.ReadFile(filepath.Join(st, "launches.log")); return len(b) > 0 }, 10*time.Second)
	f := strings.Fields(slurp(filepath.Join(st, "launches.log")))
	if len(f) != 3 || !refRE.MatchString(f[1]) || f[2] != head {
		t.Fatalf("%v", f)
	}
	if got := gitIn(t, h, bare, "rev-parse", f[1]); got != head {
		t.Fatalf("ref tip %s, HEAD %s", got, head)
	}
	p.Wait()
	h.Until(func() bool { return gitIn(t, h, bare, "for-each-ref", "refs/bitc/") == "" }, 10*time.Second)
	if left := gitIn(t, h, bare, "for-each-ref", "refs/bitc/"); left != "" {
		t.Fatalf("the job ref was not deleted: %s", left)
	}
}

func TestRB3_BuiltInKnownHostsPinTheForgeOnly(t *testing.T) {
	lines := strings.Split(strings.TrimSpace(config.BuiltinKnownHosts), "\n")
	if len(lines) != 3 {
		t.Fatalf("%d lines", len(lines))
	}
	for _, l := range lines {
		if !strings.HasPrefix(l, "github.com ") {
			t.Fatalf("%q", l)
		}
	}
	h := harness.New(t)
	wt, _, _ := originRepo(t, h)
	gitIn(t, h, wt, "remote", "set-url", "origin", "https://git.example.invalid/o/r.git")
	r := h.Run("run", "heavy", "--", "echo", "x")
	if !strings.Contains(r.Stderr, "running heavy locally: the clone host git.example.invalid is not in transport.known_hosts") {
		t.Fatalf("%+v", r)
	}
}

func TestTR1_CredentialsInTheOriginAreNeverSent(t *testing.T) {
	h := harness.New(t)
	wt, _, _ := originRepo(t, h)
	gitIn(t, h, wt, "remote", "set-url", "origin", "https://user:s3cr3t-tok@example.invalid/o/r.git")
	r := h.Run("run", "heavy", "--", "echo", "x")
	if strings.Contains(r.Stdout+r.Stderr, "s3cr3t-tok") || !strings.Contains(r.Stderr, "example.invalid") {
		t.Fatalf("%+v", r)
	}
	if strings.Contains(slurp(filepath.Join(h.State, "history.jsonl")), "s3cr3t-tok") {
		t.Fatal("history holds the credential")
	}
}

func TestTR3_PushTakesTurnsInTheGitPool(t *testing.T) {
	h := harness.New(t)
	originRepo(t, h)
	h.HostConfig(readCfg(t, h) + "\n[pools.git]\nwait = \"1s\"\n")
	hold := h.Hold(nil, "git")
	r := h.Run("run", "heavy", "--", "echo", "x")
	hold.Release()
	if r.Stdout != "x\n" || !strings.Contains(r.Stderr, "running heavy locally: could not push the commit: STARVED git") {
		t.Fatalf("the push skipped the busy git pool: %+v", r)
	}
	if r := h.Run("run", "heavy", "--", "echo", "y"); r.Stdout != "y\n" || strings.Contains(r.Stderr, "locally") {
		t.Fatalf("%+v", r)
	}
}

func TestTR2_PortInTheRemoteIsIneligible(t *testing.T) {
	h := harness.New(t)
	wt, _, _ := originRepo(t, h)
	gitIn(t, h, wt, "remote", "set-url", "origin", "https://git.example.invalid:8443/o/r.git")
	if r := h.Run("run", "heavy", "--", "echo", "x"); !strings.Contains(r.Stderr, "has a port; set clone_url") {
		t.Fatalf("%+v", r)
	}
}

// A commit landing in the work tree while the push runs must not move what is pushed: the ref
// tip is the sha the job was launched with (spec 8.4).
func TestK1_PushedRefTipIsTheResolvedShaWhenHEADMoves(t *testing.T) {
	h := harness.New(t)
	wt, bare, st := originRepo(t, h)
	head := gitIn(t, h, wt, "rev-parse", "HEAD")
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	shim := filepath.Join(h.Root, "shim")
	_ = os.MkdirAll(shim, 0o700)
	script := "#!/bin/sh\ncase \" $* \" in *\" push \"*)\n" +
		"  " + realGit + " -C \"" + wt + "\" -c user.name=t -c user.email=t@example.invalid commit -q --allow-empty -m moved\n;;\nesac\nexec " + realGit + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(shim, "git"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	p := h.StartWith([]string{"PATH=" + shim + ":" + os.Getenv("PATH")}, "run", "heavy", "--", "sh", "-c", "sleep 1")
	h.Until(func() bool { b, _ := os.ReadFile(filepath.Join(st, "launches.log")); return len(b) > 0 }, 10*time.Second)
	f := strings.Fields(slurp(filepath.Join(st, "launches.log")))
	if len(f) != 3 || f[2] != head {
		t.Fatalf("%v", f)
	}
	if got := gitIn(t, h, bare, "rev-parse", f[1]); got != head {
		t.Fatalf("ref tip %s, launched sha %s", got, head)
	}
	p.Wait()
	h.Until(func() bool { return gitIn(t, h, bare, "for-each-ref", "refs/bitc/") == "" }, 10*time.Second)
}

// agent = { source = "build" }: the agent commit rides to the forge on its own hidden ref
// next to the job ref and both are deleted at the end (spec 8.4 step 1).
func TestAG10_BuildAgentCommitIsPushedOnAHiddenRefAndDeleted(t *testing.T) {
	h := harness.New(t)
	wt, bare, st := originRepo(t, h)
	head := gitIn(t, h, wt, "rev-parse", "HEAD")
	agentLine := "agent = { source = \"build\", commit = \"" + head + "\", image = \"golang@sha256:" + strings.Repeat("b", 64) + "\", sha256 = \"" + strings.Repeat("c", 64) + "\" }\n"
	h.HostConfig(strings.Replace(readCfg(t, h), "usd_per_hour = 0.24\n", "usd_per_hour = 0.24\n"+agentLine, 1))
	p := h.Start("run", "heavy", "--", "sh", "-c", "sleep 1")
	h.Until(func() bool { b, _ := os.ReadFile(filepath.Join(st, "launches.log")); return len(b) > 0 }, 10*time.Second)
	refs := gitIn(t, h, bare, "for-each-ref", "--format=%(refname) %(objectname)", "refs/bitc/agent/")
	f := strings.Fields(refs)
	if len(f) != 2 || !strings.HasPrefix(f[0], "refs/bitc/agent/") || !strings.Contains(f[0], "/heavy/"+head[:12]+"-") || f[1] != head {
		t.Fatalf("agent ref: %q", refs)
	}
	p.Wait()
	h.Until(func() bool { return gitIn(t, h, bare, "for-each-ref", "refs/bitc/") == "" }, 10*time.Second)
	if left := gitIn(t, h, bare, "for-each-ref", "refs/bitc/"); left != "" {
		t.Fatalf("refs left: %s", left)
	}
}

func TestAG11_BuildAgentCommitMissingLocallyIsIneligible(t *testing.T) {
	h := harness.New(t)
	originRepo(t, h)
	agentLine := "agent = { source = \"build\", commit = \"" + strings.Repeat("e", 40) + "\", image = \"golang@sha256:" + strings.Repeat("b", 64) + "\", sha256 = \"" + strings.Repeat("c", 64) + "\" }\n"
	h.HostConfig(strings.Replace(readCfg(t, h), "usd_per_hour = 0.24\n", "usd_per_hour = 0.24\n"+agentLine, 1))
	r := h.Run("run", "heavy", "--", "echo", "x")
	if !strings.Contains(r.Stderr, "agent commit "+strings.Repeat("e", 12)) || r.Stdout != "x\n" {
		t.Fatalf("%+v", r)
	}
}

// A host crash or a failed detached delete leaves the agent ref on the forge; it is named like a
// job ref so sweep ages it out the same way (3h).
func TestAG12_SweepRemovesAStaleAgentRef(t *testing.T) {
	h := harness.New(t)
	_, wt := rpRepo(t, h, "")
	inst, _ := install16(t, h)
	bare := h.Origin("app")
	head := gitIn(t, h, wt, "rev-parse", "HEAD")
	stale := "refs/bitc/agent/" + inst + "/heavy/" + head[:12] + "-" + unixAgo(4*time.Hour) + "-deadbeef"
	fresh := "refs/bitc/agent/" + inst + "/heavy/" + head[:12] + "-" + unixAgo(time.Minute) + "-cafe0000"
	gitIn(t, h, bare, "update-ref", stale, head)
	gitIn(t, h, bare, "update-ref", fresh, head)
	if r := h.Run("sweep"); !strings.Contains(r.Stdout, "swept ref "+stale) || strings.Contains(r.Stdout, fresh) {
		t.Fatalf("%+v", r)
	}
	if left := gitIn(t, h, bare, "for-each-ref", "--format=%(refname)", "refs/bitc/agent/"); left != fresh {
		t.Fatalf("agent refs left: %q", left)
	}
}
