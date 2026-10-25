package conformance

import (
	"strings"
	"testing"
	"time"

	"github.com/Exploded-views/bitc/conformance/harness"
)

func TestHK1_CRLFHookOutputIsUnparsable(t *testing.T) {
	h := harness.New(t)
	nodeRepo(t, h, "", nil)
	h.Write(h.Cwd+"/scripts/stack-env.sh", "#!/bin/sh\nprintf 'A=1\\r\\n'\n", 0o755)
	r := h.Run("run", "e2e", "--", "echo", "ran")
	if r.Code != 69 || r.Stdout != "" || !strings.Contains(r.Stderr, "unparsable") {
		t.Fatalf("%+v", r)
	}
}

func TestHK2_HookTimeoutIs69(t *testing.T) {
	h := harness.New(t)
	nodeRepo(t, h, "", func(s string) string { return strings.Replace(s, `timeout = "60s"`, `timeout = "300ms"`, 1) })
	h.Write(h.Cwd+"/scripts/stack-env.sh", "#!/bin/sh\nsleep 5\n", 0o755)
	if r := h.Run("run", "e2e", "--", "true"); r.Code != 69 || !strings.Contains(r.Stderr, "timed out") {
		t.Fatalf("%+v", r)
	}
}

func TestHK3_Hook75IsStarvedWithReason(t *testing.T) {
	h := harness.New(t)
	nodeRepo(t, h, "", nil)
	h.Write(h.Cwd+"/scripts/stack-env.sh", "#!/bin/sh\necho 'stack 31 is busy' >&2\nexit 75\n", 0o755)
	if r := h.Run("run", "e2e", "--", "true"); r.Code != 75 || r.Stderr != "STARVED e2e (env hook: stack 31 is busy)\n" {
		t.Fatalf("%+v", r)
	}
}

func TestHK4_PreRunAndPostRun(t *testing.T) {
	h := harness.New(t)
	nodeRepo(t, h, "", func(s string) string {
		return s + "\n[pools.vitest]\nhooks.pre_run = { argv = [\"sh\", \"-c\", \"echo no >&2; exit 64\"] }\n" +
			"\n[pools.git]\nhooks.post_run = { argv = [\"sh\", \"-c\", \"echo post $BITC_EXIT >&2; exit 1\"] }\n"
	})
	if r := h.Run("run", "vitest", "--", "echo", "ran"); r.Code != 64 || r.Stdout != "" || r.Stderr != "bitc: vitest: refused: no\n" {
		t.Fatalf("%+v", r)
	}
	r := h.Run("run", "git", "--", "sh", "-c", "exit 4")
	if r.Code != 4 || !strings.Contains(r.Stderr, "post_run") {
		t.Fatalf("post_run failure must only warn: %+v", r)
	}
}

func TestHK5_WrapPrefixesByWorktree(t *testing.T) {
	h := harness.New(t)
	nodeRepo(t, h, "", func(s string) string {
		return s + "\n[pools.git]\nhooks.wrap = [{ when_worktree = \"**/app\", argv = [\"sh\", \"-c\", \"echo wrapped; exec \\\"$@\\\"\", \"_\"] }]\n"
	})
	if r := h.Run("run", "git", "--", "echo", "inner"); r.Code != 0 || r.Stdout != "wrapped\ninner\n" {
		t.Fatalf("%+v", r)
	}
}

func TestHK6_HookEnvPrintsTheParse(t *testing.T) {
	h := harness.New(t)
	nodeRepo(t, h, "", nil)
	r := h.Run("config", "hook-env", "e2e", "--slot", "1")
	if r.Code != 0 || !strings.Contains(r.Stdout, `APP_STACK_SLOT="31"`) {
		t.Fatalf("%+v", r)
	}
}

func TestHK7_PreRunCannotUseSlot(t *testing.T) {
	h := harness.New(t)
	nodeRepo(t, h, "", func(s string) string {
		return s + "\n[pools.vitest]\nhooks.pre_run = { argv = [\"check\", \"{slot}\"] }\n"
	})
	if r := h.Run("config", "check"); r.Code != 78 || !strings.Contains(r.Stderr, "before a slot is held") {
		t.Fatalf("%+v", r)
	}
}

func hookPool(argv string) string {
	return "[pools.lint]\nhooks.pre_run = { argv = [\"sh\", \"-c\", " + harness.Q(argv) + "], timeout = \"2s\" }\n"
}

func TestHK8_BackgroundChildOfAHookNeitherStallsNorTimesItOut(t *testing.T) {
	h := harness.New(t)
	h.HostConfig(hookPool("sleep 7 & exit 0"))
	t0 := time.Now()
	r := h.Run("run", "lint", "--", "echo", "ran")
	if r.Code != 0 || r.Stdout != "ran\n" || time.Since(t0) > 5*time.Second {
		t.Fatalf("after %v: %+v", time.Since(t0), r)
	}
}

func TestHK9_DaemonizedChildOfAHookNeverHangsTheRun(t *testing.T) {
	h := harness.New(t)
	h.HostConfig(hookPool("setsid sleep 7 & exit 0"))
	t0 := time.Now()
	r := h.Run("run", "lint", "--", "echo", "ran")
	if r.Code != 0 || r.Stdout != "ran\n" || time.Since(t0) > 5*time.Second {
		t.Fatalf("after %v: %+v", time.Since(t0), r)
	}
	// a hook that truly overruns is still a timeout, and the setsid child cannot hold the run
	h.HostConfig(hookPool("setsid sleep 7 & sleep 7"))
	t0 = time.Now()
	r = h.Run("run", "lint", "--", "echo", "ran")
	if r.Code != 69 || !strings.Contains(r.Stderr, "timed out") || time.Since(t0) > 5*time.Second {
		t.Fatalf("after %v: %+v", time.Since(t0), r)
	}
}
