package conformance

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/Exploded-views/bitc/conformance/harness"
)

// exGateMsg is the whole refusal line for a remote provider without [host] experimental_remote.
const exGateMsg = "bitc: remote providers are experimental in this release and off by default; their spend " +
	"accounting is still under review (see https://github.com/Exploded-views/bitc#known-issues). " +
	"To use them anyway, set [host] experimental_remote = true."

// exSweepMsg is the extra line for a host with remote leftovers, and for sweep and doctor --remote.
const exSweepMsg = "To clean up resources left by an earlier version, set the flag and run bitc sweep."

// exEnv is a sandbox whose host file is written whole (no harness additions besides [test]),
// so a case without the flag really has none. Pool heavy walks the fake provider, then local.
type exEnv struct {
	h    *harness.H
	fake string // the fake provider's state_dir (calls.jsonl, torn.log)
	acct string // the accounting dir
	// planted: the test wrote leftovers into acct itself, so its existence proves nothing
	planted bool
}

func exSandbox(t *testing.T) *exEnv {
	t.Helper()
	h := harness.New(t)
	e := &exEnv{h: h, fake: filepath.Join(h.Root, "fakestate"), acct: filepath.Join(h.Root, "account")}
	if err := os.MkdirAll(e.fake, 0o700); err != nil {
		t.Fatal(err)
	}
	h.Cwd = h.Repo("app", map[string]string{".bitc.toml": "version = 1\n[pools.heavy.remote]\nallow = [[\"sh\", \"*\"], [\"echo\", \"*\"]]\n"})
	return e
}

// providers is a host body with a fake provider pool; hostTable is the [host] table text.
func (e *exEnv) providers(hostTable string) string {
	return hostTable + "[remote_limits]\nworktrees = [\"**/app\"]\n" +
		"[pools.lint]\n" +
		"[pools.heavy]\nproviders = [\"fake\", \"local\"]\nremote_limits = { size = { fake = \"small\" } }\nwait = \"5s\"\n" +
		"[providers.fake]\ndaily_usd = 1\nmax_live = 6\nstate_dir = " + harness.Q(e.fake) + "\nmode = \"run\"\n"
}

// host writes the whole host file at path: body, then the [test] table.
func (e *exEnv) host(path, body string) {
	e.h.Write(path, "version = 1\n"+body+"\n[test]\naccounting_dir = "+harness.Q(e.acct)+"\n", 0o600)
}

// untouched: the fake saw no call and the run created no state, account or job files.
func (e *exEnv) untouched(t *testing.T, what string) {
	t.Helper()
	if ents, _ := os.ReadDir(e.fake); len(ents) != 0 {
		t.Errorf("%s: the fake provider was contacted: %v", what, ents)
	}
	if _, err := os.Stat(e.h.State); err == nil {
		t.Errorf("%s: the state dir was created", what)
	}
	if _, err := os.Stat(e.acct); err == nil && !e.planted {
		t.Errorf("%s: the accounting dir was created", what)
	}
}

func exRefused(t *testing.T, what string, r harness.Result, sweep bool) {
	t.Helper()
	if r.Code != 78 || !strings.Contains(r.Stderr, exGateMsg+"\n") {
		t.Fatalf("%s: want 78 and the experimental line: %+v", what, r)
	}
	if got := strings.Contains(r.Stderr, exSweepMsg); got != sweep {
		t.Fatalf("%s: sweep line shown = %v, want %v: %q", what, got, sweep, r.Stderr)
	}
}

