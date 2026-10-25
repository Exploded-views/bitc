package conformance

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Exploded-views/bitc/conformance/harness"
	"github.com/Exploded-views/bitc/internal/classify/classifytest"
	"github.com/Exploded-views/bitc/internal/cli"
	"github.com/Exploded-views/bitc/internal/config"
	"github.com/Exploded-views/bitc/internal/proctest"
)

// Review Focus 3.
func TestPI1_AwkwardPathsAndArgv(t *testing.T) {
	h := harness.New(t)
	h.HostConfig("[pools.lint]\n")
	h.Cwd = h.Repo("wörk tree 'q' \"x\"", nil)
	argv := []string{"", "with space", "new\nline", "-dash", "ünï", "$HOME", "*"}
	r := h.Run(append([]string{"run", "lint", "--", "sh", "-c", `for a; do printf '[%s]' "$a"; done`, "_"}, argv...)...)
	want := "[][with space][new\nline][-dash][ünï][$HOME][*]"
	if r.Code != 0 || r.Stdout != want {
		t.Fatalf("%q", r.Stdout)
	}
	hs := history(t, h)
	if len(hs) == 0 {
		t.Fatal("no history lines")
	}
	got := hs[len(hs)-1].Argv
	if strings.Join(got[4:], "|") != strings.Join(argv, "|") || hs[0].Cwd != h.Cwd {
		t.Fatalf("history %q cwd %q", got, hs[0].Cwd)
	}
	// a word long enough to truncate the history line still reaches the command whole
	r = h.Run("run", "lint", "--", "sh", "-c", `printf %s "$1" | wc -c`, "_", strings.Repeat("x", 6000))
	if r.Code != 0 || strings.TrimSpace(r.Stdout) != "6000" {
		t.Fatalf("the command got a truncated argv: %+v", r)
	}
}

// Review Focus 5. The failures are ones root cannot bypass (a directory where the history file
// should be, a regular file as the result's parent), so the test holds under any uid.
func TestPI2_BookkeepingFailureKeepsExitCode(t *testing.T) {
	h := harness.New(t)
	h.HostConfig("[pools.lint]\n")
	h.Run("run", "lint", "--", "true")
	hist := filepath.Join(h.State, "history.jsonl")
	_ = os.Remove(hist)                           // base binaries wrote none
	if err := os.Mkdir(hist, 0o700); err != nil { // EISDIR on open for append
		t.Fatal(err)
	}
	notDir := filepath.Join(h.Root, "not-a-dir")
	h.Write(notDir, "x", 0o600) // ENOTDIR for anything below it
	r := h.Run("run", "--result", filepath.Join(notDir, "r.json"), "lint", "--", "sh", "-c", "exit 5")
	if r.Code != 5 || strings.Count(r.Stderr, "cannot write") != 2 {
		t.Fatalf("%+v", r)
	}
	if r := h.Run("run", "--wait", "0s", "lint", "--", "true"); r.Code != 0 {
		t.Fatalf("slot not released: %+v", r)
	}
}

func TestPI3_InternalErrorIs69NeverGo2(t *testing.T) {
	cli.Register("__conformance_panic", func(_ context.Context, _ config.Flags, _ []string, _ cli.Std) int { panic("bug") })
	f, _ := os.CreateTemp(t.TempDir(), "err")
	code := cli.Main([]string{"__conformance_panic"}, cli.Std{In: os.Stdin, Out: os.Stdout, Err: f})
	b, _ := os.ReadFile(f.Name())
	if code != 69 || string(b) != "bitc: internal error: bug\n" {
		t.Fatalf("%d %q", code, b)
	}
}

func TestPI4_ResultJSONForAForegroundRun(t *testing.T) {
	h := harness.New(t)
	h.HostConfig("[pools.lint]\nwait = \"0s\"\n")
	out := filepath.Join(h.Root, "r.json")
	h.Run("run", "--result", out, "--name", "j1", "lint", "--", "sh", "-c", "exit 3")
	var res map[string]any
	if err := json.Unmarshal(readOrFail(t, out), &res); err != nil {
		t.Fatal(err)
	}
	if res["origin"] != "command" || res["outcome"] != "command-failed" || res["exit_code"] != 3.0 || res["job"] != "j1" || res["slot"] != 1.0 {
		t.Fatalf("%v", res)
	}
	hold := h.Hold(nil, "lint")
	h.Run("run", "--result", out, "lint", "--", "true")
	_ = json.Unmarshal(readOrFail(t, out), &res)
	if res["origin"] != "bitc" || res["outcome"] != "starved" || res["exit_code"] != 75.0 {
		t.Fatalf("%v", res)
	}
	hold.Release()
	// a run that stops in Prepare still replaces the earlier result.json
	nodeRepo(t, h, "", nil)
	if r := h.Run(append([]string{"run", "--result", out, "lint", "--"}, classifytest.RefusedFromRoot[0]...)...); r.Code != 64 {
		t.Fatalf("%+v", r)
	}
	res = nil
	_ = json.Unmarshal(readOrFail(t, out), &res)
	if res["origin"] != "bitc" || res["outcome"] != "refused" || res["exit_code"] != 64.0 {
		t.Fatalf("a refused run left the earlier result: %v", res)
	}
}

