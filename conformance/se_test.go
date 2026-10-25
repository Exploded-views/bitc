package conformance

import (
	"encoding/base64"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Exploded-views/bitc/conformance/harness"
)

// seEnv is a sandbox for the caller-env rows (providers delta S): pool heavy walks the providers
// in its list, then local. The repo forwards CI_PLAIN (plain) and DEPLOY_TOKEN (forced).
type seEnv struct {
	h  *harness.H
	st map[string]string // provider type -> state_dir
	wt string
}

const (
	seForcedVal = "CANARY-forced-9f3a71"
	sePlainVal  = "plainval-1"
)

// seRepo: extra maps a provider type ("fake", "fake2") to extra keys of its section; limits is
// extra [remote_limits] text. Every provider prices at $0.24/h under size cpu8.
func seRepo(t *testing.T, h *harness.H, limits string, order []string, extra map[string]string) *seEnv {
	t.Helper()
	e := &seEnv{h: h, st: map[string]string{}}
	var quoted, sizes []string
	cfg := "[remote_limits]\nworktrees = [\"**/app\"]\nforward_env_allow = [\"CI_PLAIN\", \"DEPLOY_TOKEN\"]\nforward_env_force = [\"DEPLOY_TOKEN\"]\n" + limits
	for _, typ := range order {
		quoted = append(quoted, harness.Q(typ))
		sizes = append(sizes, typ+" = \"cpu8\"")
	}
	quoted = append(quoted, `"local"`)
	cfg += "[pools.heavy]\nproviders = [" + strings.Join(quoted, ", ") + "]\nremote_limits = { size = { " + strings.Join(sizes, ", ") + " }, timeout = \"60m\" }\nwait = \"5s\"\n"
	for _, typ := range order {
		e.st[typ] = filepath.Join(h.Root, "state-"+typ)
		if err := os.MkdirAll(e.st[typ], 0o700); err != nil {
			t.Fatal(err)
		}
		cfg += "[providers." + typ + "]\napi_key = \"sk-test-" + typ + "\"\ndaily_usd = 25\nmax_live = 6\nusd_per_hour = 0.24\n" +
			"state_dir = " + harness.Q(e.st[typ]) + "\n" + extra[typ]
	}
	h.HostConfig(cfg)
	e.wt = h.Repo("app", map[string]string{".bitc.toml": "version = 1\n[remote]\nforward_env = [\"CI_PLAIN\", \"DEPLOY_TOKEN\"]\nenv = { FROM_CONFIG = \"cfgval\" }\n" +
		"[pools.heavy.remote]\nallow = [[\"sh\", \"*\"], [\"echo\", \"*\"]]\n"})
	h.Cwd = e.wt
	return e
}

// show prints what the job saw of the three names.
var seShow = []string{"run", "heavy", "--", "sh", "-c", `echo "$FROM_CONFIG|$CI_PLAIN|${DEPLOY_TOKEN:+tok}"`}

var (
	seBoth  = []string{"CI_PLAIN=" + sePlainVal, "DEPLOY_TOKEN=" + seForcedVal}
	sePlain = []string{"CI_PLAIN=" + sePlainVal}
)

func (e *seEnv) calls(typ string) string { return slurp(filepath.Join(e.st[typ], "calls.jsonl")) }

// untouched: the provider saw no call, and left no reservation, ledger line or breaker record.
func (e *seEnv) untouched(t *testing.T, typ string) {
	t.Helper()
	acct := filepath.Join(e.h.Root, "account", "remote", typ)
	for _, f := range []string{"ledger.jsonl", "reservations.json", "failures.jsonl"} {
		if s := strings.TrimSpace(slurp(filepath.Join(acct, f))); s != "" && s != "[]" {
			t.Errorf("%s: %s holds %q", typ, f, s)
		}
	}
	if c := e.calls(typ); c != "" {
		t.Errorf("%s saw calls: %s", typ, c)
	}
}

