package conformance

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Exploded-views/bitc/conformance/harness"
)

func TestV1_StreamsSetupLinesAndNoMarkers(t *testing.T) {
	h := harness.New(t)
	f, _ := rpRepo(t, h, "")
	r := h.Run("run", "heavy", "--", "sh", "-c", "echo to-out; echo to-err >&2; exit 3")
	if calls(f, "POST /v2/pods") != 1 {
		t.Fatalf("the job did not run on the pod: %v", f.Calls())
	}
	if r.Code != 3 || r.Stdout != "to-out\n" || !strings.Contains(r.Stderr, "to-err\n") || strings.Contains(r.Stdout+r.Stderr, "__BITC_") {
		t.Fatalf("%+v", r)
	}
	// the live tail delivered everything: the dropped-tail note belongs to a missed tail only
	if strings.Contains(r.Stderr, "the log stream missed the end") {
		t.Fatalf("a normal run printed the dropped-tail note: %q", r.Stderr)
	}
}

func TestK10_LaunchLineAndLedger(t *testing.T) {
	h := harness.New(t)
	rpRepo(t, h, "")
	r := h.Run("run", "heavy", "--", "echo", "x")
	if !strings.Contains(r.Stderr, "bitc: runpod: heavy job ") || !strings.Contains(r.Stderr, " (cpu8) at ") {
		t.Fatalf("%+v", r)
	}
}

func TestK5_RemoteCwdIsTheCallersSubdir(t *testing.T) {
	h := harness.New(t)
	f, wt := rpRepo(t, h, "")
	h.Cwd = filepath.Join(wt, "sub")
	if r := h.Run("run", "heavy", "--", "sh", "-c", "basename $PWD"); r.Stdout != "sub\n" || calls(f, "POST /v2/pods") != 1 {
		t.Fatalf("%+v %v", r, f.Calls())
	}
}

func TestQ6_RemoteJobsTakeNoLocalSlot(t *testing.T) {
	h := harness.New(t)
	rpRepo(t, h, "")
	hold := h.Hold([]string{"BITC_PROVIDER=local"}, "heavy")
	a := h.Start("run", "heavy", "--", "sh", "-c", "sleep 1; echo a")
	b := h.Run("run", "heavy", "--", "echo", "b")
	if ra := a.Wait(); ra.Stdout != "a\n" || b.Stdout != "b\n" {
		t.Fatalf("%+v %+v", ra, b)
	}
	hold.Release()
}

func TestV2_LogSlicesResumeAndDedupe(t *testing.T) {
	h := harness.New(t)
	f, _ := rpRepo(t, h, "")
	r := h.Run("run", "heavy", "--", "sh", "-c", "for i in 1 2 3 4; do echo line$i; sleep 1; done")
	if r.Stdout != "line1\nline2\nline3\nline4\n" {
		t.Fatalf("%q", r.Stdout)
	}
	since := 0
	for _, c := range f.Calls() {
		if strings.Contains(c, "/logs") {
			since++
		}
	}
	if since < 2 {
		t.Fatalf("one slice only: %v", f.Calls())
	}
}

func TestV3_ForgedMarkersArePlainOutput(t *testing.T) {
	h := harness.New(t)
	f, _ := rpRepo(t, h, "")
	r := h.Run("run", "heavy", "--", "sh", "-c", "echo __BITC_EXIT__ deadbeef 0; exit 4")
	if calls(f, "POST /v2/pods") != 1 {
		t.Fatalf("the job did not run on the pod: %v", f.Calls())
	}
	if r.Code != 4 || !strings.Contains(r.Stdout, "__BITC_EXIT__ deadbeef 0") {
		t.Fatalf("%+v", r)
	}
}

func TestV4_DroppedTailIsCompleted(t *testing.T) {
	h := harness.New(t)
	f, _ := rpRepo(t, h, "")
	f.Behave("drop-log-tail")
	r := h.Run("run", "heavy", "--", "sh", "-c", "seq 1 2000; exit 5")
	if r.Code != 5 || !strings.HasSuffix(r.Stdout, "1999\n2000\n") ||
		!strings.Contains(r.Stderr, "bitc: runpod: the log stream missed the end of the job; the rest of its output follows") {
		t.Fatalf("code %d, stderr %q", r.Code, r.Stderr)
	}
}

