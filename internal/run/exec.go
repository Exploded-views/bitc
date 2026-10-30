package run

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/Exploded-views/bitc/internal/exit"
	"github.com/Exploded-views/bitc/internal/proc"
)

// ExecSpec is one command run under a hold (spec 6.4).
type ExecSpec struct {
	Argv          []string
	Env           []string
	Dir           string
	Stdin         *os.File
	Stdout        *os.File
	Stderr        *os.File
	OnSignal      string // "terminate" | "forward-and-wait"
	Grace         time.Duration
	MaxSignalWait time.Duration
	Started       func(pid, pgid int, leaderStart uint64)
	Pending       chan os.Signal // optional: a channel the caller registered; Exec takes it over
	NoHangup      bool           // detached supervisor (spec 7): SIGHUP is neither handled nor forwarded
}

// ExecResult is how the command ended.
type ExecResult struct {
	Code        int            // the command's code, 128+N if it died of signal N, or 128+N after terminate
	Interrupted syscall.Signal // the signal bitc received, 0 if none
	StartErr    error          // the command could not start (Code 126 or 127)
}

func isTTY(f *os.File) bool {
	if f == nil {
		return false
	}
	_, err := unix.IoctlGetTermios(int(f.Fd()), ioctlGetTermios)
	return err == nil
}

// beforeRecord and afterWait are test seams: beforeRecord runs before Exec records the started
// command (Started), afterWait once the command has been reaped. They are package globals read by
// every Exec, so a test in internal/run that sets them must not use t.Parallel (nor run beside
// one that does).
var beforeRecord, afterWait func()

var handled = []os.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT}

// Exec runs the command as bitc's child and returns how it ended (spec 6.4).
func Exec(ctx context.Context, s ExecSpec) (ExecResult, error) {
	tty := isTTY(s.Stdin)
	cmd := exec.Command(s.Argv[0], s.Argv[1:]...)
	cmd.Env, cmd.Dir = s.Env, s.Dir
	cmd.Stdin, cmd.Stdout, cmd.Stderr = s.Stdin, s.Stdout, s.Stderr
	cmd.SysProcAttr = childAttr(!tty)

	hs := handled
	if s.NoHangup {
		hs = []os.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT}
	}
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, hs...)
	defer signal.Stop(sigs)
	if s.Pending != nil { // the caller's channel covered the hand-off; ours is registered now
		signal.Stop(s.Pending)
	drain:
		for {
			select {
			case sig := <-s.Pending:
				select { // a full sigs already escalates; never block before the command starts
				case sigs <- sig:
				default:
				}
			default:
				break drain
			}
		}
	}

	startErr := make(chan error, 1)
	done := make(chan error, 1)
	var leader proc.Stat // read before Wait can reap the command; published by the send on startErr
	var leaderErr error
	exit.Go(func() {
		// Pdeathsig belongs to the thread that forked the child: keep this goroutine on its
		// thread until Wait returns, so the child is killed only when bitc itself dies.
		runtime.LockOSThread()
		err := cmd.Start()
		if err == nil && s.Started != nil {
			leader, leaderErr = proc.ReadStat(cmd.Process.Pid)
		}
		startErr <- err
		if err != nil {
			runtime.UnlockOSThread()
			return
		}
		err = cmd.Wait()
		if afterWait != nil {
			afterWait()
		}
		done <- err
		runtime.UnlockOSThread()
	})
	if err := <-startErr; err != nil {
		code := 127
		if errors.Is(err, fs.ErrPermission) || errors.Is(err, syscall.EACCES) || errors.Is(err, syscall.ENOEXEC) {
			code = 126
		}
		return ExecResult{Code: code, StartErr: err}, nil
	}
	if beforeRecord != nil {
		beforeRecord()
	}
	pid := cmd.Process.Pid
	pgid := pid
	if tty {
		pgid = 0 // the caller's group is not ours: record and signal the command alone
	}
	if s.Started != nil {
		if leaderErr == nil {
			s.Started(pid, pgid, leader.Start)
		}
	}
	forward := func(sig syscall.Signal) {
		if tty {
			_ = syscall.Kill(pid, sig) // leader only: the caller's group is not ours
		} else {
			_ = syscall.Kill(-pgid, sig)
		}
	}
	var received syscall.Signal
	terminating := false
	var killAt, giveUpAt <-chan time.Time
	ctxDone := ctx.Done() // set to nil once handled: a closed channel would spin the loop
	for {
		select {
		case err := <-done:
			res := ExecResult{Code: codeOf(err, cmd.ProcessState), Interrupted: received}
			if !tty {
				reapStragglers(pgid, s.Grace)
			}
			if terminating {
				res.Code = 128 + int(received)
			}
			return res, nil
		case sig := <-sigs:
			n := sig.(syscall.Signal)
			if received == 0 && s.OnSignal == "forward-and-wait" {
				received = n
				forward(n)
				giveUpAt = time.After(s.MaxSignalWait)
				continue
			}
			received, terminating = n, true // terminate, or a second signal escalates
			forward(n)
			if killAt == nil { // the SIGKILL is due grace after the first escalation, whatever follows
				killAt = time.After(s.Grace)
			}
		case <-giveUpAt:
			terminating = true
			forward(syscall.SIGKILL)
		case <-killAt:
			forward(syscall.SIGKILL)
		case <-ctxDone:
			ctxDone = nil
			if !terminating {
				received, terminating = syscall.SIGTERM, true
				forward(syscall.SIGTERM)
				killAt = time.After(s.Grace)
			}
		}
	}
}

func codeOf(err error, ps *os.ProcessState) int {
	if ps == nil {
		return exit.Unavailable
	}
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	var ee *exec.ExitError
	if err != nil && !errors.As(err, &ee) {
		return exit.Unavailable
	}
	return ps.ExitCode()
}

// reapStragglers: processes left in the hold's group get SIGTERM, then SIGKILL after grace.
func reapStragglers(pgid int, grace time.Duration) {
	if syscall.Kill(-pgid, 0) != nil {
		return
	}
	_ = syscall.Kill(-pgid, syscall.SIGTERM)
	deadline := time.Now().Add(grace)
	for time.Now().Before(deadline) {
		if syscall.Kill(-pgid, 0) != nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
}
