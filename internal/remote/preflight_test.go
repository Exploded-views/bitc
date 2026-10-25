package remote

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Exploded-views/bitc/internal/config"
)

func gitTree(t *testing.T, files map[string]string) string {
	t.Helper()
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	for p, b := range files {
		_ = os.MkdirAll(filepath.Dir(filepath.Join(dir, p)), 0o755)
		_ = os.WriteFile(filepath.Join(dir, p), []byte(b), 0o644)
	}
	for _, args := range [][]string{{"init", "-q"}, {"add", "-A"}, {"-c", "user.name=t", "-c", "user.email=t@x", "commit", "-q", "-m", "i"}} {
		c := exec.Command("git", args...)
		c.Dir = dir
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("%v %s", err, out)
		}
	}
	return dir
}

func TestAllowMatching(t *testing.T) {
	allow := [][]string{{"npm", "run", "lint", "--"}, {"npm", "run", "coverage"}, {"node", "e2e/run.mjs", "*"}}
	for argv, ok := range map[string]bool{
		"npm run lint": true, "npm run lint -- --quiet": true, "npm run lint x": false,
		"npm run coverage": true, "npm run coverage x": false, "/usr/bin/node e2e/run.mjs a b": true,
		"git commit -m x": false,
	} {
		if got := allowed(allow, strings.Fields(argv)); got != ok {
			t.Errorf("%q: %v", argv, got)
		}
	}
}

func TestArgumentPaths(t *testing.T) {
	wt := gitTree(t, map[string]string{"src/a.ts": "", ".gitignore": "dist/\n"})
	_ = os.MkdirAll(filepath.Join(wt, "dist"), 0o755)
	cases := map[string]string{
		"/usr/share/doc": "a host path outside the work tree",
		"dist/x.js":      "which git ignores",
		"src/a.ts":       "",
		"--flag=/etc":    "",
		"../outside":     "a host path outside the work tree",
	}
	for arg, frag := range cases {
		err := argumentPaths(wt, wt, []string{"cmd", arg})
		if (frag == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), frag)) {
			t.Errorf("%q: %v", arg, err)
		}
	}
}

func TestRemoteEnvForwardsOnlyTheIntersection(t *testing.T) {
	eff := &config.Effective{Vars: config.Vars{}, RemoteLimits: config.RemoteLimits{ForwardEnvAllow: []string{"CI", "TZ"}},
		Remote: &config.RepoRemote{ForwardEnv: []string{"CI", "HOME"}, Env: map[string]config.Template{"DB": {Raw: "pg://localhost:5432/{pool}"}}}}
	pool := &config.Pool{Name: "heavy", Env: map[string]config.Template{"A": {Raw: "1"}},
		Remote: &config.PoolRemote{Env: map[string]config.Template{"A": {Raw: "2"}}, EnvUnset: []string{"B"}}}
	env, err := remoteEnv(eff, pool, "j1", "/wt", []string{"CI=1", "HOME=/h", "TZ=UTC", "SECRET=x"})
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(env, ",")
	if got != "CI=1,A=2,DB=pg://localhost:5432/heavy" {
		t.Fatal(got)
	}
}

func TestRemoteEnvKeyUnsetThenSetAgainAppearsOnce(t *testing.T) {
	eff := &config.Effective{Vars: config.Vars{}, RemoteLimits: config.RemoteLimits{ForwardEnvAllow: []string{"CI"}},
		Remote: &config.RepoRemote{ForwardEnv: []string{"CI"}, EnvUnset: []string{"A", "CI"}}}
	pool := &config.Pool{Name: "p", Env: map[string]config.Template{"A": {Raw: "1"}},
		Remote: &config.PoolRemote{Env: map[string]config.Template{"A": {Raw: "2"}, "CI": {Raw: "0"}}}}
	env, err := remoteEnv(eff, pool, "j1", "/wt", []string{"CI=1"})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(env, ","); got != "CI=0,A=2" && got != "A=2,CI=0" {
		t.Fatal(got)
	}
	if len(env) != 2 {
		t.Fatalf("keys repeated: %v", env)
	}
}
