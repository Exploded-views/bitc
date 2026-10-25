// Package run is the `bitc run` pipeline (spec 6) and the local runner.
package run

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/Exploded-views/bitc/internal/admit"
	"github.com/Exploded-views/bitc/internal/classify"
	"github.com/Exploded-views/bitc/internal/config"
	"github.com/Exploded-views/bitc/internal/exit"
	"github.com/Exploded-views/bitc/internal/redact"
	"github.com/Exploded-views/bitc/internal/state"
)

// Invocation is one `bitc run` call (or the supervisor's replay of one).
type Invocation struct {
	Flags  config.Flags
	Args   []string
	Stdin  *os.File
	Stdout *os.File
	Stderr *os.File
	Env    []string
	Cwd    string
	// Detached is the supervisor's replay (spec 7): SIGHUP is neither a cancel nor forwarded.
	Detached bool
	// Pending, if set, is the supervisor's early signal channel; runLocal takes it over.
	Pending chan os.Signal
}

// Signalled is a context cause: the run was cancelled as if by this signal (`cancel --kill`).
type Signalled syscall.Signal

func (s Signalled) Error() string { return "interrupted by " + syscall.Signal(s).String() }

func signalOf(ctx context.Context) syscall.Signal {
	var s Signalled
	if errors.As(context.Cause(ctx), &s) {
		return syscall.Signal(s)
	}
	return 0
}

// Prepared is what steps 1-4 decided.
type Prepared struct {
	Args   RunArgs
	Eff    *config.Effective
	Pool   *config.Pool
	Layout state.Layout
	Job    string
	Redact *redact.Set
}

// Outcome is the final exit code and result.json content.
type Outcome struct {
	Code   int
	Result Result
}

// RemoteWalk runs a remote provider (internal/remote sets it). handled=false means "try the
// next"; err is then this provider's failure, for the no-provider-left rule.
var RemoteWalk func(ctx context.Context, inv Invocation, p *Prepared, provider string, stderr io.Writer) (Outcome, bool, error)

// NoProviderLeft maps the last provider failure to 69 or 75 (set by internal/remote).
var NoProviderLeft func(pool string, last error) error

// Main runs the whole pipeline in the foreground.
func Main(ctx context.Context, inv Invocation) int {
	red := redact.New()
	stderr := red.Writer(inv.Stderr)
	defer stderr.Flush()
	ra, _ := ParseRunArgs(inv.Args) // --result is known even when a later step fails
	writeResult := func(r Result) {
		if ra.Result == "" {
			return
		}
		r.Reason = red.String(r.Reason)
		if err := WriteResult(ra.Result, r); err != nil {
			fmt.Fprintf(stderr, "bitc: cannot write %s: %v\n", ra.Result, err)
		}
	}
	p, err := Prepare(inv, red)
	if err != nil {
		fmt.Fprintln(stderr, exit.As(err).Line)
		writeResult(resultOf(err, ra.Pool, ra.Name)) // never leave an earlier run's result.json
		return exit.Code(err)
	}
	for _, w := range p.Eff.Warnings {
		fmt.Fprintln(stderr, "bitc: "+w)
	}
	// Registered until the result is written: a signal after the command ended is absorbed
	// instead of killing bitc before result.json exists.
	keep := make(chan os.Signal, 4)
	signal.Notify(keep, cancels(inv)...)
	defer signal.Stop(keep)
	out := Execute(ctx, inv, p, stderr)
	writeResult(out.Result)
	return out.Code
}

