package conformance

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Exploded-views/bitc/conformance/harness"
)

// slurp reads a file, "" if it is missing, so a missing file is an assertion failure, not a panic.
func slurp(p string) string {
	b, _ := os.ReadFile(p)
	return string(b)
}

func ownerTag(secret []byte, name string) string {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(name))
	return hex.EncodeToString(m.Sum(nil))
}

// capRepo: heavy goes to the fake at $0.24/h, start_timeout 20m, timeout 60m (E - now = 95m,
// so this job's estimate is $0.38), daily_usd 25, max_live 6. A negative spend leaves spend_usd
// out (no spend API: the cap sees the ledger, live resources and reservations).
func capRepo(t *testing.T, h *harness.H, spend float64) string {
	t.Helper()
	st := filepath.Join(h.Root, "fakestate")
	_ = os.MkdirAll(st, 0o700)
	spendLine := ""
	if spend >= 0 {
		spendLine = "spend_usd = " + strconv.FormatFloat(spend, 'f', 2, 64) + "\n"
	}
	h.HostConfig("[remote_limits]\nworktrees = [\"**/app\"]\n" +
		"[pools.heavy]\nproviders = [\"fake\", \"local\"]\nremote_limits = { size = { fake = \"cpu8\" }, timeout = \"60m\" }\nwait = \"5s\"\n" +
		"[providers.fake]\ndaily_usd = 25\nmax_live = 6\nstart_timeout = \"20m\"\nusd_per_hour = 0.24\n" +
		spendLine + "state_dir = " + harness.Q(st) + "\n")
	h.Cwd = h.Repo("app", map[string]string{".bitc.toml": "version = 1\n[pools.heavy.remote]\nallow = [[\"sh\", \"*\"], [\"echo\", \"*\"]]\n"})
	return st
}

