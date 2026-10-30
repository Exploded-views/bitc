package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Exploded-views/bitc/internal/config"
	"github.com/Exploded-views/bitc/internal/exit"
	"github.com/Exploded-views/bitc/internal/jobs"
	"github.com/Exploded-views/bitc/internal/proc"
	"github.com/Exploded-views/bitc/internal/redact"
	"github.com/Exploded-views/bitc/internal/run"
	"github.com/Exploded-views/bitc/internal/state"
)

func init() {
	Register("__supervise", supervise)
	Register("wait", waitCmd)
	Register("logs", logsCmd)
	Register("cancel", cancelCmd)
}

// supervise replays a detached run (spec 7). Its stdout and stderr are the job's log.
func supervise(ctx context.Context, g config.Flags, args []string, std Std) (code int) {
	// Handlers before anything else. HUP (inherited ignored from detach) is caught and dropped:
	// the job survives a hangup, and a caught signal resets to default in the command. INT, TERM
	// and QUIT wait in early until runLocal takes it over, so a cancel during startup is 128+N,
	// never a lost job. SIGUSR1 is `cancel --kill`: cancel the run, which escalates to terminate.
	signal.Notify(make(chan os.Signal, 1), syscall.SIGHUP)
	early := make(chan os.Signal, 4)
	signal.Notify(early, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)
	defer signal.Stop(early)
	// keep stays registered until the deferred terminate, result and status writes are done:
	// runLocal hands `early` over and drops its own registration when it returns, and a cancel
	// in that window must not kill the supervisor with the default action (deferred first, so
	// it runs last).
	keep := make(chan os.Signal, 4)
	signal.Notify(keep, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)
	defer signal.Stop(keep)
	ctx, kill := context.WithCancelCause(ctx)
	defer kill(nil)
	usr1 := make(chan os.Signal, 1)
	signal.Notify(usr1, syscall.SIGUSR1)
	exit.Go(func() {
		select {
		case <-usr1:
			kill(run.Signalled(syscall.SIGTERM))
		case <-ctx.Done():
		}
	})
	if len(args) != 2 {
		return exit.Usage
	}
	wt8, name, _ := strings.Cut(args[0], "/")
	gen, _ := strconv.Atoi(args[1])
	l := state.Layout{Root: os.Getenv("BITC_DIR")}
	rec := jobs.Open(l, wt8, name)
	sp, err := rec.ReadSpec(gen)
	if err != nil {
		fmt.Fprintf(std.Err, "bitc: internal error: cannot read the job spec: %v\n", err)
		return exit.Unavailable
	}
	terminate := jobs.Terminate(std.Out)
	red := redact.New()
	result := run.Result{Origin: "bitc", Outcome: "internal", ExitCode: exit.Unavailable, Job: name, Generation: gen}
	defer func() {
		if r := recover(); r != nil {
			e := exit.Internal(r)
			fmt.Fprintln(std.Err, e.Line)
			result = run.Result{Origin: "bitc", Outcome: "internal", ExitCode: e.Code, Reason: e.Reason, Job: name, Generation: gen}
			code = e.Code
		}
		_ = terminate(code)
		result.Reason = red.String(result.Reason)
		_ = rec.WriteResult(gen, result)
		if sp.Result != "" {
			_ = run.WriteResult(sp.Result, result)
		}
		_ = rec.UpdateStatus(gen, func(s *jobs.Status) {
			s.State, s.EndedAt = "ended", time.Now().UTC().Format(time.RFC3339Nano)
		})
	}()
	// "running" means the handlers above exist: cancel signals only a running supervisor. A cancel
	// that arrived while queued is in status.json; read it here, under the same lock, and act as
	// if the signal had come before runLocal.
	var asked string
	if err := rec.UpdateStatus(gen, func(s *jobs.Status) {
		s.State, asked = "running", s.CancelRequested
		if ps, err := proc.ReadStat(os.Getpid()); err == nil {
			s.Pid, s.Start = os.Getpid(), ps.Start
		}
	}); err != nil { // otherwise no cancel could ever reach this job
		fmt.Fprintf(std.Err, "bitc: internal error: cannot mark the job running: %v\n", err)
		return exit.Unavailable
	}
	if asked != "" {
		select { // never block: a real SIGUSR1 may already fill usr1
		case early <- syscall.SIGTERM:
		default:
		}
		if asked == "KILL" {
			select {
			case usr1 <- syscall.SIGUSR1:
			default:
			}
		}
	}
	devnull, _ := os.Open(os.DevNull)
	inv := run.Invocation{Flags: sp.Flags, Args: sp.Args, Stdin: devnull, Stdout: std.Out, Stderr: std.Err, Env: os.Environ(), Cwd: sp.Cwd,
		Detached: true, Pending: early}
	stderr := red.Writer(std.Err)
	p, err := run.Prepare(inv, red)
	if err != nil {
		e := exit.As(err)
		fmt.Fprintln(stderr, e.Line)
		_ = stderr.Flush()
		result = run.Result{Origin: "bitc", Outcome: e.Outcome, ExitCode: e.Code, Reason: e.Reason, Job: name, Generation: gen}
		return e.Code
	}
	p.Job = name
	out := run.Execute(ctx, inv, p, stderr)
	_ = stderr.Flush()
	out.Result.Generation = gen
	result = out.Result
	return out.Code
}