// noCanary: the forced value (raw, hex, base64) appears in no file under the provider's state.
func (e *seEnv) noCanary(t *testing.T, typ string) {
	t.Helper()
	forms := []string{seForcedVal, hex.EncodeToString([]byte(seForcedVal)), base64.StdEncoding.EncodeToString([]byte(seForcedVal)),
		base64.RawStdEncoding.EncodeToString([]byte(seForcedVal))}
	_ = filepath.Walk(e.st[typ], func(p string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() {
			return nil
		}
		for _, f := range forms {
			if strings.Contains(slurp(p), f) {
				t.Errorf("the forced canary (%q form) reached %s", f[:8], p)
			}
		}
		return nil
	})
}

func levelOf(t *testing.T, h *harness.H, typ string) string {
	t.Helper()
	return strings.TrimSpace(explain(h, nil, "providers."+typ+".send_env"))
}

// SE1: defaults, every value settable, and the 78 cases.
func TestSE1_DefaultsSettingAndValidation(t *testing.T) {
	// the fake declares what send_env_default says (forwarded when unset)
	for _, c := range []struct{ extra, want string }{
		{"", `providers.fake.send_env = "forwarded"  # default`},
		{"send_env_default = \"forced\"\n", `providers.fake.send_env = "forced"  # default`},
		{"send_env_default = \"none\"\n", `providers.fake.send_env = "none"  # default`},
		{"multi_job = true\nsend_env_default = \"forced\"\n", `providers.fake.send_env = "forwarded"  # default`},
		{"send_env = \"none\"\n", `providers.fake.send_env = "none"  # host`},
		{"send_env = \"forwarded\"\nsend_env_default = \"forced\"\n", `providers.fake.send_env = "forwarded"  # host`},
		{"send_env = \"forced\"\n", `providers.fake.send_env = "forced"  # host`},
	} {
		h := harness.New(t)
		seRepo(t, h, "", []string{"fake"}, map[string]string{"fake": c.extra})
		if got := levelOf(t, h, "fake"); got != c.want {
			t.Errorf("%q: %q, want %q", c.extra, got, c.want)
		}
	}
	// RunPod's default is forced (today's behaviour), and a host may narrow it
	h := harness.New(t)
	rpRepo(t, h, "")
	if got := levelOf(t, h, "runpod"); got != `providers.runpod.send_env = "forced"  # default` {
		t.Errorf("runpod default: %q", got)
	}
	h.HostConfig(strings.Replace(readCfg(t, h), "[providers.runpod]\n", "[providers.runpod]\nsend_env = \"none\"\n", 1))
	if got := levelOf(t, h, "runpod"); got != `providers.runpod.send_env = "none"  # host` {
		t.Errorf("runpod narrowed: %q", got)
	}
	// the 78 cases
	for _, c := range []struct{ extra, msg string }{
		{"send_env = \"all\"\n", `providers.fake.send_env: must be "none", "forwarded" or "forced": "all"`},
		{"send_env = 1\n", `providers.fake.send_env: must be "none", "forwarded" or "forced": 1`},
		{"multi_job = true\nsend_env = \"forced\"\n", `providers.fake.send_env: "forced" is not allowed: fake reuses resources across jobs`},
	} {
		h := harness.New(t)
		seRepo(t, h, "", []string{"fake"}, map[string]string{"fake": c.extra})
		if r := h.Run("config", "check"); r.Code != 78 || !strings.Contains(r.Stderr, c.msg) {
			t.Errorf("%q: %+v", c.extra, r)
		}
	}
	h4 := harness.New(t)
	e4 := seRepo(t, h4, "", []string{"fake"}, nil)
	h4.Write(filepath.Join(e4.wt, ".bitc.toml"), "version = 1\n[providers.fake]\nsend_env = \"none\"\n", 0o644)
	if r := h4.Run("config", "check"); r.Code != 78 || !strings.Contains(r.Stderr, "providers.fake.send_env: host-only key") {
		t.Errorf("repo file: %+v", r)
	}
	// plugin part (P.1): a plugin defaults to forwarded and may be set to forced, unless its hello
	// declares multi_job: then forced is 78 at config check and doctor --remote (checked after hello)
	for _, c := range []struct {
		multi       bool
		extra, want string
	}{
		{false, "", `providers.stub.send_env = "forwarded"  # default`},
		{true, "", `providers.stub.send_env = "forwarded"  # default`},
		{false, "send_env = \"forced\"\n", `providers.stub.send_env = "forced"  # host`},
	} {
		h := harness.New(t)
		if c.multi {
			h.Env = append(h.Env, "STUB_MULTI_JOB=1")
		}
		plRepo(t, h, "", "resource", c.extra)
		if got := levelOf(t, h, "stub"); got != c.want {
			t.Errorf("plugin multi_job %v %q: %q, want %q", c.multi, c.extra, got, c.want)
		}
		if r := h.Run("config", "check"); r.Code != 0 {
			t.Errorf("plugin multi_job %v %q: config check %+v", c.multi, c.extra, r)
		}
	}
	const mjMsg = `providers.stub.send_env: "forced" is not allowed: stub reuses resources across jobs`
	h5 := harness.New(t)
	h5.Env = append(h5.Env, "STUB_MULTI_JOB=1")
	plRepo(t, h5, "", "resource", "send_env = \"forced\"\n")
	if r := h5.Run("config", "check"); r.Code != 78 || !strings.Contains(r.Stderr, ": "+mjMsg) {
		t.Errorf("forced on a multi_job plugin, config check: %+v", r)
	}
	if r := h5.Run("doctor", "--remote"); r.Code != 78 || !strings.Contains(r.Stderr, ": "+mjMsg) || !strings.Contains(r.Stdout, "fail plugin-stub: "+mjMsg) {
		t.Errorf("forced on a multi_job plugin, doctor --remote: %+v", r)
	}
}

