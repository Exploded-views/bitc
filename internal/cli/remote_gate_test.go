package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Exploded-views/bitc/internal/config"
)

// sweep and doctor --remote load with Cleanup: their refusal of remote providers without
// [host] experimental_remote says how to clean up; status does not.
func TestSweepAndDoctorRemoteRefusalNamesSweep(t *testing.T) {
	dir := t.TempDir()
	host := filepath.Join(dir, "bitc.toml")
	// [test] accounting_dir: an empty dir of its own, never this account's real one
	body := "version = 1\n[providers.runpod]\ndaily_usd = 1\nimage = \"x\"\nsizes = {}\n" +
		"[test]\naccounting_dir = \"" + filepath.Join(dir, "account") + "\"\n"
	if err := os.WriteFile(host, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BITC_CONFIG", host)
	t.Setenv("BITC_DIR", filepath.Join(dir, "state"))
	for _, c := range []struct {
		args  []string
		sweep bool
	}{{[]string{"sweep"}, true}, {[]string{"doctor", "--remote"}, true}, {[]string{"doctor"}, false}, {[]string{"status"}, false}} {
		s, stderr := std(t)
		if code := Main(c.args, s); code != 78 {
			t.Fatalf("%v: code %d: %s", c.args, code, stderr())
		}
		if !strings.Contains(stderr(), config.ExperimentalRemoteRefusal) || strings.Contains(stderr(), config.ExperimentalRemoteCleanup) != c.sweep {
			t.Fatalf("%v: %s", c.args, stderr())
		}
	}
	for _, d := range []string{"state", "account"} {
		if _, err := os.Stat(filepath.Join(dir, d)); err == nil {
			t.Fatalf("the %s dir was created", d)
		}
	}
}
