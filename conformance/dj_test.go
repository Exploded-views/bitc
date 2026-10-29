package conformance

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Exploded-views/bitc/conformance/harness"
	"github.com/Exploded-views/bitc/internal/proctest"
)

// readyJob detaches a job whose command first creates a ready file, and waits for it.
func readyJob(t *testing.T, h *harness.H, pool, script string) (name string, pid int) {
	t.Helper()
	ready := filepath.Join(t.TempDir(), "ready")
	name, pid, _ = detached(t, h, pool, "--", "sh", "-c", `: > "$1"; `+script, "_", ready)
	h.Until(func() bool { _, err := os.Stat(ready); return err == nil }, 10*time.Second)
	return name, pid
}

func TestDJ1_ExitJobReturnsTheJobsCode(t *testing.T) {
	h := harness.New(t)
	scaledHost(h, "[pools.lint]\n")
	name, _, _ := detached(t, h, "lint", "--", "sh", "-c", "exit 9")
	if r := h.Run("wait", name, "--exit", "job"); r.Code != 9 {
		t.Fatalf("%+v", r)
	}
}

func TestDJ2_LogsTailAndFollow(t *testing.T) {
	h := harness.New(t)
	scaledHost(h, "[pools.lint]\n")
	name, _, _ := detached(t, h, "lint", "--", "sh", "-c", "seq 1 5; sleep 0.3; echo late")
	r := h.Run("logs", name, "-f")
	if r.Code != 0 || !strings.Contains(r.Stdout, "late\n__EXIT__ 0\n") {
		t.Fatalf("%+v", r)
	}
	if r := h.Run("logs", name, "--tail", "2"); r.Stdout != "late\n__EXIT__ 0\n" {
		t.Fatalf("%q", r.Stdout)
	}
}

func TestDJ3_CancelTerminates(t *testing.T) {
	h := harness.New(t)
	scaledHost(h, "[host]\ngrace = \"20s\"\n[pools.lint]\n")
	name, _ := readyJob(t, h, "lint", "sleep 60")
	if r := h.Run("cancel", name); r.Code != 0 || !strings.HasPrefix(r.Stdout, "cancelled ") {
		t.Fatalf("%+v", r)
	}
	if r := h.Run("wait", name, "--exit", "job", "--timeout", "6000s"); r.Code != 143 {
		t.Fatalf("%+v", r)
	}
	// a cancel right after started, while the supervisor starts up, is interrupted, never lost
	early, _, _ := detached(t, h, "lint", "--", "sleep", "60")
	if r := h.Run("cancel", early); r.Code != 0 {
		t.Fatalf("%+v", r)
	}
	// --timeout: on a loaded host a race-built supervisor can take more than the scaled default
	// wait to start up and see the cancel; the assertion is about the outcome, not the speed.
	// time_scale 0.01 scales --timeout too: 6000s is 60s.
	if r := h.Run("wait", early, "--exit", "job", "--timeout", "6000s"); r.Code != 143 || !strings.HasPrefix(r.Stdout, "RESULT exit 143\n") {
		t.Fatalf("startup cancel: %+v", r)
	}
	if r := h.Run("cancel", "nope"); r.Code != 66 {
		t.Fatalf("%+v", r)
	}
}

func TestDJ6_SecondCancelOrKillEscalates(t *testing.T) {
	for _, kill := range []bool{false, true} {
		h := harness.New(t)
		scaledHost(h, "[host]\ngrace = \"20s\"\nmax_signal_wait = \"1000s\"\n[pools.e2e]\non_signal = \"forward-and-wait\"\n")
		ready := filepath.Join(h.Root, "ready")
		name, _, _ := detached(t, h, "e2e", "--", "sh", "-c", `trap "" TERM; : > "$1"; while :; do sleep 0.05; done`, "_", ready)
		h.Until(func() bool { _, err := os.Stat(ready); return err == nil }, 10*time.Second)
		if kill {
			if r := h.Run("cancel", name, "--kill"); r.Code != 0 {
				t.Fatalf("%+v", r)
			}
		} else {
			h.Run("cancel", name)
			if r := h.Run("wait", name, "--timeout", "100s"); r.Code != 124 {
				t.Fatalf("one cancel ended a forward-and-wait job that ignores TERM: %+v", r)
			}
			h.Run("cancel", name)
		}
		if r := h.Run("wait", name, "--timeout", "500s", "--exit", "job"); r.Code != 143 {
			t.Fatalf("kill=%v: %+v", kill, r)
		}
	}
}

