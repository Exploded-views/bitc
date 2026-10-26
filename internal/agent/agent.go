//go:build linux

package agent

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Exploded-views/bitc/internal/exit"
)

// Options are the agent's environment.
type Options struct {
	Stdout      io.Writer
	ResultsRoot string
	ToolsRoot   string
	WorkRoot    string
	System      System
	HTTP        *http.Client
	Listener    net.Listener
	NoServe     bool // tests: do not block serving at the end
	Deadline    time.Time
	SelfDelete  func(ctx context.Context) error
	// StateRoot holds the provision marker (no-listener mode only): a Run whose spec matches it
	// skips its own package and tool installs. SelfSHA is the agent's sha256 (default: /proc/self/exe).
	StateRoot string
	SelfSHA   func() (string, error)
	// KeyRoot is where the deploy key lives in no-listener mode: a tmpfs System.MountTmpfs mounts there.
	KeyRoot string
	// KeepAlive, when set, holds a Tasks API task for the whole job (sprites-provider.md 2.3).
	KeepAlive *KeepAliveConf
	// HostCheck, when set, runs right after BOOT: an error is an infra setup failure named
	// host-check (the ssh host's C.8 re-check, providers delta C.5 step 6).
	HostCheck func() error
	// AfterCommand, when set, runs once the job's command has ended and before its results are
	// collected (the ssh host kills every process the job left behind).
	AfterCommand func()
	// Code, when set, makes the code step an ssh upload: the commit comes from the git bundle
	// WorkRoot/.bitc-upload/code.bundle, not from a clone (providers delta, upload transport).
	Code *CodeUpload
	// UploadLock is the host-wide upload.lock the checkout guard and the checkout run under.
	UploadLock string
	// Upload holds the statfs seam of the checkout guard (the other fields are the root side's).
	Upload UploadEnv
	// CodeTimeout limits each git command of the code step (default 10 minutes).
	CodeTimeout time.Duration
}

// cmdFinisher is implemented by a System that must rewrite a prepared command once its Dir and
// Env are set (the ssh host runs every command inside the job's namespaces).
type cmdFinisher interface{ FinishCmd(c *exec.Cmd) }

// bind resolves c's program against the job's PATH and lets the System finish the command.
func (o Options) bind(c *exec.Cmd, argv0 string, env []string) {
	bindPath(c, argv0, env)
	if f, ok := o.System.(cmdFinisher); ok {
		f.FinishCmd(c)
	}
}

// Secrets are the values moved out of the env at boot.
type Secrets struct {
	DeployKey string
}

