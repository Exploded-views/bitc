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
		e.HostFile + ": providers.runpod: a remote provider runs on Linux only (this is a darwin build)",
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

// Off Linux a [providers.<name>] table is a config error (78) for every provider type, known or
// not, so no remote or spend path can open on a macOS client by accident. On darwin this runs
// against the real platform; on Linux it stands darwin in.
func TestEveryProviderTableIsRefusedOffLinux(t *testing.T) {
	if runtime.GOOS == "linux" {
		defer func(g string) { platformGOOS = g }(platformGOOS)
		platformGOOS = "darwin"
	}
	for _, c := range []struct{ typ, host string }{
		{"ssh", "[providers.ssh]\ndaily_usd = 1\n"},
		{"runpod", "[providers.runpod]\ndaily_usd = 1\nimage = \"x\"\nsizes = {}\n"},
		{"sprites", "[providers.sprites]\norg = \"o\"\ndaily_usd = 1\n"},
		{"modal", "[providers.modal]\nplugin = { path = \"/nonexistent/bitc-provider-modal\", sha256 = \"" + strings.Repeat("a", 64) + "\" }\ndaily_usd = 5\nsizes = { cpu8 = { usd_per_hour = 0.60 } }\n"},
		{"fake", "[providers.fake]\ndaily_usd = 1\n[test]\nfixed_secrets = true\n"},
		{"nope", "[providers.nope]\ndaily_usd = 1\n"},
	} {
		_, err := newFixture(t, "version = 1\n"+c.host, "").load()
		expect78(t, err, "providers."+c.typ+": a remote provider runs on Linux only")
	}
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

	// Undeclared sprites: darwin refuses every provider, and SpritesRefusal is Linux's line, so
	// the generic unknown-name line is the intended answer here.
	_, err = newFixture(t, "version = 1\n[pools.x]\nproviders = [\"sprites\"]\n", "").load()
	expect78(t, err, `pools.x.providers names "sprites", which is not local or a configured [providers.<type>]`)
	if strings.Contains(err.Error(), "Sprites") {
		t.Errorf("darwin printed the Linux sprites refusal: %v", err)
	}

	host := "version = 1\n[pools.x]\nproviders = [\"runpod\"]\n[providers.runpod]\ndaily_usd = 1\nimage = \"x\"\nsizes = {}\n"
	_, err = newFixture(t, host, "").load()
	expect78(t, err, "providers.runpod: a remote provider runs on Linux only (this is a darwin build)")
	if n := strings.Count(err.Error(), "runpod"); n != 1 || strings.Contains(err.Error(), "not local or a configured") {
		t.Errorf("want exactly one runpod problem, got %d:\n%v", n, err)
	}
}