func TestDJ4_LostSupervisorIsStarved(t *testing.T) {
	h := harness.New(t)
	scaledHost(h, "[pools.lint]\n")
	name, pid := readyJob(t, h, "lint", "sleep 60")
	_ = syscall.Kill(pid, syscall.SIGKILL)
	if r := h.Run("wait", name); r.Code != 75 || !strings.HasPrefix(r.Stdout, "RESULT starved\n") {
		t.Fatalf("%+v", r)
	}
}

func TestDJ5_PreRunOfADetachedJobRunsAfterStarted(t *testing.T) {
	h := harness.New(t)
	scaledHost(h, "[pools.lint]\nhooks.pre_run = { argv = [\"sh\", \"-c\", \"echo nope >&2; exit 64\"] }\n")
	name, _, _ := detached(t, h, "lint", "--", "echo", "ran")
	r := h.Run("wait", name, "--exit", "job")
	if r.Code != 64 || !strings.Contains(r.Stdout, "bitc: lint: refused: nope") || strings.Contains(r.Stdout, "\nran\n") {
		t.Fatalf("%+v", r)
	}
}

func TestDJ7_FollowReturnsWhenTheSupervisorIsLost(t *testing.T) {
	h := harness.New(t)
	scaledHost(h, "[pools.lint]\n")
	name, pid := readyJob(t, h, "lint", "sleep 60")
	_ = syscall.Kill(pid, syscall.SIGKILL)
	// the harness kills a hung run after 60 s, so a bound of 20 s tells a hang from a return
	p := h.Start("logs", name, "-f")
	h.Until(p.Exited, 20*time.Second)
	if !p.Exited() {
		t.Fatal("logs -f never returned for a job whose supervisor was killed")
	}
	if r := p.Wait(); r.Code != 75 {
		t.Fatalf("%+v", r)
	}
}

func TestDJ8_DetachedResultReasonIsRedacted(t *testing.T) {
	h := harness.New(t)
	const secret = "sk-live-0123456789abcdef"
	h.Seam("time_scale = 0.01")
	h.HostConfig("[pools.lint]\nhooks.pre_run = { argv = [\"sh\", \"-c\", \"echo " + secret + " >&2; exit 64\"] }\n" +
		"[providers.runpod]\napi_key = \"" + secret + "\"\ndaily_usd = 1\nimage = \"x\"\nsizes = {}\n" +
		"api_base = \"http://127.0.0.1:9\"\nproxy_domain = \"127.0.0.1:9\"\n")
	res := filepath.Join(h.Root, "r.json")
	name, _, _ := detached(t, h, "--result", res, "lint", "--", "true")
	h.Run("wait", name)
	files, _ := filepath.Glob(filepath.Join(h.State, "jobs", "*", "*", "result.json"))
	files = append(files, res)
	for _, f := range files {
		b := readOrFail(t, f)
		if strings.Contains(string(b), secret) {
			t.Fatalf("%s holds the credential: %s", f, b)
		}
		if !strings.Contains(string(b), "redacted") {
			t.Fatalf("%s lacks the redaction marker: %s", f, b)
		}
	}
}