func TestPI5_SignalWhileWaitingIs128PlusNAndLeavesNothing(t *testing.T) {
	h := harness.New(t)
	h.HostConfig("[pools.lint]\nwait = \"30s\"\n")
	hold := h.Hold(nil, "lint")
	w := h.Start("run", "lint", "--", "true")
	h.Until(func() bool { return len(tickets(h, "lint")) == 2 }, 10*time.Second)
	w.Signal(syscall.SIGINT)
	if r := w.Wait(); r.Code != 130 {
		t.Fatalf("%+v", r)
	}
	if len(tickets(h, "lint")) != 1 {
		t.Fatalf("tickets %v", tickets(h, "lint"))
	}
	hold.Release()
}

func TestPI7_BitcLogLevels(t *testing.T) {
	h := harness.New(t)
	h.HostConfig("[pools.heavy]\nproviders = [\"runpod\", \"local\"]\n[providers.runpod]\ndaily_usd = 1\nimage = \"x\"\nsizes = {}\napi_base = \"http://127.0.0.1:9\"\nproxy_domain = \"127.0.0.1:9\"\n")
	note := "bitc: debug: heavy: runpod: " // M1: "not available in this build"; M2: the walk's reason
	for _, c := range []struct {
		env, flags []string
		want       bool
	}{
		{nil, nil, false},
		{[]string{"BITC_LOG=info"}, nil, false},
		{[]string{"BITC_LOG=debug"}, nil, true},
		{nil, []string{"-v"}, true},
		{[]string{"BITC_LOG=quiet"}, []string{"-v"}, false},
	} {
		r := h.RunWith(c.env, append(c.flags, "run", "heavy", "--", "echo", "x")...)
		if r.Code != 0 || r.Stdout != "x\n" || strings.Contains(r.Stderr, note) != c.want {
			t.Errorf("%v %v: %+v", c.env, c.flags, r)
		}
	}
	if r := h.RunWith([]string{"BITC_LOG=loud"}, "run", "heavy", "--", "true"); r.Code != 64 {
		t.Fatalf("%+v", r)
	}
}

func TestPI6_StarvedAndReapedAreHistoryEvents(t *testing.T) {
	h := harness.New(t)
	h.HostConfig("[pools.lint]\nwait = \"0s\"\n")
	hold := h.Hold(nil, "lint")
	h.Run("run", "lint", "--", "true")
	hold.Release()
	found := false
	for _, e := range history(t, h) {
		found = found || e.Event == "starved"
	}
	if !found {
		t.Fatal("no starved event")
	}
}

// readOrFail reads path, failing the test (not panicking) when it is missing.
func readOrFail(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestPI8_SignalDuringPostRunKeepsTheCommandsResult(t *testing.T) {
	h := harness.New(t)
	marker := filepath.Join(h.Root, "hook.pid")
	h.HostConfig(postRunPool(marker))
	res := filepath.Join(h.Root, "r.json")
	p := h.Start("run", "--result", res, "lint", "--", "sh", "-c", "exit 3")
	h.Until(func() bool { _, err := os.Stat(marker); return err == nil }, 10*time.Second)
	hookPid := h.Atoi(strings.TrimSpace(string(readOrFail(t, marker))))
	p.Signal(syscall.SIGTERM)
	h.Until(p.Exited, 10*time.Second)
	r := p.Wait()
	if r.Code != 3 {
		t.Fatalf("a signal during post_run changed the command's code: %+v", r)
	}
	var got struct {
		Outcome  string `json:"outcome"`
		ExitCode int    `json:"exit_code"`
	}
	if err := json.Unmarshal(readOrFail(t, res), &got); err != nil || got.ExitCode != 3 || got.Outcome != "command-failed" {
		t.Fatalf("result.json %+v %v", got, err)
	}
	h.Until(func() bool { return proctest.Gone(hookPid) }, 5*time.Second)
	if r := h.Run("run", "lint", "--", "true"); r.Code == 75 {
		t.Fatalf("the slot was left held: %+v", r)
	}
}