func TestEX1_RemoteProvidersNeedTheHostFlag(t *testing.T) {
	t.Run("no flag: 78 before anything, nothing contacted", func(t *testing.T) {
		e := exSandbox(t)
		e.host(e.h.Config, e.providers(""))
		r := e.h.Run("run", "heavy", "--", "echo", "ran")
		exRefused(t, "run", r, false)
		if strings.Contains(r.Stdout, "ran") {
			t.Fatalf("the command ran: %+v", r)
		}
		e.untouched(t, "run")
		// listed with the other config problems by config check
		e.host(e.h.Config, e.providers("")+"[pools.e2e]\nwait = \"soon\"\n")
		r = e.h.Run("config", "check")
		exRefused(t, "config check", r, false)
		if !strings.Contains(r.Stderr, `pools.e2e.wait = "soon"`) {
			t.Fatalf("the other problem is not listed: %q", r.Stderr)
		}
		e.untouched(t, "config check")
	})
	t.Run("a providers table alone or a pool naming a provider", func(t *testing.T) {
		e := exSandbox(t)
		e.host(e.h.Config, "[providers.fake]\ndaily_usd = 1\nstate_dir = "+harness.Q(e.fake)+"\n[pools.lint]\n")
		exRefused(t, "table alone", e.h.Run("run", "lint", "--", "true"), false)
		e.host(e.h.Config, "[pools.lint]\n[pools.heavy]\nproviders = [\"fake\", \"local\"]\n")
		exRefused(t, "pool only", e.h.Run("config", "check"), false)
		e.untouched(t, "pool only")
	})
	t.Run("a repo file cannot set it", func(t *testing.T) {
		e := exSandbox(t)
		e.host(e.h.Config, e.providers(""))
		e.h.Cwd = e.h.Repo("app2", map[string]string{".bitc.toml": "version = 1\n[host]\nexperimental_remote = true\n"})
		r := e.h.Run("config", "check")
		exRefused(t, "repo flag", r, false)
		if !strings.Contains(r.Stderr, "host.experimental_remote is a host-only key") {
			t.Fatalf("the repo key is not refused: %q", r.Stderr)
		}
		e.untouched(t, "repo flag")
	})
	t.Run("no environment variable sets it", func(t *testing.T) {
		e := exSandbox(t)
		e.host(e.h.Config, e.providers(""))
		for _, kv := range []string{"BITC_EXPERIMENTAL_REMOTE=true", "BITC_EXPERIMENTAL_REMOTE=1", "BITC_HOST_EXPERIMENTAL_REMOTE=true",
			"BITC_REMOTE=1", "BITC_EXPERIMENTAL=1", "BITC_PROVIDER=fake"} {
			exRefused(t, kv, e.h.RunWith([]string{kv}, "run", "heavy", "--", "echo", "ran"), false)
		}
		e.untouched(t, "env")
		// BITC_CONFIG names the host file: a flagged file there is the host file, so it is honoured
		alt := filepath.Join(e.h.Root, "alt", "bitc.toml")
		e.host(alt, e.providers("[host]\nexperimental_remote = true\n"))
		if r := e.h.RunWith([]string{"BITC_CONFIG=" + alt}, "config", "check"); r.Code != 0 {
			t.Fatalf("BITC_CONFIG host file with the flag: %+v", r)
		}
	})
	t.Run("flag set: today's behaviour", func(t *testing.T) {
		e := exSandbox(t)
		e.host(e.h.Config, e.providers("[host]\nexperimental_remote = true\n"))
		if r := e.h.Run("config", "check"); r.Code != 0 {
			t.Fatalf("config check: %+v", r)
		}
		r := e.h.Run("run", "heavy", "--", "echo", "ran")
		if r.Code != 0 || r.Stdout != "ran\n" || !strings.Contains(slurp(filepath.Join(e.fake, "calls.jsonl")), `"attach":true`) {
			t.Fatalf("fake run: %+v calls %q", r, slurp(filepath.Join(e.fake, "calls.jsonl")))
		}
		if r := e.h.Run("doctor"); !strings.Contains(r.Stdout, "warn remote: experimental_remote is on\n") {
			t.Fatalf("doctor: %+v", r)
		}
	})
	t.Run("local-only configs are unaffected", func(t *testing.T) {
		e := exSandbox(t)
		e.host(e.h.Config, "[pools.lint]\n")
		if r := e.h.Run("run", "lint", "--", "echo", "ran"); r.Code != 0 || r.Stdout != "ran\n" {
			t.Fatalf("local run: %+v", r)
		}
		if r := e.h.Run("doctor"); strings.Contains(r.Stdout, "remote:") || strings.Contains(r.Stderr, "experimental") {
			t.Fatalf("doctor: %+v", r)
		}
	})
	t.Run("an upgraded host with leftovers is told to sweep", func(t *testing.T) {
		e := exSandbox(t)
		e.host(e.h.Config, e.providers(""))
		dir := filepath.Join(e.acct, "remote", "fake")
		e.planted = true
		resv := `[{"pid":2147483000,"start":1,"name":"bitc-left","estimate":1000,"expiry":4102444800}]`
		ledger := `{"ts":"2026-01-01T00:00:00.000Z","event":"launch","name":"bitc-left"}` + "\n"
		e.h.Write(filepath.Join(dir, "reservations.json"), resv, 0o600)
		e.h.Write(filepath.Join(dir, "ledger.jsonl"), ledger, 0o600)
		before := exTree(t, e.acct)
		exRefused(t, "run", e.h.Run("run", "heavy", "--", "echo", "ran"), true)
		exRefused(t, "sweep", e.h.Run("sweep"), true)
		exRefused(t, "doctor --remote", e.h.Run("doctor", "--remote"), true)
		e.untouched(t, "upgraded")
		if slurp(filepath.Join(dir, "reservations.json")) != resv || slurp(filepath.Join(dir, "ledger.jsonl")) != ledger {
			t.Fatal("the leftovers were changed")
		}
		if after := exTree(t, e.acct); after != before {
			t.Fatalf("files appeared in the accounting dir:\n%s\nwas:\n%s", after, before)
		}
		// without leftovers, sweep and doctor --remote still say how to clean up
		e2 := exSandbox(t)
		e2.host(e2.h.Config, e2.providers(""))
		exRefused(t, "sweep, no leftovers", e2.h.Run("sweep"), true)
		exRefused(t, "doctor --remote, no leftovers", e2.h.Run("doctor", "--remote"), true)
		e2.untouched(t, "no leftovers")
	})
}