// SE2: send_env = "none": a job with a forwarded name is ineligible with the exact note and
// touches nothing; a job with none set runs, and its [remote] env values still cross.
func TestSE2_NoneSendsNoCallerEnv(t *testing.T) {
	h := harness.New(t)
	e := seRepo(t, h, "", []string{"fake"}, map[string]string{"fake": "send_env = \"none\"\n"})
	r := h.RunWith(sePlain, seShow...)
	want := `bitc: fake: running heavy locally: forwarded env not sent to fake (providers.fake.send_env = "none"): CI_PLAIN` + "\n"
	if r.Code != 0 || !strings.Contains(r.Stderr, want) {
		t.Fatalf("want %q in %+v", want, r)
	}
	e.untouched(t, "fake")
	r = h.RunWith(seBoth, seShow...)
	if want := `forwarded env not sent to fake (providers.fake.send_env = "none"): CI_PLAIN, DEPLOY_TOKEN` + "\n"; !strings.Contains(r.Stderr, want) {
		t.Fatalf("both names: want %q in %+v", want, r)
	}
	e.untouched(t, "fake")
	// nothing forwarded: it runs on the fake with its [remote] env
	r = h.Run(seShow...)
	if r.Code != 0 || r.Stdout != "cfgval||\n" || strings.Contains(r.Stderr, "running heavy locally") || !strings.Contains(e.calls("fake"), `"attach":true`) {
		t.Fatalf("no forwarded names: %+v calls %q", r, e.calls("fake"))
	}
}

