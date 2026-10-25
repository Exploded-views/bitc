package conformance

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Exploded-views/bitc/conformance/harness"
)

// plEnv is one sandbox for the plugin rows of the fallback boundary (providers delta A.5.4, A.6,
// B.3): pool heavy goes to the plugin "stub" (the stub provider plugin, driven by its `fault` key),
// then local. The stub keeps its resources and logs under BITC_DIR/plugins/stub.
type plEnv struct {
	h    *harness.H
	own  string // BITC_DIR/plugins/stub
	acct string // ACCOUNT/remote/stub
	wt   string
}

// plRepo configures the plugin with the given fault list. declare is what the plugin declares as
// caps.fallback_until; extra is more [providers.stub] text (for example the host's fallback_until).
func plRepo(t *testing.T, h *harness.H, fault, declare, extra string) *plEnv {
	t.Helper()
	path, sum := h.Plugin("stub")
	e := &plEnv{h: h, own: filepath.Join(h.State, "plugins", "stub"), acct: filepath.Join(h.Root, "account", "remote", "stub")}
	h.Env = append(h.Env, "STUB_FALLBACK="+declare)
	h.Seam("time_scale = 0.05")
	h.Seam("agent_url = \"http://127.0.0.1:9/bitc\"")
	h.HostConfig("[remote_limits]\nworktrees = [\"**/app\"]\n" +
		"[pools.heavy]\nproviders = [\"stub\", \"local\"]\nremote_limits = { size = { stub = \"cpu8\" }, timeout = \"60m\" }\nwait = \"5s\"\n" +
		"[providers.stub]\nplugin = { path = " + harness.Q(path) + ", sha256 = " + harness.Q(sum) + ", env_pass = [\"STUB_FALLBACK\", \"STUB_MULTI_JOB\"] }\n" +
		"api_key = \"sk-test-0123456789\"\ndaily_usd = 25\nmax_live = 6\nstart_timeout = \"1m\"\n" +
		"sizes = { cpu8 = { usd_per_hour = 0.24 } }\nendpoints = [\"127.0.0.1\"]\nfault = " + harness.Q(fault) + "\n" + extra)
	e.wt = h.Repo("app", map[string]string{".bitc.toml": "version = 1\n[pools.heavy.remote]\nallow = [[\"sh\", \"*\"], [\"echo\", \"*\"]]\n"})
	h.Cwd = e.wt
	return e
}

func (e *plEnv) launches(t *testing.T) []string {
	return lines(t, filepath.Join(e.own, "launches.log"))
}
func (e *plEnv) torn(t *testing.T) []string { return lines(t, filepath.Join(e.own, "torn.log")) }

func (e *plEnv) live(t *testing.T) []map[string]any {
	var rs []map[string]any
	if b, err := os.ReadFile(filepath.Join(e.own, "res.json")); err == nil {
		_ = json.Unmarshal(b, &rs)
	}
	return rs
}

func (e *plEnv) failures(t *testing.T) int {
	n := 0
	for _, l := range lines(t, filepath.Join(e.acct, "failures.jsonl")) {
		if strings.Contains(l, `"kind":"failure"`) {
			n++
		}
	}
	return n
}

func (e *plEnv) ran(t *testing.T) bool {
	_, err := os.Stat(filepath.Join(e.wt, "ran-locally"))
	return err == nil
}

// plAttached checks the outcome every "read as attached" row shares: 75 with the STARVED line, no
// other provider tried (the command did not run locally), one launch, the resource gone.
func plAttached(t *testing.T, e *plEnv, r harness.Result, line string) {
	t.Helper()
	if r.Code != 75 || !strings.Contains(r.Stderr, "STARVED heavy (stub: "+line) {
		t.Fatalf("want 75 and STARVED heavy (stub: %s...), got %d: %s", line, r.Code, r.Stderr)
	}
	if e.ran(t) || strings.Contains(r.Stderr, "trying") || strings.Contains(r.Stderr, "locally") {
		t.Errorf("another provider was tried: %s", r.Stderr)
	}
	if n := len(e.launches(t)); n != 1 {
		t.Errorf("%d launches, want exactly one (no retry)", n)
	}
	if len(e.live(t)) != 0 {
		t.Errorf("resources left: %v", e.live(t))
	}
}

// FB10: a launch that times out after the resource was created is ambiguous: reaped by name, 75,
// "launch ambiguous", one breaker failure, no retry and no next provider, under either boundary.
func TestFB10_AmbiguousLaunchTimeoutIsReapedAnd75(t *testing.T) {
	for _, c := range []struct{ declare, extra string }{{"resource", ""}, {"attach", "fallback_until = \"attach\"\n"}} {
		h := harness.New(t)
		e := plRepo(t, h, "launch-hang", c.declare, c.extra)
		r := h.Run(markLocal...)
		plAttached(t, e, r, "launch ambiguous: launch timed out after ")
		if len(e.torn(t)) != 1 || !strings.HasPrefix(e.torn(t)[0], "r-bitc-") {
			t.Errorf("%s: reaped by name: torn %v", c.declare, e.torn(t))
		}
		if n := e.failures(t); n != 1 {
			t.Errorf("%s: %d breaker failures, want one for the ambiguous launch", c.declare, n)
		}
	}
}

