package conformance

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Exploded-views/bitc/conformance/harness"
)

func explain(h *harness.H, env []string, key string) string {
	r := h.RunWith(env, "config", "explain", key)
	if r.Code != 0 {
		h.T.Fatalf("explain %s: %d %s", key, r.Code, r.Stderr)
	}
	return r.Stdout
}

func TestP7_HostWinsOverRepoKeyByKey(t *testing.T) {
	h := harness.New(t)
	h.HostConfig("[pools.lint]\nenv = { X = \"host\" }\n")
	h.Cwd = h.Repo("wt", map[string]string{".bitc.toml": "version = 1\n[pools.lint]\nenv = { X = \"repo\", Y = \"repo\" }\n"})
	out := explain(h, nil, "pools.lint.env")
	if !strings.Contains(out, `pools.lint.env.X = "host"`) || !strings.Contains(out, `pools.lint.env.Y = "repo"`) {
		t.Fatal(out)
	}
}

func TestP8_EnvCannotChangeCapacities(t *testing.T) {
	h := harness.New(t)
	h.HostConfig("groups = { g = { units = 1 } }\n[pools.lint]\nslots = 1\n")
	out := explain(h, []string{"BITC_SLOTS=9", "BITC_GROUPS=x", "BITC_MEMORY_FLOOR=0GiB", "SLOTS_lint=9"}, "")
	if !strings.Contains(out, "pools.lint.slots = 1") || !strings.Contains(out, "groups.g.units = 1") || !strings.Contains(out, "host.memory_floor = 6GiB") {
		t.Fatal(out)
	}
}

func TestP9_WholeFileValidatedAtLoad(t *testing.T) {
	h := harness.New(t)
	h.HostConfig("[pools.lint]\nslots = 1\n[pools.other]\nwait = \"5 minutes\"\n")
	r := h.Run("run", "lint", "--", "true")
	if r.Code != 78 || !strings.Contains(r.Stderr, "pools.other.wait") {
		t.Fatalf("%d %q", r.Code, r.Stderr)
	}
}

func TestN1_StrictIntegersAndDurations(t *testing.T) {
	h := harness.New(t)
	for _, body := range []string{"[pools.lint]\nwait = \"010s\"\n", "[host]\nmemory_floor = \"8192\"\n", "[host]\npoll = \"-5s\"\n", "[host]\npoll = \"0.5s\"\n"} {
		h.HostConfig(body)
		r := h.Run("run", "lint", "--", "true")
		if r.Code != 78 || !strings.Contains(r.Stderr, `= "`) {
			t.Errorf("%q: %d %q", body, r.Code, r.Stderr)
		}
	}
	h.HostConfig("[pools.lint]\n")
	if r := h.RunWith([]string{"BITC_POLL=010ms"}, "run", "lint", "--", "true"); r.Code != 64 {
		t.Errorf("BITC_POLL: %d %q", r.Code, r.Stderr)
	}
}

func TestN2_UnitsAtLeastOneFloorZeroLegal(t *testing.T) {
	h := harness.New(t)
	h.HostConfig("groups = { g = { units = 0 } }\n[pools.lint]\n")
	if r := h.Run("config", "check"); r.Code != 78 {
		t.Fatalf("%d %q", r.Code, r.Stderr)
	}
	h.HostConfig("groups = { g = { units = 1 } }\n[host]\nmemory_floor = \"0GiB\"\n[pools.lint]\n")
	if r := h.Run("config", "check"); r.Code != 0 {
		t.Fatalf("%d %q", r.Code, r.Stderr)
	}
}

func TestN3_UnsetTemplateVariableRefused(t *testing.T) {
	h := harness.New(t)
	h.HostConfig("[pools.db]\nenv = { URL = \"localhost:{slot+db_port_base}\" }\n")
	r := h.Run("run", "db", "--", "true")
	if r.Code != 78 || !strings.Contains(r.Stderr, "db_port_base") {
		t.Fatalf("%d %q", r.Code, r.Stderr)
	}
}

