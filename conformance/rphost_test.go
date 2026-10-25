package conformance

import (
	"strings"
	"testing"

	"github.com/Exploded-views/bitc/conformance/harness"
	"github.com/Exploded-views/bitc/internal/provider/runpod/runpodfake"
)

// rpRepo: heavy goes to RunPod (the fake), the work tree pushes to a local bare origin, and the
// agent is this bitc binary run unprivileged by the fake. Returns the fake and the work tree.
func rpRepo(t *testing.T, h *harness.H, repoExtra string) (*runpodfake.Fake, string) {
	t.Helper()
	f := runpodfake.New(t, runpodfake.Options{Bin: harness.Bin})
	h.Seam("time_scale = 0.05")
	h.Seam("http_results = true")
	h.Seam("agent_url = \"http://127.0.0.1:9/bitc\"")
	h.HostConfig("[remote_limits]\nworktrees = [\"**/app\"]\nforward_env_allow = [\"CI\"]\n" +
		"[pools.heavy]\nproviders = [\"runpod\", \"local\"]\nremote_limits = { size = { runpod = \"cpu8\" }, timeout = \"60m\" }\nwait = \"5s\"\n" +
		"[providers.runpod]\napi_key = \"sk-test-0123456789\"\ndaily_usd = 25\nstart_timeout = \"2m\"\nimage = \"debian\"\n" +
		"api_base = " + harness.Q(f.URL()) + "\nproxy_domain = " + harness.Q(f.ProxyAddr()) + "\n" +
		"sizes = { cpu8 = { cpu_type = \"cpu3c\", vcpu = 8, usd_per_hour = 0.24 } }\n")
	wt := h.Repo("app", map[string]string{".bitc.toml": "version = 1\n[pools.heavy.remote]\nallow = [[\"sh\", \"*\"], [\"echo\", \"*\"]]\n" + repoExtra,
		"sub/keep": ""})
	h.Cwd = wt // harness.Repo gave it a bare origin, h.Origin("app")
	return f, wt
}

func calls(f *runpodfake.Fake, prefix string) int {
	n := 0
	for _, c := range f.Calls() {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}
