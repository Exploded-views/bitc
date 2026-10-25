// Package provider is the provider conformance suite: one scenario set run against every
// provider's fake (spec 11).
package provider

import (
	"encoding/base64"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Exploded-views/bitc/conformance/harness"
)

// Target sets up one provider type against its fake in a harness sandbox.
type Target interface {
	Name() string
	// Configure writes host and repo config for a pool "heavy" that goes to this provider.
	Configure(t *testing.T, h *harness.H)
	// Behave sets the fake's next behaviour: "ok", "create-fails" (every create attempt fails,
	// so the job falls back), "setup-fails-after-create" (the resource is created, then setup
	// fails before BEGIN for an infrastructure or unclassified cause),
	// "setup-fails-repo-after-create" (the resource is created, then the repo's own setup step
	// deps fails by itself with the first error line "npm ERR! missing script: ci"), "lost-after-begin",
	// "restart-after-begin", "drop-log-tail", "forge-result", "preattach-fails" (the resource is
	// created, then setup fails before any job input reached it; only a target whose provider
	// declares attach), "paginate-2" (the fake lists in pages of two; a no-op where the fake API
	// does not paginate). A target fails the test on a
	// behaviour it cannot produce.
	Behave(t *testing.T, behaviour string)
	// Calls returns the fake API calls seen so far ("POST /v2/pods", ...).
	Calls() []string
	// TornDown returns the ids of the resources torn down so far.
	TornDown() []string
	// Supports reports whether a scenario applies (by capability).
	Supports(scenario string) bool
}

// Scenario is one named check run against a Target.
type Scenario struct {
	Name string
	Run  func(t *testing.T, h *harness.H, tg Target)
}

// Scenarios is extended by later tasks (cap, breaker, restart, interrupt, sweep, artifacts...).
var Scenarios []Scenario

// RunAll runs every scenario the target supports.
func RunAll(t *testing.T, tg Target, newH func(*testing.T) *harness.H) {
	for _, s := range Scenarios {
		s := s
		t.Run(s.Name, func(t *testing.T) {
			if !tg.Supports(s.Name) {
				t.Skip(tg.Name() + " does not support " + s.Name)
			}
			h := newH(t)
			tg.Configure(t, h)
			s.Run(t, h, tg)
		})
	}
}

func init() {
	Scenarios = append(Scenarios,
		Scenario{"launch", func(t *testing.T, h *harness.H, tg Target) {
			tg.Behave(t, "ok")
			if r := h.Run("run", "heavy", "--", "echo", "remote-ok"); r.Code != 0 || r.Stdout != "remote-ok\n" || strings.Contains(r.Stderr, "locally") {
				t.Fatalf("%+v", r)
			}
		}},
		Scenario{"fallback", func(t *testing.T, h *harness.H, tg Target) {
			tg.Behave(t, "create-fails")
			r := h.Run("run", "heavy", "--", "echo", "ran-local")
			if r.Code != 0 || r.Stdout != "ran-local\n" || !strings.Contains(r.Stderr, "running heavy locally: ") {
				t.Fatalf("%+v", r)
			}
		}},
		// Once a resource exists the job never falls back (spec 8.2, 15.8), although "local" is
		// next in the pool's providers: torn down, 75 STARVED, the command never runs here.
		Scenario{"setup-fails-after-create", func(t *testing.T, h *harness.H, tg Target) {
			tg.Behave(t, "setup-fails-after-create")
			r := h.Run("run", "heavy", "--", "sh", "-c", "echo ran > ran-locally")
			if r.Code != 75 || r.Stdout != "" || !strings.Contains(r.Stderr, "STARVED heavy ("+tg.Name()+": ") ||
				strings.Contains(r.Stderr, "running heavy locally") {
				t.Fatalf("%+v", r)
			}
			if _, err := os.Stat(filepath.Join(h.Cwd, "ran-locally")); err == nil {
				t.Fatal("the job ran locally after its resource was created")
			}
			if torn := tg.TornDown(); len(torn) != 1 {
				t.Fatalf("torn down %v, want the one created resource", torn)
			}
		}},
		// The repo's own setup step failed by itself after the create (spec 8.2, 15.9): torn
		// down, 69 with the setup-step line, never STARVED and never a local run.
		Scenario{"setup-fails-repo-after-create", func(t *testing.T, h *harness.H, tg Target) {
			tg.Behave(t, "setup-fails-repo-after-create")
			r := h.Run("run", "heavy", "--", "sh", "-c", "echo ran > ran-locally")
			if r.Code != 69 || r.Stdout != "" ||
				!strings.Contains(r.Stderr, "bitc: "+tg.Name()+": heavy: setup step deps failed: npm ERR! missing script: ci\n") ||
				strings.Contains(r.Stderr, "STARVED") || strings.Contains(r.Stderr, "running heavy locally") {
				t.Fatalf("%+v", r)
			}
			if _, err := os.Stat(filepath.Join(h.Cwd, "ran-locally")); err == nil {
				t.Fatal("the job ran locally after its resource was created")
			}
			if torn := tg.TornDown(); len(torn) != 1 {
				t.Fatalf("torn down %v, want the one created resource", torn)
			}
		}},
		Scenario{"lost", func(t *testing.T, h *harness.H, tg Target) {
			tg.Behave(t, "lost-after-begin")
			if r := h.Run("run", "heavy", "--", "echo", "x"); r.Code != 75 {
				t.Fatalf("%+v", r)
			}
		}},
	)
}

