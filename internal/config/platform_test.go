package config

import (
	"runtime"
	"strings"
	"testing"
)

// A macOS client build runs local pools only: memory admission (/proc/meminfo, cgroup v2) and
// the remote providers are Linux-only, so a config that asks for them is a config error (78)
// on darwin, listed like every other problem, before any side effect. Linux is unchanged.
func TestCheckPlatformRefusesLinuxOnlyKeysOffLinux(t *testing.T) {
	pinLinux(t)
	host := `version = 1
[pools]
lint = {}
heavy = { slots = 1, memory = "8GiB" }
[providers.runpod]
daily_usd = 1
image = "x"
sizes = {}
`
	e := mustLoad(t, newFixture(t, host, ""))
	var lin problems
	checkPlatform(&lin, e, "linux")
	if len(lin.list) != 0 {
		t.Fatalf("linux: %q", lin.list)
	}
	var mac problems
	checkPlatform(&mac, e, "darwin")
	got := strings.Join(mac.list, "\n")
	for _, want := range []string{
		e.HostFile + ": pools.heavy.memory: memory admission runs on Linux only (this is a darwin build)",
		e.HostFile + ": providers.runpod: RunPod runs on Linux only for now (this is a darwin build)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("darwin problems lack %q:\n%s", want, got)
		}
	}
	if len(mac.list) != 2 || strings.Contains(got, "pools.lint") {
		t.Errorf("want exactly the two problems:\n%s", got)
	}
	if err := mac.err(); err == nil || !strings.HasPrefix(err.Error(), "bitc: ") {
		t.Fatalf("err %v", err)
	}

	// a local-only config without memory is fine everywhere
	plain := mustLoad(t, newFixture(t, "version = 1\n[pools]\nlint = { slots = 2 }\n", ""))
	var ok problems
	checkPlatform(&ok, plain, "darwin")
	if len(ok.list) != 0 {
		t.Fatalf("darwin, local only: %q", ok.list)
	}
}

// Load runs the check: on darwin the same config is refused with 78 before anything else runs.
func TestLoadRefusesLinuxOnlyKeysOnDarwin(t *testing.T) {
	defer func(g string) { platformGOOS = g }(platformGOOS)
	platformGOOS = "darwin"
	_, err := newFixture(t, "version = 1\n[pools]\nheavy = { slots = 1, memory = \"8GiB\" }\n", "").load()
	expect78(t, err, "pools.heavy.memory: memory admission runs on Linux only (this is a darwin build)")
	if _, err := newFixture(t, "version = 1\n[pools]\nlint = { slots = 2 }\n", "").load(); err != nil {
		t.Fatalf("darwin, local only: %v", err)
	}
}

// Off Linux a [providers.<name>] table is a config error (78) for every provider type except a
// plugin (TestPluginProvidersLoadOnDarwin), ssh and sprites (the darwin client, #4: sprites is
// refused for #6 on every OS), known or not, so no other path opens on a macOS client by
// accident. RunPod says it is parked. On darwin this runs against the real platform; on Linux it
// stands darwin in.
func TestEveryOtherProviderTableIsRefusedOffLinux(t *testing.T) {
	if runtime.GOOS == "linux" {
		defer func(g string) { platformGOOS = g }(platformGOOS)
		platformGOOS = "darwin"
	}
	for _, c := range []struct{ typ, host, want string }{
		{"runpod", "[providers.runpod]\ndaily_usd = 1\nimage = \"x\"\nsizes = {}\n", "RunPod runs on Linux only for now"},
		{"fake", "[providers.fake]\ndaily_usd = 1\n[test]\nfixed_secrets = true\n", "a remote provider runs on Linux only"},
		{"nope", "[providers.nope]\ndaily_usd = 1\n", "a remote provider runs on Linux only"},
	} {
		_, err := newFixture(t, "version = 1\n"+c.host, "").load()
		expect78(t, err, "providers."+c.typ+": "+c.want)
	}
}