func TestN4_NonIntegerArithmeticRefused(t *testing.T) {
	h := harness.New(t)
	h.HostConfig("vars = { base = \"ten\" }\n[pools.e2e]\nenv = { S = \"{slot+base}\" }\n")
	r := h.Run("run", "e2e", "--", "true")
	if r.Code != 78 || !strings.Contains(r.Stderr, "not an integer") {
		t.Fatalf("%d %q", r.Code, r.Stderr)
	}
}

func TestM4_GroupsAndFloorHostOnly(t *testing.T) {
	h := harness.New(t)
	h.HostConfig("groups = { g = { units = 1 } }\n[pools.lint]\n")
	h.Cwd = h.Repo("wt", map[string]string{".bitc.toml": "version = 1\ngroups = { g = { units = 5 } }\n[host]\nmemory_floor = \"0GiB\"\n"})
	r := h.Run("run", "lint", "--", "true")
	if r.Code != 78 || !strings.Contains(r.Stderr, "host-only key") {
		t.Fatalf("%d %q", r.Code, r.Stderr)
	}
}

func TestF6_TestSeamNeedsLoopbackProviders(t *testing.T) {
	h := harness.New(t)
	h.HostConfig("[pools.lint]\n[providers.runpod]\ndaily_usd = 1\nimage = \"x\"\nsizes = {}\n")
	r := h.Run("run", "lint", "--", "true")
	if r.Code != 78 || !strings.Contains(r.Stderr, "loopback") {
		t.Fatalf("%d %q", r.Code, r.Stderr)
	}
}

func TestD1_StateDirResolution(t *testing.T) {
	h := harness.New(t)
	h.HostConfig("[pools.lint]\n")
	out := explain(h, []string{"!BITC_DIR"}, "host.state_dir")
	if !strings.Contains(out, filepath.Join(h.Root, "xdg-state", "bitc")) {
		t.Fatal(out)
	}
	out = explain(h, []string{"!BITC_DIR", "XDG_STATE_HOME="}, "host.state_dir")
	if !strings.Contains(out, filepath.Join(h.Root, ".local", "state", "bitc")) {
		t.Fatal(out)
	}
	// an empty BITC_DIR counts as unset: the default, silently (reference W:466)
	r0 := h.RunWith([]string{"BITC_DIR="}, "config", "explain", "host.state_dir")
	if r0.Code != 0 || r0.Stderr != "" || !strings.Contains(r0.Stdout, filepath.Join(h.Root, "xdg-state", "bitc")) || !strings.Contains(r0.Stdout, "# default") {
		t.Fatalf("%d %q %q", r0.Code, r0.Stdout, r0.Stderr)
	}
	r := h.RunWith(nil, "--state-dir", filepath.Join(h.Root, "flag-state"), "config", "explain", "host.state_dir")
	if !strings.Contains(r.Stdout, "flag-state") || !strings.Contains(r.Stdout, "flag --state-dir") {
		t.Fatal(r.Stdout)
	}
}

func TestD2_RelativeBitcDirIsAbsolute(t *testing.T) {
	h := harness.New(t)
	h.HostConfig("[pools.lint]\n")
	out := explain(h, []string{"BITC_DIR=rel"}, "host.state_dir")
	if !strings.Contains(out, filepath.Join(h.Root, "rel")) {
		t.Fatal(out)
	}
}

func TestQ4_UnknownProviderTypeIsConfigError(t *testing.T) {
	h := harness.New(t)
	h.HostConfig("[pools.heavy]\nproviders = [\"cloudx\", \"local\"]\n")
	if r := h.Run("run", "heavy", "--", "true"); r.Code != 78 || !strings.Contains(r.Stderr, "cloudx") {
		t.Fatalf("%d %q", r.Code, r.Stderr)
	}
}

