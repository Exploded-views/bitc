package remote

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/Exploded-views/bitc/internal/agent"
	"github.com/Exploded-views/bitc/internal/config"
	"github.com/Exploded-views/bitc/internal/exit"
	"github.com/Exploded-views/bitc/internal/provider"
	"github.com/Exploded-views/bitc/internal/provider/runpod"
	"github.com/Exploded-views/bitc/internal/provider/sprites"
	"github.com/Exploded-views/bitc/internal/provider/ssh"
	"github.com/Exploded-views/bitc/internal/remote/artifacts"
	"github.com/Exploded-views/bitc/internal/run"
	"github.com/Exploded-views/bitc/internal/state"
)

// sigRegs counts live signal registrations (a test asserts none is left behind).
var sigRegs atomic.Int32

func init() {
	RunJob = Orchestrate
	runpod.AgentSpec = AgentSpecFor
	sprites.AgentSpec = AgentSpecFor
	ssh.AgentSpec = AgentSpecFor
	ssh.ResolveAgentFile = ResolveAgentFile
}

// AgentSpecFor converts the host's job into the pod's spec.
func AgentSpecFor(j *provider.JobSpec) agent.Spec {
	s := agent.Spec{Job: j.Job, Pool: j.Pool, Argv: j.Argv, Cwd: j.Cwd, Env: j.Env, CloneURL: j.CloneURL, Ref: j.Ref, Sha: j.Sha,
		KnownHosts: config.BuiltinKnownHosts, Timeout: agent.Duration(j.Timeout), Packages: j.Packages, RootSteps: j.RootSteps,
		Bearer: hex.EncodeToString(j.Bearer[:]), MacKey: hex.EncodeToString(j.MacKey[:]), Nonce: hex.EncodeToString(j.Nonce[:]),
		Unprivileged: j.Unprivileged, TestRoot: j.TestRoot}
	for _, t := range j.Tools {
		s.Tools = append(s.Tools, agent.Tool{Name: t.Name, URL: t.URL, SHA256: t.SHA256})
	}
	for _, st := range j.Setup {
		s.Setup = append(s.Setup, agent.Step{Name: st.Name, RunAs: st.RunAs, Argv: st.Argv})
	}
	for _, sv := range j.Services {
		s.Services = append(s.Services, agent.Service{Name: sv.Name, RunAs: sv.RunAs, Argv: sv.Argv, Ready: sv.Ready, ReadyTimeout: agent.Duration(sv.ReadyTimeout)})
	}
	for _, a := range j.Artifacts {
		s.Artifacts = append(s.Artifacts, a.Path)
	}
	if j.TestRoot != "" {
		s.Port = 0 // the fake pod picks a free port and writes it to <TestRoot>/port
	}
	return s
}

// counter forwards live output and remembers every byte for the sha256 check.
type counter struct {
	mu  sync.Mutex
	w   io.Writer
	buf bytes.Buffer
}

func (c *counter) Write(b []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.buf.Write(b)
	return c.w.Write(b)
}

type hostSink struct {
	j        *Job
	out, err *counter
	begun    sync.Once
}

func (s *hostSink) Phase(p provider.Phase, _ string) {
	if p == provider.Begun {
		s.begun.Do(func() {
			s.j.Began = time.Now()
			if OnBegin != nil {
				OnBegin(s.j)
			}
			if s.j.DeleteRef != nil {
				s.j.DeleteRef() // the ref is deleted once the command starts (spec 8.4)
			}
		})
	}
}
func (s *hostSink) Setup(line string) {
	fmt.Fprintf(s.j.P.InfoWriter(s.j.Stderr), "pod: %s\n", s.j.Red.String(line)) // silent under BITC_LOG=quiet
}
func (s *hostSink) Out(st provider.Stream, b []byte) {
	if st == provider.Stderr {
		_, _ = s.err.Write(b)
		return
	}
	_, _ = s.out.Write(b)
}

