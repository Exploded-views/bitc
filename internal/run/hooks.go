package run

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/Exploded-views/bitc/internal/config"
	"github.com/Exploded-views/bitc/internal/exit"
)

// HookKind names the pipeline step a hook runs at (spec 6.5).
type HookKind string

const (
	PreRun  HookKind = "pre_run"
	EnvHook HookKind = "env"
	PostRun HookKind = "post_run"
)

// display names the hook in bitc: lines: "env hook", "pre_run hook", "post_run hook"
// (spec 6.5: `<hook> hook could not start`).
func (k HookKind) display() string { return string(k) + " hook" }

// starved names the hook inside STARVED: `(env hook: <why>)`, `(pre_run: <why>)` (spec 4).
func (k HookKind) starved() string {
	if k == EnvHook {
		return "env hook"
	}
	return string(k)
}

// HookCall is one hook invocation.
type HookCall struct {
	Kind     HookKind
	Hook     config.Hook
	Pool     string
	Vars     config.Vars
	Env      []string
	Worktree string
	Stderr   io.Writer
}

type capped struct {
	b   bytes.Buffer
	max int
}

func (c *capped) Write(p []byte) (int, error) {
	if room := c.max - c.b.Len(); room > 0 {
		if len(p) > room {
			c.b.Write(p[:room])
		} else {
			c.b.Write(p)
		}
	}
	return len(p), nil
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return s
}

// hookWaitDelay bounds how long Wait lingers after the hook process ended or was killed.
const hookWaitDelay = time.Second

// RunHook runs a hook argv with the job's env, cwd = work-tree top, stdin /dev/null and its
// timeout, and maps the result (spec 6.5).
func RunHook(ctx context.Context, c HookCall) ([]KV, error) {
	name := c.Kind.display()
	argv := make([]string, 0, len(c.Hook.Argv))
	for _, t := range c.Hook.Argv {
		v, err := t.Expand(c.Vars)
		if err != nil {
			return nil, exit.Configf("%s: %s: %v", c.Pool, name, err)
		}
		argv = append(argv, v)
	}
	if strings.Contains(argv[0], "/") && !filepath.IsAbs(argv[0]) {
		argv[0] = filepath.Join(c.Worktree, argv[0])
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir, cmd.Env = c.Worktree, c.Env
	stdout, stderr := &capped{max: 1 << 20}, &capped{max: 64 << 10}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// Wait must not outlive the hook itself: a background or daemonized child holding the output
	// pipes would otherwise stall the call (or, after a kill, hang it) for as long as it lives.
	cmd.WaitDelay = hookWaitDelay
	if err := cmd.Start(); err != nil {
		return nil, exit.Unavailablef("%s: %s could not start: %v", c.Pool, name, err)
	}
	done := make(chan error, 1)
	exit.Go(func() { done <- cmd.Wait() })
	timeout := c.Hook.Timeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	var err error
	select {
	case err = <-done:
		if errors.Is(err, exec.ErrWaitDelay) { // the hook exited 0; only pipe holders lingered
			err = nil
		}
	case <-time.After(timeout):
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
		return nil, exit.Unavailablef("%s: %s timed out after %s: %s", c.Pool, name, timeout, firstLine(stderr.b.String()))
	case <-ctx.Done():
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
		return nil, ctx.Err()
	}
	code := 0
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code = ee.ExitCode()
		if code < 0 {
			code = 128 + int(ee.Sys().(syscall.WaitStatus).Signal())
		}
	} else if err != nil {
		return nil, exit.Unavailablef("%s: %s: %v", c.Pool, name, err)
	}
	why := firstLine(stderr.b.String())
	switch {
	case code == 0:
	case contains(c.Hook.RefuseExit, code):
		return nil, exit.Refused(c.Pool, why)
	case code == 75:
		return nil, exit.Starved(c.Pool, c.Kind.starved()+": "+why)
	default:
		return nil, exit.Unavailablef("%s: %s exited %d: %s", c.Pool, name, code, why)
	}
	if c.Kind != EnvHook {
		return nil, nil
	}
	kv, perr := ParseEnvOutput(stdout.b.Bytes())
	if perr != nil {
		return nil, exit.Unavailablef("%s: %s printed unparsable output (%v): %s", c.Pool, name, perr, why)
	}
	return kv, nil
}

func contains(list []int, n int) bool {
	for _, x := range list {
		if x == n {
			return true
		}
	}
	return false
}

// WrapArgv prefixes the first wrap rule whose when_worktree matches the work tree (spec 6.5).
// It always returns a new slice, never argv itself, so a caller may change the result without
// touching argv (and the reverse).
func WrapArgv(eff *config.Effective, pool *config.Pool, vars config.Vars, argv []string) ([]string, error) {
	for _, w := range pool.Hooks.Wrap {
		ok, _ := config.Match(w.WhenWorktree, eff.Worktree)
		if !ok {
			continue
		}
		var out []string
		for i, t := range w.Argv {
			v, err := t.Expand(vars)
			if err != nil {
				return nil, exit.Configf("pools.%s.hooks.wrap: %v", pool.Name, err)
			}
			if i == 0 && strings.Contains(v, "/") && !filepath.IsAbs(v) {
				v = filepath.Join(eff.Worktree, v)
			}
			out = append(out, v)
		}
		return append(out, argv...), nil
	}
	return append([]string(nil), argv...), nil
}
