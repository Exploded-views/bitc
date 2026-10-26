package conformance

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Exploded-views/bitc/conformance/harness"
	"github.com/Exploded-views/bitc/internal/provider/sprites"
	"github.com/Exploded-views/bitc/internal/state"
)

// spr2Canary is a forwarded-env value: no request of a cleanup may carry it.
const spr2Canary = "CANARY-sprites-cleanup-5d1e"

// spr2Host is a production-shaped sprites host (no [test] allow_sprites; the [test] table only
// holds the accounting dir) with the flag, a forwarded variable, and a deploy key file of mode 000.
func spr2Host(e *sprEnv, flag bool) {
	if err := os.Chmod(e.token, 0o600); err != nil { // the cleanup reads the API token
		e.h.T.Fatal(err)
	}
	if err := os.Chmod(e.key, 0o000); err != nil { // never opened: mode 000 must cause no error
		e.h.T.Fatal(err)
	}
	host := ""
	if flag {
		host = "[host]\nexperimental_remote = true\n"
	}
	body := strings.Replace(e.body(host, ""), "worktrees = [\"**/app\"]\n",
		"worktrees = [\"**/app\"]\nforward_env_allow = [\"CANARY_VALUE\"]\n", 1)
	e.write(body, true, "")
	e.h.Cwd = e.h.Repo("w/app", map[string]string{".bitc.toml": "version = 1\n[remote]\nforward_env = [\"CANARY_VALUE\"]\n" +
		"[pools.heavy.remote]\nallow = [[\"echo\", \"*\"]]\n"})
}

// cleanupCalls fails on any request other than the list, an organization read and a delete of
// one Sprite (no create, exec, attach, fs, policy, checkpoint or egress call), and on any request
// carrying the canary. It returns the deleted names.
func cleanupCalls(t *testing.T, e *sprEnv) []string {
	t.Helper()
	del := regexp.MustCompile(`^/v1/sprites/[^/]+$`)
	var deleted []string
	for _, c := range e.f.Calls() {
		switch {
		case c.Method == "GET" && (c.Path == "/v1/sprites" || c.Path == "/v1/organization"):
		case c.Method == "DELETE" && del.MatchString(c.Path):
			deleted = append(deleted, strings.TrimPrefix(c.Path, "/v1/sprites/"))
		default:
			t.Errorf("a cleanup made a %s %s call", c.Method, c.Path)
		}
		if strings.Contains(c.Path+c.RawQuery+string(c.Body), spr2Canary) {
			t.Errorf("the forwarded env reached the API: %s %s", c.Method, c.Path)
		}
	}
	return deleted
}

