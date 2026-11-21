package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// withFlag adds [host] experimental_remote = true to a host file.
func withFlag(host string) string {
	return strings.Replace(host, "version = 1\n", "version = 1\n[host]\nexperimental_remote = true\n", 1)
}

// The opt-in applies on every OS: on darwin an exec plugin provider (it passes the platform
// gate) is a remote provider with spend accounting, so without [host] experimental_remote it is
// refused with the Linux text. A pool naming it is the same table, refused once.
func TestExperimentalRemoteDarwinPluginRefused(t *testing.T) {
	defer func(g string) { platformGOOS = g }(platformGOOS)
	platformGOOS = "darwin"
	pl := newPlug(t, "modal")
	host := pluginHost("modal", pl, "", "")
	_, err := newRawFixture(t, host, "").load()
	expect78(t, err, "bitc: "+ExperimentalRemoteRefusal)
	if strings.Contains(err.Error(), ExperimentalRemoteCleanup) || strings.Contains(err.Error(), "runs on Linux only") {
		t.Fatalf("no leftovers and a plugin is not platform-refused: %v", err)
	}
	_, err = newRawFixture(t, host+"[pools.heavy]\nproviders = [\"modal\", \"local\"]\n", "").load()
	expect78(t, err, ExperimentalRemoteRefusal)
	if n := strings.Count(err.Error(), ExperimentalRemoteRefusal); n != 1 {
		t.Fatalf("want the refusal once, got %d: %v", n, err)
	}
	// a plugin whose pin is bad still gets the opt-in refusal (with the pin error), not silence
	bad := "version = 1\n[providers.modal]\nplugin = { path = \"/nonexistent/bitc-provider-modal\", sha256 = \"" + strings.Repeat("a", 64) + "\" }\ndaily_usd = 5\nsizes = { cpu8 = { usd_per_hour = 0.60 } }\n"
	_, err = newRawFixture(t, bad, "").load()
	expect78(t, err, ExperimentalRemoteRefusal)
	// the repo file cannot set the flag: the refusal stands, next to the host-only-key problem
	_, err = newRawFixture(t, pluginHost("modal", pl, "", ""), "version = 1\n[host]\nexperimental_remote = true\n").load()
	expect78(t, err, "host.experimental_remote is a host-only key", ExperimentalRemoteRefusal)
}

// With the flag a darwin plugin provider loads as in 0.1.5. This passes before and after the
// fix: it guards against over-refusing.
func TestExperimentalRemoteDarwinPluginWithFlagLoads(t *testing.T) {
	defer func(g string) { platformGOOS = g }(platformGOOS)
	platformGOOS = "darwin"
	pl := newPlug(t, "modal")
	host := withFlag(pluginHost("modal", pl, "", "") + "[pools.heavy]\nproviders = [\"modal\"]\n")
	e := mustLoad(t, newRawFixture(t, host, ""))
	if pr := e.Providers["modal"]; pr == nil || pr.Plugin == nil || !e.Host.ExperimentalRemote {
		t.Fatalf("flag set, plugin not loaded on darwin: %+v", e.Providers["modal"])
	}
	// local only on darwin: nothing to gate
	if _, err := newRawFixture(t, "version = 1\n[pools]\nlint = { providers = [\"local\"] }\n", "").load(); err != nil {
		t.Fatal(err)
	}
}

// sweep and doctor --remote (Cleanup) and a host with leftovers get the cleanup line on darwin too.
func TestExperimentalRemoteDarwinPluginCleanupLine(t *testing.T) {
	defer func(g string) { platformGOOS = g }(platformGOOS)
	platformGOOS = "darwin"
	pl := newPlug(t, "modal")
	home := t.TempDir()
	acct := filepath.Join(home, "acct")
	f := newRawFixture(t, pluginHost("modal", pl, "", "[test]\naccounting_dir = \""+acct+"\"\n"), "")
	_, err := Load(LoadOptions{Env: []string{"HOME=" + f.home, "BITC_CONFIG=" + f.host}, Cwd: f.wt, Cleanup: true})
	expect78(t, err, ExperimentalRemoteRefusal+"\n"+ExperimentalRemoteCleanup)
	if _, err := f.load(); err == nil || strings.Contains(err.Error(), ExperimentalRemoteCleanup) {
		t.Fatalf("no leftovers, not a cleanup: %v", err)
	}
	dir := filepath.Join(acct, "remote", "modal")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "reservations.json"), []byte(`[{"name":"bitc-x"}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = f.load()
	expect78(t, err, ExperimentalRemoteRefusal+"\n"+ExperimentalRemoteCleanup)
}

// When both apply on darwin (a built-in remote table and a plugin, no flag), every problem is
// listed: the opt-in refusal first (as on Linux, load.go order), then the platform line for the
// built-in provider only. The plugin is never platform-refused.
func TestExperimentalRemoteDarwinBothRefusals(t *testing.T) {
	defer func(g string) { platformGOOS = g }(platformGOOS)
	platformGOOS = "darwin"
	pl := newPlug(t, "modal")
	host := pluginHost("modal", pl, "", "[providers.runpod]\ndaily_usd = 1\nimage = \"x\"\nsizes = {}\n")
	_, err := newRawFixture(t, host, "").load()
	plat := "providers.runpod: a remote provider runs on Linux only (this is a darwin build)"
	expect78(t, err, ExperimentalRemoteRefusal, plat)
	if i, j := strings.Index(err.Error(), ExperimentalRemoteRefusal), strings.Index(err.Error(), plat); i < 0 || j < 0 || i > j {
		t.Fatalf("order: %v", err)
	}
	if strings.Contains(err.Error(), "providers.modal: a remote provider runs on Linux only") {
		t.Fatalf("plugin platform-refused: %v", err)
	}
}