// Spender is a target whose fake reports today's spend.
type Spender interface {
	SetSpend(t *testing.T, h *harness.H, usd float64)
}

func init() {
	Scenarios = append(Scenarios,
		Scenario{"cap", func(t *testing.T, h *harness.H, tg Target) {
			sp, ok := tg.(Spender)
			if !ok {
				t.Skip(tg.Name() + " has no spend control")
			}
			sp.SetSpend(t, h, 1000) // over any daily_usd a target configures
			tg.Behave(t, "ok")
			r := h.Run("run", "heavy", "--", "echo", "x")
			if r.Stdout != "x\n" || !strings.Contains(r.Stderr, "running heavy locally: daily cap $") {
				t.Fatalf("%+v", r)
			}
			for _, c := range tg.Calls() {
				if strings.HasPrefix(c, "POST ") {
					t.Fatalf("created over the cap: %v", tg.Calls())
				}
			}
		}},
		Scenario{"breaker", func(t *testing.T, h *harness.H, tg Target) {
			for i := 0; i < 3; i++ {
				want := 75
				if tr, ok := tg.(Tripper); ok {
					want = tr.Trip(t)
				} else {
					tg.Behave(t, "lost-after-begin")
				}
				if r := h.Run("run", "heavy", "--", "echo", "x"); r.Code != want {
					t.Fatalf("run %d: %+v", i, r)
				}
			}
			tg.Behave(t, "ok")
			r := h.Run("run", "heavy", "--", "echo", "x")
			if r.Stdout != "x\n" || !strings.Contains(r.Stderr, " is paused: 3 failures after start") {
				t.Fatalf("%+v", r)
			}
		}},
	)
}

func init() {
	Scenarios = append(Scenarios,
		Scenario{"restart", func(t *testing.T, h *harness.H, tg Target) {
			tg.Behave(t, "restart-after-begin")
			r := h.Run("run", "heavy", "--", "sh", "-c", "sleep 2; echo late")
			if r.Code != 75 || strings.Contains(r.Stderr, "locally") || strings.Contains(r.Stdout, "late") {
				t.Fatalf("%+v", r)
			}
		}},
		Scenario{"interrupt", func(t *testing.T, h *harness.H, tg Target) {
			tg.Behave(t, "ok")
			acct := filepath.Join(h.Root, "account", "remote", tg.Name())
			p := h.Start("run", "heavy", "--", "sh", "-c", "sleep 30")
			h.Until(func() bool {
				b, _ := os.ReadFile(filepath.Join(acct, "ledger.jsonl"))
				return strings.Contains(string(b), `"event":"begin"`)
			}, 30*time.Second)
			p.Signal(syscall.SIGTERM)
			if r := p.Wait(); r.Code != 143 {
				t.Fatalf("%+v", r)
			}
			if b, _ := os.ReadFile(filepath.Join(acct, "ledger.jsonl")); !strings.Contains(string(b), `"exit":"interrupted"`) {
				t.Fatalf("ledger: %s", b)
			}
			if b, _ := os.ReadFile(filepath.Join(acct, "failures.jsonl")); len(b) != 0 {
				t.Fatalf("an interrupt tripped the breaker: %s", b)
			}
			deletes := 0
			for _, c := range tg.Calls() {
				if strings.HasPrefix(c, "DELETE ") {
					deletes++
				}
			}
			if deletes != 1 {
				t.Fatalf("%d DELETEs: %v", deletes, tg.Calls())
			}
		}},
		Scenario{"dropped-tail", func(t *testing.T, h *harness.H, tg Target) {
			tg.Behave(t, "drop-log-tail")
			r := h.Run("run", "heavy", "--", "sh", "-c", "seq 1 500; exit 5")
			_, recovers := tg.(TailRecoverer)
			if r.Code != 5 || !strings.HasSuffix(r.Stdout, "499\n500\n") || (!recovers && !strings.Contains(r.Stderr, "the log stream missed the end of the job")) {
				t.Fatalf("code %d, stderr %q", r.Code, r.Stderr)
			}
		}},
		Scenario{"forged-result", func(t *testing.T, h *harness.H, tg Target) {
			tg.Behave(t, "forge-result")
			r := h.Run("run", "heavy", "--", "sh", "-c", "exit 1")
			if r.Code != 75 || !strings.Contains(r.Stderr, "result failed verification") || strings.Contains(r.Stderr, "locally") {
				t.Fatalf("%+v", r)
			}
		}},
		Scenario{"artifacts", func(t *testing.T, h *harness.H, tg Target) {
			tg.Behave(t, "ok")
			commitRepoLine(t, h, `artifacts = [{ path = "out" }]`) // appended to [pools.heavy.remote]
			r := h.Run("run", "heavy", "--", "sh", "-c", "mkdir -p out && echo hi > out/f")
			if r.Code != 0 || strings.Contains(r.Stderr, "locally") {
				t.Fatalf("%+v", r)
			}
			if b, _ := os.ReadFile(filepath.Join(h.Cwd, "out", "f")); string(b) != "hi\n" {
				t.Fatalf("artifact %q", b)
			}
		}},
	)
}