// Run is stage 2: one job, then serve until torn down (spec 8.4 steps 2-7).
func Run(ctx context.Context, o Options, s Spec, sec Secrets) int {
	cloexecInherited()
	if o.KeepAlive != nil { // the renewal goroutine prints too
		o.Stdout = &syncWriter{w: o.Stdout}
	}
	say := func(line string) { fmt.Fprintln(o.Stdout, line) }
	if o.Code != nil { // the bundle goes on every return path, whatever happens first
		defer func() { _ = removeUploadDir(o.WorkRoot) }()
	}
	mark := func(kind string, arg ...string) { say(FormatMarker(kind, s.Nonce, arg...)) }
	res, err := NewResults(o.ResultsRoot, s.Job)
	if err != nil {
		mark(MarkSetupFailed, "results", "infra", errorLine(err))
		return 69
	}
	// The ran create decides the owner before anything is written to agent.log: a second boot
	// of a running job must not put its BOOT or LOST into the first agent's record.
	var ranErr error
	if f, err := os.OpenFile(filepath.Join(res.Dir, "ran"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600); err != nil {
		ranErr = err
	} else {
		f.Close()
	}
	if s.Port == -1 && ranErr == nil { // no-listener mode (exec delivery): the owner's log and identity live in the results dir
		lf, err := os.OpenFile(filepath.Join(res.Dir, "agent.log"), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
		if err != nil {
			fmt.Fprintln(o.Stdout, "bitc agent:", err)
			return 69
		}
		defer lf.Close()
		o.Stdout = &logTee{log: lf, out: o.Stdout}
	}
	bearer, _ := hex.DecodeString(s.Bearer)
	k, _ := hex.DecodeString(s.MacKey)
	res.Bind(k, s.Job, s.Nonce)
	if o.Listener != nil && !o.NoServe {
		h, err := NewServer(res.Dir, bearer)
		if err != nil {
			mark(MarkSetupFailed, "results-server", "infra", errorLine(err))
			select {}
		}
		srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}
		exit.Go(func() { _ = srv.Serve(o.Listener) })
	}
	// serve keeps the container alive after the job (a container whose main process exits is
	// restarted, spec 8.4 step 7); tests set NoServe.
	serve := func() {
		if !o.NoServe && s.Port != -1 { // no-listener mode exits after result.json
			select {}
		}
	}
	mark(MarkBoot)
	// self-delete at D + 10m, from the create env's deadline only; scheduled before the ran
	// check so a restarted container (RE21) keeps the original deadline.
	if o.SelfDelete != nil && !o.Deadline.IsZero() {
		at := o.Deadline.Add(10 * time.Minute)
		time.AfterFunc(time.Until(at), func() { _ = o.SelfDelete(context.Background()) })
	}
	if ranErr != nil {
		mark(MarkLost) // a restarted container never runs the job twice
		serve()
		return 75
	}
	if s.Port == -1 { // only the agent that owns the job names itself: a second boot must not overwrite it
		_ = os.WriteFile(filepath.Join(res.Dir, "agent.pid"), []byte(identity(os.Getpid())), 0o600)
	}
	_ = res.WriteState("setup", nil)
	// fail reports a setup failure with its class (spec 8.4 steps 4-5, 15.9): repo only for a
	// stepError marked Repo; any other error is infra.
	fail := func(err error) int {
		var se *stepError
		name, class := "setup", "infra"
		if errors.As(err, &se) {
			name, class = se.Name, se.class()
			if se.Tail != "" {
				say(se.Tail)
			}
		}
		say(err.Error())
		mark(MarkSetupFailed, name, class, errorLine(err))
		_ = res.WriteState("setup-failed", nil)
		serve()
		return 75
	}
	if o.HostCheck != nil {
		if err := o.HostCheck(); err != nil {
			say("pod: host check failed: " + err.Error())
			return fail(&stepError{Name: "host-check", Err: err})
		}
	}
	if o.KeepAlive != nil { // the Sprite must not pause under a quiet command (sprites-provider.md 2.3)
		stopKA, err := startKeepAlive(*o.KeepAlive, say)
		if err != nil {
			return fail(&stepError{Name: "keep-alive", Err: err})
		}
		defer stopKA()
	}
	// Names go into the marker's space-separated fields: an unsafe one would shift the class (spec 8.4 step 5).
	for _, n := range append(stepNames(s.Setup), serviceNames(s.Services)...) {
		if !toolNameRE.MatchString(n) {
			return fail(&stepError{Name: "step-name", Repo: true, Err: fmt.Errorf("step or service name %q must match %s", n, toolNameRE)})
		}
	}
	pathDirs := []string{}
	if o.provisioned(s) { // a provisioning (job-neutral, before attach) already installed exactly these
		say("provisioned: skipping package and tool installs")
		dirs, err := reuseTools(o.ToolsRoot, s.Tools)
		if err != nil {
			return fail(err)
		}
		pathDirs = append(pathDirs, dirs...)
	} else {
		if err := o.System.InstallPackages(s.Packages); err != nil {
			return fail(&stepError{Name: "packages", Err: err})
		}
		client := o.HTTP
		if client == nil {
			client = http.DefaultClient
		}
		for _, t := range s.Tools {
			dir, err := installTool(client, o.ToolsRoot, t)
			if err != nil {
				return fail(err)
			}
			pathDirs = append(pathDirs, dir)
		}
	}
	work := filepath.Join(o.WorkRoot, s.Job)
	keyDir := filepath.Join(o.ResultsRoot, ".ssh-"+s.Job)
	var mount func(string) (func() error, error)
	if s.Port == -1 && o.KeyRoot != "" { // exec delivery: the key goes on a tmpfs of its own
		keyDir, mount = o.KeyRoot, o.System.MountTmpfs
	}
	var (
		uid, gid int
		home     string
	)
	if o.Code != nil { // upload: the job user and its tree first, then git runs as that user
		if uid, gid, home, err = o.System.EnsureJobUser(); err != nil {
			return fail(&stepError{Name: "job-user", Err: err})
		}
		if err := ensureDir(o.WorkRoot, 0o755); err != nil {
			return fail(&stepError{Name: "code", Err: err})
		}
		if err := os.Mkdir(work, 0o750); err != nil {
			return fail(&stepError{Name: "code", Err: err})
		}
		if err := o.System.Chown(work, uid, gid); err != nil {
			_ = os.RemoveAll(work)
			return fail(&stepError{Name: "job-user", Err: err})
		}
		if err := o.fetchUpload(s, work, uid, gid); err != nil {
			_ = os.RemoveAll(work)
			return fail(err)
		}
		// the bundle is never readable while setup or the job runs
		if err := removeUploadDir(o.WorkRoot); err != nil {
			_ = os.RemoveAll(work)
			return fail(&stepError{Name: "code", Err: fmt.Errorf("remove the upload dir: %w", err)})
		}
	} else {
		if err := fetchCode(s, work, keyDir, sec.DeployKey, mount); err != nil {
			return fail(err)
		}
		if uid, gid, home, err = o.System.EnsureJobUser(); err != nil {
			return fail(&stepError{Name: "job-user", Err: err})
		}
		if err := o.System.Chown(work, uid, gid); err != nil {
			return fail(&stepError{Name: "job-user", Err: err})
		}
	}
	pathDirs = append(pathDirs, "/usr/local/sbin", "/usr/local/bin", "/usr/sbin", "/usr/bin", "/sbin", "/bin")
	env := append(append([]string{}, s.Env...), "PATH="+strings.Join(pathDirs, ":"), "HOME="+home)
	step := func(name string, argv []string, root bool, dir string) error {
		errTxt, err := o.runCmd(argv, root, dir, env, uid, gid, o.Stdout, 0)
		if err != nil { // a repo (or root) step: repo only if it exited non-zero by itself
			return &stepError{Name: name, Tail: tail(errTxt, 20), First: firstLine(errTxt), Repo: byItself(err), Err: err}
		}
		return nil
	}
	if s.RootSteps {
		for _, st := range s.Setup {
			if st.RunAs == "root" {
				if err := step(st.Name, st.Argv, true, work); err != nil {
					return fail(err)
				}
			}
		}
		for _, sv := range s.Services {
			c := o.System.Cmd(sv.Argv, sv.RunAs == "root", uid, gid)
			c.Dir, c.Env = work, env
			bindPath(c, sv.Argv[0], env)
			var sverr bytes.Buffer
			c.Stderr = &sverr
			if err := c.Start(); err != nil {
				return fail(&stepError{Name: sv.Name, Err: err}) // could not start: infra
			}
			exited := make(chan error, 1)
			exit.Go(func() { exited <- c.Wait() })
			if err := waitReady(o.System, sv, env, work, uid, gid, exited, &sverr); err != nil {
				return fail(err)
			}
		}
	}
	for _, st := range s.Setup {
		if st.RunAs != "root" {
			if err := step(st.Name, st.Argv, false, work); err != nil {
				return fail(err)
			}
		}
	}
	stdout, _ := res.Create("out.log")
	stderr, _ := res.Create("err.log")
	devnull, _ := os.Open(os.DevNull)
	limit := time.Duration(s.Timeout)
	cctx, cancel := context.WithDeadline(ctx, minTime(time.Now().Add(limit), o.Deadline))
	defer cancel()
	cmd := o.System.Cmd(s.Argv, false, uid, gid)
	cmd.Dir, cmd.Env = filepath.Join(work, s.Cwd), env
	o.bind(cmd, s.Argv[0], env)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = devnull, stdout, stderr
	_ = res.WriteState("begun", nil)
	mark(MarkBegin)
	code := runUntil(cctx, cmd, say, func(pid int) {
		if s.Port == -1 {
			_ = os.WriteFile(filepath.Join(res.Dir, "job.pid"), []byte(identity(pid)), 0o600)
		}
	})
	if o.AfterCommand != nil {
		o.AfterCommand()
	}
	stdout.Close()
	stderr.Close()
	if _, err := res.Finish(k, code, work, s.Artifacts); err != nil {
		say("results: " + err.Error())
	}
	_ = res.WriteState("ended", &code)
	mark(MarkExit, fmt.Sprint(code))
	serve()
	return code
}

// syncWriter serialises writes to w.
type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

// logTee is the no-listener agent's stdout: agent.log first, which is the record, then the exec
// channel's stdout as a best effort. Once the channel fails (the host dropped it) it is let go.
type logTee struct {
	mu  sync.Mutex
	log *os.File
	out io.Writer
}

func (l *logTee) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, err := l.log.Write(p); err != nil {
		return 0, err
	}
	if l.out != nil {
		if _, err := l.out.Write(p); err != nil {
			l.out = nil
		}
	}
	return len(p), nil
}

