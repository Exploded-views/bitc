package conformance

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Exploded-views/bitc/conformance/harness"
	"github.com/Exploded-views/bitc/internal/provider/sprites/spritesfake"
)

// sprRefusal is the whole refusal line for any [providers.sprites] table or pool naming sprites,
// on every path but a cleanup (sweep, doctor --remote: SPR2).
const sprRefusal = "bitc: Sprites jobs are refused in this release: their network policy does not restrict " +
	"egress yet, and a job would receive the forwarded environment and the deploy key. `bitc sweep` can " +
	"still clean up Sprites left by 0.1.0 (see https://github.com/Exploded-views/bitc/issues/6 and " +
	"https://github.com/Exploded-views/bitc#known-issues)."

// sprEnv: an EX sandbox (host files written whole) plus a sprites fake, a token file and a deploy
// key file, both unreadable (the token mode 000, the key 040): a refusal that reads or checks
// either names it in an error.
type sprEnv struct {
	*exEnv
	f     *spritesfake.Fake
	token string
	key   string
}

func sprSandbox(t *testing.T) *sprEnv {
	t.Helper()
	e := &sprEnv{exEnv: exSandbox(t), f: spritesfake.New(t, spritesfake.Options{Bin: harness.Bin})}
	e.token, e.key = filepath.Join(e.h.Root, "sprites.token"), filepath.Join(e.h.Root, "deploy.key")
	e.h.Write(e.token, e.f.Token()+"\n", 0o000)
	e.h.Write(e.key, "not-a-key\n", 0o040) // unreadable by its owner, and a mode the key-file check names
	return e
}

// body is a host body: hostTable, a grant with the deploy key, pool heavy [sprites, local] and
// [providers.sprites] at api_base (the fake's loopback URL unless given).
func (e *sprEnv) body(hostTable, apiBase string) string {
	if apiBase == "" {
		apiBase = e.f.URL()
	}
	return hostTable + "[remote_limits]\nworktrees = [\"**/app\"]\n" +
		"[[repos]]\nmatch = \"**/app\"\ndeploy_key_file = " + harness.Q(e.key) + "\n" +
		"[pools.heavy]\nproviders = [\"sprites\", \"local\"]\nremote_limits = { size = { sprites = \"cpu8\" } }\nwait = \"5s\"\n" +
		"[providers.sprites]\napi_key_file = " + harness.Q(e.token) + "\norg = " + harness.Q(e.f.Org()) + "\ndaily_usd = 10\n" +
		"api_base = " + harness.Q(apiBase) + "\nsizes = { cpu8 = { memory = \"16GiB\" } }\n"
}

// write writes the host file: body, then the [test] table (accounting_dir and extra lines);
// test = false writes no [test] table at all (a production host).
func (e *sprEnv) write(body string, test bool, extra string) {
	s := "version = 1\n" + body
	if test {
		s += "\n[test]\naccounting_dir = " + harness.Q(e.acct) + "\n" + extra
	}
	e.h.Write(e.h.Config, s, 0o600)
}

// sprRefused: 78 with the refusal, no word about the token or the key file, no call to the fake,
// no state or accounting dir, and the command never ran.
func (e *sprEnv) refused(t *testing.T, what string, args ...string) string {
	t.Helper()
	r := e.h.Run(args...)
	if r.Code != 78 || !strings.Contains(r.Stderr, sprRefusal+"\n") || strings.Contains(r.Stdout, "ran") {
		t.Fatalf("%s: want 78 and the sprites refusal: %+v", what, r)
	}
	if strings.Contains(r.Stderr, e.token) || strings.Contains(r.Stderr, e.key) {
		t.Errorf("%s: the token or deploy key file was read: %q", what, r.Stderr)
	}
	if c := e.f.Calls(); len(c) != 0 {
		t.Errorf("%s: the sprites API was called: %v", what, c)
	}
	e.untouched(t, what)
	return r.Stderr
}

func TestSPR1_SpritesIsRefusedInThisRelease(t *testing.T) {
	t.Run("refused even with experimental_remote", func(t *testing.T) {
		e := sprSandbox(t)
		e.write(e.body("[host]\nexperimental_remote = true\n", ""), true, "")
		e.refused(t, "run", "run", "heavy", "--", "echo", "ran")
		e.refused(t, "config check", "config", "check")
		e.refused(t, "status", "status")
		e.refused(t, "doctor", "doctor")
	})
	t.Run("without the flag both lines, sprites first", func(t *testing.T) {
		e := sprSandbox(t)
		e.write(e.body("", ""), true, "")
		stderr := e.refused(t, "no flag", "run", "heavy", "--", "echo", "ran")
		i, j := strings.Index(stderr, sprRefusal), strings.Index(stderr, exGateMsg)
		if j < 0 || i > j {
			t.Fatalf("want the sprites line, then the experimental line: %q", stderr)
		}
	})
	t.Run("a pool naming sprites without a table", func(t *testing.T) {
		e := sprSandbox(t)
		e.write("[host]\nexperimental_remote = true\n[pools.heavy]\nproviders = [\"sprites\", \"local\"]\n", true, "")
		e.refused(t, "pool only", "config", "check")
	})
	t.Run("a production host cannot bypass it", func(t *testing.T) {
		e := sprSandbox(t)
		// no [test] table: the bypass key exists nowhere else, and no variable sets it
		e.write(e.body("[host]\nexperimental_remote = true\nallow_sprites = true\n", ""), false, "")
		if r := e.h.Run("run", "heavy", "--", "echo", "ran"); r.Code != 78 || !strings.Contains(r.Stderr, "unknown key host.allow_sprites") || len(e.f.Calls()) != 0 {
			t.Fatalf("[host] allow_sprites: %+v", r)
		}
		e.untouched(t, "[host] allow_sprites")
		e.write(e.body("[host]\nexperimental_remote = true\n", ""), false, "")
		for _, kv := range []string{"BITC_ALLOW_SPRITES=1", "BITC_TEST_ALLOW_SPRITES=true", "BITC_SPRITES=1"} {
			r := e.h.RunWith([]string{kv}, "config", "check")
			if r.Code != 78 || !strings.Contains(r.Stderr, sprRefusal) {
				t.Fatalf("%s: %+v", kv, r)
			}
		}
		// the [test] bypass is honoured only with loopback endpoints: a real API base is refused
		e.write(e.body("[host]\nexperimental_remote = true\n", "https://api.sprites.dev"), true, "allow_sprites = true\n")
		e.refused(t, "[test] bypass, real endpoint", "config", "check")
	})
	t.Run("the [test] bypass with a loopback fake", func(t *testing.T) {
		e := sprSandbox(t)
		for _, f := range []string{e.token, e.key} { // readable now: the bypass reads them
			if err := os.Chmod(f, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		e.write(e.body("[host]\nexperimental_remote = true\n", ""), true, "allow_sprites = true\n")
		if r := e.h.Run("config", "check"); r.Code != 0 || strings.Contains(r.Stderr, "Sprites jobs are refused") {
			t.Fatalf("bypass: %+v", r)
		}
	})
}