// FB11: the plugin is SIGKILLed mid-launch after it created the resource: a respawned plugin
// reaps it by name; 75, no next provider.
func TestFB11_PluginKilledMidLaunchIsReapedOnTheRespawn(t *testing.T) {
	for _, c := range []struct{ declare, extra string }{{"resource", ""}, {"attach", "fallback_until = \"attach\"\n"}} {
		h := harness.New(t)
		e := plRepo(t, h, "launch-die", c.declare, c.extra)
		r := h.Run(markLocal...)
		plAttached(t, e, r, "launch ambiguous: plugin ")
		if len(e.torn(t)) != 1 {
			t.Errorf("%s: torn %v", c.declare, e.torn(t))
		}
	}
}

// FB12: a malformed frame, or a final error with an unknown class (with or without id), or a
// begun/ended phase event, mid-launch after the create: reaped by name, 75, no next provider.
func TestFB12_MalformedOrUnknownLaunchOutcomeIsAmbiguous(t *testing.T) {
	for _, f := range []string{"launch-malformed", "launch-unknown-class", "launch-unknown-class-id", "launch-phase-begun"} {
		h := harness.New(t)
		e := plRepo(t, h, f, "attach", "fallback_until = \"attach\"\n")
		r := h.Run(markLocal...)
		plAttached(t, e, r, "launch ambiguous: ")
		if len(e.torn(t)) != 1 {
			t.Errorf("%s: torn %v", f, e.torn(t))
		}
	}
}

// FB13: a `transient` launch error with an id is attached (75) under both boundaries; a
// `pre_attach` under a narrowed resource boundary is read as attached (75); a `pre_attach` under
// attach takes the FB2 path: torn down, one retry on a new name, then the next provider.
func TestFB13_ClassifiedLaunchErrorsFollowTheBoundary(t *testing.T) {
	for _, c := range []struct{ name, fault, declare, extra string }{
		{"transient with id, resource", "launch-transient-id", "resource", ""},
		{"transient with id, attach", "launch-transient-id", "attach", "fallback_until = \"attach\"\n"},
		{"pre_attach, plugin default resource", "launch-preattach", "attach", ""},
		{"pre_attach, narrowed to resource", "launch-preattach", "attach", "fallback_until = \"resource\"\n"},
	} {
		h := harness.New(t)
		e := plRepo(t, h, c.fault, c.declare, c.extra)
		r := h.Run(markLocal...)
		want := "capacity after create"
		if c.fault == "launch-preattach" {
			want = "apt timeout"
		}
		plAttached(t, e, r, want)
		if e.failures(t) != 0 {
			t.Errorf("%s: an attached failure before BEGIN is no breaker failure", c.name)
		}
	}
	h := harness.New(t)
	e := plRepo(t, h, "launch-preattach", "attach", "fallback_until = \"attach\"\n")
	r := h.Run(markLocal...)
	names := e.launches(t)
	if r.Code != 0 || !e.ran(t) || len(names) != 2 || names[0] == names[1] {
		t.Fatalf("pre_attach under attach: want two attempts on distinct names, then local: code %d launches %v: %s", r.Code, names, r.Stderr)
	}
	if !strings.Contains(r.Stderr, "bitc: stub: running heavy locally: apt timeout; "+names[1]+" torn down before attach") {
		t.Errorf("fallback note: %s", r.Stderr)
	}
	if len(e.live(t)) != 0 || e.failures(t) != 2 {
		t.Errorf("live %v, breaker failures %d (one per discarded resource)", e.live(t), e.failures(t))
	}
}

// A plugin that rejects its configuration makes `bitc doctor --remote` exit 78 with the host-file
// line, not 69 (providers delta A.4.5, A.8), and says so in a `fail plugin-<type>` line.
func TestPD1_DoctorRemotePluginConfigErrorsAre78(t *testing.T) {
	for _, c := range []struct{ name, fault, declare, extra, want string }{
		{"configure class config", "config-error", "resource", "", `providers.stub: region "mars" is not served`},
		{"fallback_until wider than declared", "", "resource", "fallback_until = \"attach\"\n",
			`providers.stub.fallback_until: "attach" is wider than stub allows ("resource")`},
	} {
		h := harness.New(t)
		plRepo(t, h, c.fault, c.declare, c.extra)
		r := h.Run("doctor", "--remote")
		if r.Code != 78 {
			t.Errorf("%s: exit %d, want 78\n%s%s", c.name, r.Code, r.Stdout, r.Stderr)
		}
		if !strings.Contains(r.Stderr, "bitc: ") || !strings.Contains(r.Stderr, ": "+c.want) {
			t.Errorf("%s: no host-file line with %q:\n%s", c.name, c.want, r.Stderr)
		}
		if !strings.Contains(r.Stdout, "fail plugin-stub: "+c.want) {
			t.Errorf("%s: no `fail plugin-stub` line:\n%s", c.name, r.Stdout)
		}
	}
}

func TestPD2_DoctorRemotePluginReportsProtocolVersionAndPin(t *testing.T) {
	h := harness.New(t)
	plRepo(t, h, "", "resource", "")
	_, sum := h.Plugin("stub")
	r := h.Run("doctor", "--remote")
	want := "ok plugin-stub: protocol 1, 0.0.1-stub, sha256 " + sum[:12] + "\n"
	if !strings.Contains(r.Stdout, want) {
		t.Errorf("doctor lacks %q:\n%s", want, r.Stdout)
	}
	if r.Code == 78 {
		t.Errorf("a healthy plugin gave 78:\n%s%s", r.Stdout, r.Stderr)
	}
}
