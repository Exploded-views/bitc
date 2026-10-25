package conformance

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Exploded-views/bitc/conformance/harness"
	"github.com/Exploded-views/bitc/internal/provider/runpod/runpodfake"
)

// fbEnv is one fake-provider sandbox for the fallback-boundary rows (spec 8.2, 15.8).
type fbEnv struct {
	h    *harness.H
	st   string // the fake's state_dir
	acct string // ACCOUNT/remote/fake
	wt   string
	cfg  func(mode string) string
}

// fbRepo: pool heavy goes to the fake ($0.24/h, declaring `attach` unless the host narrows it),
// then local. remoteLimits is extra [remote_limits] text, provExtra extra [providers.fake] text.
func fbRepo(t *testing.T, h *harness.H, mode, remoteLimits, provExtra string) *fbEnv {
	t.Helper()
	e := &fbEnv{h: h, st: filepath.Join(h.Root, "fakestate"), acct: filepath.Join(h.Root, "account", "remote", "fake")}
	if err := os.MkdirAll(e.st, 0o700); err != nil {
		t.Fatal(err)
	}
	maxLive := "max_live = 6\n"
	if strings.Contains(provExtra, "max_live") {
		maxLive = ""
	}
	e.cfg = func(mode string) string {
		return "[remote_limits]\nworktrees = [\"**/app\"]\n" + remoteLimits +
			"[pools.heavy]\nproviders = [\"fake\", \"local\"]\nremote_limits = { size = { fake = \"cpu8\" }, timeout = \"60m\" }\nwait = \"5s\"\n" +
			"[providers.fake]\ndaily_usd = 25\n" + maxLive + "usd_per_hour = 0.24\nfallback_until_cap = \"attach\"\n" +
			"state_dir = " + harness.Q(e.st) + "\nmode = " + harness.Q(mode) + "\n" + provExtra
	}
	h.HostConfig(e.cfg(mode))
	e.wt = h.Repo("app", map[string]string{".bitc.toml": "version = 1\n[pools.heavy.remote]\nallow = [[\"sh\", \"*\"], [\"echo\", \"*\"]]\n"})
	h.Cwd = e.wt
	return e
}

func (e *fbEnv) setMode(mode string) { e.h.HostConfig(e.cfg(mode)) }

func (e *fbEnv) attempts(t *testing.T) []string { return lines(t, filepath.Join(e.st, "attempts")) }
func (e *fbEnv) torn(t *testing.T) []string     { return lines(t, filepath.Join(e.st, "torn.log")) }

// ledgerEnds returns the ledger's end lines.
func (e *fbEnv) ledger(t *testing.T, event string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, l := range lines(t, filepath.Join(e.acct, "ledger.jsonl")) {
		var m map[string]any
		if json.Unmarshal([]byte(l), &m) == nil && m["event"] == event {
			out = append(out, m)
		}
	}
	return out
}

func (e *fbEnv) failures(t *testing.T) int {
	n := 0
	for _, l := range lines(t, filepath.Join(e.acct, "failures.jsonl")) {
		if strings.Contains(l, `"kind":"failure"`) {
			n++
		}
	}
	return n
}

type resv struct {
	Name   string `json:"name"`
	Orphan bool   `json:"orphan"`
}

func (e *fbEnv) reservations(t *testing.T) []resv {
	t.Helper()
	var rs []resv
	if b, err := os.ReadFile(filepath.Join(e.acct, "reservations.json")); err == nil {
		_ = json.Unmarshal(b, &rs)
	}
	return rs
}

func ranLocally(e *fbEnv) bool {
	_, err := os.Stat(filepath.Join(e.wt, "ran-locally"))
	return err == nil
}

var markLocal = []string{"run", "heavy", "--", "sh", "-c", "echo ran > ran-locally"}

// plain leaves the work tree clean, so one sandbox can run several jobs.
var plain = []string{"run", "heavy", "--", "echo", "x"}

