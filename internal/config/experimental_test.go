package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const remoteHost = "version = 1\n[pools]\nheavy = {}\n[providers.runpod]\ndaily_usd = 1\nimage = \"x\"\nsizes = {}\n"

// The gate at load: refused without the host flag, accepted with it, never set by the repo file.
func TestExperimentalRemoteGate(t *testing.T) {
	_, err := newRawFixture(t, remoteHost, "").load()
	expect78(t, err, "bitc: "+ExperimentalRemoteRefusal)
	if strings.Contains(err.Error(), ExperimentalRemoteCleanup) {
		t.Fatalf("no leftovers, not sweep: %v", err)
	}
	e := mustLoad(t, newRawFixture(t, strings.Replace(remoteHost, "version = 1\n", "version = 1\n[host]\nexperimental_remote = true\n", 1), ""))
	if !e.Host.ExperimentalRemote || e.Providers["runpod"] == nil {
		t.Fatalf("flag set: %+v", e.Host)
	}
	_, err = newRawFixture(t, remoteHost, "version = 1\n[host]\nexperimental_remote = true\n").load()
	expect78(t, err, "host.experimental_remote is a host-only key", ExperimentalRemoteRefusal)
	// a pool naming a provider without a table is refused too (and named as unconfigured)
	_, err = newRawFixture(t, "version = 1\n[pools]\nheavy = { providers = [\"runpod\"] }\n", "").load()
	expect78(t, err, ExperimentalRemoteRefusal, `names "runpod"`)
	// local-only: nothing to gate, and "local" alone is not remote
	if _, err := newRawFixture(t, "version = 1\n[pools]\nlint = { providers = [\"local\"] }\n", "").load(); err != nil {
		t.Fatal(err)
	}
}

