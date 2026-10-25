package run

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Exploded-views/bitc/internal/admit"
	"github.com/Exploded-views/bitc/internal/config"
	"github.com/Exploded-views/bitc/internal/exit"
)

func tpl(m map[string]string) map[string]config.Template {
	out := map[string]config.Template{}
	for k, v := range m {
		out[k] = config.Template{Raw: v}
	}
	return out
}

func envOf(env []string) map[string]string {
	m := map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		m[k] = v
	}
	return m
}

func TestBaseEnvOrder(t *testing.T) {
	eff := &config.Effective{Host: config.Host{StateDir: "/s"}, Vars: config.Vars{"base": "47410"}, Worktree: "/w", Cwd: "/w"}
	pool := &config.Pool{Name: "db", Slots: 2,
		DefaultsEnvUnset: []string{"NODE_OPTIONS"},
		EnvDefault:       tpl(map[string]string{"WORKERS": "3", "KEEP": "default"}),
		Env:              tpl(map[string]string{"URL": "pg://localhost:{slot+base}/t", "NODE_OPTIONS": "--x"}),
		EnvUnset:         []string{"DROP"}}
	hold := &admit.Hold{Pool: "db", Slot: 2, Ticket: "1-0000000000000000001-00000000.json", Token: "tok"}
	in := EnvInput{Caller: []string{"NODE_OPTIONS=--old", "KEEP=caller", "DROP=x", "BITC_HOLD=forged", "PATH=/bin"},
		Eff: eff, Pool: pool, Hold: hold, Job: "j1"}
	in.Vars = TemplateVars(eff, pool, hold, "j1")
	env, err := BaseEnv(in)
	if err != nil {
		t.Fatal(err)
	}
	m := envOf(env)
	want := map[string]string{"URL": "pg://localhost:47412/t", "NODE_OPTIONS": "--x", "KEEP": "caller", "WORKERS": "3",
		"BITC_POOL": "db", "BITC_SLOT": "db.2", "BITC_HOLD": "db:1-0000000000000000001-00000000.json:tok",
		"BITC_JOB": "j1", "BITC_DIR": "/s", "PATH": "/bin"}
	for k, v := range want {
		if m[k] != v {
			t.Errorf("%s = %q, want %q", k, m[k], v)
		}
	}
	if _, ok := m["DROP"]; ok {
		t.Error("env_unset ignored")
	}
}

func TestRunHookResults(t *testing.T) {
	dir := t.TempDir()
	script := func(name, body string) string {
		p := filepath.Join(dir, name)
		_ = os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755)
		return p
	}
	call := func(kind HookKind, argv0 string, refuse []int) ([]KV, error) {
		return RunHook(context.Background(), HookCall{Kind: kind, Pool: "e2e", Worktree: dir, Env: os.Environ(),
			Hook: config.Hook{Name: string(kind), Argv: []config.Template{{Raw: argv0}}, Timeout: 500 * time.Millisecond, RefuseExit: refuse}})
	}
	kv, err := call(EnvHook, script("ok", "echo A=1; echo 'export B=\"two\"'"), []int{64})
	if err != nil || len(kv) != 2 || kv[1].Value != "two" {
		t.Fatal(kv, err)
	}
	cases := []struct {
		body string
		code int
		line string
	}{
		{"echo 'slot is outside 1..40' >&2; exit 64", 64, "bitc: e2e: refused: slot is outside 1..40"},
		{"echo 'stack busy' >&2; exit 75", 75, "STARVED e2e (env hook: stack busy)"},
		{"echo 'boom' >&2; exit 3", 69, "bitc: e2e: env hook exited 3: boom"},
		{"sleep 5", 69, "bitc: e2e: env hook timed out after 500ms"},
		{"echo 'A=$HOME'", 69, "bitc: e2e: env hook printed unparsable output (line 1"},
	}
	for i, c := range cases {
		_, err := call(EnvHook, script("h"+string(rune('a'+i)), c.body), []int{64})
		if exit.Code(err) != c.code || !strings.HasPrefix(exit.As(err).Line, c.line) {
			t.Errorf("%q: %v", c.body, err)
		}
	}
	_, err = call(PreRun, filepath.Join(dir, "missing"), []int{64})
	if exit.Code(err) != 69 || !strings.Contains(err.Error(), "bitc: e2e: pre_run hook could not start") {
		t.Fatal(err)
	}
	_, err = call(PreRun, script("p75", "echo busy >&2; exit 75"), nil)
	if exit.Code(err) != 75 || err.Error() != "STARVED e2e (pre_run: busy)" { // no " hook" inside STARVED
		t.Fatal(err)
	}
}

func TestWrapArgv(t *testing.T) {
	eff := &config.Effective{Worktree: "/srv/worktrees/agent-1"}
	pool := &config.Pool{Hooks: config.Hooks{Wrap: []config.WrapRule{
		{WhenWorktree: "**/worktrees/other-*", Argv: []config.Template{{Raw: "no"}}},
		{WhenWorktree: "**/worktrees/agent-*", Argv: []config.Template{{Raw: "scripts/with-db.sh"}, {Raw: "{worktree}"}}},
	}}}
	got, err := WrapArgv(eff, pool, config.Vars{"worktree": eff.Worktree}, []string{"npm", "test"})
	if err != nil || strings.Join(got, " ") != "/srv/worktrees/agent-1/scripts/with-db.sh /srv/worktrees/agent-1 npm test" {
		t.Fatal(got, err)
	}
	in := []string{"npm", "test"} // no rule matches: still a new slice
	got, _ = WrapArgv(&config.Effective{Worktree: "/elsewhere"}, pool, config.Vars{}, in)
	got[0] = "changed"
	if in[0] != "npm" {
		t.Fatal("WrapArgv returned the caller's slice")
	}
}