func writeRes(t *testing.T, st string, rs []map[string]any) {
	b, _ := json.Marshal(rs)
	if err := os.WriteFile(filepath.Join(st, "resources.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func ownRes(n int, created time.Time) []map[string]any {
	var out []map[string]any
	for i := 0; i < n; i++ {
		out = append(out, map[string]any{"id": fmt.Sprintf("p%d", i), "name": fmt.Sprintf("bitc-0123456789abcdef-heavy-cpu8-%08x-1", i),
			"created": created, "expires": created.Add(95 * time.Minute), "usd_per_hour": 0.24})
	}
	return out
}

func TestK7_CapFiguresAndNote(t *testing.T) {
	h := harness.New(t)
	capRepo(t, h, 24.80)
	r := h.Run("run", "heavy", "--", "echo", "x")
	if !strings.Contains(r.Stderr, "running heavy locally: daily cap $25.00: spent $24.80 + committed $0.00 + this job $0.38") {
		t.Fatalf("%+v", r)
	}
	h2 := harness.New(t)
	st := capRepo(t, h2, 21.00)
	writeRes(t, st, ownRes(11, time.Now()))
	r = h2.Run("run", "heavy", "--", "echo", "x")
	if !strings.Contains(r.Stderr, "spent $21.00 + committed $4.18 + this job $0.38") {
		t.Fatalf("%+v", r)
	}
	others := ownRes(11, time.Now())
	for _, o := range others {
		o["name"] = "someone-else-" + o["id"].(string)
	}
	writeRes(t, st, others)
	if r := h2.Run("run", "heavy", "--", "echo", "x"); strings.Contains(r.Stderr, "daily cap") {
		t.Fatalf("other prefixes counted: %+v", r)
	}
	h3 := harness.New(t)
	st3 := capRepo(t, h3, 0)
	writeRes(t, st3, ownRes(6, time.Now()))
	if r := h3.Run("run", "heavy", "--", "echo", "x"); !strings.Contains(r.Stderr, "running heavy locally: 6 live of max_live 6") {
		t.Fatalf("%+v", r)
	}
}

func TestK6_LaunchLockAroundCheckAndCreateReleasedBeforeTheJob(t *testing.T) {
	h := harness.New(t)
	st := capRepo(t, h, 24.60)
	gate := filepath.Join(h.Root, "gate")
	t.Cleanup(func() { _ = os.WriteFile(gate, nil, 0o600) }) // never leave a held job behind
	// Estimate sleeps inside the check: without the launch lock both calls would pass the cap
	// before either wrote its reservation, and both would launch. The launched job is held at the
	// gate, its reservation alive, until the other call has decided, run locally and ended: a job
	// that ended first would drop its reservation and let the second launch too.
	h.HostConfig(strings.Replace(readCfg(t, h), "usd_per_hour = 0.24",
		"usd_per_hour = 0.24\nestimate_delay = \"500ms\"\ngate = "+harness.Q(gate), 1))
	a := h.Start("run", "heavy", "--", "echo", "ok")
	b := h.Start("run", "heavy", "--", "echo", "ok")
	h.Until(func() bool { return a.Exited() || b.Exited() }, 30*time.Second)
	if a.Exited() && b.Exited() {
		t.Fatalf("both calls ended while the launched job was held: %+v %+v", a.Wait(), b.Wait())
	}
	h.Write(gate, "", 0o600)
	ra, rb := a.Wait(), b.Wait()
	if ra.Stdout != "ok\n" || rb.Stdout != "ok\n" {
		t.Fatalf("%+v %+v", ra, rb)
	}
	if n := strings.Count(slurp(filepath.Join(st, "launches.log")), "\n"); n != 1 {
		t.Fatalf("%d launches", n)
	}
	capped := 0
	for _, r := range []harness.Result{ra, rb} {
		if strings.Contains(r.Stderr, "running heavy locally: daily cap $25.00: spent $24.60 + committed $0.38 + this job $0.38") {
			capped++
		}
		if strings.Contains(r.Stderr, "the launch lock is busy") {
			t.Fatalf("the lock was held through the job: %+v", r) // it is released before the job runs
		}
	}
	if capped != 1 {
		t.Fatalf("want exactly one capped call: %+v %+v", ra, rb)
	}
}

func TestK8_ExpiredOwnResourceTornDownNotCounted(t *testing.T) {
	h := harness.New(t)
	st := capRepo(t, h, 24.50)
	// the install secret is created by the first remote attempt; read it to tag the resource
	h.Run("run", "heavy", "--", "echo", "warm-up")
	secret := must(os.ReadFile(filepath.Join(h.Root, "account", "install-secret")))
	name := "bitc-0123456789abcdef-heavy-cpu8-0000000a-1"
	writeRes(t, st, []map[string]any{{"id": "old", "name": name, "created": time.Now().Add(-3 * time.Hour),
		"expires": time.Now().Add(-time.Hour), "usd_per_hour": 0.24, "owner": ownerTag(secret, name)}})
	r := h.Run("run", "heavy", "--", "echo", "x")
	if !strings.Contains(r.Stderr, "tore down old") || strings.Contains(r.Stderr, "daily cap") || r.Stdout != "x\n" {
		t.Fatalf("%+v", r)
	}
}

func TestC4_LedgerLaunchBeginEnd(t *testing.T) {
	h := harness.New(t)
	capRepo(t, h, 0)
	h.Run("run", "heavy", "--", "echo", "x")
	ledger := slurp(filepath.Join(h.Root, "account", "remote", "fake", "ledger.jsonl"))
	for _, ev := range []string{`"event":"launch"`, `"event":"begin"`, `"event":"end"`, `"exit":"0"`} {
		if !strings.Contains(ledger, ev) {
			t.Fatalf("ledger lacks %s:\n%s", ev, ledger)
		}
	}
}

func TestC5_OneBreakerFailurePer75AfterBegin(t *testing.T) {
	h := harness.New(t)
	capRepo(t, h, 0)
	h.HostConfig(strings.Replace(readCfg(t, h), "usd_per_hour = 0.24", "usd_per_hour = 0.24\nmode = \"lost\"", 1))
	h.Run("run", "heavy", "--", "echo", "x")
	f := slurp(filepath.Join(h.Root, "account", "remote", "fake", "failures.jsonl"))
	if strings.Count(f, `"kind":"failure"`) != 1 {
		t.Fatal(f)
	}
}

// After the create and before BEGIN (spec 8.2, 8.3): ledger end "75", never "fallback", and no
// breaker failure; the resource is torn down.
func TestRA7_FailureAfterCreateBeforeBeginIs75NoBreaker(t *testing.T) {
	h := harness.New(t)
	st := capRepo(t, h, 0)
	h.HostConfig(strings.Replace(readCfg(t, h), "usd_per_hour = 0.24", "usd_per_hour = 0.24\nmode = \"setup-fails\"", 1))
	if r := h.Run("run", "heavy", "--", "echo", "x"); r.Code != 75 || r.Stdout != "" || strings.Contains(r.Stderr, "running heavy locally") {
		t.Fatalf("%+v", r)
	}
	acct := filepath.Join(h.Root, "account", "remote", "fake")
	l := lines(t, filepath.Join(acct, "ledger.jsonl"))
	if len(l) == 0 || !strings.Contains(l[len(l)-1], `"event":"end"`) || !strings.Contains(l[len(l)-1], `"exit":"75"`) {
		t.Fatalf("ledger %v", l)
	}
	if b, _ := os.ReadFile(filepath.Join(acct, "failures.jsonl")); strings.Contains(string(b), `"kind":"failure"`) {
		t.Fatalf("a failure before BEGIN tripped the breaker: %s", b)
	}
	if len(lines(t, filepath.Join(st, "torn.log"))) != 1 {
		t.Fatal("the resource was not torn down")
	}
}

func TestRR10_BreakerPausesLaunches(t *testing.T) {
	h := harness.New(t)
	capRepo(t, h, 0)
	h.HostConfig(strings.Replace(readCfg(t, h), "usd_per_hour = 0.24", "usd_per_hour = 0.24\nmode = \"lost\"", 1))
	for i := 0; i < 3; i++ {
		h.Run("run", "heavy", "--", "echo", "x")
	}
	r := h.Run("run", "heavy", "--", "echo", "x")
	if !strings.Contains(r.Stderr, "running heavy locally: fake is paused: 3 failures after start in the last 1h0m0s") || r.Stdout != "x\n" {
		t.Fatalf("%+v", r)
	}
}

func TestRA1_RunningJobCountedOnce(t *testing.T) {
	h := harness.New(t)
	st := capRepo(t, h, 24.20)
	a := h.Start("run", "heavy", "--", "sh", "-c", "sleep 2")
	h.Until(func() bool { b, _ := os.ReadFile(filepath.Join(st, "launches.log")); return len(b) > 0 }, 10*time.Second)
	launched := strings.Fields(string(must(os.ReadFile(filepath.Join(st, "launches.log")))))[0]
	writeRes(t, st, []map[string]any{{"id": "live", "name": launched, "created": time.Now(), "expires": time.Now().Add(95 * time.Minute), "usd_per_hour": 0.24}})
	b := h.Run("run", "heavy", "--", "echo", "b") // 24.20 + 0.38 (listed, reservation not added) + 0.38 = 24.96
	if strings.Contains(b.Stderr, "daily cap") {
		t.Fatalf("a running job was counted twice: %+v", b)
	}
	a.Wait()
}

func TestRA5_UnavailablePausesTheProvider(t *testing.T) {
	h := harness.New(t)
	fakeRepo(t, h, "unavailable", "")
	r := h.Run("run", "heavy", "--", "echo", "x")
	if r.Stdout != "x\n" || !strings.Contains(r.Stderr, "running heavy locally: fake: account disabled") {
		t.Fatalf("%+v", r)
	}
	r = h.Run("run", "heavy", "--", "echo", "y")
	if r.Stdout != "y\n" || !strings.Contains(r.Stderr, "running heavy locally: fake is paused: fake: account disabled (until ") {
		t.Fatalf("the second call reached the provider: %+v", r)
	}
	f := slurp(filepath.Join(h.Root, "account", "remote", "fake", "failures.jsonl"))
	if strings.Count(f, `"kind":"pause"`) != 1 {
		t.Fatal(f)
	}
}

func TestRA6_LedgerCountsCreatedJobsOnlyAndOnce(t *testing.T) {
	h := harness.New(t)
	st := capRepo(t, h, -1) // no spend API
	base := strings.Replace(readCfg(t, h), "daily_usd = 25", "daily_usd = 1", 1)
	h.HostConfig(strings.Replace(base, "usd_per_hour = 0.24", "usd_per_hour = 0.24\nmode = \"transient\"", 1))
	for i := 0; i < 3; i++ { // capacity fallbacks: nothing was created, so nothing was spent
		if r := h.Run("run", "heavy", "--", "echo", "x"); !strings.Contains(r.Stderr, "running heavy locally: fake: no capacity") {
			t.Fatalf("%+v", r)
		}
	}
	h.HostConfig(base)
	a := h.Start("run", "heavy", "--", "sh", "-c", "sleep 2")
	h.Until(func() bool { b, _ := os.ReadFile(filepath.Join(st, "launches.log")); return len(b) > 0 }, 10*time.Second)
	launched := strings.Fields(string(must(os.ReadFile(filepath.Join(st, "launches.log")))))[0]
	writeRes(t, st, []map[string]any{{"id": "live", "name": launched, "created": time.Now(), "expires": time.Now().Add(95 * time.Minute), "usd_per_hour": 0.24}})
	b := h.Run("run", "heavy", "--", "echo", "b") // $0 + $0.38 (a, listed once) + $0.38 = $0.76 <= $1
	if strings.Contains(b.Stderr, "daily cap") || b.Stdout != "b\n" {
		t.Fatalf("fallbacks or the running job were counted against the cap: %+v", b)
	}
	a.Wait()
}

func TestRA2_SecondStateDirCannotExceedTheCap(t *testing.T) {
	h := harness.New(t)
	st := capRepo(t, h, 24.60)
	a := h.Start("run", "heavy", "--", "sh", "-c", "sleep 2")
	h.Until(func() bool { b, _ := os.ReadFile(filepath.Join(st, "launches.log")); return len(b) > 0 }, 10*time.Second)
	b := h.RunWith([]string{"BITC_DIR=" + filepath.Join(h.Root, "other-state")}, "run", "heavy", "--", "echo", "b")
	if !strings.Contains(b.Stderr, "daily cap") {
		t.Fatalf("a second state dir got its own allowance: %+v", b)
	}
	a.Wait()
}

func TestRA3_OtherInstallsAndAccountSpendCount(t *testing.T) {
	h := harness.New(t)
	st := capRepo(t, h, 23.50)
	writeRes(t, st, []map[string]any{{"id": "x", "name": "bitc-ffffffffffffffff-heavy-cpu8-0000000f-1", "created": time.Now(),
		"expires": time.Now().Add(95 * time.Minute), "usd_per_hour": 0.96}}) // another install: $1.52
	if r := h.Run("run", "heavy", "--", "echo", "x"); !strings.Contains(r.Stderr, "spent $23.50 + committed $1.52 + this job $0.38") {
		t.Fatalf("%+v", r)
	}
}

func TestRA4_CompatSharesLegacyLaunchLockAndReservations(t *testing.T) {
	h := harness.New(t)
	capRepo(t, h, 0)
	legacy := filepath.Join(h.Root, "legacy")
	_ = os.MkdirAll(filepath.Join(legacy, "locks"), 0o700)
	h.HostConfig(readCfg(t, h) + "\n[compat]\nlegacy_dir = " + harness.Q(legacy) + "\nname_prefix = \"legacy-pool-\"\n")
	a := h.Start("run", "heavy", "--", "sh", "-c", "sleep 1")
	h.Until(func() bool { b, _ := os.ReadFile(filepath.Join(legacy, "runpod-reservations")); return len(b) > 0 }, 10*time.Second)
	line := string(must(os.ReadFile(filepath.Join(legacy, "runpod-reservations"))))
	if f := strings.Fields(line); len(f) != 4 || f[2] != "0.38" {
		t.Fatalf("%q", line)
	}
	a.Wait()
	if b, _ := os.ReadFile(filepath.Join(legacy, "runpod-reservations")); strings.TrimSpace(string(b)) != "" {
		t.Fatalf("legacy reservation not removed: %q", b)
	}
}
