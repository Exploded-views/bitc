package conformance

import (
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
	r := h.Run("run", "lint", "--", "sleep", "60")
	if r.Code != 124 || !strings.Contains(r.Stderr, "bitc: lint: time limit 10s reached; terminated\n") {
		t.Fatalf("%+v", r)
	}
	// a command that ends within the limit is untouched
	if r := h.Run("run", "lint", "--", "sh", "-c", "exit 3"); r.Code != 3 || strings.Contains(r.Stderr, "time limit") {
		t.Fatalf("%+v", r)
	}
	// 124 from the command itself is not a timeout: no line
	if r := h.Run("run", "lint", "--", "sh", "-c", "exit 124"); r.Code != 124 || strings.Contains(r.Stderr, "time limit") {
		t.Fatalf("%+v", r)
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
}

func TestTL5_ARemoteRunIgnoresTheTimeoutFlag(t *testing.T) {
	h := harness.New(t)
	f, _ := rpRepo(t, h, "")
	r := h.Run("run", "--timeout", "1s", "heavy", "--", "echo", "x")
	if r.Code != 0 || r.Stdout != "x\n" || calls(f, "POST /v2/pods") != 1 || strings.Contains(r.Stderr, "time limit") {
		t.Fatalf("%+v %v", r, f.Calls())
	}
}