// Prepare runs steps 1-4: parse, load and merge config, narrow, resolve the pool, refuse rules.
// Credentials join red as soon as the config loads.
func Prepare(inv Invocation, red *redact.Set) (*Prepared, error) {
	ra, err := ParseRunArgs(inv.Args)
	if err != nil {
		return nil, err
	}
	if ra.Name != "" && !jobNameRE.MatchString(ra.Name) {
		return nil, exit.Usagef("invalid job name %q ([A-Za-z0-9._-]+)", ra.Name)
	}
	eff, err := config.Load(config.LoadOptions{Flags: inv.Flags, Env: inv.Env, Cwd: inv.Cwd})
	if err != nil {
		return nil, err
	}
	for _, s := range eff.Secrets {
		red.Add(s)
	}
	if err := eff.ApplyRunFlags(config.RunFlags{Wait: ra.Wait, Local: ra.Local, Provider: ra.Provider, Artifacts: ra.Artifacts}); err != nil {
		return nil, err
	}
	pool, err := eff.Resolve(ra.Pool)
	if err != nil {
		return nil, err
	}
	cctx := classify.Context{Cwd: eff.Cwd, LogicalCwd: eff.LogicalCwd, Worktree: eff.Worktree}
	if reason, refused := classify.Check(pool.Refuse, ra.Argv, cctx); refused {
		return nil, exit.Refused(pool.Name, reason)
	}
	layout, err := state.Open(eff.Host.StateDir)
	if err != nil {
		return nil, err
	}
	job := ra.Name
	if job == "" {
		job = randJob()
	}
	return &Prepared{Args: ra, Eff: eff, Pool: pool, Layout: layout, Job: job, Redact: red}, nil
}

// Execute runs step 6 (provider walk) and, for local, steps 7-11.
func Execute(ctx context.Context, inv Invocation, p *Prepared, stderr io.Writer) Outcome {
	var last error
	for _, prov := range p.Pool.Providers {
		if prov == "local" {
			return runLocal(ctx, inv, p, stderr)
		}
		if RemoteWalk == nil {
			Debugf(p, stderr, "%s: %s: not available in this build", p.Pool.Name, prov)
			last = exit.Unavailablef("%s: no provider left (%s is not available in this build)", p.Pool.Name, prov)
			continue
		}
		out, handled, err := RemoteWalk(ctx, inv, p, prov, stderr)
		if handled {
			return out
		}
		Debugf(p, stderr, "%s: %s: %v", p.Pool.Name, prov, err)
		last = err
	}
	if last == nil {
		last = exit.Unavailablef("%s: no provider left", p.Pool.Name)
	}
	if NoProviderLeft != nil {
		last = NoProviderLeft(p.Pool.Name, last)
	}
	fmt.Fprintln(stderr, exit.As(last).Line)
	return Outcome{Code: exit.Code(last), Result: resultOf(last, p.Pool.Name, p.Job)}
}

// cancels are the signals that interrupt a run: INT/TERM/HUP/QUIT, without HUP for a detached
// supervisor (spec 7: the job survives SIGHUP).
func cancels(inv Invocation) []os.Signal {
	if inv.Detached {
		return []os.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT}
	}
	return []os.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT}
}

// signalCtx cancels on sigs until stop is called, remembering the signal. pending (optional) is a
// channel the caller registered earlier: it is stopped once ours is registered, and a signal
// already in it cancels at once. stop is idempotent.
func signalCtx(parent context.Context, sigs []os.Signal, pending chan os.Signal) (context.Context, func() syscall.Signal) {
	ctx, stop, _ := signalCtxPeek(parent, sigs, pending)
	return ctx, stop
}

// signalCtxPeek is signalCtx plus peek, which reports the remembered signal without stopping the
// registration (runLocal keeps it for its whole life, so no signal ever finds the default action).
func signalCtxPeek(parent context.Context, sigs []os.Signal, pending chan os.Signal) (context.Context, func() syscall.Signal, func() syscall.Signal) {
	ctx, cancel := context.WithCancel(parent)
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, sigs...)
	var mu sync.Mutex
	var got syscall.Signal
	if pending != nil {
		signal.Stop(pending)
		select {
		case s := <-pending:
			got = s.(syscall.Signal)
			cancel()
		default:
		}
	}
	stopped := make(chan struct{})
	exit.Go(func() {
		select {
		case s := <-ch:
			mu.Lock()
			if got == 0 {
				got = s.(syscall.Signal)
			}
			mu.Unlock()
			cancel()
		case <-stopped:
		}
	})
	peek := func() syscall.Signal {
		mu.Lock()
		defer mu.Unlock()
		return got
	}
	var once sync.Once
	return ctx, func() syscall.Signal {
		once.Do(func() {
			signal.Stop(ch)
			close(stopped)
			cancel()
		})
		return peek()
	}, peek
}