// exLeftoverHint is what doctor (as a warn line) and sweep (on stderr) say about a provider type
// whose reservations an earlier run did not finish, when that type is not configured.
const exLeftoverHint = "an earlier version's run may have left paid resources"

// plant writes an accounting dir as a remote run leaves it: reservations.json and ledger.jsonl.
func (e *exEnv) plant(t *testing.T, typ, resv, ledger string) {
	t.Helper()
	dir := filepath.Join(e.acct, "remote", typ)
	e.h.Write(filepath.Join(dir, "reservations.json"), resv, 0o600)
	e.h.Write(filepath.Join(dir, "ledger.jsonl"), ledger, 0o600)
	e.planted = true
}

func TestEX2_LeftoversOfAnUnconfiguredProviderAreReported(t *testing.T) {
	const launch = `{"ts":"2026-01-01T00:00:00Z","event":"launch","name":"bitc-left","est_usd":0.4}` + "\n"
	const end = `{"ts":"2026-01-01T00:20:00Z","event":"end","name":"bitc-left","minutes":20,"cost_usd":0.1,"created":true,"exit":"0"}` + "\n"
	t.Run("an unfinished reservation, no providers configured", func(t *testing.T) {
		e := exSandbox(t)
		e.host(e.h.Config, "[pools.lint]\n")
		e.plant(t, "runpod", `[{"pid":2147483000,"start":1,"name":"bitc-left","estimate":1000,"expiry":4102444800}]`, launch)
		if r := e.h.Run("run", "lint", "--", "echo", "ran"); r.Code != 0 || r.Stdout != "ran\n" {
			t.Fatalf("a local run is not refused: %+v", r)
		}
		r := e.h.Run("doctor")
		if !strings.Contains(r.Stdout, "warn remote: runpod: "+exLeftoverHint) || !strings.Contains(r.Stdout, "experimental_remote = true and run bitc sweep") {
			t.Fatalf("doctor: %+v", r)
		}
		r = e.h.Run("sweep")
		if r.Code != 0 || !strings.Contains(r.Stderr, "bitc: remote runpod: "+exLeftoverHint) {
			t.Fatalf("sweep: %+v", r)
		}
		if ents, _ := os.ReadDir(e.fake); len(ents) != 0 {
			t.Fatalf("a provider was contacted: %v", ents)
		}
		// a configured provider's leftovers are sweep's own work: no hint for that type
		e.host(e.h.Config, e.providers("[host]\nexperimental_remote = true\n"))
		e.plant(t, "fake", `[{"pid":2147483000,"start":1,"name":"bitc-f","estimate":1000,"expiry":4102444800}]`, "")
		r = e.h.Run("doctor")
		if !strings.Contains(r.Stdout, "warn remote: runpod: ") || strings.Contains(r.Stdout, "warn remote: fake: ") {
			t.Fatalf("doctor, fake configured: %+v", r)
		}
	})
	t.Run("a cleanly finished host says nothing", func(t *testing.T) {
		e := exSandbox(t)
		e.host(e.h.Config, "[pools.lint]\n")
		e.plant(t, "runpod", "[]", launch+end)
		if r := e.h.Run("doctor"); strings.Contains(r.Stdout, "remote:") {
			t.Fatalf("doctor: %+v", r)
		}
		if r := e.h.Run("sweep"); r.Code != 0 || strings.Contains(r.Stderr, exLeftoverHint) {
			t.Fatalf("sweep: %+v", r)
		}
		// with providers but no flag: the 78 has no cleanup line either
		e.host(e.h.Config, e.providers(""))
		exRefused(t, "clean host, run", e.h.Run("run", "heavy", "--", "echo", "ran"), false)
	})
	t.Run("a FIFO in the accounting dir never blocks", func(t *testing.T) {
		e := exSandbox(t)
		e.host(e.h.Config, e.providers(""))
		dir := filepath.Join(e.acct, "remote", "fake")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		e.planted = true
		for _, f := range []string{"reservations.json", "ledger.jsonl"} {
			if err := syscall.Mkfifo(filepath.Join(dir, f), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		r := e.h.Run("run", "heavy", "--", "echo", "ran") // the harness kills it after 60 s
		exRefused(t, "fifo", r, false)
		e.untouched(t, "fifo")
	})
}

// exTree lists every path under dir, one per line, sorted (WalkDir's order).
func exTree(t *testing.T, dir string) string {
	t.Helper()
	var b strings.Builder
	_ = filepath.WalkDir(dir, func(p string, _ os.DirEntry, err error) error {
		if err == nil {
			b.WriteString(p + "\n")
		}
		return nil
	})
	return b.String()
}
