package plugin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Exploded-views/bitc/conformance/harness"
)

// opts shapes one sandbox for the adapter rows: pool `pool` goes to the reference plugin "fakex"
// (in the fault modes of `fault`), then local.
type opts struct {
	fault       string            // the plugin's fault list (the `fault` key)
	pluginExtra string            // more keys inside the plugin = { ... } table, each starting with ", "
	extra       string            // more lines of [providers.fakex]
	limits      string            // more lines of [remote_limits]
	repoRemote  string            // more lines of the repo's [remote] table
	sizes       string            // the sizes table (default: cpu8 at $0.24 an hour)
	daily       string            // daily_usd (default 25)
	maxLive     string            // max_live (default 6)
	pool        string            // pool name (default heavy)
	size        string            // size key (default cpu8)
	apiBase     string            // api_base (default: the fake API)
	helloFault  string            // faults the plugin shows before configure (env FAKEX_FAULT, passed through env_pass)
	declare     string            // the secrets the plugin's hello declares (env FAKEX_DECLARE, passed through env_pass)
	secrets     map[string]string // plugin.secret_files: name -> value, each written to a 0600 file
}

// env is one running sandbox: the bitc harness, the fake API and the pinned reference plugin.
type env struct {
	t    *testing.T
	h    *harness.H
	api  *harness.FakeAPI
	path string // the pinned plugin
	sum  string
	wt   string
	pool string
	own  string // BITC_DIR/plugins/fakex
	acct string // ACCOUNT/remote/fakex
}

func newEnv(t *testing.T, o opts) *env {
	t.Helper()
	h := harness.New(t)
	e := &env{t: t, h: h, pool: o.pool, own: filepath.Join(h.State, "plugins", "fakex"), acct: filepath.Join(h.Root, "account", "remote", "fakex")}
	if e.pool == "" {
		e.pool = "heavy"
	}
	def := func(v, d string) string {
		if v == "" {
			return d
		}
		return v
	}
	e.api = h.StartFakeAPI(harness.RefFakeAPI)
	e.path, e.sum = h.PluginFrom("fakex", harness.RefPlugin)
	h.Seam("fixed_secrets = true")
	h.Seam("agent_url = \"http://127.0.0.1:9/bitc\"")
	size := def(o.size, "cpu8")
	var pass []string
	if o.declare != "" {
		pass = append(pass, harness.Q("FAKEX_DECLARE"))
		h.Env = append(h.Env, "FAKEX_DECLARE="+o.declare)
	}
	if o.helloFault != "" {
		pass = append(pass, harness.Q("FAKEX_FAULT"))
		h.Env = append(h.Env, "FAKEX_FAULT="+o.helloFault)
	}
	if len(pass) > 0 {
		o.pluginExtra += ", env_pass = [" + strings.Join(pass, ", ") + "]"
	}
	if len(o.secrets) > 0 {
		var parts []string
		for n, v := range o.secrets {
			f := filepath.Join(h.Root, n+".secret")
			h.Write(f, v, 0o600)
			parts = append(parts, n+" = "+harness.Q(f))
		}
		o.pluginExtra += ", secret_files = { " + strings.Join(parts, ", ") + " }"
	}
	cfg := "[remote_limits]\nworktrees = [\"**/app\"]\n" + o.limits +
		"[pools." + e.pool + "]\nproviders = [\"fakex\", \"local\"]\nremote_limits = { size = { fakex = " + harness.Q(size) + " }, timeout = \"60m\" }\nwait = \"5s\"\n" +
		"[providers.fakex]\nplugin = { path = " + harness.Q(e.path) + ", sha256 = " + harness.Q(e.sum) + o.pluginExtra + " }\n" +
		"api_key = \"sk-test-0123456789\"\ndaily_usd = " + def(o.daily, "25") + "\nmax_live = " + def(o.maxLive, "6") + "\nstart_timeout = \"2m\"\n" +
		"sizes = " + def(o.sizes, "{ cpu8 = { usd_per_hour = 0.24 } }") + "\napi_base = " + harness.Q(def(o.apiBase, e.api.URL)) + "\n"
	if o.fault != "" {
		cfg += "fault = " + harness.Q(o.fault) + "\n"
	}
	h.HostConfig(cfg + o.extra)
	e.wt = h.Repo("app", map[string]string{".bitc.toml": "version = 1\n[remote]\n" + o.repoRemote +
		"[pools." + e.pool + ".remote]\nallow = [[\"sh\", \"*\"], [\"echo\", \"*\"]]\n"})
	h.Cwd = e.wt
	return e
}

// job runs `echo hi` under the pool; if it ran on this host it also leaves ran-locally.
func (e *env) job() harness.Result {
	e.t.Helper()
	return e.h.Run(e.jobArgs("echo hi; echo ran > ran-locally")...)
}

func (e *env) jobArgs(script string) []string {
	return []string{"run", e.pool, "--", "sh", "-c", script}
}

func (e *env) ranLocally() bool {
	_, err := os.Stat(filepath.Join(e.wt, "ran-locally"))
	return err == nil
}

// creates counts the create calls the fake API saw.
func (e *env) creates() int {
	n := 0
	for _, c := range e.api.Calls() {
		if c.Method == "POST" && c.Path == "/v1/instances" {
			n++
		}
	}
	return n
}

// failures counts the breaker failures written for the plugin type.
func (e *env) failures() int {
	n := 0
	for _, l := range lines(filepath.Join(e.acct, "failures.jsonl")) {
		if strings.Contains(l, `"kind":"failure"`) {
			n++
		}
	}
	return n
}

func lines(path string) []string {
	b, _ := os.ReadFile(path)
	var out []string
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}

func slurp(path string) string { b, _ := os.ReadFile(path); return string(b) }
