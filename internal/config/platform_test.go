package config

import (
	"strings"
	"testing"
)

// A macOS client build runs local pools only: memory admission (/proc/meminfo, cgroup v2) and
// the remote providers are Linux-only, so a config that asks for them is a config error (78)
// on darwin, listed like every other problem, before any side effect. Linux is unchanged.
func TestCheckPlatformRefusesLinuxOnlyKeysOffLinux(t *testing.T) {
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
