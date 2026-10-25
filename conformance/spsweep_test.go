package conformance

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Exploded-views/bitc/conformance/harness"
	"github.com/Exploded-views/bitc/internal/provider/sprites"
)

// The sweep, doctor, status and worst-case rows for Sprites.

const otherInstall = "ffffffffffffffff"

var otherSecret = []byte("another-install-secret-0123456789")

// spSeed is a Sprite listed by the fake: kind "own" (this install's tag), "forged" (this install's
// name with a wrong tag) or "other" (another install). exp is relative to now, age is how long ago
// the Sprite was created.
func (e *spEnv) seed(t *testing.T, kind, job8 string, exp, age time.Duration) string {
	t.Helper()
	var name string
	switch kind {
	case "own":
		name = sprites.EncodeName(e.secret(t), "bitc", sha16(e.secret(t)), "cpu8", job8, time.Now().Add(exp))
	case "forged":
		name = sprites.EncodeName(e.secret(t), "bitc", sha16(e.secret(t)), "cpu8", job8, time.Now().Add(exp))
		name = name[:len(name)-1] + map[bool]string{true: "1", false: "0"}[!strings.HasSuffix(name, "1")]
	case "other":
		name = sprites.EncodeName(otherSecret, "bitc", otherInstall, "cpu8", job8, time.Now().Add(exp))
	default:
		t.Fatal(kind)
	}
	e.f.AddSprite(name, e.f.Org(), time.Now().Add(-age))
	return name
}

// spInit runs one job so the install secret and the account directory exist.
func spInit(t *testing.T, h *harness.H) {
	t.Helper()
	if r := h.Run(spRun("echo init")...); r.Code != 0 {
		t.Fatalf("init job: %+v", r)
	}
}