// FB1: the default equals the declaration; narrowing works; going past the declaration, a bad
// value and a repo file are 78.
func TestFB1_FallbackUntilDefaultNarrowAndValidation(t *testing.T) {
	h := harness.New(t)
	e := fbRepo(t, h, "preattach-fails", "", "")
	if r := h.Run(markLocal...); r.Code != 0 || len(e.attempts(t)) != 2 {
		t.Fatalf("default (the declared attach): %d attempts, %+v", len(e.attempts(t)), r)
	}
	h2 := harness.New(t)
	e2 := fbRepo(t, h2, "preattach-fails", "", "fallback_until = \"resource\"\n")
	if r := h2.Run(markLocal...); r.Code != 75 || len(e2.attempts(t)) != 1 {
		t.Fatalf("narrowed to resource: %d attempts, %+v", len(e2.attempts(t)), r)
	}
	for _, c := range []struct{ extra, msg string }{
		{"fallback_until = \"later\"\n", `providers.fake.fallback_until: must be "resource" or "attach": "later"`},
		{"fallback_until = 1\n", `providers.fake.fallback_until: must be "resource" or "attach": 1`},
	} {
		h3 := harness.New(t)
		fbRepo(t, h3, "run", "", c.extra)
		if r := h3.Run("config", "check"); r.Code != 78 || !strings.Contains(r.Stderr, c.msg) {
			t.Fatalf("%q: %+v", c.extra, r)
		}
	}
	// a repo file may not set it
	h4 := harness.New(t)
	e4 := fbRepo(t, h4, "run", "", "")
	h4.Write(filepath.Join(e4.wt, ".bitc.toml"), "version = 1\n[providers.fake]\nfallback_until = \"attach\"\n", 0o644)
	if r := h4.Run("config", "check"); r.Code != 78 || !strings.Contains(r.Stderr, "providers.fake.fallback_until: host-only key") {
		t.Fatalf("repo file: %+v", r)
	}
	// RunPod declares resource: attach is wider than it allows
	h5 := harness.New(t)
	rpRepo(t, h5, "")
	cfg := readCfg(t, h5)
	h5.HostConfig(strings.Replace(cfg, "[providers.runpod]\n", "[providers.runpod]\nfallback_until = \"attach\"\n", 1))
	if r := h5.Run("config", "check"); r.Code != 78 || !strings.Contains(r.Stderr, `providers.runpod.fallback_until: "attach" is wider than runpod allows ("resource")`) {
		t.Fatalf("runpod: %+v", r)
	}
}

// FB2: a pre-attach failure under attach, teardown ok: one retry with a new name and
// reservation, then the next provider with the suffix; one ledger launch and one end per
// attempt name.
func TestFB2_PreAttachDiscardRetriesOnceThenFallsBack(t *testing.T) {
	h := harness.New(t)
	e := fbRepo(t, h, "preattach-fails", "", "")
	r := h.Run(markLocal...)
	names := e.attempts(t)
	if r.Code != 0 || !ranLocally(e) || len(names) != 2 || names[0] == names[1] {
		t.Fatalf("%d attempts %v, %+v", len(names), names, r)
	}
	want := "bitc: fake: running heavy locally: setup step packages failed: apt timeout; " + "fake-" + names[1] + " torn down before attach\n"
	if !strings.Contains(r.Stderr, want) {
		t.Fatalf("want %q in %q", want, r.Stderr)
	}
	if torn := e.torn(t); len(torn) != 2 {
		t.Fatalf("torn down %v", torn)
	}
	launches, ends := e.ledger(t, "launch"), e.ledger(t, "end")
	if len(launches) != 2 || len(ends) != 2 {
		t.Fatalf("ledger: %v %v", launches, ends)
	}
	for i, n := range names {
		if launches[i]["name"] != n || ends[i]["name"] != n || ends[i]["exit"] != "fallback" || ends[i]["created"] != true {
			t.Fatalf("attempt %d (%s): %v %v", i, n, launches[i], ends[i])
		}
	}
	if rs := e.reservations(t); len(rs) != 0 {
		t.Fatalf("reservations left: %v", rs)
	}
}

// FB3: narrowed to resource, a pre-attach failure is read as attached: torn down, 75, nothing
// else tried.
func TestFB3_PreAttachUnderResourceIs75(t *testing.T) {
	h := harness.New(t)
	e := fbRepo(t, h, "preattach-fails", "", "fallback_until = \"resource\"\n")
	r := h.Run(markLocal...)
	if r.Code != 75 || !strings.Contains(r.Stderr, "STARVED heavy (fake: setup step packages failed: apt timeout)") ||
		strings.Contains(r.Stderr, "locally") || ranLocally(e) || len(e.attempts(t)) != 1 || len(e.torn(t)) != 1 {
		t.Fatalf("%d attempts, %+v", len(e.attempts(t)), r)
	}
}