// commitRepoLine appends a line to the work tree's .bitc.toml and commits it (a remote job runs
// a commit, so the tree must stay clean).
func commitRepoLine(t *testing.T, h *harness.H, line string) {
	t.Helper()
	p := filepath.Join(h.Cwd, ".bitc.toml")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	h.Write(p, string(b)+line+"\n", 0o644)
	for _, a := range [][]string{{"add", "-A"}, {"-c", "user.name=t", "-c", "user.email=t@example.invalid", "commit", "-q", "-m", "artifacts"}} {
		c := exec.Command("git", a...)
		c.Dir, c.Env = h.Cwd, h.Env
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", a, err, out)
		}
	}
}

// Tripper is a target whose breaker counts another failure than "lost after begin" (Sprites counts
// discarded Sprites): Trip arms one failing job and returns the exit code that job ends with.
type Tripper interface{ Trip(t *testing.T) int }

// TailRecoverer marks a target whose provider re-reads a dropped log tail by offset, so the
// output is whole and the host has nothing to report.
type TailRecoverer interface{ RecoversTail() }

// Nouner is a target whose sweep names a resource by another noun than "resource" ("swept sprite <name>").
type Nouner interface{ Noun() string }

// Expirer is a target that can list an expired resource of this install, tagged as its own.
type Expirer interface {
	AddExpired(t *testing.T, h *harness.H) (id string)
}

func init() {
	Scenarios = append(Scenarios, Scenario{"sweep", func(t *testing.T, h *harness.H, tg Target) {
		ex, ok := tg.(Expirer)
		if !ok {
			t.Skip(tg.Name() + " cannot list an expired resource")
		}
		tg.Behave(t, "ok")
		h.Run("run", "heavy", "--", "echo", "warm-up") // creates the install secret
		id := ex.AddExpired(t, h)
		noun := "resource"
		if n, ok := tg.(Nouner); ok {
			noun = n.Noun()
		}
		if r := h.Run("sweep"); r.Code != 0 || !strings.Contains(r.Stdout, "swept "+noun+" "+id+" (") {
			t.Fatalf("%+v", r)
		}
		if r := h.Run("sweep"); strings.Contains(r.Stdout, "swept "+noun+" "+id) {
			t.Fatalf("swept twice: %+v", r)
		}
	}})
}

// APICall is one call a provider's fake saw, in order. Attach marks the call the fake treats as
// the one that carries job input (spec 8.2, B.1); Data is everything it was sent.
type APICall struct {
	Op     string
	Attach bool
	Data   string
}

// CallRecorder is a target whose fake records its calls in order, with the attach call marked.
type CallRecorder interface {
	// DeclaresAttach reports whether the provider under test declares `attach` (Caps.FallbackUntil).
	DeclaresAttach() bool
	APICalls(t *testing.T) []APICall
	// Canaries returns the job-specific values (B, K, M, ...) the target's sandbox generated.
	Canaries(t *testing.T) []string
}

// Lister is a target that can ask the provider under test for its list of live resources and read
// the fake's own live set.
type Lister interface {
	ListNames(t *testing.T, h *harness.H) []string
	LiveNames(t *testing.T) []string
}

// canaryForms is v raw, hex and base64 (standard, URL, with and without padding). A canary that
// is itself hex (B, K and M come from canaries.log as hex) also stands for its raw bytes: those
// bytes, their hex and their base64 are checked too (design A.10.2: raw, hex or base64 form).
func canaryForms(v string) []string {
	forms := encodings([]byte(v))
	if raw, err := hex.DecodeString(v); err == nil && len(raw) > 0 {
		forms = append(forms, string(raw))
		forms = append(forms, encodings(raw)[2:]...) // the bytes' hex is v itself (any case aside)
	}
	return forms
}

