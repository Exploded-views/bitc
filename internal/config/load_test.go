package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Exploded-views/bitc/internal/exit"
)

type fixture struct {
	t    *testing.T
	home string
	host string
	wt   string
}

// newFixture writes host (with experimental_remote = true added when it names providers, see
// withRemoteFlag) and repo; newRawFixture writes host as given.
func newFixture(t *testing.T, host, repo string) *fixture {
	t.Helper()
	return newRawFixture(t, withRemoteFlag(host), repo)
}

// withRemoteFlag adds [host] experimental_remote = true to a host file that mentions providers
// and lacks the key (the remote gate is tested in experimental_test.go and conformance EX1).
func withRemoteFlag(host string) string {
	if !strings.Contains(host, "providers") || strings.Contains(host, "experimental_remote") {
		return host
	}
	if i := strings.Index(host, "\n[host]\n"); i >= 0 {
		j := i + len("\n[host]\n")
		return host[:j] + "experimental_remote = true\n" + host[j:]
	}
	if host != "" && !strings.HasSuffix(host, "\n") {
		host += "\n"
	}
	return host + "[host]\nexperimental_remote = true\n"
}

func newRawFixture(t *testing.T, host, repo string) *fixture {
	t.Helper()
	home := t.TempDir()
	f := &fixture{t: t, home: home, host: filepath.Join(home, "bitc.toml"), wt: filepath.Join(home, "wt")}
	if err := os.MkdirAll(f.wt, 0o755); err != nil {
		t.Fatal(err)
	}
	if host != "" {
		if err := os.WriteFile(f.host, []byte(host), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if repo != "" {
		if err := os.WriteFile(filepath.Join(f.wt, ".bitc.toml"), []byte(repo), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

func (f *fixture) load(env ...string) (*Effective, error) {
	base := []string{"HOME=" + f.home, "BITC_CONFIG=" + f.host}
	return Load(LoadOptions{Env: append(base, env...), Cwd: f.wt})
}

func mustLoad(t *testing.T, f *fixture, env ...string) *Effective {
	t.Helper()
	e, err := f.load(env...)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func expect78(t *testing.T, err error, frags ...string) {
	t.Helper()
	if exit.Code(err) != exit.Config {
		t.Fatalf("got %v, want 78", err)
	}
	for _, fr := range frags {
		if !strings.Contains(err.Error(), fr) {
			t.Errorf("error %q lacks %q", err.Error(), fr)
		}
	}
}

func TestDefaultsAndPools(t *testing.T) {
	f := newFixture(t, `version = 1
defaults = { slots = 2, wait = "5m" }
groups = { bigmem = { units = 1 } }
[pools]
lint = {}
heavy = { slots = 1, wait = "1h", memory = "8GiB", groups = { bigmem = 1 } }
`, "")
	e := mustLoad(t, f)
	if e.Host.Poll != 5*time.Second || e.Host.MemoryFloor != 6<<30 || e.Host.UnknownPools != "refuse" {
		t.Fatalf("host %+v", e.Host)
	}
	lint, heavy := e.Pools["lint"], e.Pools["heavy"]
	if lint.Slots != 2 || lint.Wait != 5*time.Minute || lint.OnSignal != "terminate" || strings.Join(lint.Providers, ",") != "local" {
		t.Fatalf("lint %+v", lint)
	}
	if heavy.Slots != 1 || heavy.Wait != time.Hour || heavy.Memory != 8<<30 || heavy.Groups["bigmem"] != 1 {
		t.Fatalf("heavy %+v", heavy)
	}
	if e.Detach.WaitTimeout != 9*time.Minute || e.RemoteLimits.OnFailure != "next" {
		t.Fatalf("detach/remote %+v %+v", e.Detach, e.RemoteLimits)
	}
}

func TestHostWinsEnvKeyByKey(t *testing.T) {
	f := newFixture(t, `version = 1
[defaults]
env = { A = "host-default" }
[pools.lint]
env = { B = "host-pool" }
`, `version = 1
[defaults]
env = { A = "repo-default", C = "repo-default" }
env_unset = ["NODE_OPTIONS"]
[pools.lint]
env = { B = "repo-pool", D = "repo-pool", A = "repo-pool" }
`)
	p := mustLoad(t, f).Pools["lint"]
	got := map[string]string{}
	for k, v := range p.Env {
		got[k] = v.Raw
	}
	// each layer resolves defaults < pool, then the host layer wins key by key
	want := map[string]string{"A": "host-default", "B": "host-pool", "C": "repo-default", "D": "repo-pool"}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q (all %v)", k, got[k], v, got)
		}
	}
	if strings.Join(p.DefaultsEnvUnset, ",") != "NODE_OPTIONS" {
		t.Errorf("unset %v", p.DefaultsEnvUnset)
	}
}

func TestRepoMayOnlyNarrow(t *testing.T) {
	host := "version = 1\n[pools.lint]\nwait = \"10m\"\n"
	e := mustLoad(t, newFixture(t, host, "version = 1\n[pools.lint]\nwait = \"1m\"\n"))
	if e.Pools["lint"].Wait != time.Minute {
		t.Fatal(e.Pools["lint"].Wait)
	}
	_, err := newFixture(t, host, "version = 1\n[pools.lint]\nwait = \"1h\"\n").load()
	expect78(t, err, "pools.lint.wait", "may only narrow")
	_, err = newFixture(t, host, "version = 1\n[pools.lint]\nslots = 4\n").load()
	expect78(t, err, "pools.lint.slots", "host-only key")
	_, err = newFixture(t, host, "version = 1\ngroups = { g = { units = 9 } }\n").load()
	expect78(t, err, "groups", "host-only key")
}

func TestStrictValuesNameFileKeyAndValue(t *testing.T) {
	cases := map[string][]string{
		"[pools.lint]\nwait = \"05s\"\n":       {"pools.lint.wait", `"05s"`},
		"[host]\nmemory_floor = \"6GB\"\n":     {"host.memory_floor", `"6GB"`},
		"[pools.lint]\nslots = 0\n":            {"pools.lint.slots", "at least 1"},
		"groups = { g = { units = 0 } }\n":     {"groups.g.units", "at least 1"},
		"[host]\nunknown_pools = \"Refuse\"\n": {"host.unknown_pools", `"Refuse"`},
		"[pools.lint]\nwait = \"\"\n":          {"pools.lint.wait", `""`},
		"[pools.a_b]\n[pools.a-b]\n":           {"a-b", "same pool"},
		"bogus = 1\n[pools.lint]\nalso = 2\n":  {"bogus", "also"},
	}
	for body, frags := range cases {
		_, err := newFixture(t, "version = 1\n"+body, "").load()
		expect78(t, err, frags...)
	}
	if _, err := newFixture(t, "version = 1\n[host]\nmemory_floor = \"0GiB\"\n", "").load(); err != nil {
		t.Fatalf("0GiB floor refused: %v", err)
	}
}

func TestTemplatesCheckedAtLoad(t *testing.T) {
	_, err := newFixture(t, "version = 1\n[pools.db]\nenv = { URL = \"x:{slot+db_port_base}\" }\n", "").load()
	expect78(t, err, "pools.db.env.URL", "unknown template variable db_port_base")
	_, err = newFixture(t, "version = 1\nvars = { base = \"ten\" }\n[pools.db]\nenv = { P = \"{slot+base}\" }\n", "").load()
	expect78(t, err, "pools.db.env.P", "not an integer")
	e := mustLoad(t, newFixture(t, "version = 1\nvars = { base = 47410 }\n[pools.db]\nenv = { P = \"{slot+base}\" }\n", ""))
	if e.Vars["base"] != "47410" {
		t.Fatal(e.Vars)
	}
	// a template from the repo file names the repo file (spec 5.2: file, key, quoted value)
	_, err = newFixture(t, "version = 1\n[pools.db]\n", "version = 1\n[pools.db]\nenv = { P = \"{slot+x}\" }\n").load()
	expect78(t, err, `.bitc.toml: pools.db.env.P = "{slot+x}": unknown template variable x`)
	_, err = newFixture(t, "version = 1\n[pools.db]\nhooks.pre_run = { argv = [\"x\", \"{nope}\"] }\n", "").load()
	expect78(t, err, `bitc.toml: pools.db.hooks.pre_run.argv[1] = "{nope}": unknown template variable nope`)
}

func TestEnvOverrides(t *testing.T) {
	f := newFixture(t, "version = 1\n[pools.lint]\n", "")
	e := mustLoad(t, f, "BITC_POLL=250ms", "BITC_DIR=rel/state", "BITC_SLOTS=9", "BITC_PROVIDER=")
	if e.Host.Poll != 250*time.Millisecond || e.Host.StateDir != filepath.Join(f.wt, "rel", "state") {
		t.Fatalf("%+v", e.Host)
	}
	for _, bad := range []string{"BITC_POLL=50ms", "BITC_POLL=05s", "BITC_PROVIDER=runpod", "BITC_LOG=loud"} {
		if _, err := f.load(bad); exit.Code(err) != exit.Usage {
			t.Errorf("%s: got %v, want 64", bad, err)
		}
	}
	e = mustLoad(t, f, "XDG_STATE_HOME=")
	if e.Host.StateDir != filepath.Join(f.home, ".local", "state", "bitc") {
		t.Fatal(e.Host.StateDir)
	}
}

func TestCredentialFiles(t *testing.T) {
	home := t.TempDir()
	key := filepath.Join(home, "rp.key")
	host := "version = 1\n[providers.runpod]\napi_key_file = " + `"` + key + `"` + "\ndaily_usd = 25\nimage = \"x\"\nsizes = { cpu8 = { cpu_type = \"cpu3c\", vcpu = 8, usd_per_hour = 0.24 } }\n"
	for body, mode := range map[string]os.FileMode{"k1\n": 0o644, "k1\nk2\n": 0o600, "RUNPOD_API_KEY=k1\n": 0o600, "": 0o600} {
		if err := os.WriteFile(key, []byte(body), mode); err != nil {
			t.Fatal(err)
		}
		_ = os.Chmod(key, mode)
		_, err := newFixture(t, host, "").load()
		expect78(t, err, "api_key_file")
		if strings.Contains(err.Error(), "k1") {
			t.Fatalf("key printed: %v", err)
		}
	}
	if err := os.WriteFile(key, []byte("k1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// WriteFile keeps the mode of an existing file, so the 0o644 case above may have left it loose.
	if err := os.Chmod(key, 0o600); err != nil {
		t.Fatal(err)
	}
	e := mustLoad(t, newFixture(t, host, ""))
	if e.Providers["runpod"].APIKey != "k1" || len(e.Secrets) != 1 {
		t.Fatal("key not loaded")
	}
}

// The 0600 rule follows the decoded providers, not the bytes: a comment naming api_key_file
// must not hide an inline api_key.
func TestInlineKeyNeedsMode600(t *testing.T) {
	f := newFixture(t, "version = 1\n# api_key_file is the usual way\n[providers.runpod]\napi_key = \"inline-k\"\ndaily_usd = 1\nimage = \"x\"\nsizes = {}\n", "")
	if err := os.Chmod(f.host, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := f.load()
	expect78(t, err, "holds a credential", "mode 600")
	if strings.Contains(err.Error(), "inline-k") {
		t.Fatalf("key printed: %v", err)
	}
	if err := os.Chmod(f.host, 0o600); err != nil {
		t.Fatal(err)
	}
	mustLoad(t, f)
}

func TestTestTableNeedsLoopbackEndpoints(t *testing.T) {
	prov := "\n[providers.runpod]\ndaily_usd = 1\nimage = \"x\"\nsizes = {}\n"
	_, err := newFixture(t, "version = 1\n[test]\nprocfs_root = \"/tmp/p\"\n"+prov, "").load()
	expect78(t, err, "[test]", "loopback")
	ok := "version = 1\n[test]\nprocfs_root = \"/tmp/p\"\n" + prov + "api_base = \"http://127.0.0.1:9\"\nproxy_domain = \"127.0.0.1:9\"\n"
	if _, err := newFixture(t, ok, "").load(); err != nil {
		t.Fatal(err)
	}
	if _, err := newFixture(t, "version = 1\n[test]\ntime_scale = 0.01\n", "").load(); err != nil {
		t.Fatal(err)
	}
}

func TestResolve(t *testing.T) {
	f := newFixture(t, "version = 1\ngroups = { bigmem = { units = 1 } }\n[pools.release_tag]\nwait = \"10m\"\n", "")
	e := mustLoad(t, f)
	if p, err := e.Resolve("release_tag"); err != nil || p.Name != "release-tag" {
		t.Fatalf("%v %v", p, err)
	}
	for name, frag := range map[string]string{"a/b": "invalid pool name", "nope": "unknown pool nope (known: release-tag)", "bigmem": "bigmem is a group, not a pool"} {
		if _, err := e.Resolve(name); exit.Code(err) != exit.Usage || !strings.Contains(err.Error(), frag) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := e.ApplyRunFlags(RunFlags{Wait: "1h"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Resolve("release-tag"); exit.Code(err) != exit.Usage {
		t.Fatalf("--wait widening: %v", err)
	}
	if err := e.ApplyRunFlags(RunFlags{Wait: "5x"}); exit.Code(err) != exit.Usage {
		t.Fatalf("bad --wait: %v", err)
	}
}

func TestUnknownProviderType(t *testing.T) {
	_, err := newFixture(t, "version = 1\n[providers.nope]\ndaily_usd = 1\n", "").load()
	expect78(t, err, "providers.nope", "unknown provider type")
	_, err = newFixture(t, "version = 1\n[pools.x]\nproviders = [\"nope\", \"local\"]\n", "").load()
	expect78(t, err, "pools.x.providers", "nope")
}

func TestRepoSectionForUnknownPoolWarns(t *testing.T) {
	e := mustLoad(t, newFixture(t, "version = 1\n[pools.lint]\n", "version = 1\n[pools.ghost]\nwait = \"1m\"\n"))
	// one line naming the pool once (config check prints it; CF6 counts the name)
	if len(e.Warnings) != 1 || strings.Count(e.Warnings[0], "ghost") != 1 {
		t.Fatal(e.Warnings)
	}
}

func TestCompatGroupKeys(t *testing.T) {
	host := func(groups string) string {
		return "version = 1\ngroups = { big_mem = { units = 1 } }\n[compat]\nlegacy_dir = \"/x\"\ngroups = " + groups + "\n"
	}
	e := mustLoad(t, newFixture(t, host(`{ big_mem = "bigmem" }`), ""))
	if got := e.Compat.Groups["big-mem"]; got != "bigmem" || len(e.Compat.Groups) != 1 {
		t.Fatalf("keys not canonicalised: %v", e.Compat.Groups)
	}
	_, err := newFixture(t, host(`{ bigmem = "bigmem" }`), "").load()
	expect78(t, err, "compat.groups.bigmem names no declared group")
	_, err = newFixture(t, host(`{ big_mem = "a", "big-mem" = "b" }`), "").load()
	expect78(t, err, "same group spelled twice")
	_, err = newFixture(t, host(`{ big_mem = "a/b" }`), "").load()
	expect78(t, err, "legacy lock stem")
}

func TestRepoDefaultWaitNarrowsExplicitHostPoolWait(t *testing.T) {
	f := newFixture(t, "version = 1\n[pools]\nlint = { wait = \"30m\" }\ndb = {}\nquick = { wait = \"30s\" }\n",
		"version = 1\ndefaults = { wait = \"1m\" }\n")
	e := mustLoad(t, f)
	if e.Pools["lint"].Wait != time.Minute || e.Pools["db"].Wait != time.Minute || e.Pools["quick"].Wait != 30*time.Second {
		t.Fatalf("lint %v db %v quick %v", e.Pools["lint"].Wait, e.Pools["db"].Wait, e.Pools["quick"].Wait)
	}
}

func TestRepoFileMustBeSmallRegularFile(t *testing.T) {
	f := newFixture(t, "version = 1\n", "")
	repoFile := filepath.Join(f.wt, ".bitc.toml")
	if err := os.Symlink("/dev/null", repoFile); err != nil {
		t.Fatal(err)
	}
	_, err := f.load()
	expect78(t, err, "is not a regular file")
	os.Remove(repoFile)
	big := "version = 1\n# " + strings.Repeat("x", 2<<20) + "\n"
	if err := os.WriteFile(repoFile, []byte(big), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = f.load()
	expect78(t, err, "is larger than")
}

func TestRepoDefaultWaitZeroFailFastNarrowsExplicitHostPoolWait(t *testing.T) {
	f := newFixture(t, "version = 1\n[pools]\nlint = { wait = \"30m\" }\ndb = {}\n",
		"version = 1\ndefaults = { wait = \"0s\" }\n")
	e := mustLoad(t, f)
	if e.Pools["lint"].Wait != 0 || e.Pools["db"].Wait != 0 {
		t.Fatalf("lint %v db %v, want both 0 (fail-fast)", e.Pools["lint"].Wait, e.Pools["db"].Wait)
	}
}

func TestTestTableFixedSecretsSeam(t *testing.T) {
	e, err := newFixture(t, "version = 1\n[test]\nfixed_secrets = true\n", "").load()
	if err != nil {
		t.Fatalf("[test] fixed_secrets = true: %v", err)
	}
	if e.Test == nil || !e.Test.FixedSecrets {
		t.Errorf("Test = %+v, want FixedSecrets", e.Test)
	}
	e, err = newFixture(t, "version = 1\n[test]\ntime_scale = 0.5\n", "").load()
	if err != nil || e.Test.FixedSecrets {
		t.Errorf("unset fixed_secrets: %+v %v", e.Test, err)
	}
}

// Unit tests never look at this account's real accounting dir (a host that ran remote jobs would
// change what Load says): without [test] accounting_dir the leftover check finds nothing.
func init() { accountDir = func() string { return "" } }