// Orchestrate runs one remote job end to end (spec 8.4 "Host side", 8.3 cleanup).
// A TestOnly provider (the in-process fake) has no agent and no result.json: it keeps basicRun.
func Orchestrate(parent context.Context, j *Job) (code int, began bool, err error) {
	if f, ok := provider.Get(j.Type); ok && f.TestOnly {
		return basicRun(parent, j)
	}
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	if j.KeepSignals && j.sigs != nil {
		// A retry attempt: the registration from the first attempt is still live; reuse it so a
		// signal caught between attempts is kept, not lost (spec 8.3).
	} else {
		j.sigs = make(chan os.Signal, 2)
		cancels := []os.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT}
		if !j.Inv.Detached { // a detached supervisor survives SIGHUP (spec 7)
			cancels = append(cancels, syscall.SIGHUP)
		}
		sigs := j.sigs
		signal.Notify(sigs, cancels...)
		sigRegs.Add(1)
		if j.KeepSignals {
			// Walk's cleanup (teardown, reap, staging, ledger end, reservation, launch lock, ref) runs
			// after this function returns: later signals stay absorbed until Walk calls StopSignals,
			// so none finds Go's default action before the pod is gone (spec 8.3).
			j.StopSignals = func() { signal.Stop(sigs); sigRegs.Add(-1) }
		} else {
			defer func() { signal.Stop(sigs); sigRegs.Add(-1) }()
		}
		if j.Inv.Pending != nil { // the supervisor's early channel: take over a signal it already caught
			signal.Stop(j.Inv.Pending)
			select {
			case s := <-j.Inv.Pending:
				sigs <- s
			default:
			}
		}
	}
	exit.Go(func() {
		select {
		case s := <-j.sigs:
			j.noteSignal(s)
			cancel()
		case <-ctx.Done():
		}
	})
	interrupted := func() error { return j.interruption(parent) }
	defer func() { // a signal wins over whatever failure it caused (cancelled requests)
		if err != nil {
			if ie := interrupted(); ie != nil {
				code, err = 0, ie
			}
		}
	}()
	if j.Prov.Info().Caps.Fixed {
		// A fixed host takes the agent as bytes over its session (providers delta C.4 item 4): the
		// provider resolves and sends them itself, so there is no URL to resolve here.
	} else if j.Spec.AgentURL, j.Spec.AgentSHA256, err = ResolveAgentFor(j.P.Eff, j.Core, firstNonEmpty(j.Spec.Arch, "amd64")); err != nil {
		return 0, false, err
	}
	if ie := interrupted(); ie != nil { // a signal caught before the create: no resource is made
		return 0, false, ie
	}
	h, err := j.launchWithRetry(ctx, interrupted)
	if err != nil {
		return 0, false, err
	}
	j.Handle, j.Created = &h, time.Now() // billing starts here (the ledger prices from Created)
	if !h.Created.IsZero() {
		j.Created = h.Created // the resource existed, and billed, since its create
	}
	j.Cleanup = append(j.Cleanup, func() {
		tctx, c := context.WithTimeout(context.Background(), 10*time.Second)
		defer c()
		_ = j.Prov.Teardown(tctx, h.ID)
	})
	j.Info("%s job %s on %s (%s) at %.12s", j.Spec.Pool, j.Spec.Job, h.ID, j.Spec.SizeKey, j.Spec.Sha)
	history(j, "start", h.ID, nil)
	sink := &hostSink{j: j, out: &counter{w: j.Stdout}, err: &counter{w: j.Stderr}}
	res, err := j.Prov.Watch(ctx, h, sink)
	if ie := interrupted(); ie != nil {
		return 0, res.Began, ie
	}
	if err != nil { // j.Created is set: Walk tears down and exits 75 (69 for a repo setup failure) even before BEGIN (spec 8.2, 15.9)
		return 0, res.Began, err
	}
	if err := complete(ctx, j, h, res, sink); err != nil {
		return 0, true, err
	}
	code = res.Exit
	if len(j.Spec.Artifacts) > 0 && j.Prov.Info().Caps.Artifacts {
		if aerr := fetchArtifacts(ctx, j, h); aerr != nil {
			if code == 0 {
				return 0, true, fmt.Errorf("artifacts: %w", aerr)
			}
			j.Note("artifacts not fetched: %v (the command already failed with %d)", aerr, code)
		}
	}
	history(j, "end", h.ID, &code)
	return code, true, nil
}

// retriesCreate: whether Launch's create is retried once (spec 8.3, clause 1). A real provider
// always is; the in-process fake only when its test config says retry_create, so the older tests
// keep their single create.
func (j *Job) retriesCreate() bool {
	if f, ok := provider.Get(j.Type); ok && f.TestOnly {
		on, _ := j.Core.Raw["retry_create"].(bool)
		return on
	}
	return true
}