func TestRL3_HostKeysOnlyFromHostFile(t *testing.T) {
	h := harness.New(t)
	h.HostConfig("[pools.lint]\n")
	h.Cwd = h.Repo("wt", map[string]string{".bitc.toml": "version = 1\n[pools.lint]\nslots = 3\n"})
	if r := h.Run("config", "check"); r.Code != 78 || !strings.Contains(r.Stderr, "pools.lint.slots is a host-only key") {
		t.Fatalf("%d %q", r.Code, r.Stderr)
	}
}

func TestRL4_ProviderSettingsInEnvIgnored(t *testing.T) {
	h := harness.New(t)
	h.HostConfig("[pools.heavy]\n")
	out := explain(h, []string{"RUNPOD_API_KEY=k", "BITC_PROVIDERS=runpod"}, "pools.heavy.providers")
	if !strings.Contains(out, `["local"]`) {
		t.Fatal(out)
	}
	if r := h.RunWith([]string{"BITC_PROVIDER=runpod"}, "run", "heavy", "--", "true"); r.Code != 64 {
		t.Fatalf("%d %q", r.Code, r.Stderr)
	}
}

func TestRL6_CredentialFileRules(t *testing.T) {
	h := harness.New(t)
	key := filepath.Join(h.Root, "rp.key")
	h.Write(key, "secret-value\n", 0o644)
	h.HostConfig("[pools.lint]\n[providers.runpod]\napi_key_file = " + harness.Q(key) + "\ndaily_usd = 1\nimage = \"x\"\nsizes = {}\napi_base = \"http://127.0.0.1:9\"\nproxy_domain = \"127.0.0.1:9\"\n")
	r := h.Run("config", "check")
	if r.Code != 78 || strings.Contains(r.Stdout+r.Stderr, "secret-value") {
		t.Fatalf("%d %q", r.Code, r.Stderr)
	}
}

func TestRR11_BadCredentialIsConfigNotFallback(t *testing.T) {
	h := harness.New(t)
	key := filepath.Join(h.Root, "rp.key")
	h.Write(key, "API_KEY=secret-value\n", 0o600)
	h.HostConfig("[pools.heavy]\n[providers.runpod]\napi_key_file = " + harness.Q(key) + "\ndaily_usd = 1\nimage = \"x\"\nsizes = {}\napi_base = \"http://127.0.0.1:9\"\nproxy_domain = \"127.0.0.1:9\"\n")
	r := h.Run("run", "heavy", "--", "echo", "ran")
	if r.Code != 78 || strings.Contains(r.Stdout, "ran") || strings.Contains(r.Stderr, "secret-value") {
		t.Fatalf("%d %q %q", r.Code, r.Stdout, r.Stderr)
	}
}

func TestCF1_MinVersion(t *testing.T) {
	h := harness.New(t)
	bin := filepath.Join(h.Root, "bitc-1.0.3")
	build := exec.Command("go", "build", "-o", bin, "-ldflags",
		"-X github.com/Exploded-views/bitc/internal/version.Version=1.0.3", "../cmd/bitc")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	h.HostConfig("min_version = \"1.2.0\"\nbogus = 1\n")
	cmd := exec.Command(bin, "config", "check")
	cmd.Env = h.Env
	out, _ := cmd.CombinedOutput()
	if !strings.Contains(string(out), "needs bitc >= 1.2.0 (this is 1.0.3)") || strings.Contains(string(out), "bogus") {
		t.Fatalf("%s", out)
	}
}

func TestCF2_AllUnknownKeysListed(t *testing.T) {
	h := harness.New(t)
	h.HostConfig("bogus = 1\n[pools.lint]\nalso_bogus = 2\n")
	r := h.Run("config", "check")
	if r.Code != 78 || !strings.Contains(r.Stderr, "unknown key bogus") || !strings.Contains(r.Stderr, "unknown key pools.lint.also_bogus") {
		t.Fatalf("%d %q", r.Code, r.Stderr)
	}
}

