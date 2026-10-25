package conformance

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Exploded-views/bitc/conformance/harness"
)

// nodeRepo copies examples/node-monorepo into a git work tree, installs its host file (memory
// floor 0, so CI hosts with little memory pass) and makes the tree the cwd. hostExtra (top-level
// keys such as vars) goes before the example's tables; repoEdit (if non-nil) rewrites the repo file.
func nodeRepo(t *testing.T, h *harness.H, hostExtra string, repoEdit func(string) string) string {
	t.Helper()
	host := string(must(os.ReadFile("../examples/node-monorepo/host.bitc.toml")))
	host = strings.Replace(host, "version = 1\n", "", 1)
	host = strings.Replace(host, `memory_floor = "3GiB"`, `memory_floor = "0GiB"`, 1)
	h.HostConfig(hostExtra + "\n" + host)
	repo := string(must(os.ReadFile("../examples/node-monorepo/.bitc.toml")))
	if repoEdit != nil {
		repo = repoEdit(repo)
	}
	files := map[string]string{".bitc.toml": repo}
	for _, s := range []string{"stack-env.sh", "check-db.sh"} {
		files["scripts/"+s] = string(must(os.ReadFile("../examples/node-monorepo/scripts/" + s)))
	}
	wt := h.Repo("app", files)
	for _, s := range []string{"stack-env.sh", "check-db.sh"} {
		if err := os.Chmod(filepath.Join(wt, "scripts", s), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	h.Cwd = wt
	return wt
}

// envIn runs `env` in pool and returns it as a map.
func envIn(t *testing.T, h *harness.H, extraEnv []string, pool string) map[string]string {
	t.Helper()
	r := h.RunWith(extraEnv, "run", pool, "--", "env")
	if r.Code != 0 {
		t.Fatalf("%s: %+v", pool, r)
	}
	m := map[string]string{}
	for _, l := range strings.Split(r.Stdout, "\n") {
		if k, v, ok := strings.Cut(l, "="); ok {
			m[k] = v
		}
	}
	return m
}
func readFile(p string) ([]byte, error) { return os.ReadFile(p) }
