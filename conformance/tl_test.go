package conformance

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Exploded-views/bitc/conformance/harness"
	"github.com/Exploded-views/bitc/internal/proctest"
)

func TestTL1_ForegroundRunIsKilledAtTheLimit(t *testing.T) {
	h := harness.New(t)
	scaledHost(h, "[host]\ngrace = \"20s\"\n[pools.lint]\ntimeout = \"10s\"\n")
	res := filepath.Join(h.Root, "r.json")
	r := h.Run("run", "--result", res, "lint", "--", "sleep", "60")
	if r.Code != 124 || !strings.Contains(r.Stderr, "bitc: lint: time limit 10s reached; terminated\n") {
		t.Fatalf("%+v", r)
	}
	var got struct{ Origin, Outcome string }
	if err := json.Unmarshal(must(os.ReadFile(res)), &got); err != nil || got.Origin != "bitc" || got.Outcome != "timed-out" {
		t.Fatalf("result.json: %s %v", must(os.ReadFile(res)), err)
	}
	if end := lastEnd(t, h); end["outcome"] != "timed-out" || end["exit"] != float64(124) {
		t.Fatalf("history end: %v", end)
	}
	// a command that ends within the limit is untouched
	if r := h.Run("run", "lint", "--", "sh", "-c", "exit 3"); r.Code != 3 || strings.Contains(r.Stderr, "time limit") {
		t.Fatalf("%+v", r)
	}
	// 124 from the command itself is not a timeout: no line, and its own outcome
	if r := h.Run("run", "lint", "--", "sh", "-c", "exit 124"); r.Code != 124 || strings.Contains(r.Stderr, "time limit") {
		t.Fatalf("%+v", r)
	}
	if end := lastEnd(t, h); end["outcome"] != "command-failed" {
		t.Fatalf("history end: %v", end)
	}
}

func TestTL2_TimeoutFlagNarrowsNeverWidens(t *testing.T) {
	h := harness.New(t)
	scaledHost(h, "[host]\ngrace = \"20s\"\n[pools.lint]\ntimeout = \"3600s\"\n[pools.short]\ntimeout = \"10s\"\n[pools.open]\n")
	limit := func(pool string, flag ...string) string {
		r := h.Run(append(append([]string{"run"}, flag...), pool, "--", "sleep", "60")...)
		if r.Code != 124 {
			t.Fatalf("%s %v: %+v", pool, flag, r)
		}
		return r.Stderr
	}
	if e := limit("lint", "--timeout", "10s"); !strings.Contains(e, "time limit 10s reached") {
		t.Fatalf("--timeout did not narrow: %q", e)
	}
	if e := limit("short", "--timeout", "3600s"); !strings.Contains(e, "time limit 10s reached") {
		t.Fatalf("--timeout widened the pool's limit: %q", e)
	}
	if e := limit("open", "--timeout", "10s"); !strings.Contains(e, "time limit 10s reached") {
		t.Fatalf("--timeout on a pool without a limit: %q", e)
	}
	for _, bad := range []string{"5x", "0s", "30"} {
		if r := h.Run("run", "--timeout", bad, "lint", "--", "true"); r.Code != 64 || !strings.Contains(r.Stderr, "--timeout") {
			t.Fatalf("--timeout %s: %+v", bad, r)
		}
	}
	h.HostConfig("[pools.lint]\ntimeout = \"soon\"\n")
	if r := h.Run("run", "lint", "--", "true"); r.Code != 78 || !strings.Contains(r.Stderr, "pools.lint.timeout") {
		t.Fatalf("%+v", r)
	}
}

func TestTL3_TheCommandsBackgroundChildIsGoneAfterTheLimit(t *testing.T) {
	h := harness.New(t)
	scaledHost(h, "[host]\ngrace = \"20s\"\n[pools.lint]\ntimeout = \"300s\"\n")
	pidFile := filepath.Join(h.Root, "bg")
	r := h.Run("run", "lint", "--", "sh", "-c", `sleep 60 & echo $! > "$1"; wait`, "_", pidFile)
	if r.Code != 124 {
		t.Fatalf("%+v", r)
	}
	pid := h.Atoi(string(must(os.ReadFile(pidFile))))
	h.Until(func() bool { return proctest.Gone(pid) }, 5*time.Second)
	if r := h.Run("run", "lint", "--", "true"); r.Code != 0 { // and the slot is free
		t.Fatalf("%+v", r)
	}
}

func TestTL4_DetachedJobHitsTheLimit(t *testing.T) {
	h := harness.New(t)
	scaledHost(h, "[host]\ngrace = \"20s\"\n[pools.lint]\ntimeout = \"3600s\"\n")
	name, _, _ := detached(t, h, "--timeout", "10s", "lint", "--", "sleep", "60")
	r := h.Run("wait", name, "--exit", "job", "--timeout", "6000s")
	if r.Code != 124 || !strings.HasPrefix(r.Stdout, "RESULT exit 124\n") || !strings.Contains(r.Stdout, "time limit 10s reached; terminated") {
		t.Fatalf("%+v", r)
	}
	files, _ := filepath.Glob(filepath.Join(h.State, "jobs", "*", name, "result.json"))
	if len(files) != 1 {
		t.Fatalf("result.json: %v", files)
	}
	var got struct{ Origin, Outcome string }
	if err := json.Unmarshal(must(os.ReadFile(files[0])), &got); err != nil || got.Origin != "bitc" || got.Outcome != "timed-out" {
		t.Fatalf("result.json: %s %v", must(os.ReadFile(files[0])), err)
	}
}

// lastEnd is the history's last `end` line.
func lastEnd(t *testing.T, h *harness.H) map[string]any {
	t.Helper()
	var end map[string]any
	for _, l := range strings.Split(strings.TrimSpace(string(must(os.ReadFile(filepath.Join(h.State, "history.jsonl"))))), "\n") {
		var e map[string]any
		if err := json.Unmarshal([]byte(l), &e); err != nil {
			t.Fatal(err)
		}
		if e["event"] == "end" {
			end = e
		}
	}
	if end == nil {
		t.Fatal("no end line in the history")
	}
	return end
}