func TestDJ9_LogsOfADetachFailedOnItsLogReturns(t *testing.T) {
	h := harness.New(t)
	scaledHost(h, "[pools.lint]\n")
	bad := filepath.Join(h.Root, "missing-dir", "x.log")
	if r := h.Run("run", "--detach", "--name", "zz", "--log", bad, "lint", "--", "true"); r.Code != 64 {
		t.Fatalf("detach with an unusable log: %+v", r)
	}
	for _, args := range [][]string{{"logs", "zz"}, {"logs", "zz", "-f"}} {
		p := h.Start(args...)
		h.Until(p.Exited, 20*time.Second)
		if !p.Exited() {
			t.Fatalf("%v never returned for a job that never got a spec", args)
		}
		if r := p.Wait(); r.Code != 66 || r.Stderr == "" {
			t.Fatalf("%v: %+v", args, r)
		}
	}
}

// postRunPool is a pool whose post_run records its pid in marker and then sleeps for a minute.
func postRunPool(marker string) string {
	return "[pools.lint]\nhooks.post_run = { argv = [\"sh\", \"-c\", " + harness.Q(`echo $$ > "$1.tmp" && mv "$1.tmp" "$1"; exec sleep 60`) + ", \"_\", " + harness.Q(marker) + "] }\n"
}

func TestDJ10_CancelDuringPostRunKeepsTheCommandsCode(t *testing.T) {
	h := harness.New(t)
	marker := filepath.Join(h.Root, "hook.pid")
	scaledHost(h, postRunPool(marker))
	// one cancel after the command exited, while post_run runs
	name, _, _ := detached(t, h, "lint", "--", "sh", "-c", "exit 3")
	h.Until(func() bool { _, err := os.Stat(marker); return err == nil }, 10*time.Second)
	hookPid := h.Atoi(strings.TrimSpace(string(must(os.ReadFile(marker)))))
	if r := h.Run("cancel", name); r.Code != 0 {
		t.Fatalf("%+v", r)
	}
	if r := h.Run("wait", name, "--exit", "job"); r.Code != 3 || !strings.HasPrefix(r.Stdout, "RESULT exit 3\n") || !strings.Contains(r.Stdout, "__EXIT__ 3") {
		t.Fatalf("cancel during post_run lost the job: %+v", r)
	}
	h.Until(func() bool { return proctest.Gone(hookPid) }, 5*time.Second)
	// a second cancel that arrives while post_run runs after a cancelled command
	_ = os.Remove(marker)
	name2, _ := readyJob(t, h, "lint", "sleep 60")
	h.Run("cancel", name2)
	h.Until(func() bool { _, err := os.Stat(marker); return err == nil }, 10*time.Second)
	h.Run("cancel", name2)
	if r := h.Run("wait", name2, "--exit", "job"); r.Code != 143 || !strings.HasPrefix(r.Stdout, "RESULT exit 143\n") {
		t.Fatalf("double cancel lost the job: %+v", r)
	}
}

func TestDJ11_WaiterBoundToASupersededGenerationGetsItsOwnResult(t *testing.T) {
	h := harness.New(t)
	scaledHost(h, "[pools.lint]\nslots = 2\n")
	ready := filepath.Join(h.Root, "ready")
	detached(t, h, "--name", "r", "lint", "--", "sh", "-c", `: > "$1"; sleep 2; echo first; exit 7`, "_", ready)
	h.Until(func() bool { _, err := os.Stat(ready); return err == nil }, 10*time.Second)
	w := h.Start("wait", "r", "--timeout", "1000s", "--exit", "job")
	time.Sleep(600 * time.Millisecond) // the waiter has bound to generation 1
	detached(t, h, "--name", "r", "lint", "--", "sh", "-c", "echo second; sleep 6")
	r := w.Wait()
	if r.Code != 7 || !strings.HasPrefix(r.Stdout, "RESULT exit 7\n") {
		t.Fatalf("the gen-1 waiter did not get gen 1's result: %+v", r)
	}
	if !strings.Contains(r.Stdout, "first\n__EXIT__ 7\n") || strings.Contains(r.Stdout, "second") {
		t.Fatalf("the gen-1 waiter tailed another generation's log: %+v", r)
	}
}