// encodings is b as raw text, hex and the four base64 alphabets.
func encodings(b []byte) []string {
	return []string{string(b), hex.EncodeToString(b), base64.StdEncoding.EncodeToString(b), base64.RawStdEncoding.EncodeToString(b),
		base64.URLEncoding.EncodeToString(b), base64.RawURLEncoding.EncodeToString(b)}
}

// RunFB15 is conformance row FB15: nothing job-specific crosses before the attach call. It needs
// a fresh sandbox per behaviour (newH), because a call log is cut at its first attach.
func RunFB15(t *testing.T, tg Target, newH func(*testing.T) *harness.H) {
	cr, ok := tg.(CallRecorder)
	if !ok || !cr.DeclaresAttach() {
		t.Skip(tg.Name() + " does not declare attach")
	}
	const argv, env = "ARGV-CANARY-7f3a9c", "ENV-CANARY-51d2e8"
	for _, b := range []string{"ok", "preattach-fails", "setup-fails-after-create"} {
		t.Run(b, func(t *testing.T) {
			h := newH(t)
			tg.Configure(t, h)
			tg.Behave(t, b)
			h.RunWith([]string{"CANARY_VALUE=" + env}, "run", "heavy", "--", "echo", argv)
			calls := cr.APICalls(t)
			cut := slices.IndexFunc(calls, func(c APICall) bool { return c.Attach })
			if b != "preattach-fails" && cut < 0 {
				t.Fatalf("no call is marked attach: %v", calls)
			}
			pre := calls
			if cut >= 0 {
				pre = calls[:cut]
			}
			canaries := append(cr.Canaries(t), argv, env)
			if len(canaries) < 5 {
				t.Fatalf("only %d canaries seeded", len(canaries))
			}
			for _, c := range pre {
				for _, v := range canaries {
					for _, form := range canaryForms(v) {
						if strings.Contains(c.Data, form) {
							t.Fatalf("job input %q crossed before attach, in %s: %s", form, c.Op, c.Data)
						}
					}
				}
			}
			if cut >= 0 && b == "ok" { // not vacuous: the attach call does carry them
				for _, v := range []string{argv, env} {
					if !strings.Contains(calls[cut].Data, v) {
						t.Fatalf("the attach call does not carry %q: %s", v, calls[cut].Data)
					}
				}
			}
		})
	}
}

// RunPS1 is conformance row PS1: with the fake's list paginated, five jobs with overlapping
// lifetimes; after each launch, each job's end and each teardown, the provider's list is
// exactly the fake's live set (no omission, no duplicate, no foreign name).
func RunPS1(t *testing.T, tg Target, h *harness.H) {
	ls, ok := tg.(Lister)
	if !ok {
		t.Skip(tg.Name() + " cannot list")
	}
	tg.Behave(t, "paginate-2")
	same := func() bool {
		for try := 0; try < 5; try++ {
			// one list call can take a second under load, and jobs end about a second apart, so
			// the live set is read before and after it: the list must equal one of the two
			before := ls.LiveNames(t)
			got := ls.ListNames(t, h)
			after := ls.LiveNames(t)
			slices.Sort(got)
			slices.Sort(before)
			slices.Sort(after)
			if slices.Equal(got, before) || slices.Equal(got, after) {
				return true
			}
			if try == 4 {
				t.Errorf("list %v, live before %v, after %v", got, before, after)
			}
			time.Sleep(50 * time.Millisecond)
		}
		return false
	}
	const jobs = 5
	done := make(chan harness.Result, jobs)
	for i := 0; i < jobs; i++ { // lifetimes of 6..10 s start within a second or two of each other and end one after another
		p := h.Start("run", "heavy", "--", "sh", "-c", "sleep "+strconv.Itoa(6+i))
		go func() { done <- p.Wait() }()
	}
	finished, peak := 0, 0
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) && !t.Failed() {
		if !same() {
			break
		}
		peak = max(peak, len(ls.LiveNames(t)))
		select {
		case r := <-done:
			if r.Code != 0 {
				t.Fatalf("a job failed: %+v", r)
			}
			finished++
		default:
		}
		if finished == jobs && len(ls.LiveNames(t)) == 0 {
			break // every teardown has been compared too
		}
		time.Sleep(50 * time.Millisecond)
	}
	if t.Failed() {
		t.FailNow()
	}
	if finished != jobs || peak < jobs {
		t.Fatalf("%d of %d jobs finished, at most %d live at once: the lifetimes did not overlap", finished, jobs, peak)
	}
}