// A plugin provider is not a remote provider the platform gate refuses: it loads on darwin (its
// shim, pin and money rails run there, #26). Pinned to darwin so Linux CI exercises the same path.
func TestPluginProvidersLoadOnDarwin(t *testing.T) {
	defer func(g string) { platformGOOS = g }(platformGOOS)
	platformGOOS = "darwin"
	pl := newPlug(t, "modal")
	e := mustLoad(t, newFixture(t, pluginHost("modal", pl, "", ""), ""))
	if pr := e.Providers["modal"]; pr == nil || pr.Plugin == nil || pr.Plugin.Path != pl.path || pr.Sizes["cpu8"] != 600000 {
		t.Fatalf("the plugin provider was not decoded on darwin: %+v", pr)
	}
	// a plugin that is not there is a plugin error, not the platform refusal
	missing := "version = 1\n[providers.modal]\nplugin = { path = \"/nonexistent/bitc-provider-modal\", sha256 = \"" + strings.Repeat("a", 64) + "\" }\ndaily_usd = 5\nsizes = { cpu8 = { usd_per_hour = 0.60 } }\n"
	_, err := newFixture(t, missing, "").load()
	expect78(t, err, "providers.modal.plugin.path: missing")
	if strings.Contains(err.Error(), "runs on Linux only") {
		t.Errorf("a plugin was refused for the platform: %v", err)
	}
	// a pool may name it
	host := pluginHost("modal", pl, "", "") + "[pools.heavy]\nproviders = [\"modal\"]\n"
	mustLoad(t, newFixture(t, host, ""))
}

// An unknown provider name in a pool is a config error (78) on every OS, not only on Linux:
// the platform gate refuses real remote providers, but a typo must read the same everywhere.
// This test deliberately does not pin platformGOOS, so it runs as the host OS.
func TestUnknownPoolProviderIsConfigErrorOnEveryOS(t *testing.T) {
	_, err := newFixture(t, "version = 1\n[pools.x]\nproviders = [\"nope\", \"local\"]\n", "").load()
	expect78(t, err, "pools.x.providers", `"nope"`)
}

// The off-Linux pool-name path, pinned to darwin so Linux CI exercises it too: an unknown name
// reads as on Linux, and a declared remote provider a pool names is refused exactly once (by
// checkPlatform), not twice.
func TestPoolProviderNamesOffLinux(t *testing.T) {
	defer func(g string) { platformGOOS = g }(platformGOOS)
	platformGOOS = "darwin"

	_, err := newFixture(t, "version = 1\n[pools.x]\nproviders = [\"nope\", \"local\"]\n", "").load()
	expect78(t, err, `pools.x.providers names "nope", which is not local or a configured [providers.<type>]`)

	// Undeclared sprites: sprites is refused for #6 on every OS, so darwin prints Linux's line and
	// not the generic unknown-name line.
	_, err = newFixture(t, "version = 1\n[pools.x]\nproviders = [\"sprites\"]\n", "").load()
	expect78(t, err, SpritesRefusal)
	if strings.Contains(err.Error(), "not local or a configured") || strings.Contains(err.Error(), "runs on Linux only") {
		t.Errorf("darwin printed a platform or unknown-name line for sprites: %v", err)
	}

	host := "version = 1\n[pools.x]\nproviders = [\"runpod\"]\n[providers.runpod]\ndaily_usd = 1\nimage = \"x\"\nsizes = {}\n"
	_, err = newFixture(t, host, "").load()
	expect78(t, err, "providers.runpod: RunPod runs on Linux only for now (this is a darwin build)")
	if n := strings.Count(err.Error(), "runpod"); n != 1 || strings.Contains(err.Error(), "not local or a configured") {
		t.Errorf("want exactly one runpod problem, got %d:\n%v", n, err)
	}
}

// A plugin provider on darwin is gated by [host] experimental_remote like on Linux (the flag is
// not skipped off Linux): without it the load is 78 with the same text, and no platform line.
func TestPluginProviderOnDarwinNeedsExperimentalRemote(t *testing.T) {
	defer func(g string) { platformGOOS = g }(platformGOOS)
	platformGOOS = "darwin"
	pl := newPlug(t, "modal")
	_, err := newRawFixture(t, pluginHost("modal", pl, "", ""), "").load()
	expect78(t, err, "bitc: "+ExperimentalRemoteRefusal)
	if strings.Contains(err.Error(), "runs on Linux only") {
		t.Fatalf("a plugin was refused for the platform: %v", err)
	}
	mustLoad(t, newFixture(t, pluginHost("modal", pl, "", ""), "")) // newFixture adds the flag
}