func waitCmd(_ context.Context, g config.Flags, args []string, std Std) int {
	var job, logPath string
	timeoutRaw, exitMode := "", "zero"
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "--timeout" && i+1 < len(args):
			timeoutRaw, i = args[i+1], i+1
		case a == "--exit" && i+1 < len(args) && (args[i+1] == "zero" || args[i+1] == "job"):
			exitMode, i = args[i+1], i+1
		case a == "--log" && i+1 < len(args):
			logPath, i = args[i+1], i+1
		case !strings.HasPrefix(a, "-") && job == "":
			job = a
		default:
			fmt.Fprintln(std.Err, "usage: bitc wait <job> | --log PATH [--timeout 9m] [--exit zero|job]")
			return exit.Usage
		}
	}
	if (job == "") == (logPath == "") {
		fmt.Fprintln(std.Err, "usage: bitc wait <job> | --log PATH [--timeout 9m] [--exit zero|job]")
		return exit.Usage
	}
	e, err := loadFor(g, "")
	if err != nil {
		fmt.Fprintln(std.Err, exit.As(err).Line)
		return exit.Code(err)
	}
	timeout := e.Detach.WaitTimeout
	if timeoutRaw != "" {
		if timeout, err = config.ParseDuration(timeoutRaw); err != nil {
			fmt.Fprintf(std.Err, "bitc: --timeout %q %v\n", timeoutRaw, err)
			return exit.Usage
		}
	}
	o := jobs.WaitOptions{LogPath: logPath, Timeout: e.Scale(timeout), Poll: e.Scale(10 * time.Second), ExitJob: exitMode == "job"}
	if job != "" {
		rec, err := jobs.Resolve(state.Layout{Root: e.Host.StateDir}, e.WT8, job)
		if err != nil {
			fmt.Fprintf(std.Out, "NO LOG %s (no such job: check the name and the work tree)\n", job)
			return exit.NoInput
		}
		o.Record = &rec
	}
	out, code := jobs.Wait(o)
	fmt.Fprint(std.Out, out)
	return code
}