// FB4: ErrAttached, or any failure after the handle: 75 under both boundaries; a repo setup
// failure after the handle is 69.
func TestFB4_PostAttachNeverFallsBack(t *testing.T) {
	for _, extra := range []string{"", "fallback_until = \"resource\"\n"} {
		for _, mode := range []string{"attached-fails", "lost"} {
			h := harness.New(t)
			e := fbRepo(t, h, mode, "", extra)
			r := h.Run(markLocal...)
			if r.Code != 75 || !strings.Contains(r.Stderr, "STARVED heavy (fake: ") || strings.Contains(r.Stderr, "locally") || ranLocally(e) || len(e.attempts(t)) != 1 {
				t.Fatalf("%s %q: %d attempts, %+v", mode, extra, len(e.attempts(t)), r)
			}
			if mode == "attached-fails" && len(e.torn(t)) != 1 {
				t.Fatalf("the resource was not torn down: %v", e.torn(t))
			}
		}
		h := harness.New(t)
		e := fbRepo(t, h, "setup-fails-repo", "", extra)
		if r := h.Run(markLocal...); r.Code != 69 || strings.Contains(r.Stderr, "STARVED") || ranLocally(e) {
			t.Fatalf("repo setup %q: %+v", extra, r)
		}
	}
}

// FB5: a repo-class setup failure before attach: 69, no fallback, no retry, no breaker record.
func TestFB5_PreAttachRepoSetupFailureIs69(t *testing.T) {
	h := harness.New(t)
	e := fbRepo(t, h, "preattach-repo-setup", "", "")
	r := h.Run(markLocal...)
	if r.Code != 69 || !strings.Contains(r.Stderr, "bitc: fake: heavy: setup step deps failed: npm ERR! missing script: ci\n") ||
		strings.Contains(r.Stderr, "STARVED") || strings.Contains(r.Stderr, "locally") || ranLocally(e) {
		t.Fatalf("%+v", r)
	}
	if len(e.attempts(t)) != 1 || len(e.torn(t)) != 1 || e.failures(t) != 0 {
		t.Fatalf("%d attempts, torn %v, %d failures", len(e.attempts(t)), e.torn(t), e.failures(t))
	}
}

// fbTwo: pool heavy = fake (pre-attach failures, attach), then RunPod (the fake API), then local.
func fbTwo(t *testing.T, h *harness.H) (*fbEnv, *runpodfake.Fake) {
	t.Helper()
	return fbTwoMode(t, h, "preattach-fails")
}