func TestSPR2_SweepCleansUpSpritesOnly(t *testing.T) {
	canary := []string{"CANARY_VALUE=" + spr2Canary}
	t.Run("sweep lists, owner-checks and deletes, and nothing else", func(t *testing.T) {
		e := sprSandbox(t)
		spr2Host(e, true)
		secret, err := state.InstallSecret(e.acct)
		if err != nil {
			t.Fatal(err)
		}
		e.planted = true
		past := time.Now().Add(-time.Hour)
		own := sprites.EncodeName(secret, "bitc", sha16(secret), "cpu8", "0000aaaa", past)
		forged := sprites.EncodeName(secret, "bitc", sha16(secret), "cpu8", "0000bbbb", past)
		forged = forged[:len(forged)-1] + map[bool]string{true: "1", false: "0"}[!strings.HasSuffix(forged, "1")]
		other := sprites.EncodeName(otherSecret, "bitc", otherInstall, "cpu8", "0000cccc", past)
		for _, n := range []string{own, forged, other} {
			e.f.AddSprite(n, e.f.Org(), past.Add(-time.Hour))
		}
		// a ledger launch line for the forged name: a name (or a ledger line) is never enough
		e.h.Write(filepath.Join(e.acct, "remote", "sprites", "ledger.jsonl"),
			`{"ts":"2026-01-01T00:00:00Z","event":"launch","name":"`+forged+`"}`+"\n", 0o600)
		r := e.h.RunWith(canary, "sweep", "--foreign")
		if r.Code != 0 || strings.Contains(r.Stderr, e.key) || strings.Contains(r.Stderr, "sprites provider is refused") || strings.Contains(r.Stderr, "Sprites jobs are refused") {
			t.Fatalf("sweep: %+v", r)
		}
		if got := cleanupCalls(t, e); len(got) != 1 || got[0] != own {
			t.Fatalf("deleted %q, want only the owner-verified %s", got, own)
		}
		if !isLiveName(e, forged) || !isLiveName(e, other) || isLiveName(e, own) {
			t.Fatalf("live after the sweep: %v", e.f.Live())
		}
		// doctor --remote: lists, says jobs are refused, and makes no other call. The key file is
		// now mode 040, which the key-file check would name: it is not even checked.
		if err := os.Chmod(e.key, 0o040); err != nil {
			t.Fatal(err)
		}
		r = e.h.RunWith(canary, "doctor", "--remote")
		if !strings.Contains(r.Stdout, "warn sprites jobs: refused in this release; bitc sweep only lists, owner-checks and deletes Sprites\n") ||
			!strings.Contains(r.Stdout, "ok sprites sweep timer:") || strings.Contains(r.Stderr, e.key) {
			t.Fatalf("doctor --remote: %+v", r)
		}
		cleanupCalls(t, e)
	})
	t.Run("job paths stay refused", func(t *testing.T) {
		e := sprSandbox(t)
		spr2Host(e, true)
		for _, args := range [][]string{{"run", "heavy", "--", "echo", "ran"}, {"run", "--detach", "--name", "j1", "heavy", "--", "echo", "ran"},
			{"status"}, {"doctor"}, {"config", "check"}} {
			r := e.h.RunWith(canary, args...)
			if r.Code != 78 || !strings.Contains(r.Stderr, sprRefusal+"\n") || strings.Contains(r.Stdout, "ran") {
				t.Fatalf("%v: %+v", args, r)
			}
		}
		if c := e.f.Calls(); len(c) != 0 {
			t.Fatalf("a job path called the API: %v", c)
		}
		if ents, _ := os.ReadDir(filepath.Join(e.h.State, "jobs")); len(ents) != 0 {
			t.Fatalf("a detached job was recorded: %v", ents)
		}
	})
	t.Run("cleanup still needs the flag", func(t *testing.T) {
		e := sprSandbox(t)
		spr2Host(e, false)
		for _, args := range [][]string{{"sweep"}, {"doctor", "--remote"}} {
			r := e.h.RunWith(canary, args...)
			exRefused(t, strings.Join(args, " "), r, true)
			if strings.Contains(r.Stderr, "Sprites jobs are refused") {
				t.Fatalf("%v: the jobs line for a cleanup: %q", args, r.Stderr)
			}
		}
		if c := e.f.Calls(); len(c) != 0 {
			t.Fatalf("no flag, but the API was called: %v", c)
		}
	})
	t.Run("the sweep timer's command succeeds", func(t *testing.T) {
		doc := string(must(os.ReadFile("../docs/sweep-timer.md")))
		m := regexp.MustCompile(`(?m)^ExecStart=\S*/bitc (.+)$`).FindStringSubmatch(doc)
		if m == nil {
			t.Fatal("docs/sweep-timer.md has no ExecStart=.../bitc line")
		}
		e := sprSandbox(t)
		spr2Host(e, true)
		if r := e.h.Run(strings.Fields(m[1])...); r.Code != 0 {
			t.Fatalf("%q: %+v", m[1], r)
		}
		cleanupCalls(t, e)
	})
}

func isLiveName(e *sprEnv, name string) bool {
	for _, n := range e.f.Live() {
		if n == name {
			return true
		}
	}
	return false
}

// keep harness imported for the helpers this file shares with spr_test.go
var _ = harness.Q
