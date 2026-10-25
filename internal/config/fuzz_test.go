package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Exploded-views/bitc/internal/exit"
)

// FuzzLoadHostFile: any host file either loads or fails with 78 (or 64 for a bad value that
// came from the environment); it never panics or hangs.
func FuzzLoadHostFile(f *testing.F) {
	for _, s := range []string{
		"version = 1\n",
		"version = 1\n[pools.lint]\nslots = 2\nwait = \"10s\"\n",
		"version = 1\nvars = { base = 1 }\n[pools.db]\nenv = { P = \"{slot+base}\" }\n",
		"version = 1\ngroups = { g = { units = 2 } }\n[pools.a]\ngroups = { g = 3 }\n",
		"version = 2\n",
		"version = 1\n[[repos]]\nmatch = \"**\"\nconfig = \"/nope\"\n",
		"version = 1\n[providers.runpod]\ndaily_usd = 1\nimage = \"x\"\nsizes = {}\n",
		"version = 1\n[pools.x]\nenv = { A = \"{\" }\n",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, body string) {
		dir := t.TempDir()
		host := filepath.Join(dir, "bitc.toml")
		if err := os.WriteFile(host, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := Load(LoadOptions{Env: []string{"HOME=" + dir, "BITC_CONFIG=" + host, "BITC_DIR=" + filepath.Join(dir, "s")}, Cwd: dir})
		if err != nil {
			if c := exit.Code(err); c != exit.Config && c != exit.Usage {
				t.Fatalf("exit %d for %q: %v", c, body, err)
			}
		}
	})
}