func runLocal(parent context.Context, inv Invocation, p *Prepared, stderr io.Writer) Outcome {
	eff, pool := p.Eff, p.Pool
	histWarned := false // one warning per call, however many events fail
	hist := func(e state.Event) {
		if e.Event != "reaped" {
			e.Pool, e.Job, e.Pid, e.Argv, e.Cwd = pool.Name, p.Job, os.Getpid(), p.Args.Argv, eff.Cwd
		}
		e.Provider = "local"
		if err := state.AppendHistory(p.Layout.History(), e, p.Redact); err != nil && !histWarned {
			histWarned = true
			fmt.Fprintf(stderr, "bitc: cannot write %s: %v\n", p.Layout.History(), err)
		}
	}
	sigs := cancels(inv)
	// One cancel-signal registration lives from here until runLocal returns: after Exec, during
	// post_run and the caller's result writing, a signal must never find the default action.
	ctx, stop, peek := signalCtxPeek(parent, sigs, inv.Pending)
	defer stop()
	interrupted := func() (Outcome, bool) {
		sig := peek()
		if sig == 0 {
			sig = signalOf(parent) // the supervisor cancelled parent (`cancel --kill`)
		}
		if sig != 0 {
			return Outcome{Code: 128 + int(sig), Result: Result{Origin: "bitc", Outcome: "interrupted",
				ExitCode: 128 + int(sig), Pool: pool.Name, Provider: "local", Job: p.Job}}, true
		}
		return Outcome{}, false
	}
	failed := func(err error) Outcome {
		if out, ok := interrupted(); ok {
			return out
		}
		e := exit.As(err)
		fmt.Fprintln(stderr, e.Line)
		return Outcome{Code: e.Code, Result: resultOf(err, pool.Name, p.Job)}
	}
	if ctx.Err() != nil { // a cancel that reached the supervisor before this handler: run nothing
		if out, ok := interrupted(); ok {
			return out
		}
	}
	vars := TemplateVars(eff, pool, nil, p.Job)
	for _, h := range scaled(eff, pool.Hooks.PreRun) { // 7
		if _, err := RunHook(ctx, HookCall{Kind: PreRun, Hook: h, Pool: pool.Name, Vars: vars, Env: inv.Env, Worktree: eff.Worktree, Stderr: stderr}); err != nil {
			return failed(err)
		}
	}
	hold, err := admit.Acquire(ctx, admit.Request{ // 8
		Layout: p.Layout, Eff: eff, Pool: pool, Argv: p.Args.Argv, Job: p.Job,
		CallerHold: lookup(inv.Env, "BITC_HOLD"), CallerEnv: inv.Env, Stderr: stderr,
		OnReap: func(rp, job string) { hist(state.Event{Event: "reaped", Pool: rp, Job: job}) },
	})
	if err != nil {
		if exit.Code(err) == exit.TempFail {
			hist(state.Event{Event: "starved", Outcome: "starved", Reason: exit.As(err).Reason})
		}
		return failed(err)
	}
	defer hold.Release()
	vars = TemplateVars(eff, pool, hold, p.Job)
	in := EnvInput{Caller: inv.Env, Eff: eff, Pool: pool, Hold: hold, Job: p.Job, Vars: vars}
	env, err := BaseEnv(in)
	if err != nil {
		return failed(err)
	}
	if pool.Hooks.Env != nil { // 9
		h := scaled(eff, []config.Hook{*pool.Hooks.Env})[0]
		kv, err := RunHook(ctx, HookCall{Kind: EnvHook, Hook: h, Pool: pool.Name, Vars: vars, Env: env, Worktree: eff.Worktree, Stderr: stderr})
		if err != nil {
			return failed(err)
		}
		env = ApplyHookEnv(env, kv, BitcVars(in))
	}
	argv, err := WrapArgv(eff, pool, vars, p.Args.Argv) // 10
	if err != nil {
		return failed(err)
	}
	// gap covers the hand-off from signalCtx to Exec's own handler: without it a SIGTERM
	// between stop() and Exec's signal.Notify would kill bitc with no cleanup.
	gap := make(chan os.Signal, 4)
	signal.Notify(gap, sigs...)
	if out, ok := interrupted(); ok {
		signal.Stop(gap)
		return out
	}
	waitMS := hold.WaitMS
	hist(state.Event{Event: "start", Slot: hold.Slot, Nested: hold.Nested, WaitMS: &waitMS})
	t0 := time.Now()
	res, err := Exec(parent, ExecSpec{
		Argv: argv, Env: env, Dir: inv.Cwd, Stdin: inv.Stdin, Stdout: inv.Stdout, Stderr: inv.Stderr,
		OnSignal: pool.OnSignal, Grace: eff.Scale(eff.Host.Grace), MaxSignalWait: eff.Scale(eff.Host.MaxSignalWait),
		Started:  func(pid, pgid int, start uint64) { _ = hold.SetProcess(pid, pgid, start) },
		Pending:  gap,          // Exec stops it once its own handler is registered
		NoHangup: inv.Detached, // spec 7: a hangup neither escalates nor reaches the command
	})
	if err != nil {
		return failed(err)
	}
	if res.StartErr != nil {
		fmt.Fprintf(stderr, "bitc: %s: %v\n", argv[0], res.StartErr)
	}
	runMS := time.Since(t0).Milliseconds()
	code := res.Code
	outcome := "ok"
	switch {
	case res.Interrupted != 0 && code == 128+int(res.Interrupted):
		outcome = "interrupted"
	case code != 0:
		outcome = "command-failed"
	}
	hist(state.Event{Event: "end", Slot: hold.Slot, Nested: hold.Nested, RunMS: &runMS, Exit: &code, Outcome: outcome})
	postEnv := append(append([]string{}, env...), "BITC_EXIT="+strconv.Itoa(code))
	// A signal during post_run cuts it short (the hook's group is killed); the command's own
	// code and result still come back. A fresh context: a signal that ended the command must
	// not also skip its cleanup hooks.
	pctx, pstop := signalCtx(context.Background(), sigs, nil)
	defer pstop()
	for _, h := range scaled(eff, pool.Hooks.PostRun) { // 11: failure only warns
		if _, err := RunHook(pctx, HookCall{Kind: PostRun, Hook: h, Pool: pool.Name, Vars: vars, Env: postEnv, Worktree: eff.Worktree, Stderr: stderr}); err != nil {
			if pctx.Err() != nil {
				fmt.Fprintf(stderr, "bitc: %s: post_run hook cut short by a signal\n", pool.Name)
				break
			}
			fmt.Fprintf(stderr, "bitc: %s: post_run hook failed: %s\n", pool.Name, exit.As(err).Reason)
		}
	}
	origin := "command"
	if outcome == "interrupted" {
		origin = "bitc"
	}
	return Outcome{Code: code, Result: Result{Origin: origin, Outcome: outcome, ExitCode: code, Pool: pool.Name,
		Slot: hold.Slot, Provider: "local", Job: p.Job, HoldStarted: hold.Started.UTC().Format(time.RFC3339Nano),
		WaitMS: hold.WaitMS, RunMS: runMS}}
}