func TestV5_AfterBeginFailuresAre75(t *testing.T) {
	for behave, why := range map[string]string{"restart-after-begin": "restarted while the command ran", "lost-after-begin": "was lost"} {
		h := harness.New(t)
		f, _ := rpRepo(t, h, "")
		f.Behave(behave)
		r := h.Run("run", "heavy", "--", "sh", "-c", "sleep 2; echo late")
		if r.Code != 75 || !strings.Contains(r.Stderr, "STARVED heavy (runpod: pod") || !strings.Contains(r.Stderr, why) || strings.Contains(r.Stderr, "locally") {
			t.Errorf("%s: %+v", behave, r)
		}
	}
}

func TestV6_BeforeBeginTearsDownAnd75RefDeleted(t *testing.T) {
	h := harness.New(t)
	// a tool that cannot be downloaded fails setup before BEGIN, after the pod was created
	f, _ := rpRepo(t, h, "[remote]\ntools = [{ name = \"t\", url = \"https://127.0.0.1:9/t.tar.gz\", sha256 = \""+strings.Repeat("0", 64)+"\" }]\n")
	r := h.Run("run", "heavy", "--", "echo", "ran")
	if r.Code != 75 || r.Stdout != "" || !strings.Contains(r.Stderr, "STARVED heavy (runpod: setup step t failed: ") ||
		strings.Contains(r.Stderr, "locally") {
		t.Fatalf("%+v", r)
	}
	if calls(f, "DELETE /v2/pods/") != 1 {
		t.Fatalf("the pod was not torn down: %v", f.Calls())
	}
	acct := filepath.Join(h.Root, "account", "remote", "runpod")
	if b, _ := os.ReadFile(filepath.Join(acct, "failures.jsonl")); strings.Contains(string(b), `"kind":"failure"`) {
		t.Fatalf("a failure before BEGIN tripped the breaker: %s", b)
	}
	if l := lines(t, filepath.Join(acct, "ledger.jsonl")); len(l) == 0 || !strings.Contains(l[len(l)-1], `"exit":"75"`) {
		t.Fatalf("ledger %v", l)
	}
	bare := h.Origin("app")
	h.Until(func() bool { return gitIn(t, h, bare, "for-each-ref", "refs/bitc/") == "" }, 10*time.Second)
}

func TestV7_StatusPolledOnTheFixedInterval(t *testing.T) {
	h := harness.New(t)
	f, _ := rpRepo(t, h, "")
	h.Run("run", "heavy", "--", "sh", "-c", "sleep 3")
	n := 0
	for _, c := range f.Calls() {
		if c == "GET /v2/pods/pod1" {
			n++
		}
	}
	if n < 3 {
		t.Fatalf("%d status polls in 3s at 15s x 0.05", n)
	}
}

func TestC1_SignalTearsDownAndExits128PlusN(t *testing.T) {
	h := harness.New(t)
	f, _ := rpRepo(t, h, "")
	p := h.Start("run", "heavy", "--", "sh", "-c", "sleep 30")
	h.Until(func() bool { return calls(f, "POST /v2/pods") == 1 }, 20*time.Second)
	time.Sleep(time.Second)
	p.Signal(syscall.SIGTERM)
	if r := p.Wait(); r.Code != 143 {
		t.Fatalf("%+v", r)
	}
	if calls(f, "DELETE /v2/pods/") < 1 {
		t.Fatal("no teardown")
	}
	acct := filepath.Join(h.Root, "account", "remote", "runpod")
	if b, _ := os.ReadFile(filepath.Join(acct, "failures.jsonl")); len(b) != 0 {
		t.Fatalf("an interrupt tripped the breaker: %s", b)
	}
	if l := lines(t, filepath.Join(acct, "ledger.jsonl")); len(l) == 0 || !strings.Contains(l[len(l)-1], `"exit":"interrupted"`) {
		t.Fatalf("ledger %v", l)
	}
}

func TestOR1_ForgedResultIs75(t *testing.T) {
	h := harness.New(t)
	f, _ := rpRepo(t, h, "")
	f.Behave("forge-result")
	r := h.Run("run", "heavy", "--", "sh", "-c", "exit 1")
	if r.Code != 75 || !strings.Contains(r.Stderr, "result failed verification") || strings.Contains(r.Stderr, "locally") {
		t.Fatalf("%+v", r)
	}
}

