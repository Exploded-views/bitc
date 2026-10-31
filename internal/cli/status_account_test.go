package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Exploded-views/bitc/internal/config"
	"github.com/Exploded-views/bitc/internal/exit"
	"github.com/Exploded-views/bitc/internal/state"
)

// status refuses (78, one line on stderr) like it does for a state dir on a network filesystem,
// when a remote provider is configured and the accounting dir is on one.
func TestStatusRefusesAnAccountDirOnANetworkFilesystem(t *testing.T) {
	dir := t.TempDir()
	e := &config.Effective{Test: &config.Test{AccountingDir: dir},
		Providers: map[string]*config.Provider{"fake": {}}}
	defer state.StubNetworkFS("")()
	err := accountDirRefusal(e)
	if exit.Code(err) != exit.Config || err == nil || !strings.Contains(err.Error(), "accounting dir "+dir+" is on NFS") {
		t.Fatalf("%v", err)
	}
	if err := accountDirRefusal(&config.Effective{Test: e.Test}); err != nil {
		t.Fatalf("no remote provider configured: %v", err)
	}
}

// config check leaves a local-only host alone even when its old account dir is on NFS.
func TestConfigCheckIgnoresAnNFSAccountDirOnALocalOnlyHost(t *testing.T) {
	dir := t.TempDir()
	acct := filepath.Join(dir, "account")
	if err := os.Mkdir(acct, 0o700); err != nil {
		t.Fatal(err)
	}
	host := filepath.Join(dir, "bitc.toml")
	body := "version = 1\n[pools.lint]\nslots = 1\n[test]\naccounting_dir = \"" + acct + "\"\n"
	if err := os.WriteFile(host, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BITC_CONFIG", host)
	t.Setenv("BITC_DIR", filepath.Join(dir, "state"))
	defer state.StubNetworkFS(acct)()
	s, stderr := std(t)
	if code := Main([]string{"config", "check"}, s); code != 0 {
		t.Fatalf("code %d: %s", code, stderr())
	}
}