// fbTwoMode is fbTwo with the fake's mode.
func fbTwoMode(t *testing.T, h *harness.H, mode string) (*fbEnv, *runpodfake.Fake) {
	t.Helper()
	f, _ := rpRepo(t, h, "")
	e := &fbEnv{h: h, st: filepath.Join(h.Root, "fakestate"), acct: filepath.Join(h.Root, "account", "remote", "fake")}
	if err := os.MkdirAll(e.st, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := readCfg(t, h)
	cfg = strings.Replace(cfg, `providers = ["runpod", "local"]`, `providers = ["fake", "runpod", "local"]`, 1)
	cfg = strings.Replace(cfg, `size = { runpod = "cpu8" }`, `size = { fake = "cpu8", runpod = "cpu8" }`, 1)
	cfg += "[providers.fake]\ndaily_usd = 25\nmax_live = 6\nusd_per_hour = 0.24\nfallback_until_cap = \"attach\"\nmode = " + harness.Q(mode) + "\nstate_dir = " + harness.Q(e.st) + "\n"
	h.HostConfig(cfg)
	e.wt = h.Cwd
	return e, f
}

// FB6: "trying <next>" only when the next provider is remote; "running locally" only when
// local is next.
func TestFB6_TryingNoteOnlyForARemoteNext(t *testing.T) {
	h := harness.New(t)
	e, f := fbTwo(t, h)
	r := h.Run("run", "heavy", "--", "echo", "on-runpod")
	names := e.attempts(t)
	if r.Code != 0 || r.Stdout != "on-runpod\n" || calls(f, "POST /v2/pods") != 1 || len(names) != 2 {
		t.Fatalf("%d attempts, %+v", len(names), r)
	}
	want := "bitc: fake: heavy: trying runpod: setup step packages failed: apt timeout; fake-" + names[1] + " torn down before attach\n"
	if !strings.Contains(r.Stderr, want) || strings.Contains(r.Stderr, "running heavy locally") {
		t.Fatalf("want %q in %q", want, r.Stderr)
	}
	h2 := harness.New(t)
	fbRepo(t, h2, "preattach-fails", "", "")
	r2 := h2.Run("run", "heavy", "--", "echo", "here")
	if r2.Code != 0 || !strings.Contains(r2.Stderr, "bitc: fake: running heavy locally: ") || strings.Contains(r2.Stderr, "trying") {
		t.Fatalf("%+v", r2)
	}
}

// FB7: a failed teardown before attach: no retry, "left for sweep", an orphan reservation that
// outlives the process, counts while unlisted, and never shields its resource from the sweep or
// the next launch's cap check.
func TestFB7_UnconfirmedDiscardIsAnOrphanAndNeverRetried(t *testing.T) {
	h := harness.New(t)
	e := fbRepo(t, h, "preattach-teardown-fails", "", "")
	r := h.Run(markLocal...)
	names := e.attempts(t)
	if r.Code != 0 || !ranLocally(e) || len(names) != 1 {
		t.Fatalf("%d attempts, %+v", len(names), r)
	}
	if want := "; fake-" + names[0] + " left for sweep\n"; !strings.Contains(r.Stderr, want) {
		t.Fatalf("want %q in %q", want, r.Stderr)
	}
	rs := e.reservations(t)
	if len(rs) != 1 || rs[0].Name != names[0] || !rs[0].Orphan {
		t.Fatalf("reservations %+v", rs)
	}
	if e.failures(t) != 1 {
		t.Fatalf("%d breaker failures", e.failures(t))
	}
	// a sweep whose teardown fails keeps the orphan
	if r := h.Run("sweep"); !strings.Contains(r.Stdout, "swept fake "+names[0]+" (orphaned by a pre-attach discard)") || len(e.reservations(t)) != 1 {
		t.Fatalf("failed teardown: %+v %+v", r, e.reservations(t))
	}
	// the sweep tears it down at once (the job's pid is not part of the rule) and drops the orphan
	e.setMode("run")
	if r := h.Run("sweep"); r.Code != 0 || !strings.Contains(r.Stdout, "swept fake "+names[0]+" (orphaned by a pre-attach discard)") {
		t.Fatalf("sweep: %+v", r)
	}
	if len(e.reservations(t)) != 0 || len(e.torn(t)) != 1 {
		t.Fatalf("after the sweep: %+v, torn %v", e.reservations(t), e.torn(t))
	}
}

func TestFB7_OrphanCountsWhileUnlistedAndCapCheckTearsItDown(t *testing.T) {
	h := harness.New(t)
	e := fbRepo(t, h, "preattach-teardown-fails", "", "")
	h.Run(plain...)
	names := e.attempts(t)
	// unlisted (the provider forgot it): the orphan still counts in max_live
	h2 := harness.New(t)
	e2 := fbRepo(t, h2, "preattach-teardown-fails", "", "max_live = 1\n")
	h2.Run(plain...)
	if err := os.Remove(filepath.Join(e2.st, "resources.json")); err != nil {
		t.Fatal(err)
	}
	e2.setMode("run")
	if r := h2.Run("run", "heavy", "--", "echo", "x"); !strings.Contains(r.Stderr, "running heavy locally: 1 live of max_live 1") {
		t.Fatalf("an unlisted orphan did not count: %+v", r)
	}
	// listed: the next launch's cap check tears it down under the launch lock and runs the job
	e.setMode("run")
	if r := h.Run("run", "heavy", "--", "echo", "on-fake"); r.Code != 0 || !strings.Contains(r.Stdout, "on-fake") || strings.Contains(r.Stderr, "running heavy locally") {
		t.Fatalf("%+v", r)
	}
	if torn := e.torn(t); len(torn) != 2 || torn[0] != "fake-"+names[0] { // the orphan, then the job's own resource
		t.Fatalf("the cap check did not tear the orphan down: %v", torn)
	}
	for _, rv := range e.reservations(t) {
		if rv.Orphan {
			t.Fatalf("orphan kept: %+v", e.reservations(t))
		}
	}
}

// FB8: on_failure = "stop" after a pre-attach failure ends the walk (the one retry only).
func TestFB8_StopEndsTheWalkAfterThePreAttachRetry(t *testing.T) {
	h := harness.New(t)
	e := fbRepo(t, h, "preattach-fails", "on_failure = \"stop\"\n", "")
	r := h.Run(markLocal...)
	if r.Code != 75 || !strings.Contains(r.Stderr, "STARVED heavy (no provider left: setup step packages failed: apt timeout; fake-") ||
		strings.Contains(r.Stderr, "locally") || ranLocally(e) || len(e.attempts(t)) != 2 {
		t.Fatalf("%d attempts, %+v", len(e.attempts(t)), r)
	}
}

// FB9: RunPod declares resource: nothing changes. A create that left a pod is never retried
// and ends 75; a failed create that left nothing is retried once.
func TestFB9_RunPodKeepsTheResourceBoundary(t *testing.T) {
	h := harness.New(t)
	f, wt := rpRepo(t, h, "")
	f.Behave("create-500-after-create")
	r := h.Run(markLocal...)
	if r.Code != 75 || strings.Contains(r.Stderr, "locally") || calls(f, "POST /v2/pods") != 1 {
		t.Fatalf("%d creates, %+v", calls(f, "POST /v2/pods"), r)
	}
	if _, err := os.Stat(filepath.Join(wt, "ran-locally")); err == nil {
		t.Fatal("ran locally after a pod was created")
	}
	h2 := harness.New(t)
	f2, _ := rpRepo(t, h2, "")
	f2.Behave("create-500-twice")
	r2 := h2.Run("run", "heavy", "--", "echo", "x")
	if r2.Code != 0 || calls(f2, "POST /v2/pods") != 2 || !strings.Contains(r2.Stderr, "running heavy locally: ") {
		t.Fatalf("%d creates, %+v", calls(f2, "POST /v2/pods"), r2)
	}
}

// FB14: each resource discarded before attach is one breaker failure; after `failures` of them
// the provider is paused; a create that left nothing writes none.
func TestFB14_DiscardsFeedTheBreaker(t *testing.T) {
	h := harness.New(t)
	e := fbRepo(t, h, "preattach-fails", "", "")
	h.Run(plain...)
	if n := e.failures(t); n != 2 {
		t.Fatalf("job 1: %d failures", n)
	}
	h.Run(plain...) // the third failure trips the breaker before the retry
	if n, a := e.failures(t), len(e.attempts(t)); n != 3 || a != 3 {
		t.Fatalf("job 2: %d failures, %d attempts", n, a)
	}
	r := h.Run(plain...)
	if len(e.attempts(t)) != 3 || !strings.Contains(r.Stderr, "fake is paused: 3 failures after start") {
		t.Fatalf("job 3: %d attempts, %+v", len(e.attempts(t)), r)
	}
	h2 := harness.New(t)
	e2 := fbRepo(t, h2, "transient", "", "")
	h2.Run(plain...)
	if n := e2.failures(t); n != 0 {
		t.Fatalf("a create that left nothing wrote %d failures", n)
	}
}

// retryMode is fbRepo for the create-retry rows: the fake retries a create that left nothing, as a
// real provider does, and the 5 s wait is scaled down.
func retryRepo(t *testing.T, mode string, remoteLimits, provExtra string) (*harness.H, *fbEnv) {
	t.Helper()
	h := harness.New(t)
	h.Seam("time_scale = 0.01")
	return h, fbRepo(t, h, mode, remoteLimits, "retry_create = true\n"+provExtra)
}

// FB2 (B.3.3.4): a job has at most one retry in total. A create that left nothing is retried once
// (clause 1); if that retry itself returns a pre-attach discard, it is final and follows on_failure.
func TestFB2_TransientThenPreAttachSharesTheOneRetry(t *testing.T) {
	h, e := retryRepo(t, "transient-then-preattach", "", "")
	r := h.Run(markLocal...)
	names := e.attempts(t)
	if r.Code != 0 || !ranLocally(e) || len(names) != 2 {
		t.Fatalf("%d creates %v, %+v", len(names), names, r)
	}
	want := "bitc: fake: running heavy locally: setup step packages failed: apt timeout; fake-" + names[1] + " torn down before attach\n"
	if !strings.Contains(r.Stderr, want) {
		t.Fatalf("want %q in %q", want, r.Stderr)
	}
	ends := e.ledger(t, "end")
	if len(e.ledger(t, "launch")) != 1 || len(ends) != 1 || ends[0]["exit"] != "fallback" || ends[0]["created"] != true {
		t.Fatalf("ledger: %v %v", e.ledger(t, "launch"), ends)
	}
	if n := e.failures(t); n != 1 || len(e.torn(t)) != 1 || len(e.reservations(t)) != 0 {
		t.Fatalf("%d failures, torn %v, reservations %v", n, e.torn(t), e.reservations(t))
	}
}

// The reverse order: the pre-attach discard is retried by Walk, and that attempt's create answer
// (capacity) is not retried a third time.
func TestFB2_PreAttachThenTransientSharesTheOneRetry(t *testing.T) {
	h, e := retryRepo(t, "preattach-then-transient", "", "")
	r := h.Run(markLocal...)
	names := e.attempts(t)
	if r.Code != 0 || !ranLocally(e) || len(names) != 2 || names[0] == names[1] {
		t.Fatalf("%d creates %v, %+v", len(names), names, r)
	}
	want := "bitc: fake: running heavy locally: fake: no capacity; fake-" + names[0] + " torn down before attach\n"
	if !strings.Contains(r.Stderr, want) {
		t.Fatalf("want %q in %q", want, r.Stderr)
	}
	ends := e.ledger(t, "end")
	if len(ends) != 2 || ends[0]["created"] != true || ends[1]["exit"] != "fallback" || ends[1]["created"] == true {
		t.Fatalf("ledger ends: %v", ends)
	}
	if n := e.failures(t); n != 1 || len(e.reservations(t)) != 0 {
		t.Fatalf("%d failures, reservations %v", n, e.reservations(t))
	}
}

// FB2: the retry succeeds: attempt 1 is discarded and confirmed gone, attempt 2 attaches and the
// command runs on the fake, not locally.
func TestFB2_PreAttachRetrySucceedsOnTheFake(t *testing.T) {
	h := harness.New(t)
	e := fbRepo(t, h, "preattach-fails-once", "", "")
	r := h.Run("run", "heavy", "--", "echo", "from-the-fake")
	names := e.attempts(t)
	if r.Code != 0 || r.Stdout != "from-the-fake\n" || len(names) != 2 || names[0] == names[1] ||
		!strings.Contains(r.Stderr, "bitc: fake: heavy job ") || strings.Contains(r.Stderr, "locally") {
		t.Fatalf("%d attempts, %+v", len(names), r)
	}
	ends := e.ledger(t, "end")
	if len(ends) != 2 || ends[0]["name"] != names[0] || ends[0]["exit"] != "fallback" || ends[0]["created"] != true ||
		ends[1]["name"] != names[1] || ends[1]["exit"] != "0" {
		t.Fatalf("ledger ends: %v", ends)
	}
	if n := e.failures(t); n != 1 || len(e.reservations(t)) != 0 {
		t.Fatalf("%d failures, reservations %v", n, e.reservations(t))
	}
}

// FB2: the retry's own rails run again. Attempt 1's discarded minutes are in LedgerToday, so a
// daily cap that fitted attempt 1 refuses attempt 2: no second create, the next provider, with
// the discard's suffix.
func TestFB2_RetryRechecksTheCapWithTheDiscardedMinutes(t *testing.T) {
	short := func(h *harness.H, e *fbEnv, daily string) {
		cfg := strings.Replace(e.cfg("preattach-fails"), `timeout = "60m"`, `timeout = "1m"`, 1)
		cfg = strings.Replace(cfg, "daily_usd = 25", "daily_usd = "+daily, 1)
		h.HostConfig(cfg + "start_timeout = \"1m\"\n")
	}
	// calibrate: one run with a roomy cap shows attempt 1's estimate and what its discard cost
	h0 := harness.New(t)
	e0 := fbRepo(t, h0, "preattach-fails", "", "")
	short(h0, e0, "25")
	h0.Run(markLocal...)
	l0, d0 := e0.ledger(t, "launch"), e0.ledger(t, "end")
	if len(l0) != 2 || len(d0) != 2 {
		t.Fatalf("calibration: %v %v", l0, d0)
	}
	est, cost := l0[0]["est_usd"].(float64), d0[0]["cost_usd"].(float64)
	if est <= 0 || cost <= 0 {
		t.Fatalf("calibration: est %v cost %v", est, cost)
	}
	h := harness.New(t)
	e := fbRepo(t, h, "preattach-fails", "", "")
	short(h, e, fmt.Sprintf("%.4f", est+cost/2)) // attempt 1 fits; the discarded minutes tip attempt 2 over
	r := h.Run(markLocal...)
	names := e.attempts(t)
	if r.Code != 0 || !ranLocally(e) || len(names) != 1 {
		t.Fatalf("%d creates, %+v", len(names), r)
	}
	if !strings.Contains(r.Stderr, "bitc: fake: running heavy locally: daily cap ") || !strings.Contains(r.Stderr, "; fake-"+names[0]+" torn down before attach\n") {
		t.Fatalf("%q", r.Stderr)
	}
	if ends := e.ledger(t, "end"); len(e.ledger(t, "launch")) != 1 || len(ends) != 1 || ends[0]["created"] != true {
		t.Fatalf("ledger: %v", ends)
	}
}

// FB7: a resource that later appears in List is counted once: the orphan reservation and the
// listing are one live slot, not two.
func TestFB7_AnOrphanThatIsListedCountsOnce(t *testing.T) {
	h := harness.New(t)
	e := fbRepo(t, h, "preattach-teardown-fails", "", "max_live = 1\n")
	h.Run(plain...)
	if rs := e.reservations(t); len(rs) != 1 || !rs[0].Orphan {
		t.Fatalf("reservations %+v", rs)
	}
	r := h.Run(plain...) // the teardown keeps failing: the resource stays listed beside its orphan
	if !strings.Contains(r.Stderr, "running heavy locally: 1 live of max_live 1\n") || len(e.attempts(t)) != 1 {
		t.Fatalf("%d attempts, %+v", len(e.attempts(t)), r)
	}
}

// FB7: a live, non-orphan reservation of the same name still protects the resource from the
// sweep and the cap check; the sweep then tears it down once only the orphan is left.
func TestFB7_ALiveReservationProtectsItsNameFromSweepAndCapCheck(t *testing.T) {
	h := harness.New(t)
	e := fbRepo(t, h, "preattach-teardown-fails", "", "")
	h.Run(plain...)
	names := e.attempts(t)
	orphans := e.reservations(t)
	if len(orphans) != 1 || !orphans[0].Orphan {
		t.Fatalf("reservations %+v", orphans)
	}
	path := filepath.Join(e.acct, "reservations.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var all []map[string]any
	if err := json.Unmarshal(raw, &all); err != nil {
		t.Fatal(err)
	}
	live := map[string]any{"pid": os.Getpid(), "start": 0, "name": names[0], "estimate": all[0]["estimate"], "expiry": all[0]["expiry"]}
	b, _ := json.Marshal(append(all, live))
	h.Write(path, string(b), 0o600)
	e.setMode("run")
	if r := h.Run("sweep"); strings.Contains(r.Stdout, "swept fake "+names[0]) || len(e.torn(t)) != 0 {
		t.Fatalf("the sweep tore down a protected resource: %+v, torn %v", r, e.torn(t))
	}
	if r := h.Run("run", "heavy", "--", "echo", "on-fake"); r.Code != 0 || slices.Contains(e.torn(t), "fake-"+names[0]) { // the job's own resource is torn down too
		t.Fatalf("the cap check tore down a protected resource: %+v, torn %v", r, e.torn(t))
	}
	b, _ = json.Marshal(all) // the live reservation goes: only the orphan is left
	h.Write(path, string(b), 0o600)
	if r := h.Run("sweep"); !strings.Contains(r.Stdout, "swept fake "+names[0]+" (orphaned by a pre-attach discard)") || !slices.Contains(e.torn(t), "fake-"+names[0]) {
		t.Fatalf("sweep: %+v", r)
	}
}

// FB7: the orphan does not depend on the job's pid. The job moved on to local and is still running
// when `bitc sweep` tears the discarded resource down and drops the orphan.
func TestFB7_SweepTearsTheOrphanDownWhileTheJobStillRuns(t *testing.T) {
	h := harness.New(t)
	e := fbRepo(t, h, "preattach-teardown-fails", "", "")
	gate := filepath.Join(h.Root, "gate")
	p := h.Start("run", "heavy", "--", "sh", "-c", "while [ ! -e "+gate+" ]; do sleep 0.05; done; echo finished")
	h.Until(func() bool { rs := e.reservations(t); return len(rs) == 1 && rs[0].Orphan }, 20*time.Second)
	e.setMode("run") // the sweep is a new process: its teardown works
	names := e.attempts(t)
	r := h.Run("sweep")
	if p.Exited() || !strings.Contains(r.Stdout, "swept fake "+names[0]+" (orphaned by a pre-attach discard)") {
		t.Fatalf("exited %v, sweep %+v", p.Exited(), r)
	}
	if len(e.reservations(t)) != 0 || len(e.torn(t)) != 1 {
		t.Fatalf("after the sweep: %+v, torn %v", e.reservations(t), e.torn(t))
	}
	h.Write(gate, "", 0o600)
	if res := p.Wait(); res.Code != 0 || res.Stdout != "finished\n" {
		t.Fatalf("%+v", res)
	}
}

// FB9: a 429 on create is retried once (clause 1), then the walk goes to the next remote
// provider with "trying <next>"; the pool is RunPod, then the fake, then local.
func TestFB9_ACreate429IsRetriedOnceThenTriesTheNextProvider(t *testing.T) {
	h := harness.New(t)
	f, _ := rpRepo(t, h, "")
	st := filepath.Join(h.Root, "fakestate")
	if err := os.MkdirAll(st, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := readCfg(t, h)
	cfg = strings.Replace(cfg, `providers = ["runpod", "local"]`, `providers = ["runpod", "fake", "local"]`, 1)
	cfg = strings.Replace(cfg, `size = { runpod = "cpu8" }`, `size = { runpod = "cpu8", fake = "cpu8" }`, 1)
	cfg += "[providers.fake]\ndaily_usd = 25\nmax_live = 6\nusd_per_hour = 0.24\nmode = \"run\"\nstate_dir = " + harness.Q(st) + "\n"
	h.HostConfig(cfg)
	f.Behave("create-429-twice")
	r := h.Run("run", "heavy", "--", "echo", "on-fake")
	if r.Code != 0 || r.Stdout != "on-fake\n" || calls(f, "POST /v2/pods") != 2 {
		t.Fatalf("%d creates, %+v", calls(f, "POST /v2/pods"), r)
	}
	if !strings.Contains(r.Stderr, "bitc: runpod: heavy: trying fake: ") || strings.Contains(r.Stderr, "running heavy locally") ||
		!strings.Contains(r.Stderr, "bitc: fake: heavy job ") {
		t.Fatalf("%q", r.Stderr)
	}
}

// 8.2: an ineligible job whose next provider is remote prints the not-eligible line (never the
// trying note), and the walk goes on; the call ends 69 only at the last provider.
func TestFB6_IneligibleWithARemoteNextPrintsTheNotEligibleLine(t *testing.T) {
	h := harness.New(t)
	fbTwoMode(t, h, "ineligible")
	r := h.Run("run", "heavy", "--", "echo", "on-runpod")
	if r.Code != 0 || r.Stdout != "on-runpod\n" || !strings.Contains(r.Stderr, "bitc: fake: heavy: not eligible for remote: fake: request too large\n") ||
		strings.Contains(r.Stderr, "trying") || strings.Contains(r.Stderr, "running heavy locally") {
		t.Fatalf("%+v", r)
	}
	h2 := harness.New(t)
	fbTwoMode(t, h2, "ineligible")
	cfg := readCfg(t, h2)
	h2.HostConfig(strings.Replace(cfg, "[remote_limits]\n", "[remote_limits]\non_failure = \"stop\"\n", 1))
	if r := h2.Run("run", "heavy", "--", "echo", "x"); r.Code != 69 || strings.Contains(r.Stderr, "trying") ||
		!strings.Contains(r.Stderr, "bitc: fake: heavy: not eligible for remote: fake: request too large\n") {
		t.Fatalf("stop: %+v", r)
	}
}