// sweep and doctor --remote (Cleanup) and a host with remote leftovers get the cleanup line.
func TestExperimentalRemoteCleanupLine(t *testing.T) {
	f := newRawFixture(t, remoteHost+"[test]\naccounting_dir = \"ACCT\"\n", "")
	acct := filepath.Join(f.home, "acct")
	body, _ := os.ReadFile(f.host)
	if err := os.WriteFile(f.host, []byte(strings.Replace(string(body), "ACCT", acct, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(LoadOptions{Env: []string{"HOME=" + f.home, "BITC_CONFIG=" + f.host}, Cwd: f.wt, Cleanup: true})
	expect78(t, err, ExperimentalRemoteRefusal+"\n"+ExperimentalRemoteCleanup)
	for _, c := range []struct{ file, body string }{{"ledger.jsonl", ""}, {"reservations.json", "[]\n"}} {
		dir := filepath.Join(acct, "remote", "runpod")
		_ = os.MkdirAll(dir, 0o700)
		_ = os.WriteFile(filepath.Join(dir, c.file), []byte(c.body), 0o600)
	}
	if _, err := f.load(); err == nil || strings.Contains(err.Error(), ExperimentalRemoteCleanup) {
		t.Fatalf("empty leftovers: %v", err)
	}
	// the ledger only says remote jobs ran once (it is append-only): an open launch is not a leftover
	_ = os.WriteFile(filepath.Join(acct, "remote", "runpod", "ledger.jsonl"), []byte("{\"event\":\"launch\"}\n"), 0o600)
	if _, err := f.load(); err == nil || strings.Contains(err.Error(), ExperimentalRemoteCleanup) {
		t.Fatalf("ledger only: %v", err)
	}
	// a reservation listing a job is; only its first bytes are read
	_ = os.WriteFile(filepath.Join(acct, "remote", "runpod", "reservations.json"), []byte("  [ {\"name\":\"bitc-x\"}"+strings.Repeat(" ", 1<<20)+"]"), 0o600)
	_, err = f.load()
	expect78(t, err, ExperimentalRemoteCleanup)
	_ = os.WriteFile(filepath.Join(acct, "remote", "runpod", "reservations.json"), []byte(" [ \n ] "), 0o600)
	if _, err := f.load(); err == nil || strings.Contains(err.Error(), ExperimentalRemoteCleanup) {
		t.Fatalf("an empty list with spaces: %v", err)
	}
}

// On darwin the platform refusal of providers stands alone (it takes precedence).
func TestExperimentalRemoteDarwinRefusalTakesPrecedence(t *testing.T) {
	defer func(g string) { platformGOOS = g }(platformGOOS)
	platformGOOS = "darwin"
	_, err := newRawFixture(t, remoteHost, "").load()
	expect78(t, err, "providers.runpod: a remote provider runs on Linux only (this is a darwin build)")
	if strings.Contains(err.Error(), "experimental") {
		t.Fatalf("darwin: %v", err)
	}
}

// On darwin the platform refusal of every provider stands alone: no sprites line either. (The
// sprites type is not registered in this package's tests, so the darwin line itself is checked
// by TestExperimentalRemoteDarwinRefusalTakesPrecedence with runpod.)
func TestSpritesRefusalDarwinPrecedence(t *testing.T) {
	defer func(g string) { platformGOOS = g }(platformGOOS)
	platformGOOS = "darwin"
	_, err := newRawFixture(t, "version = 1\n[pools]\nheavy = { providers = [\"sprites\", \"local\"] }\n[providers.sprites]\norg = \"o\"\ndaily_usd = 1\n", "").load()
	expect78(t, err)
	if strings.Contains(err.Error(), "sprites provider is refused") || strings.Contains(err.Error(), "experimental") {
		t.Fatalf("darwin: %v", err)
	}
}

// On Linux the refusal is listed first, before the experimental line, and only the [test] seam
// with loopback endpoints bypasses it (the endpoint part is SPR1's: sprites registers there).
func TestSpritesRefusalOnLinux(t *testing.T) {
	host := "version = 1\n[pools]\nheavy = { providers = [\"sprites\", \"local\"] }\n"
	_, err := newRawFixture(t, host, "").load()
	expect78(t, err, "bitc: "+SpritesRefusal, ExperimentalRemoteRefusal)
	if i, j := strings.Index(err.Error(), SpritesRefusal), strings.Index(err.Error(), ExperimentalRemoteRefusal); i > j {
		t.Fatalf("order: %v", err)
	}
	_, err = newRawFixture(t, "version = 1\n[host]\nexperimental_remote = true\n"+host[len("version = 1\n"):]+"[test]\nallow_sprites = true\n", "").load()
	if err != nil && strings.Contains(err.Error(), SpritesRefusal) {
		t.Fatalf("[test] allow_sprites with no sprites table: %v", err)
	}
}

// Off Linux every provider is refused for the platform, so nothing of theirs is read first: no
// api_key_file is opened and no deploy_key_file checked (the darwin line is the whole answer).
func TestPlatformRefusalReadsNoProviderMaterial(t *testing.T) {
	defer func(g string) { platformGOOS = g }(platformGOOS)
	platformGOOS = "darwin"
	dir := t.TempDir()
	token, key := filepath.Join(dir, "runpod.key"), filepath.Join(dir, "deploy.key")
	if err := os.WriteFile(token, []byte("k\n"), 0o000); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(key, []byte("k\n"), 0o040); err != nil {
		t.Fatal(err)
	}
	host := "version = 1\n[[repos]]\nmatch = \"**\"\ndeploy_key_file = \"" + key + "\"\n[pools]\nheavy = { providers = [\"runpod\", \"local\"] }\n" +
		"[providers.runpod]\napi_key_file = \"" + token + "\"\ndaily_usd = 1\nimage = \"x\"\nsizes = {}\n"
	_, err := newRawFixture(t, host, "").load()
	expect78(t, err, "providers.runpod: a remote provider runs on Linux only (this is a darwin build)")
	if strings.Contains(err.Error(), token) || strings.Contains(err.Error(), key) || strings.Contains(err.Error(), "not local or a configured") {
		t.Fatalf("darwin read or checked provider material: %v", err)
	}
}

// Leftover type names come from directory names: only plugin-type-shaped ones are reported, so
// doctor and sweep never print an escape sequence or other odd bytes from the accounting dir.
func TestRemoteLeftoversListsOnlyTypeShapedNames(t *testing.T) {
	acct := t.TempDir()
	job := []byte(`[{"name":"bitc-x"}]`)
	for _, typ := range []string{"runpod", "evil\x1b[31m", "UPPER", "a b"} {
		d := filepath.Join(acct, "remote", typ)
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "reservations.json"), job, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got := RemoteLeftovers(&Effective{Test: &Test{AccountingDir: acct}})
	if strings.Join(got, ",") != "runpod" {
		t.Fatalf("leftover types %q", got)
	}
}

// The 64-byte peek: a list that is only whitespace past the peek is not a job; an object is.
func TestRemoteLeftoversPeek(t *testing.T) {
	for body, want := range map[string]bool{
		"[" + strings.Repeat(" ", 100) + "]":   false,
		"[\n" + strings.Repeat("\n", 70) + "]": false,
		"[\n  {\"name\": \"bitc-x\"}]":         true,
		"[]":                                   false, "": false, "null": false, "[ ] ": false,
		// damaged: a list whose peek holds something other than whitespace, "]" or an object
		"[x" + strings.Repeat(" ", 80): true,
		"[1, 2]":                       true,
	} {
		acct := t.TempDir()
		d := filepath.Join(acct, "remote", "runpod")
		_ = os.MkdirAll(d, 0o700)
		_ = os.WriteFile(filepath.Join(d, "reservations.json"), []byte(body), 0o600)
		if got := len(RemoteLeftovers(&Effective{Test: &Test{AccountingDir: acct}})) == 1; got != want {
			t.Errorf("%q: leftover %v, want %v", body, got, want)
		}
	}
}

// In a cleanup, a pool naming sprites without a [providers.sprites] table is told how to clean up.
func TestCleanupPoolNamingSpritesWithoutATableGetsTheHint(t *testing.T) {
	f := newRawFixture(t, "version = 1\n[host]\nexperimental_remote = true\n[pools]\nheavy = { providers = [\"sprites\", \"local\"] }\n", "")
	_, err := Load(LoadOptions{Env: []string{"HOME=" + f.home, "BITC_CONFIG=" + f.host}, Cwd: f.wt, Cleanup: true})
	expect78(t, err, `pools.heavy.providers names "sprites"`, "to clean up Sprites left by 0.1.0, configure [providers.sprites] with [host] experimental_remote = true and run bitc sweep")
	_, err = f.load() // not a cleanup: the jobs refusal, no hint
	if err == nil || strings.Contains(err.Error(), "to clean up Sprites left by 0.1.0, configure") {
		t.Fatalf("job path: %v", err)
	}
}