// The examples are checked as private 0600 copies: a checkout under umask 002 leaves them 0664,
// which the loader refuses (git records no mode but the exec bit).
func TestCF3_ExamplesPassCheck(t *testing.T) {
	h := harness.New(t)
	for host, repo := range map[string]string{
		"../examples/config/host.bitc.toml":          "../examples/config/repo.bitc.toml",
		"../examples/node-monorepo/host.bitc.toml":   "../examples/node-monorepo/.bitc.toml",
		"../examples/python-monorepo/host.bitc.toml": "../examples/python-monorepo/.bitc.toml",
	} {
		dir := filepath.Join(h.Root, "examples", filepath.Base(filepath.Dir(host)))
		copies := [2]string{filepath.Join(dir, "host.toml"), filepath.Join(dir, "repo.toml")}
		for i, src := range []string{host, repo} {
			b, err := os.ReadFile(src)
			if err != nil {
				t.Fatal(err)
			}
			h.Write(copies[i], string(b), 0o600)
		}
		r := h.RunWith([]string{"BITC_CONFIG=" + copies[0]}, "config", "check", "--repo", copies[1])
		if r.Code != 0 {
			t.Errorf("%s: %d %q", host, r.Code, r.Stderr)
		}
	}
}

func TestCF4_ExplainRedactsCredentials(t *testing.T) {
	h := harness.New(t)
	h.HostConfig("[providers.runpod]\napi_key = \"inline-secret\"\ndaily_usd = 1\nimage = \"x\"\nsizes = {}\napi_base = \"http://127.0.0.1:9\"\nproxy_domain = \"127.0.0.1:9\"\n")
	out := explain(h, nil, "providers")
	if strings.Contains(out, "inline-secret") || !strings.Contains(out, "<redacted>") {
		t.Fatal(out)
	}
}

func TestCF5_RepoGrantSuppliesRepoFile(t *testing.T) {
	h := harness.New(t)
	repoCfg := filepath.Join(h.Root, "etc", "app.toml")
	h.Write(repoCfg, "version = 1\n[pools.lint]\nenv = { FROM_GRANT = \"1\" }\n", 0o600)
	h.HostConfig("[pools.lint]\n\n[[repos]]\nmatch = \"**/wt\"\nconfig = " + harness.Q(repoCfg) + "\n")
	h.Cwd = h.Repo("wt", nil)
	if out := explain(h, nil, "pools.lint.env"); !strings.Contains(out, "FROM_GRANT") {
		t.Fatal(out)
	}
}

func TestCF6_RepoSectionForUnknownPoolWarns(t *testing.T) {
	h := harness.New(t)
	h.HostConfig("[pools.lint]\n")
	h.Cwd = h.Repo("wt", map[string]string{".bitc.toml": "version = 1\n[pools.ghost]\nwait = \"1m\"\n"})
	r := h.Run("config", "check")
	if r.Code != 0 || strings.Count(r.Stderr, "ghost") != 1 {
		t.Fatalf("%d %q", r.Code, r.Stderr)
	}
}

func TestCF7_RemoteEnvCheck(t *testing.T) {
	h := harness.New(t)
	h.HostConfig("[pools.heavy]\nproviders = [\"runpod\", \"local\"]\nenv = { PORT = \"{slot}\" }\n[providers.runpod]\ndaily_usd = 1\nimage = \"x\"\nsizes = {}\napi_base = \"http://127.0.0.1:9\"\nproxy_domain = \"127.0.0.1:9\"\n")
	h.Cwd = h.Repo("wt", map[string]string{".bitc.toml": "version = 1\n[pools.heavy.remote]\nallow = [[\"true\"]]\n"})
	r := h.Run("config", "check")
	if r.Code != 78 || !strings.Contains(r.Stderr, "pools.heavy.env.PORT uses the host-only {slot}") {
		t.Fatalf("%d %q", r.Code, r.Stderr)
	}
}

