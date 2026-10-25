package provider

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Exploded-views/bitc/conformance/harness"
	"github.com/Exploded-views/bitc/internal/config"
	"github.com/Exploded-views/bitc/internal/provider/runpod"
	"github.com/Exploded-views/bitc/internal/provider/runpod/runpodfake"
)

// runpodTarget drives the RunPod provider against its fake API, with this bitc as each pod's agent.
type runpodTarget struct {
	f *runpodfake.Fake
	h *harness.H
}

func (r *runpodTarget) Name() string { return "runpod" }

func (r *runpodTarget) Configure(t *testing.T, h *harness.H) {
	r.f, r.h = runpodfake.New(t, runpodfake.Options{Bin: harness.Bin}), h
	h.Seam("time_scale = 0.05")
	h.Seam("http_results = true")
	h.Seam("agent_url = \"http://127.0.0.1:9/bitc\"")
	h.HostConfig("[remote_limits]\nworktrees = [\"**/app\"]\n" +
		"[pools.heavy]\nproviders = [\"runpod\", \"local\"]\nremote_limits = { size = { runpod = \"cpu8\" }, timeout = \"60m\" }\nwait = \"5s\"\n" +
		"[providers.runpod]\napi_key = \"sk-test-0123456789\"\ndaily_usd = 25\nmax_live = 6\nstart_timeout = \"2m\"\nimage = \"debian\"\n" +
		"api_base = " + harness.Q(r.f.URL()) + "\nproxy_domain = " + harness.Q(r.f.ProxyAddr()) + "\n" +
		"sizes = { cpu8 = { cpu_type = \"cpu3c\", vcpu = 8, usd_per_hour = 0.24 } }\n")
	h.Cwd = h.Repo("app", map[string]string{".bitc.toml": "version = 1\n[pools.heavy.remote]\nallow = [[\"sh\", \"*\"], [\"echo\", \"*\"]]\n"})
}

func (r *runpodTarget) Behave(t *testing.T, b string) {
	switch b {
	case "ok":
	case "create-fails":
		r.f.Behave("create-500-twice") // the create and its one retry both fail
	case "setup-fails-after-create": // a tool that cannot be downloaded: an infra setup failure after the create
		commitRepoLine(t, r.h, "[remote]\ntools = [{ name = \"t\", url = \"https://127.0.0.1:9/t.tar.gz\", sha256 = \""+strings.Repeat("0", 64)+"\" }]")
	case "setup-fails-repo-after-create": // the repo's own setup step fails by itself after the create
		commitRepoLine(t, r.h, "[remote]\nsetup = [{ name = \"deps\", run_as = \"job\", argv = [\"sh\", \"-c\", \"echo 'npm ERR! missing script: ci' >&2; exit 1\"] }]")
	case "lost-after-begin", "restart-after-begin", "drop-log-tail", "forge-result":
		r.f.Behave(b)
	case "paginate-2": // the fake's list is one page (limit=1000): nothing to switch
	default:
		t.Fatalf("the RunPod fake cannot produce %q", b)
	}
}

func (r *runpodTarget) Calls() []string { return r.f.Calls() }

func (r *runpodTarget) TornDown() []string {
	var ids []string
	for _, c := range r.f.Calls() {
		if id, ok := strings.CutPrefix(c, "DELETE /v2/pods/"); ok {
			ids = append(ids, id)
		}
	}
	return ids
}

func (r *runpodTarget) Supports(string) bool { return true }

func (r *runpodTarget) SetSpend(_ *testing.T, _ *harness.H, usd float64) { r.f.SetSpend(usd) }

func TestRP1_ProviderSuiteOnRunPodFake(t *testing.T) { RunAll(t, &runpodTarget{}, harness.New) }

func (r *runpodTarget) AddExpired(t *testing.T, h *harness.H) string {
	secret, err := os.ReadFile(filepath.Join(h.Root, "account", "install-secret"))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(secret)
	old := time.Now().Add(-2 * time.Hour)
	name := runpod.EncodeName("bitc", hex.EncodeToString(sum[:])[:16], "heavy", "cpu8", "0000000a", old)
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(name))
	r.f.AddPod("expired1", name, old.Add(-time.Hour), map[string]string{"BITC_OWNER": hex.EncodeToString(m.Sum(nil))})
	return "expired1"
}

func (r *runpodTarget) ListNames(t *testing.T, _ *harness.H) []string {
	core := &config.Provider{Type: "runpod", NamePrefix: "bitc", APIKey: "sk-test-0123456789", Raw: map[string]any{
		"image": "debian", "api_base": r.f.URL(), "proxy_domain": r.f.ProxyAddr(),
		"sizes": map[string]any{"cpu8": map[string]any{"cpu_type": "cpu3c", "vcpu": int64(8), "usd_per_hour": 0.24}}}}
	p, err := runpod.New(core, &config.Effective{Test: &config.Test{HTTPResults: true}})
	if err != nil {
		t.Fatal(err)
	}
	rs, err := p.List(context.Background(), []string{"bitc-"})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, x := range rs {
		names = append(names, x.Name)
	}
	return names
}

func (r *runpodTarget) LiveNames(*testing.T) []string { return r.f.Live() }