func runUntil(ctx context.Context, cmd *exec.Cmd, say func(string), started func(pid int)) int {
	if err := cmd.Start(); err != nil {
		// the reason (never the env) goes to the job's stderr stream and the agent log, spec 8.4 step 5
		msg := fmt.Sprintf("bitc agent: cannot start %s: %v", cmd.Args[0], err)
		if cmd.Stderr != nil {
			fmt.Fprintln(cmd.Stderr, msg)
		}
		say(msg)
		return 127
	}
	started(cmd.Process.Pid)
	done := make(chan error, 1)
	exit.Go(func() { done <- cmd.Wait() })
	select {
	case err := <-done:
		return exitCode(err, cmd)
	case <-ctx.Done():
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
		return 124
	}
}

func exitCode(err error, cmd *exec.Cmd) int {
	if cmd.ProcessState == nil {
		return 69
	}
	if ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return cmd.ProcessState.ExitCode()
}

// waitReady polls the readiness argv. A service that exits non-zero by itself first is the
// repo's failure; one that exits otherwise (a signal, the OOM killer, exit 0) or never gets ready
// is infra (spec 8.4 step 5).
func waitReady(sys System, sv Service, env []string, dir string, uid, gid int, exited <-chan error, stderr *bytes.Buffer) error {
	deadline := time.Now().Add(time.Duration(sv.ReadyTimeout))
	for {
		select {
		case err := <-exited:
			if err == nil {
				err = errors.New("exited before it was ready")
			}
			return &stepError{Name: sv.Name, Tail: tail(stderr.String(), 20), First: firstLine(stderr.String()), Repo: byItself(err), Err: err}
		default:
		}
		c := sys.Cmd(sv.Ready, sv.RunAs == "root", uid, gid)
		c.Dir, c.Env = dir, env
		bindPath(c, sv.Ready[0], env)
		if c.Run() == nil {
			return nil // intermediate failures are never printed (RE18)
		}
		if time.Now().After(deadline) {
			return &stepError{Name: sv.Name, Err: fmt.Errorf("not ready after %s", time.Duration(sv.ReadyTimeout))}
		}
		time.Sleep(time.Second)
	}
}

func minTime(a, b time.Time) time.Time {
	if b.IsZero() || a.Before(b) {
		return a
	}
	return b
}

func stepNames(st []Step) (n []string) {
	for _, x := range st {
		n = append(n, x.Name)
	}
	return n
}

func serviceNames(sv []Service) (n []string) {
	for _, x := range sv {
		n = append(n, x.Name)
	}
	return n
}
