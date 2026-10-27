package provider

import (
	"context"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Exploded-views/bitc/conformance/harness"
	"github.com/Exploded-views/bitc/internal/config"
	"github.com/Exploded-views/bitc/internal/provider"
	"github.com/Exploded-views/bitc/internal/provider/plugin"
	"github.com/Exploded-views/bitc/internal/state"
)

// pluginTarget runs the provider suite against any provider plugin and the plugin author's
// loopback fake of their API (providers delta A.10.2). The reference plugin and its fake are
// the default; -plugin, -plugin-sha256 and -plugin-fake name another pair. The plugin is pinned by
// path and sha256 like a real host does, and gets the fake's URL as its `api_base` key.
//
// The fake contract is POST /__bitc/behave {"behaviour": ...} for the Behave names below,
// GET /__bitc/calls (every API call in order, the attach call marked), GET /__bitc/live and
// GET /__bitc/torn; POST /__bitc/spend and /__bitc/seed are optional and enable the cap and
// sweep scenarios.
type pluginTarget struct {
	h      *harness.H
	api    *harness.FakeAPI
	path   string // the pinned plugin
	sum    string
	apiURL string
}

const pluginType = "fakex"

func (p *pluginTarget) Name() string { return pluginType }

func (p *pluginTarget) Configure(t *testing.T, h *harness.H) {
	bin, fake := pluginBins(t)
	p.h = h
	p.api = h.StartFakeAPI(fake)
	p.apiURL = p.api.URL
	p.path, p.sum = h.PluginFrom(pluginType, bin)
	if *pluginSumFlag != "" && *pluginSumFlag != p.sum {
		t.Fatalf("-plugin-sha256 %s is not the sha256 of %s (%s)", *pluginSumFlag, bin, p.sum)
	}
	h.Seam("fixed_secrets = true")
	h.Seam("agent_url = \"http://127.0.0.1:9/bitc\"")
	h.HostConfig("[remote_limits]\nworktrees = [\"**/app\"]\nforward_env_allow = [\"CANARY_VALUE\"]\n" +
		"[pools.heavy]\nproviders = [\"" + pluginType + "\", \"local\"]\nremote_limits = { size = { " + pluginType + " = \"cpu8\" }, timeout = \"60m\" }\nwait = \"5s\"\n" +
		"[providers." + pluginType + "]\nplugin = { path = " + harness.Q(p.path) + ", sha256 = " + harness.Q(p.sum) + " }\n" +
		"api_key = \"sk-test-0123456789\"\ndaily_usd = 25\nmax_live = 6\nstart_timeout = \"2m\"\nfallback_until = \"attach\"\n" +
		"sizes = { cpu8 = { usd_per_hour = 0.24 } }\napi_base = " + harness.Q(p.apiURL) + "\n")
	h.Cwd = h.Repo("app", map[string]string{".bitc.toml": repoFile("")})
}

// repoFile is the work tree's .bitc.toml: [remote] first (forward_env and any extra lines), the
// pool's remote table last, so a scenario can append its own keys to the pool table.
func repoFile(remoteExtra string) string {
	return "version = 1\n[remote]\nforward_env = [\"CANARY_VALUE\"]\n" + remoteExtra +
		"[pools.heavy.remote]\nallow = [[\"sh\", \"*\"], [\"echo\", \"*\"]]\n"
}

// commitRepoFile replaces the work tree's .bitc.toml and commits it (a remote job runs a commit).
func commitRepoFile(t *testing.T, h *harness.H, body string) {
	t.Helper()
	h.Write(filepath.Join(h.Cwd, ".bitc.toml"), body, 0o644)
	for _, a := range [][]string{{"add", "-A"}, {"-c", "user.name=t", "-c", "user.email=t@example.invalid", "commit", "-q", "-m", "repo file"}} {
		c := exec.Command("git", a...)
		c.Dir, c.Env = h.Cwd, h.Env
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", a, err, out)
		}
	}
}

func (p *pluginTarget) Behave(t *testing.T, b string) {
	switch b {
	case "ok", "create-fails", "lost-after-begin", "restart-after-begin", "drop-log-tail", "forge-result", "preattach-fails", "paginate-2":
		p.api.Behave(b)
	case "setup-fails-after-create": // a tool that cannot be downloaded: an infra setup failure after the attach
		p.api.Behave("ok")
		commitRepoFile(t, p.h, repoFile("tools = [{ name = \"t\", url = \"https://127.0.0.1:9/t.tar.gz\", sha256 = \""+strings.Repeat("0", 64)+"\" }]\n"))
	case "setup-fails-repo-after-create": // the repo's own setup step fails by itself
		p.api.Behave("ok")
		commitRepoFile(t, p.h, repoFile("setup = [{ name = \"deps\", run_as = \"job\", argv = [\"sh\", \"-c\", \"echo 'npm ERR! missing script: ci' >&2; exit 1\"] }]\n"))
	default:
		t.Fatalf("the reference fake cannot produce %q", b)
	}
}