func scaled(eff *config.Effective, hooks []config.Hook) []config.Hook {
	out := make([]config.Hook, len(hooks))
	for i, h := range hooks {
		h.Timeout = eff.Scale(h.Timeout)
		out[i] = h
	}
	return out
}

// resultOf is result.json for a bitc outcome (origin = bitc).
func resultOf(err error, pool, job string) Result {
	e := exit.As(err)
	return Result{Origin: "bitc", Outcome: e.Outcome, ExitCode: e.Code, Reason: e.Reason, Pool: pool, Provider: "local", Job: job}
}

func lookup(env []string, key string) string {
	for i := len(env) - 1; i >= 0; i-- {
		if len(env[i]) > len(key) && env[i][:len(key)] == key && env[i][len(key)] == '=' {
			return env[i][len(key)+1:]
		}
	}
	return ""
}

var jobNameRE = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

func randJob() string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return fmt.Sprintf("%x", b)
}

// Debugf prints a "bitc: debug:" line at log level debug (BITC_LOG=debug or -v).
func Debugf(p *Prepared, w io.Writer, format string, a ...any) {
	if p.Eff.LogLevel == "debug" {
		fmt.Fprintf(w, "bitc: debug: "+format+"\n", a...)
	}
}

// InfoWriter is w, or io.Discard under BITC_LOG=quiet. Only informational lines use it.
func (p *Prepared) InfoWriter(w io.Writer) io.Writer {
	if p.Eff.LogLevel == "quiet" {
		return io.Discard
	}
	return w
}