func TestOR2_CancelOfARemoteDetachedJob(t *testing.T) {
	h := harness.New(t)
	f, _ := rpRepo(t, h, "")
	if r := h.Run("run", "--detach", "--name", "rj", "heavy", "--", "sh", "-c", "sleep 30"); r.Code != 0 {
		t.Fatalf("%+v", r)
	}
	acct := filepath.Join(h.Root, "account", "remote", "runpod")
	h.Until(func() bool {
		b, _ := os.ReadFile(filepath.Join(acct, "ledger.jsonl"))
		return strings.Contains(string(b), `"event":"begin"`)
	}, 30*time.Second)
	if r := h.Run("cancel", "rj"); r.Code != 0 {
		t.Fatalf("%+v", r)
	}
	if r := h.Run("wait", "rj", "--exit", "job", "--timeout", "400s"); r.Code != 143 {
		t.Fatalf("%+v", r)
	}
	l := lines(t, filepath.Join(acct, "ledger.jsonl"))
	if last := l[len(l)-1]; !strings.Contains(last, `"event":"end"`) || !strings.Contains(last, `"exit":"interrupted"`) {
		t.Fatal(last)
	}
	if n := calls(f, "DELETE /v2/pods/"); n != 1 {
		t.Fatalf("%d DELETEs", n)
	}
	if b, _ := os.ReadFile(filepath.Join(acct, "failures.jsonl")); len(b) != 0 {
		t.Fatalf("a cancel tripped the breaker: %s", b)
	}
}

// A repo setup step that fails by itself (a broken `npm ci`) is the repo's own failure (spec 8.2,
// 8.4 "Host side", 15.9): torn down, 69 with the setup-step line, no STARVED, no local run, ledger
// 69 and no breaker record, so a retrying agent does not loop on it.
func TestOR3_RepoSetupFailureIs69NoBreaker(t *testing.T) {
	h := harness.New(t)
	f, _ := rpRepo(t, h, "[remote]\nsetup = [{ name = \"deps\", run_as = \"job\", argv = [\"sh\", \"-c\", \"echo 'npm ERR! missing script: ci' >&2; exit 1\"] }]\n")
	r := h.Run("run", "heavy", "--", "echo", "ran")
	if r.Code != 69 || r.Stdout != "" || !strings.Contains(r.Stderr, "bitc: runpod: heavy: setup step deps failed: npm ERR! missing script: ci\n") ||
		strings.Contains(r.Stderr, "STARVED") || strings.Contains(r.Stderr, "locally") {
		t.Fatalf("%+v", r)
	}
	if calls(f, "DELETE /v2/pods/") != 1 {
		t.Fatalf("the pod was not torn down: %v", f.Calls())
	}
	acct := filepath.Join(h.Root, "account", "remote", "runpod")
	if b, _ := os.ReadFile(filepath.Join(acct, "failures.jsonl")); strings.Contains(string(b), `"kind":"failure"`) {
		t.Fatalf("a repo setup failure tripped the breaker: %s", b)
	}
	if l := lines(t, filepath.Join(acct, "ledger.jsonl")); len(l) == 0 || !strings.Contains(l[len(l)-1], `"exit":"69"`) {
		t.Fatalf("ledger %v", l)
	}
}

// A log stream that goes quiet after its first batch (live bug 2026-10-23: BOOT and BEGIN printed,
// the host never saw them) must not hide BEGIN: the job still begins, runs and returns its exit and output.
func TestOR4_BeginSurvivesALogStreamThatStallsAfterTheFirstBatch(t *testing.T) {
	h := harness.New(t)
	f, _ := rpRepo(t, h, "[remote]\nsetup = [{ name = \"slow\", run_as = \"job\", argv = [\"sleep\", \"2\"] }]\n")
	f.Behave("logs-stall-after-first")
	r := h.Run("run", "heavy", "--", "sh", "-c", "echo to-out; exit 4")
	if r.Code != 4 || r.Stdout != "to-out\n" || strings.Contains(r.Stdout+r.Stderr, "__BITC_") || strings.Contains(r.Stderr, "did not start") {
		t.Fatalf("%+v", r)
	}
}

// A pod that restarts after BEGIN without ever booting the agent again (build mode: the agent ref is
// gone, the stub crash-loops; live proof case C 2026-10-23) is seen by its changed startedAt.
func TestOR5_RestartWithoutASecondBootIsSeenFromStartedAt(t *testing.T) {
	h := harness.New(t)
	f, _ := rpRepo(t, h, "")
	f.Behave("restart-after-begin-no-boot")
	r := h.Run("run", "heavy", "--", "sh", "-c", "sleep 60")
	if r.Code != 75 || !strings.Contains(r.Stderr, "STARVED heavy (runpod: pod") || !strings.Contains(r.Stderr, "restarted while the command ran") {
		t.Fatalf("%+v", r)
	}
	if calls(f, "DELETE /v2/pods/") != 1 {
		t.Fatalf("the pod was not torn down: %v", f.Calls())
	}
}