// Calls are the API calls as "METHOD /path", the control contract aside.
func (p *pluginTarget) Calls() []string {
	var out []string
	for _, c := range p.api.Calls() {
		out = append(out, c.Method+" "+c.Path)
	}
	return out
}

func (p *pluginTarget) TornDown() []string { return p.api.Torn() }

func (p *pluginTarget) Supports(string) bool { return true }

func (p *pluginTarget) SetSpend(_ *testing.T, _ *harness.H, usd float64) { p.api.SetSpend(usd) }

func (p *pluginTarget) AddExpired(t *testing.T, h *harness.H) string {
	secret, err := os.ReadFile(filepath.Join(h.Root, "account", "install-secret"))
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * time.Hour)
	name := provider.ResourceNameMax("bitc", state.Install16(secret), "heavy", "cpu8", "expired-job", old, 63)
	return p.api.Seed(name, provider.OwnerTag(secret, name), old.Add(-time.Hour), old)
}

// DeclaresAttach: the reference plugin declares attach, and the host section sets it.
func (p *pluginTarget) DeclaresAttach() bool { return true }

func (p *pluginTarget) APICalls(t *testing.T) []APICall {
	var out []APICall
	for _, c := range p.api.Calls() {
		if strings.HasPrefix(c.Path, "/__bitc/") {
			continue
		}
		data := c.Method + " " + c.Path + "?" + c.Query + "\n" + c.Body
		for k, v := range c.Headers {
			data += "\n" + k + ": " + v
		}
		out = append(out, APICall{Op: c.Method + " " + c.Path, Attach: c.Attach, Data: data})
	}
	return out
}

// Canaries: B, K and M under the [test] seam fixed_secrets (32 bytes of 0x0b, 0x0c and 0x0d).
func (p *pluginTarget) Canaries(t *testing.T) []string {
	var out []string
	for _, b := range []byte{0x0b, 0x0c, 0x0d} {
		out = append(out, strings.Repeat(hex.EncodeToString([]byte{b}), 32))
	}
	return out
}

func (p *pluginTarget) LiveNames(t *testing.T) []string { return p.api.Live() }

// ListNames asks the adapter (the plugin under the host's pin and spawn rules) for the resources
// with the provider's prefix.
func (p *pluginTarget) ListNames(t *testing.T, h *harness.H) []string {
	core := &config.Provider{Type: pluginType, NamePrefix: "bitc", APIKey: "sk-test-0123456789", DailyUSD: 25_000_000, MaxLive: 6,
		LaunchWait: time.Minute, StartTimeout: 2 * time.Minute, Breaker: config.Breaker{Failures: 3, Window: time.Hour},
		FallbackUntil: "attach", FallbackCap: "attach", SendEnv: "forwarded",
		Plugin:   &config.PluginPin{Path: p.path, SHA256: p.sum, SecretFiles: map[string]string{}},
		Sizes:    map[string]config.Micros{"cpu8": 240_000},
		SizesRaw: map[string]any{"cpu8": map[string]any{"usd_per_hour": 0.24}},
		Raw:      map[string]any{"api_base": p.apiURL}}
	eff := &config.Effective{Host: config.Host{StateDir: h.State}, Test: &config.Test{AccountingDir: filepath.Join(h.Root, "account"), TimeScale: 1}}
	pr, err := plugin.New(core, eff)
	if err != nil {
		t.Fatal(err)
	}
	defer pr.Close()
	if err := pr.Start(context.Background(), provider.Runtime{}); err != nil {
		t.Fatal(err)
	}
	rs, err := pr.List(context.Background(), []string{"bitc-"})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, r := range rs {
		names = append(names, r.Name)
	}
	return names
}

// pluginBins are the plugin and fake binaries under test: -plugin / -plugin-fake, else the
// reference pair TestMain built.
func pluginBins(t *testing.T) (bin, fake string) {
	t.Helper()
	bin, fake = harness.RefPlugin, harness.RefFakeAPI
	if *pluginFlag != "" {
		bin = *pluginFlag
	}
	if *pluginFakeFlag != "" {
		fake = *pluginFakeFlag
	}
	return bin, fake
}

// TestPX1_ProviderSuiteOnReferencePlugin runs every provider scenario against the reference
// plugin and its fake API: the scenarios the compiled-in fake passes, and the RunPod-only ones.
func TestPX1_ProviderSuiteOnReferencePlugin(t *testing.T) { RunAll(t, &pluginTarget{}, harness.New) }