func logsCmd(_ context.Context, g config.Flags, args []string, std Std) int {
	follow, tail, job := false, -1, ""
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "-f":
			follow = true
		case args[i] == "--tail" && i+1 < len(args):
			n, err := config.ParseInt(args[i+1])
			if err != nil {
				return exit.Usage
			}
			tail, i = int(n), i+1
		case job == "":
			job = args[i]
		default:
			return exit.Usage
		}
	}
	e, err := loadFor(g, "")
	if err != nil {
		fmt.Fprintln(std.Err, exit.As(err).Line)
		return exit.Code(err)
	}
	rec, err := jobs.Resolve(state.Layout{Root: e.Host.StateDir}, e.WT8, job)
	if err != nil {
		fmt.Fprintln(std.Err, exit.As(err).Line)
		return exit.Code(err)
	}
	gen := rec.Gen() // bind once: a later run of the same name is another generation
	sp, err := rec.ReadSpec(gen)
	if err != nil || sp.Log == "" {
		fmt.Fprintf(std.Err, "bitc: no log for job %s (its detach failed before the job was set up)\n", job)
		return exit.NoInput
	}
	var off int64
	for {
		b, _ := os.ReadFile(rec.LogFor(gen, sp.Log))
		if off == 0 && tail >= 0 {
			for _, l := range jobs.Tail(b, tail) {
				fmt.Fprintln(std.Out, l)
			}
		} else if int64(len(b)) > off {
			_, _ = std.Out.Write(b[off:])
		}
		off = int64(len(b))
		if _, done := jobs.LastExit(b); !follow || done {
			return 0
		}
		// a lost supervisor never writes __EXIT__: stop on its result, or when it is dead
		var r struct {
			Outcome string `json:"outcome"`
		}
		if rec.ReadResult(gen, &r) == nil {
			return logsLostCode(r.Outcome)
		}
		if st, err := rec.ReadStatus(gen); err == nil && (jobs.SupervisorDead(st) || neverStarted(st)) {
			return exit.TempFail
		}
		time.Sleep(e.Scale(time.Second))
	}
}

// neverStarted: queued with no supervisor pid past the startup window, so cmd.Start failed.
func neverStarted(st jobs.Status) bool {
	if st.State != "queued" || st.Pid != 0 {
		return false
	}
	t, err := time.Parse(time.RFC3339Nano, st.StartedAt)
	return err == nil && time.Since(t) > 30*time.Second
}

// logsLostCode is the code of `logs -f` once a result exists but the log has no exit line.
func logsLostCode(outcome string) int {
	if outcome == "lost" || outcome == "starved" {
		return exit.TempFail
	}
	return 0
}

func cancelCmd(_ context.Context, g config.Flags, args []string, std Std) int {
	kill := len(args) == 2 && args[1] == "--kill"
	if len(args) < 1 || (len(args) == 2 && !kill) || len(args) > 2 {
		fmt.Fprintln(std.Err, "usage: bitc cancel <job> [--kill]")
		return exit.Usage
	}
	e, err := loadFor(g, "")
	if err != nil {
		fmt.Fprintln(std.Err, exit.As(err).Line)
		return exit.Code(err)
	}
	rec, err := jobs.Resolve(state.Layout{Root: e.Host.StateDir}, e.WT8, args[0])
	if err != nil {
		fmt.Fprintln(std.Err, exit.As(err).Line)
		return exit.Code(err)
	}
	gen := rec.Gen()
	// One locked decision. A queued supervisor may have no handlers yet, so it is never
	// signalled: the request waits in status.json for it to flip to running (see supervise).
	sig := "TERM"
	if kill {
		sig = "KILL"
	}
	// no status.json yet: the job has not started, and cancel never creates the file
	if _, err := rec.ReadStatus(gen); errors.Is(err, fs.ErrNotExist) {
		fmt.Fprintf(std.Out, "%s already ended\n", args[0])
		return 0
	}
	var st jobs.Status
	queued := false
	err = rec.UpdateStatus(gen, func(s *jobs.Status) {
		st = *s
		if s.State == "queued" && !jobs.SupervisorDead(*s) && !neverStarted(*s) {
			if s.CancelRequested != "KILL" { // a later cancel never downgrades --kill
				s.CancelRequested = sig
			}
			queued = true
		}
	})
	if err != nil {
		fmt.Fprintf(std.Err, "bitc: cancel %s: status.json: %v\n", args[0], err)
		return exit.Unavailable
	}
	if !queued && (st.State == "ended" || st.Pid == 0 || jobs.SupervisorDead(st)) {
		fmt.Fprintf(std.Out, "%s already ended\n", args[0])
		return 0
	}
	if !queued {
		_ = syscall.Kill(st.Pid, syscall.SIGTERM)
		if kill {
			// a different signal never merges with the TERM: the supervisor escalates at once
			_ = syscall.Kill(st.Pid, syscall.SIGUSR1)
		}
	}
	if st.Pid == 0 { // detach has not recorded the pid yet
		fmt.Fprintf(std.Out, "cancelled %s\n", args[0])
		return 0
	}
	fmt.Fprintf(std.Out, "cancelled %s pid %d\n", args[0], st.Pid)
	return 0
}