// Live output (live proof 2026-10-23): a line the command printed reaches the caller's stdout while
// the job still runs, not at the end. The job waits on a gate file before its second print, so
// nothing depends on timing.
func TestOR6_OutputIsStreamedWhileTheJobRuns(t *testing.T) {
	h := harness.New(t)
	rpRepo(t, h, "")
	gate := filepath.Join(t.TempDir(), "gate")
	p := h.Start("run", "heavy", "--", "sh", "-c", "echo first; while [ ! -e "+harness.Q(gate)+" ]; do sleep 0.05; done; echo second")
	h.Until(func() bool { return strings.Contains(p.Stdout(), "first\n") || p.Exited() }, 45*time.Second)
	if !strings.Contains(p.Stdout(), "first\n") || p.Exited() || strings.Contains(p.Stdout(), "second") {
		t.Fatalf("the first line did not arrive while the job ran: exited=%v out=%q", p.Exited(), p.Stdout())
	}
	if err := os.WriteFile(gate, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if r := p.Wait(); r.Code != 0 || r.Stdout != "first\nsecond\n" {
		t.Fatalf("%+v", r)
	}
}

// Log reads are tail reads only (the live API answered since= with nothing and rate-limited the
// fallback, 2026-10-23); a 429 burst is backed off, the job still completes and output is exact.
func TestOR7_LogReadsAreTailOnlyAndA429BurstIsBackedOff(t *testing.T) {
	h := harness.New(t)
	f, _ := rpRepo(t, h, "")
	f.Behave("logs-429-burst")
	r := h.Run("run", "heavy", "--", "sh", "-c", "echo a; sleep 1; echo b; exit 3")
	if r.Code != 3 || r.Stdout != "a\nb\n" || strings.Contains(r.Stdout+r.Stderr, "__BITC_") {
		t.Fatalf("%+v", r)
	}
	qs := f.LogQueries()
	for _, q := range qs {
		if strings.Contains(q, "since") || !strings.Contains(q, "tail=5000") {
			t.Fatalf("a log read was not a plain tail: %q (all %v)", q, qs)
		}
	}
	if len(qs) < 5 || len(qs) > 40 {
		t.Fatalf("%d log reads", len(qs))
	}
}

func TestC3_CleanupRunsInOrderOnEveryPath(t *testing.T) {
	h := harness.New(t)
	f, _ := rpRepo(t, h, "")
	h.Run("run", "heavy", "--", "echo", "x")
	if calls(f, "DELETE /v2/pods/") != 1 {
		t.Fatal(f.Calls())
	}
	ledger := string(must(os.ReadFile(filepath.Join(h.Root, "account", "remote", "runpod", "ledger.jsonl"))))
	if !strings.Contains(ledger, `"event":"end"`) {
		t.Fatal(ledger)
	}
	res := string(must(os.ReadFile(filepath.Join(h.Root, "account", "remote", "runpod", "reservations.json"))))
	if strings.Contains(res, "heavy") {
		t.Fatal("reservation kept")
	}
}

func TestK9_CreateRetriedOnceAndReapedByName(t *testing.T) {
	h := harness.New(t)
	f, _ := rpRepo(t, h, "")
	f.Behave("transient-create")
	if r := h.Run("run", "heavy", "--", "echo", "x"); r.Stdout != "x\n" || calls(f, "POST /v2/pods") != 2 {
		t.Fatalf("%+v %v", r, f.Calls())
	}
	f.Behave("create-500-twice")
	r := h.Run("run", "heavy", "--", "echo", "y")
	if !strings.Contains(r.Stderr, "running heavy locally") || r.Stdout != "y\n" {
		t.Fatalf("%+v", r)
	}
	// SIGTERM while the create is in flight (C1, C3): the fake made the pod but holds the
	// answer; the call exits 143 and tears down the pod named like the job: one create, one delete.
	h2 := harness.New(t)
	f2, _ := rpRepo(t, h2, "")
	f2.Behave("hold POST /v2/pods")
	p := h2.Start("run", "heavy", "--", "echo", "z")
	h2.Until(func() bool { return f2.Held("POST /v2/pods") }, 30*time.Second)
	p.Signal(syscall.SIGTERM)
	r = p.Wait()
	f2.Release("POST /v2/pods")
	if r.Code != 143 || r.Stdout != "" || strings.Contains(r.Stderr, "locally") {
		t.Fatalf("interrupted create: %+v", r)
	}
	if calls(f2, "POST /v2/pods") != 1 || calls(f2, "DELETE /v2/pods/") != 1 || calls(f2, "DELETE /v2/pods/pod1") != 1 {
		t.Fatalf("want one create and one delete of its pod: %v", f2.Calls())
	}
	if name := fmt.Sprint(f2.Bodies()[0]["name"]); !strings.HasPrefix(name, "bitc-") || !strings.Contains(name, "-heavy-") {
		t.Fatalf("the created pod is not named like the job: %q", name)
	}
}

func TestA5_ArtifactFailureAfterPassIs75AfterFailKeepsCode(t *testing.T) {
	h := harness.New(t)
	rpRepo(t, h, "artifacts = [{ path = \"out\" }]\n")
	// an archive over max_compressed cannot be fetched
	h.HostConfig(strings.Replace(readCfg(t, h), "start_timeout = \"2m\"", "start_timeout = \"2m\"\nartifacts = { max_compressed = \"1KiB\", max_unpacked = \"2GiB\", max_entries = 100000 }", 1))
	r := h.Run("run", "heavy", "--", "sh", "-c", "mkdir -p out; head -c 200000 /dev/urandom > out/big; exit 0")
	if r.Code != 75 || !strings.Contains(r.Stderr, "artifacts") {
		t.Fatalf("pass: %+v", r)
	}
	r = h.Run("run", "heavy", "--", "sh", "-c", "mkdir -p out; head -c 200000 /dev/urandom > out/big; exit 6")
	if r.Code != 6 || !strings.Contains(r.Stderr, "the command already failed with 6") {
		t.Fatalf("fail: %+v", r)
	}
}

func TestH4_RemoteHistoryNamesResourceAndCommit(t *testing.T) {
	h := harness.New(t)
	rpRepo(t, h, "")
	h.Run("run", "heavy", "--", "echo", "x")
	b := string(must(os.ReadFile(filepath.Join(h.State, "history.jsonl"))))
	if !strings.Contains(b, `"provider":"runpod"`) || !strings.Contains(b, `"resource":"pod`) || !strings.Contains(b, `"sha":"`) {
		t.Fatal(b)
	}
}

func TestRR12_APIKeyNeverInBitcsSinksOrArgv(t *testing.T) {
	h := harness.New(t)
	f, _ := rpRepo(t, h, "")
	r := h.Run("run", "heavy", "--", "sh", "-c", "env; cat /proc/$PPID/cmdline")
	if calls(f, "POST /v2/pods") != 1 {
		t.Fatalf("the job did not run on the pod: %v", f.Calls())
	}
	for _, where := range []string{r.Stdout, r.Stderr, string(must(os.ReadFile(filepath.Join(h.State, "history.jsonl")))),
		string(must(os.ReadFile(filepath.Join(h.Root, "account", "remote", "runpod", "ledger.jsonl"))))} {
		if strings.Contains(where, "sk-test-0123456789") {
			t.Fatal("the API key leaked")
		}
	}
	for _, b := range f.Bodies() {
		if strings.Contains(strings.Join(mapValues(b), " "), "sk-test-0123456789") {
			t.Fatal("the API key is in a request body")
		}
	}
}

func TestRR13_ResultsServerNeverGetsTheAPIKey(t *testing.T) {
	h := harness.New(t)
	f, _ := rpRepo(t, h, "")
	h.Run("run", "heavy", "--", "echo", "x")
	if len(f.Proxied()) == 0 {
		t.Fatal("the results server was never asked")
	}
	for _, a := range f.Proxied() {
		if strings.Contains(a, "sk-test-0123456789") {
			t.Fatal(a)
		}
	}
}

func TestRR15_PushFailureFallsBackBeforeAnyResource(t *testing.T) {
	h := harness.New(t)
	f, wt := rpRepo(t, h, "")
	gitIn(t, h, wt, "remote", "set-url", "origin", filepath.Join(h.Root, "missing.git"))
	r := h.Run("run", "heavy", "--", "echo", "x")
	if !strings.Contains(r.Stderr, "running heavy locally: could not push") || calls(f, "POST /v2/pods") != 0 {
		t.Fatalf("%+v", r)
	}
}

func mapValues(m map[string]any) []string {
	var out []string
	for _, v := range m {
		out = append(out, fmt.Sprint(v))
	}
	return out
}

// A second SIGTERM while the post-run teardown is in flight is absorbed: bitc finishes the
// teardown and the ledger end and exits 128+first signal (spec 8.3 cleanup on interrupt).
func TestMS1_SecondSignalDuringTeardownIsAbsorbed(t *testing.T) {
	h := harness.New(t)
	f, _ := rpRepo(t, h, "")
	f.Behave("hold DELETE /v2/pods/pod1")
	p := h.Start("run", "heavy", "--", "sh", "-c", "sleep 30")
	h.Until(func() bool { return calls(f, "POST /v2/pods") == 1 }, 20*time.Second)
	time.Sleep(time.Second)
	p.Signal(syscall.SIGTERM)
	h.Until(func() bool { return f.Held("DELETE /v2/pods/pod1") }, 20*time.Second)
	p.Signal(syscall.SIGTERM)
	time.Sleep(500 * time.Millisecond)
	if p.Exited() {
		t.Fatal("the second SIGTERM ended bitc before the teardown answered")
	}
	f.Release("DELETE /v2/pods/pod1")
	r := p.Wait()
	if r.Code != 143 {
		t.Fatalf("%+v", r)
	}
	if calls(f, "DELETE /v2/pods/") != 1 {
		t.Fatalf("want exactly one delete: %v", f.Calls())
	}
	acct := filepath.Join(h.Root, "account", "remote", "runpod")
	if l := lines(t, filepath.Join(acct, "ledger.jsonl")); len(l) == 0 || !strings.Contains(l[len(l)-1], `"exit":"interrupted"`) {
		t.Fatalf("no ledger end after the second signal: %v", l)
	}
}

// A signal during the wait before the create retry ends the call at once: 143, no second create.
func TestMS2_SignalDuringCreateRetryWaitIsPrompt(t *testing.T) {
	h := harness.New(t)
	f, _ := rpRepo(t, h, "")
	h.SetSeam("time_scale", "1") // the retry wait is the real 5s
	f.Behave("transient-create")
	p := h.Start("run", "heavy", "--", "echo", "x")
	h.Until(func() bool { return calls(f, "POST /v2/pods") == 1 }, 30*time.Second)
	time.Sleep(500 * time.Millisecond)
	start := time.Now()
	p.Signal(syscall.SIGTERM)
	r := p.Wait()
	if r.Code != 143 || r.Stdout != "" || strings.Contains(r.Stderr, "locally") {
		t.Fatalf("%+v", r)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("the signal waited %v for the retry sleep", d)
	}
	if calls(f, "POST /v2/pods") != 1 {
		t.Fatalf("a create followed the signal: %v", f.Calls())
	}
}

// Spec 15.8: a failed create that had left a pod (found and torn down by name) is 75, never a
// local run and never a retry.
func TestMS3_FailedCreateThatLeftAPodIs75NotLocal(t *testing.T) {
	h := harness.New(t)
	f, _ := rpRepo(t, h, "")
	f.Behave("create-500-after-create")
	r := h.Run("run", "heavy", "--", "sh", "-c", "echo ran-locally")
	if r.Code != 75 || strings.Contains(r.Stdout+r.Stderr, "ran-locally") || strings.Contains(r.Stderr, "locally") ||
		!strings.Contains(r.Stderr, "STARVED heavy") || !strings.Contains(r.Stderr, "pod1") {
		t.Fatalf("%+v", r)
	}
	if calls(f, "POST /v2/pods") != 1 || calls(f, "DELETE /v2/pods/") != 1 || calls(f, "DELETE /v2/pods/pod1") != 1 {
		t.Fatalf("want one create and one delete of its pod: %v", f.Calls())
	}
}

// Spec 8.2, 15.8: when the name lookup after a failed create itself fails, nothing proves the
// create left nothing: 75, no retry, never a local run.
func TestMS4_FailedCreateWithFailedLookupIs75NotLocal(t *testing.T) {
	h := harness.New(t)
	f, _ := rpRepo(t, h, "")
	f.Behave("create-500-after-create")
	f.Behave("list-500-after-create")
	r := h.Run("run", "heavy", "--", "sh", "-c", "echo ran-locally")
	if r.Code != 75 || strings.Contains(r.Stdout+r.Stderr, "ran-locally") || !strings.Contains(r.Stderr, "STARVED heavy") {
		t.Fatalf("%+v", r)
	}
	if calls(f, "POST /v2/pods") != 1 {
		t.Fatalf("want exactly one create: %v", f.Calls())
	}
}