// launchWithRetry calls Launch with spec 8.3's three retry clauses folded into one per-job
// budget: a job has at most one retry in total, whichever clause spends it (j.Retried, shared with
// Walk's pre-attach retry). A create that left nothing (plain ErrTransient) is retried once when
// no retry has happened yet; a resource-carrying error from either call goes through classifyLaunch.
// interrupted reports a signal that cut the call short (nil for none).
func (j *Job) launchWithRetry(ctx context.Context, interrupted func() error) (h provider.Handle, err error) {
	launchAt := time.Now()
	// leftover turns a reaped resource into the call's failure: a create that left a resource
	// never falls back to local (spec 8.2, 15.8). Billing is priced from the create attempt.
	leftover := func(reaped []string, listed bool) error {
		if len(reaped) == 0 && listed {
			return nil
		}
		j.Created = launchAt
		if !listed {
			return errors.New("a failed create request may have left a resource, and the name lookup failed")
		}
		return fmt.Errorf("a failed create request left %s, now torn down", strings.Join(reaped, ", "))
	}
	h, err = j.Prov.Launch(ctx, j.Spec)
	if err != nil && ctx.Err() == nil && carriesResource(err) { // a resource exists: spec 8.2's boundary rules, no create retry
		return h, j.classifyLaunch(err, launchAt)
	}
	if err != nil && ctx.Err() == nil && nothingMade(err) { // proven: no name lookup, no create retry
		return h, err
	}
	if err != nil && ctx.Err() == nil && !j.Retried {
		var tr *provider.ErrTransient
		if errors.As(err, &tr) {
			reaped, listed := reapByName(j)
			if lerr := leftover(reaped, listed); lerr != nil {
				if ctx.Err() == nil {
					return h, lerr
				}
			} else {
				select { // a signal cuts the wait short
				case <-ctx.Done():
				case <-time.After(j.P.Eff.Scale(5 * time.Second)):
					j.Retried = true
					h, err = j.Prov.Launch(ctx, j.Spec) // retried once (spec 8.3)
					if err != nil && ctx.Err() == nil && carriesResource(err) {
						return h, j.classifyLaunch(err, launchAt) // the retry's discard is final: Walk sees the retry spent
					}
				}
			}
		}
	}
	if err != nil {
		reaped, listed := reapByName(j) // after a failed or interrupted create
		if interrupted != nil {
			if ie := interrupted(); ie != nil {
				// A resource that billed before the signal is created for the ledger (spec 8.3, design
				// 5.3); an unconfirmed reap keeps the reservation as an orphan (providers delta B.3.3).
				switch {
				case !h.Created.IsZero():
					j.Created = h.Created
				case len(reaped) > 0:
					j.Created = launchAt
				}
				j.reapUnconfirmed = !listed
				return h, ie
			}
		}
		if lerr := leftover(reaped, listed); lerr != nil {
			return h, lerr
		}
		return h, err
	}
	return h, nil
}

// nothingMade: Launch proved it made no resource (provider.NothingCreated).
func nothingMade(err error) bool {
	var nc provider.NothingCreated
	return errors.As(err, &nc) && nc.NothingCreated()
}

// complete fetches result.json, verifies it, completes both streams and checks the sha256s.
func complete(ctx context.Context, j *Job, h provider.Handle, res provider.Result, s *hostSink) error {
	var raw bytes.Buffer
	if err := j.Prov.Output(ctx, h, provider.ResultFile, 0, &raw); err != nil {
		return fmt.Errorf("%s result: %w", h.ID, err)
	}
	rf, err := agent.VerifyResult(j.Spec.MacKey[:], raw.Bytes())
	if err != nil || rf.Exit != res.Exit {
		return fmt.Errorf("%s result failed verification", h.ID)
	}
	noted := false
	for _, st := range []struct {
		s    provider.Stream
		c    *counter
		want int64
		sum  string
	}{{provider.Stdout, s.out, rf.OutBytes, rf.OutSHA256}, {provider.Stderr, s.err, rf.ErrBytes, rf.ErrSHA256}} {
		have := int64(st.c.buf.Len())
		if have < st.want {
			if !noted {
				j.Note("the log stream missed the end of the job; the rest of its output follows")
				noted = true
			}
			if err := j.Prov.Output(ctx, h, st.s, have, st.c); err != nil {
				return fmt.Errorf("%s output: %w", h.ID, err)
			}
		}
		sum := sha256.Sum256(st.c.buf.Bytes())
		if hex.EncodeToString(sum[:]) != st.sum {
			return fmt.Errorf("%s result failed verification", h.ID)
		}
	}
	return nil
}