func (e *spEnv) writeReservations(t *testing.T, rs ...map[string]any) {
	t.Helper()
	b, _ := json.Marshal(rs)
	if err := os.MkdirAll(e.acct, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.acct, "reservations.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func resv1(name string, pid int, orphan bool) map[string]any {
	return map[string]any{"pid": pid, "start": 0, "name": name, "estimate": 1000, "expiry": time.Now().Add(time.Hour).Unix(), "orphan": orphan}
}

func isLive(e *spEnv, name string) bool {
	for _, n := range e.f.Live() {
		if n == name {
			return true
		}
	}
	return false
}

func TestSP14_SweepTable(t *testing.T) {
	h := harness.New(t)
	e := spRepo(t, h, spOpts{})
	spInit(t, h)
	const hour, old = time.Hour, 30 * time.Minute
	expired := e.seed(t, "own", "00000001", -hour, 3*hour)    // own, past E
	gone := e.seed(t, "own", "00000002", hour, old)           // own, no reservation, older than start_timeout
	young := e.seed(t, "own", "00000003", hour, 0)            // own, no reservation, created just now
	live := e.seed(t, "own", "00000004", hour, old)           // own, a reservation whose pid is alive
	orphan := e.seed(t, "own", "00000005", hour, 0)           // own, an orphan reservation (its pid alive, running on RunPod)
	otherExp := e.seed(t, "other", "00000006", -hour, 3*hour) // another install, past E
	otherLive := e.seed(t, "other", "00000007", hour, old)    // another install, not past E
	forged := e.seed(t, "forged", "00000008", -hour, 3*hour)  // this install's name, a wrong tag
	e.writeReservations(t, resv1(live, os.Getpid(), false), resv1(orphan, os.Getpid(), true))

	r := h.Run("sweep", "--dry-run")
	for _, want := range []string{"swept sprite " + expired + " (past its expiry)", "swept sprite " + gone + " (job process gone)", "swept sprite " + orphan + " (orphaned by a pre-attach discard)"} {
		if !strings.Contains(r.Stdout, want+"\n") {
			t.Fatalf("dry run lacks %q:\n%s", want, r.Stdout)
		}
	}
	if n := len(e.f.Live()); n != 8 {
		t.Fatalf("a dry run deleted: %v", e.f.Live())
	}

	r = h.Run("sweep")
	if r.Code != 0 {
		t.Fatalf("%+v", r)
	}
	for _, n := range []string{young, live, otherExp, otherLive, forged} {
		if strings.Contains(r.Stdout, n) || !isLive(e, n) {
			t.Fatalf("%s must be kept: %s", n, r.Stdout)
		}
	}
	for _, n := range []string{expired, gone, orphan} {
		if isLive(e, n) {
			t.Fatalf("%s must be deleted", n)
		}
	}
	if !strings.Contains(r.Stderr, forged+" has no verified owner tag; kept") {
		t.Fatalf("stderr: %s", r.Stderr)
	}
	if rs := e.reservations(t); len(rs) != 1 || rs[0].Name != live || rs[0].Orphan {
		t.Fatalf("a confirmed delete drops the orphan and keeps the live reservation: %v", rs)
	}

	r = h.Run("sweep", "--foreign")
	if !strings.Contains(r.Stdout, "swept sprite "+otherExp+" (past its expiry; --foreign)\n") || isLive(e, otherExp) || !isLive(e, otherLive) || !isLive(e, forged) {
		t.Fatalf("--foreign: %s live %v", r.Stdout, e.f.Live())
	}

	// the next launch's cap check deletes an orphan at once as well
	orphan2 := e.seed(t, "own", "00000009", hour, 0)
	e.writeReservations(t, resv1(live, os.Getpid(), false), resv1(orphan2, os.Getpid(), true))
	if r := h.Run(spRun("echo next")...); r.Code != 0 || r.Stdout != "next\n" {
		t.Fatalf("the next job: %+v", r)
	}
	if isLive(e, orphan2) {
		t.Fatalf("the cap check left the orphan %s", orphan2)
	}
}

func TestSP21_WorstCaseScriptSprites(t *testing.T) {
	out, err := exec.Command("sh", "../scripts/live-proof-worst-case.sh", "--sprites", "0.7571", "3", "10", "30", "6", "15").CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	// per job: 2 x 3 min provisioning + (10 + 30 + 15 + 15) min = 76 min x $0.7571/h = $0.9590
	want := "worst case per job: 76 min (2 x 3 provisioning + 10 + 30 + 15 + 15 sweep) x $0.7571/h = $0.96\nworst case for 6 jobs: $5.75\n"
	if string(out) != want {
		t.Fatalf("got %q, want %q", out, want)
	}
	if out, err := exec.Command("sh", "../scripts/live-proof-worst-case.sh", "0.24", "20", "10", "6").CombinedOutput(); err != nil || !strings.HasPrefix(string(out), "worst case per job: 70 min x $0.2400/h") {
		t.Fatalf("the default mode changed: %v %s", err, out)
	}
}

// RW rows for Sprites: the RW1, RW4 and RW6 behaviours against the Sprites provider.

func TestRW7_SweepSpritesTearsDownOwnVerifiedOnly(t *testing.T) {
	h := harness.New(t)
	e := spRepo(t, h, spOpts{})
	spInit(t, h)
	mine := e.seed(t, "own", "0000000a", -2*time.Hour, 3*time.Hour)
	untagged := e.seed(t, "forged", "0000000b", -2*time.Hour, 3*time.Hour)
	other := e.seed(t, "other", "0000000c", -2*time.Hour, 3*time.Hour)
	live := e.seed(t, "own", "0000000d", time.Hour, 0)
	e.f.AddSprite("unrelated-sprite", e.f.Org(), time.Now().Add(-5*time.Hour))
	r := h.Run("sweep")
	if r.Code != 0 || !strings.Contains(r.Stdout, "swept sprite "+mine+" (past its expiry)\n") || strings.Contains(r.Stdout, untagged) ||
		strings.Contains(r.Stdout, other) || strings.Contains(r.Stdout, live) || strings.Contains(r.Stdout, "unrelated") {
		t.Fatalf("%+v", r)
	}
	if isLive(e, mine) || !isLive(e, untagged) || !isLive(e, other) || !isLive(e, live) || !isLive(e, "unrelated-sprite") {
		t.Fatalf("live %v", e.f.Live())
	}
	if r := h.Run("sweep", "--foreign"); !strings.Contains(r.Stdout, "swept sprite "+other+" (past its expiry; --foreign)") || isLive(e, other) ||
		!isLive(e, untagged) || !isLive(e, "unrelated-sprite") {
		t.Fatalf("--foreign: %+v live %v", r, e.f.Live())
	}
}

func TestRW8_SweepSpritesReportsErrorsAndExitsZero(t *testing.T) {
	h := harness.New(t)
	e := spRepo(t, h, spOpts{})
	spInit(t, h)
	mine := e.seed(t, "own", "0000000a", -2*time.Hour, 3*time.Hour)
	e.f.BehaveN("list-500", 2)
	if r := h.Run("sweep"); r.Code != 0 || !strings.Contains(r.Stderr, "bitc: sweep: sprites: ") || !isLive(e, mine) {
		t.Fatalf("list: %+v", r)
	}
	e.f.Behave("delete-500")
	r := h.Run("sweep")
	if r.Code != 0 || !strings.Contains(r.Stderr, "bitc: sweep: sprites: could not tear down "+mine) || !isLive(e, mine) {
		t.Fatalf("delete: %+v", r)
	}
	if r := h.Run("sweep"); r.Code != 0 || isLive(e, mine) {
		t.Fatalf("the next sweep: %+v", r)
	}
	// a Sprite of another organization fails the listing: it is named, never touched, exit 0
	stranded := e.seed(t, "own", "0000000e", -2*time.Hour, 3*time.Hour)
	e.f.AddSprite("bitc-foreign", "somebody-else", time.Now())
	r = h.Run("sweep")
	if r.Code != 0 || !strings.Contains(r.Stderr, "bitc-foreign") || !strings.Contains(r.Stderr, "somebody-else") || !strings.Contains(r.Stderr, "never touched") ||
		!isLive(e, stranded) || !isLive(e, "bitc-foreign") {
		t.Fatalf("foreign org: %+v", r)
	}
	for _, c := range e.f.Calls() {
		if c.Method == "DELETE" && strings.HasSuffix(c.Path, "bitc-foreign") {
			t.Fatalf("a DELETE of another org's Sprite: %s", c.Path)
		}
	}
}

func TestRW9_SweepSpritesPrunesDeadReservations(t *testing.T) {
	h := harness.New(t)
	e := spRepo(t, h, spOpts{})
	spInit(t, h)
	e.writeReservations(t, resv1("bitc-dead", 2147483000, false))
	if r := h.Run("sweep", "--dry-run"); !strings.Contains(r.Stdout, "swept reservation bitc-dead (its process is gone)") || len(e.reservations(t)) != 1 {
		t.Fatalf("dry run: %+v", r)
	}
	if r := h.Run("sweep"); !strings.Contains(r.Stdout, "swept reservation bitc-dead (its process is gone)") || len(e.reservations(t)) != 0 {
		t.Fatalf("sweep: %+v", r)
	}
}

func spDoctor(h *harness.H) (string, int) {
	r := h.Run("doctor", "--remote")
	return r.Stdout, r.Code
}

func TestSW3_DoctorRemoteSprites(t *testing.T) {
	h := harness.New(t)
	e := spRepo(t, h, spOpts{})
	spInit(t, h)
	out, code := spDoctor(h)
	for _, want := range []string{"ok sprites api:", "ok sprites org:", "ok sprites outside prefix:", "ok sprites installs:", "ok sprites connectors:",
		"fail sprites sweep timer: no sweep has run in the last 30 minutes"} {
		if !strings.Contains(out, want) {
			t.Fatalf("lacks %q:\n%s", want, out)
		}
	}
	if code != 69 || strings.Contains(out, e.f.Token()) {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	h.Run("sweep", "--dry-run")
	if out, _ := spDoctor(h); !strings.Contains(out, "fail sprites sweep timer") {
		t.Fatalf("a dry run is no sweep:\n%s", out)
	}
	h.Run("sweep")
	if out, _ := spDoctor(h); !strings.Contains(out, "ok sprites sweep timer: the last sweep was 0m ago") {
		t.Fatalf("after a sweep:\n%s", out)
	}
	e.f.AddConnection("github-app")
	e.f.AddSprite("stray-app", e.f.Org(), time.Now())
	e.seed(t, "other", "0000000c", time.Hour, time.Minute)
	e.seed(t, "other", "0000000e", -time.Hour, time.Hour)
	e.seed(t, "own", "0000000f", time.Hour, 0)
	out, _ = spDoctor(h)
	for _, want := range []string{"fail sprites connectors: 1 Connector(s): github-app;", "fail sprites outside prefix: 1 Sprites do not start with \"bitc-\": stray-app;",
		"warn sprites installs: 2 installs hold Sprites in this org (", "warn sprites foreign:"} {
		if !strings.Contains(out, want) {
			t.Fatalf("lacks %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, otherInstall) {
		t.Fatalf("the other install is not named:\n%s", out)
	}
	e.f.AddSprite("bitc-foreign", "somebody-else", time.Now())
	out, code = spDoctor(h)
	if code != 69 || !strings.Contains(out, "fail sprites org: sprite bitc-foreign belongs to organization \"somebody-else\"") || !strings.Contains(out, "fail sprites api:") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
}

func TestSW4_StatusSpritesSection(t *testing.T) {
	h := harness.New(t)
	e := spRepo(t, h, spOpts{})
	spInit(t, h)
	e.seed(t, "other", "0000000c", time.Hour, time.Minute)
	r := h.Run("--json", "status")
	var got map[string]any
	if err := json.Unmarshal([]byte(r.Stdout), &got); err != nil {
		t.Fatalf("%v: %s", err, r.Stdout)
	}
	var schema map[string]any
	if err := json.Unmarshal(must(os.ReadFile("../docs/status.schema.json")), &schema); err != nil {
		t.Fatal(err)
	}
	checkSchema(t, "$", schema, got)
	sec, _ := got["remote"].(map[string]any)["sprites"].(map[string]any)
	if sec == nil || len(sec["foreign"].([]any)) != 1 || sec["spend_usd"] != "" || sec["est_usd_today"] == "" || sec["est_usd_today"] == nil || sec["last_sweep"] != nil {
		t.Fatalf("before a sweep: %s", r.Stdout)
	}
	h.Run("sweep")
	r = h.Run("--json", "status")
	_ = json.Unmarshal([]byte(r.Stdout), &got)
	sec, _ = got["remote"].(map[string]any)["sprites"].(map[string]any)
	if ls, _ := sec["last_sweep"].(string); ls == "" {
		t.Fatalf("after a sweep: %s", r.Stdout)
	}
	e.f.AddSprite("bitc-foreign", "somebody-else", time.Now())
	r = h.Run("--json", "status")
	_ = json.Unmarshal([]byte(r.Stdout), &got)
	sec, _ = got["remote"].(map[string]any)["sprites"].(map[string]any)
	if sec["foreign_org"] != "bitc-foreign" || !strings.Contains(fmt.Sprint(sec["error"]), "bitc-foreign") {
		t.Fatalf("foreign org: %s", r.Stdout)
	}
}
