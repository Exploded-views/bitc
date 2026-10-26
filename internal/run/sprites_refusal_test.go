package run

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Exploded-views/bitc/internal/exit"
	"github.com/Exploded-views/bitc/internal/redact"
)

// The supervisor (`__supervise`, a detached job's replay) prepares through Prepare like every
// job path: a host file with [providers.sprites] is 78 with the Sprites jobs line, whatever the
// flag, before anything is created (SPR1, SPR2).
func TestPrepareRefusesSpritesJobs(t *testing.T) {
	dir := t.TempDir()
	host := filepath.Join(dir, "bitc.toml")
	body := "version = 1\n[host]\nexperimental_remote = true\n[pools.heavy]\nproviders = [\"sprites\", \"local\"]\n" +
		"[providers.sprites]\norg = \"o\"\ndaily_usd = 1\n"
	if err := os.WriteFile(host, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	inv := Invocation{Args: []string{"heavy", "--", "echo", "ran"}, Cwd: dir, Detached: true,
		Env: []string{"HOME=" + dir, "BITC_CONFIG=" + host, "BITC_DIR=" + filepath.Join(dir, "state")}}
	_, err := Prepare(inv, redact.New())
	if exit.Code(err) != exit.Config || !strings.Contains(err.Error(), "Sprites jobs are refused in this release") {
		t.Fatalf("Prepare: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "state")); err == nil {
		t.Fatal("the state dir was created")
	}
}