func fetchArtifacts(ctx context.Context, j *Job, h provider.Handle) error {
	acct, err := state.AccountDir(j.P.Eff)
	if err != nil {
		return err
	}
	tmp, err := os.MkdirTemp("", "bitc-artifacts-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	dst := filepath.Join(tmp, "a.tar.gz")
	max := int64(j.Core.Artifacts.MaxCompressed)
	err = artifacts.Download(ctx, func(off int64, w io.Writer) error {
		return j.Prov.Fetch(ctx, h, provider.ArtifactSpec{MaxCompressed: max, Offset: off}, w)
	}, dst, max)
	if err != nil {
		return err
	}
	if err := artifacts.Validate(dst, j.Spec.Artifacts, j.Core.Artifacts); err != nil {
		return err
	}
	return artifacts.Apply(acct, j.P.Eff.Worktree, j.Spec.Job, dst, j.Spec.Artifacts)
}

// reapByName tears down resources named like the job (the create may have succeeded server-side).
// It returns the ids of the resources found: one that was found existed, whether or not the
// teardown succeeded, so the call must not fall back to local (spec 15.8). listed is false when the
// name lookup itself failed: that proves nothing, so the caller must not treat it as "left nothing".
func reapByName(j *Job) (found []string, listed bool) {
	ctx, c := context.WithTimeout(context.Background(), 20*time.Second)
	defer c()
	list, err := j.Prov.List(ctx, []string{j.Core.NamePrefix + "-"})
	if err != nil {
		return nil, false
	}
	for _, r := range list {
		if r.Name == j.Spec.Name {
			found = append(found, r.ID)
			if j.Prov.Teardown(ctx, r.ID) == nil {
				j.Note("tore down %s, made by a create request that failed", r.ID)
			} else {
				j.Note("could not tear down %s, made by a create request that failed", r.ID)
			}
		}
	}
	return found, true
}

func history(j *Job, event, resource string, code *int) {
	e := state.Event{Event: event, Pool: j.P.Pool.Name, Job: j.P.Job, Pid: os.Getpid(), Argv: j.P.Args.Argv,
		Cwd: j.P.Eff.Cwd, Provider: j.Type, Resource: resource, Sha: j.Spec.Sha, Exit: code}
	_ = state.AppendHistory(j.P.Layout.History(), e, j.Red)
}

// noteSignal keeps the first signal caught.
func (j *Job) noteSignal(s os.Signal) {
	j.sigMu.Lock()
	defer j.sigMu.Unlock()
	if j.sigGot == 0 {
		j.sigGot, _ = s.(syscall.Signal)
	}
}

// interruption reports the signal that cut the job short, if any: one caught (also one still
// queued on the registration), or a `cancel --kill` cause on the parent context.
func (j *Job) interruption(parent context.Context) error {
	if j.sigs != nil {
		select {
		case s := <-j.sigs:
			j.noteSignal(s)
		default:
		}
	}
	j.sigMu.Lock()
	got := j.sigGot
	j.sigMu.Unlock()
	if got != 0 {
		return Interrupted{Sig: got}
	}
	var sg run.Signalled
	if errors.As(context.Cause(parent), &sg) {
		return Interrupted{Sig: syscall.Signal(sg)}
	}
	return nil
}

// signalWindow makes ctx end when a signal arrives, for the stretch between two attempts where no
// Orchestrate watcher runs (the retry's rails can wait for the launch lock). stop ends the watch.
func (j *Job) signalWindow(ctx context.Context) (context.Context, func()) {
	wctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	exit.Go(func() {
		defer close(done)
		select {
		case s := <-j.sigs:
			j.noteSignal(s)
			cancel()
		case <-wctx.Done():
		}
	})
	return wctx, func() { cancel(); <-done }
}