// SE3: a forced job on a "forwarded" provider is ineligible there and runs on the next one.
func TestSE3_ForcedJobMovesToAForcedProvider(t *testing.T) {
	h := harness.New(t)
	e := seRepo(t, h, "", []string{"fake", "fake2"}, map[string]string{"fake": "send_env = \"forwarded\"\n", "fake2": "send_env = \"forced\"\n"})
	r := h.RunWith(seBoth, seShow...)
	want := `bitc: fake: heavy: not eligible for remote: forced env not sent to fake (providers.fake.send_env = "forwarded"): DEPLOY_TOKEN` + "\n"
	if r.Code != 0 || !strings.Contains(r.Stderr, want) || r.Stdout != "cfgval|"+sePlainVal+"|tok\n" {
		t.Fatalf("want %q in %+v", want, r)
	}
	e.untouched(t, "fake")
	if !strings.Contains(e.calls("fake2"), `"attach":true`) {
		t.Fatalf("fake2 calls: %q", e.calls("fake2"))
	}
	// only plain names: the first provider runs it
	r = h.RunWith(sePlain, seShow...)
	if r.Code != 0 || r.Stdout != "cfgval|"+sePlainVal+"|\n" || strings.Contains(r.Stderr, "not eligible") || !strings.Contains(e.calls("fake"), `"attach":true`) {
		t.Fatalf("plain only: %+v", r)
	}
	// on_failure = "stop": the call ends 69 with the not-eligible line
	h2 := harness.New(t)
	e2 := seRepo(t, h2, "on_failure = \"stop\"\n", []string{"fake", "fake2"}, map[string]string{"fake": "send_env = \"forwarded\"\n", "fake2": "send_env = \"forced\"\n"})
	r = h2.RunWith(seBoth, seShow...)
	if r.Code != 69 || !strings.Contains(r.Stderr, want) {
		t.Fatalf("stop: %+v", r)
	}
	e2.untouched(t, "fake")
	e2.untouched(t, "fake2")
}

// SE4: "forwarded" never strips: a job runs with all its forwarded names or not on that provider.
func TestSE4_ForwardedNeverStrips(t *testing.T) {
	h := harness.New(t)
	e := seRepo(t, h, "", []string{"fake"}, nil) // fake defaults to forwarded
	r := h.RunWith(seBoth, seShow...)
	if r.Code != 0 || !strings.Contains(r.Stderr, `running heavy locally: forced env not sent to fake (providers.fake.send_env = "forwarded"): DEPLOY_TOKEN`) {
		t.Fatalf("%+v", r)
	}
	// it ran locally with its own env, not on the fake with the plain name alone
	e.untouched(t, "fake")
	e.noCanary(t, "fake")
	r = h.RunWith(sePlain, seShow...)
	if r.Code != 0 || r.Stdout != "cfgval|"+sePlainVal+"|\n" || !strings.Contains(e.calls("fake"), sePlainVal) {
		t.Fatalf("plain: %+v calls %q", r, e.calls("fake"))
	}
	e.noCanary(t, "fake")
}

// SE5: a multi-job resource never gets a forced name, whatever it declares.
func TestSE5_MultiJobNeverGetsForcedNames(t *testing.T) {
	h := harness.New(t)
	e := seRepo(t, h, "", []string{"fake"}, map[string]string{"fake": "multi_job = true\nsend_env_default = \"forced\"\n"})
	if got := levelOf(t, h, "fake"); got != `providers.fake.send_env = "forwarded"  # default` {
		t.Fatalf("multi_job default: %q", got)
	}
	r := h.RunWith(seBoth, seShow...)
	if r.Code != 0 || !strings.Contains(r.Stderr, `running heavy locally: forced env not sent to fake (providers.fake.send_env = "forwarded"): DEPLOY_TOKEN`) {
		t.Fatalf("%+v", r)
	}
	e.untouched(t, "fake")
	e.noCanary(t, "fake")
	r = h.RunWith(sePlain, seShow...)
	if r.Code != 0 || r.Stdout != "cfgval|"+sePlainVal+"|\n" || !strings.Contains(e.calls("fake"), `"attach":true`) {
		t.Fatalf("plain job: %+v", r)
	}
	e.noCanary(t, "fake")
}