func TestCF8_SecretLikeForwardEnvNeedsForce(t *testing.T) {
	h := harness.New(t)
	h.HostConfig("[pools.lint]\n")
	h.Cwd = h.Repo("wt", map[string]string{".bitc.toml": "version = 1\n[remote]\nforward_env = [\"NPM_TOKEN\"]\n"})
	if r := h.Run("config", "check"); r.Code != 78 || !strings.Contains(r.Stderr, "NPM_TOKEN") {
		t.Fatalf("%d %q", r.Code, r.Stderr)
	}
}

func TestCF10_WiderSecretLikeNamesNeedForce(t *testing.T) {
	h := harness.New(t)
	wt := h.Repo("wt", map[string]string{".bitc.toml": "version = 1\n"})
	h.Cwd = wt
	for _, n := range []string{"DB_PASS", "AWS_CREDENTIALS", "GH_AUTH_HEADER", "SENTRY_DSN", "SSH_PRIVATE_FILE",
		"COOKIE_JAR", "SESSION_ID", "TLS_CERT", "DATABASE_URL", "DIRECT_URL"} {
		h.Write(filepath.Join(wt, ".bitc.toml"), "version = 1\n[remote]\nforward_env = ["+harness.Q(n)+"]\n", 0o644)
		h.HostConfig("[pools.lint]\n")
		if r := h.Run("config", "check"); r.Code != 78 || !strings.Contains(r.Stderr, n+", which looks like a secret") {
			t.Errorf("%s: %d %q", n, r.Code, r.Stderr)
		}
		h.HostConfig("[remote_limits]\nforward_env_force = [" + harness.Q(n) + "]\n[pools.lint]\n")
		if r := h.Run("config", "check"); r.Code != 0 {
			t.Errorf("%s with forward_env_force: %d %q", n, r.Code, r.Stderr)
		}
	}
}

func TestCF11_ForwardEnvEntriesMustBeVariableNames(t *testing.T) {
	h := harness.New(t)
	wt := h.Repo("wt", map[string]string{".bitc.toml": "version = 1\n"})
	h.Cwd = wt
	for _, bad := range []string{"1BAD", "BAD-NAME", "A=B", "A$(x)"} {
		h.HostConfig("[pools.lint]\n")
		h.Write(filepath.Join(wt, ".bitc.toml"), "version = 1\n[remote]\nforward_env = ["+harness.Q(bad)+"]\n", 0o644)
		if r := h.Run("config", "check"); r.Code != 78 || !strings.Contains(r.Stderr, "remote.forward_env: \""+bad+"\" is not a valid variable name") {
			t.Errorf("forward_env %q: %d %q", bad, r.Code, r.Stderr)
		}
		h.Write(filepath.Join(wt, ".bitc.toml"), "version = 1\n", 0o644)
		for _, key := range []string{"forward_env_allow", "forward_env_force"} {
			h.HostConfig("[remote_limits]\n" + key + " = [" + harness.Q(bad) + "]\n[pools.lint]\n")
			if r := h.Run("config", "check"); r.Code != 78 || !strings.Contains(r.Stderr, "remote_limits."+key+": \""+bad+"\" is not a valid variable name") {
				t.Errorf("%s %q: %d %q", key, bad, r.Code, r.Stderr)
			}
		}
	}
}

func TestCF9_WritableConfigRefused(t *testing.T) {
	h := harness.New(t)
	h.HostConfig("[pools.lint]\n")
	if err := os.Chmod(h.Config, 0o666); err != nil {
		t.Fatal(err)
	}
	if r := h.Run("config", "check"); r.Code != 78 || !strings.Contains(r.Stderr, "writable") {
		t.Fatalf("%d %q", r.Code, r.Stderr)
	}
}
