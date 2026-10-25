package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
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
)

func init() {
	Register("run", func(ctx context.Context, g config.Flags, args []string, std Std) int {
		cwd, err := os.Getwd()
		if err != nil {
			cwd = os.Getenv("PWD")
		}
		inv := run.Invocation{Flags: g, Args: args, Stdin: std.In, Stdout: std.Out, Stderr: std.Err, Env: os.Environ(), Cwd: cwd}
		if ra, err := run.ParseRunArgs(args); err == nil && ra.Detach {
			return detach(inv)
		}
		return run.Main(ctx, inv)
	})
}

// detach runs steps 1-4 here, opens the log, writes spec.json, starts the supervisor in a new
// session and prints exactly "started <name> pid <pid> log <path>" (spec 3.1, 7).
func detach(inv run.Invocation) int {
	red := redact.New()
	stderr := red.Writer(inv.Stderr)
	defer stderr.Flush()
	fail := func(err error) int {
		fmt.Fprintln(stderr, exit.As(err).Line)
		return exit.Code(err)
	}
	args := withoutDetach(inv.Args)
	inv.Args = args
	inv.Env = withoutKey(inv.Env, "BITC_HOLD") // a detached job takes its own units (spec 6.6)
	p, err := run.Prepare(inv, red)
	if err != nil {
		return fail(err)
	}
	rec := jobs.Open(p.Layout, p.Eff.WT8, p.Job)
	gen, err := rec.Bump()
	if err != nil {
		return fail(exit.Unavailablef("job record %s: %v", rec.Dir, err))
	}
	logPath := filepath.Join(rec.Dir, "out.log")
	if p.Args.Log != "" {
		logPath, _ = filepath.Abs(p.Args.Log)
		_ = os.Remove(logPath) // a rerun unlinks PATH and creates it O_EXCL
	}
	logFile, err := os.OpenFile(logPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL|os.O_APPEND|syscall.O_CLOEXEC, 0o600)
	if err != nil {
		return fail(exit.Usagef("cannot create the log %s: %v", logPath, err))
	}
	defer logFile.Close()
	spec := jobs.Spec{Gen: gen, Name: p.Job, WT8: p.Eff.WT8, Log: logPath, Args: args, Cwd: inv.Cwd,
		Flags: config.Flags{ConfigFile: inv.Flags.ConfigFile, StateDir: p.Eff.Host.StateDir}, Result: p.Args.Result}
	if err := rec.WriteSpec(spec); err != nil {
		return fail(exit.Unavailablef("spec.json: %v", err))
	}
	// queued before the supervisor exists; from here on both sides only merge (UpdateStatus)
	if err := rec.UpdateStatus(gen, func(s *jobs.Status) {
		s.State, s.Log, s.StartedAt = "queued", logPath, time.Now().UTC().Format(time.RFC3339Nano)
	}); err != nil {
		return fail(exit.Unavailablef("status.json: %v", err))
	}
	self, err := os.Executable()
	if err != nil {
		return fail(exit.Unavailablef("cannot find the bitc binary: %v", err))
	}
	devnull, _ := os.Open(os.DevNull)
	defer devnull.Close()
	cmd := exec.Command(self, "__supervise", p.Eff.WT8+"/"+p.Job, strconv.Itoa(gen))
	cmd.Env = append(withoutKey(inv.Env, "BITC_DIR"), "BITC_DIR="+p.Eff.Host.StateDir)
	cmd.Dir, cmd.Stdin, cmd.Stdout, cmd.Stderr = inv.Cwd, devnull, logFile, logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	// The supervisor inherits SIGHUP ignored, so a hangup before its own handler exists cannot
	// kill it (spec 7); it then catches HUP itself, so the command gets the default again.
	signal.Ignore(syscall.SIGHUP)
	if err := cmd.Start(); err != nil {
		return fail(exit.Unavailablef("cannot start the supervisor: %v", err))
	}
	pid := cmd.Process.Pid
	ps, _ := proc.ReadStat(pid)
	_ = rec.UpdateStatus(gen, func(s *jobs.Status) {
		if s.Pid == 0 { // the supervisor may already have written its own
			s.Pid, s.Start = pid, ps.Start
		}
	})
	_ = cmd.Process.Release()
	fmt.Fprintf(inv.Stdout, "started %s pid %d log %s\n", p.Job, pid, logPath)
	return 0
}

// withoutDetach drops --detach from the flags before <pool> (ParseRunArgs's loop); the command's
// argv after them stays verbatim (spec 3.1), even a literal --detach.
func withoutDetach(args []string) []string {
	takesValue := map[string]bool{"--name": true, "--log": true, "--wait": true, "--provider": true, "--result": true, "--artifacts": true}
	out := make([]string, 0, len(args))
	i := 0
	for ; i < len(args) && strings.HasPrefix(args[i], "-") && args[i] != "--"; i++ {
		if args[i] == "--detach" {
			continue
		}
		out = append(out, args[i])
		if takesValue[args[i]] && i+1 < len(args) {
			i++
			out = append(out, args[i])
		}
	}
	return append(out, args[i:]...)
}

func withoutKey(env []string, key string) []string {
	var out []string
	for _, kv := range env {
		if len(kv) <= len(key) || kv[:len(key)+1] != key+"=" {
			out = append(out, kv)
		}
	}
	return out
}
