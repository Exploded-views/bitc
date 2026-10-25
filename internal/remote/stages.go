// Package remote runs a job on a remote provider (spec 8.2-8.4) with the shared rails.
package remote

import (
	"context"
	"fmt"
	"io"
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/Exploded-views/bitc/internal/config"
	"github.com/Exploded-views/bitc/internal/provider"
	"github.com/Exploded-views/bitc/internal/redact"
	"github.com/Exploded-views/bitc/internal/run"
)

// Job is one remote attempt.
type Job struct {
	reapUnconfirmed bool // an interrupt before attach and the name lookup failed: the reservation becomes an orphan
	Inv             run.Invocation
	P               *run.Prepared
	Type            string
	Core            *config.Provider
	Prov            provider.Provider
	Spec            *provider.JobSpec
	Stdout          io.Writer
	Stderr          io.Writer
	Red             *redact.Set
	Handle          *provider.Handle // set once Launch succeeded
	Cleanup         []func()         // run in reverse on every exit path (spec 8.3)

	Estimate config.Micros // set by Rails
	RailsDir string        // ACCOUNT/remote/<type>, set by Rails
	// Retried: this job's one retry (spec 8.3) is spent, by a create retry or by Walk's pre-attach retry.
	Retried bool
	Created time.Time // when Launch succeeded (a resource exists and bills); zero if none
	Began   time.Time // when BEGIN was seen
	// Release drops this attempt's reservation (idempotent); Orphan keeps it as an orphan instead
	// (spec 8.3): both set by Rails, nil before it ran.
	Release func()
	Orphan  func()
	// DeleteRef deletes the job ref once; it runs at BEGIN and, at the latest, at the end.
	DeleteRef func()
	// KeepSignals (set by Walk) makes RunJob leave its signal registration in place and hand the
	// stop to StopSignals, which Walk calls after all cleanup (a signal during cleanup is absorbed).
	KeepSignals bool
	StopSignals func()

	// One signal registration serves every attempt of a Walk (spec 8.3): sigs is created by the
	// first Orchestrate and reused by the retry; sigGot is the first signal caught.
	sigs   chan os.Signal
	sigMu  sync.Mutex
	sigGot syscall.Signal
}

// Ledger and breaker hooks (set by internal/remote/rails); nil means no-op.
var (
	OnLaunch      func(j *Job)
	OnBegin       func(j *Job)
	OnEnd         func(j *Job, outcome string) // a code, "75", "fallback" (nothing created) or "interrupted"
	OnFailure     func(j *Job)                 // a 75 after BEGIN (never an interrupt)
	OnUnavailable func(j *Job, why string)     // a provider-side ErrUnavailable: one pause record
)

// CloneURLCheck is set by the transport: a usable clone URL whose host is in known
// hosts. The preflight calls it when it is set.
var CloneURLCheck func(j *Job) error

// Interrupted is a signal during a remote job: cleanup ran, exit 128+N (spec 8.3). RunJob
// returns it; Walk records "interrupted" and never counts it as a breaker failure.
type Interrupted struct{ Sig syscall.Signal }

func (i Interrupted) Error() string { return fmt.Sprintf("interrupted by signal %d", int(i.Sig)) }

// Note prints "bitc: <provider>: <msg>".
func (j *Job) Note(format string, a ...any) {
	fmt.Fprintf(j.Stderr, "bitc: %s: %s\n", j.Type, fmt.Sprintf(format, a...))
}

// Info prints "bitc: <provider>: <msg>" unless BITC_LOG=quiet (informational lines only).
func (j *Job) Info(format string, a ...any) {
	fmt.Fprintf(j.P.InfoWriter(j.Stderr), "bitc: %s: %s\n", j.Type, fmt.Sprintf(format, a...))
}

// Stages; later tasks replace the defaults (2.1b Preflight, 2.4 Rails, 2.5 Transport, 2.6a RunJob).
var (
	// Preflight returns *provider.ErrIneligible (or ErrUnavailable for credentials).
	Preflight = func(ctx context.Context, j *Job) error { return j.Prov.Preflight(ctx, j.Spec) }
	// Rails checks breaker, launch lock, cap and max_live; release runs at the end.
	Rails = func(ctx context.Context, j *Job) (release func(), err error) { return func() {}, nil }
	// Transport makes the commit reachable (git push); cleanup deletes the ref.
	Transport = func(ctx context.Context, j *Job) (cleanup func(), err error) { return func() {}, nil }
	// RunJob launches, watches and completes; began reports whether the command started.
	RunJob = basicRun
)

// basicRun is the minimal launch-and-watch used until orchestration replaces it.
func basicRun(ctx context.Context, j *Job) (int, bool, error) {
	var h provider.Handle
	var err error
	if j.retriesCreate() {
		h, err = j.launchWithRetry(ctx, nil)
	} else {
		launchAt := time.Now()
		if h, err = j.Prov.Launch(ctx, j.Spec); err != nil && ctx.Err() == nil && carriesResource(err) {
			err = j.classifyLaunch(err, launchAt)
		}
	}
	if err != nil {
		return 0, false, err
	}
	j.Handle, j.Created = &h, time.Now() // a resource exists: from here on, never a fallback (spec 8.2)
	j.Cleanup = append(j.Cleanup, func() { _ = j.Prov.Teardown(context.Background(), h.ID) })
	j.Info("%s job %s on %s (%s) at %.12s", j.Spec.Pool, j.Spec.Job, h.Name, j.Spec.SizeKey, j.Spec.Sha)
	res, err := j.Prov.Watch(ctx, h, sink{j})
	if err != nil {
		return 0, res.Began, err
	}
	return res.Exit, true, nil
}

type sink struct{ j *Job }

// Phase records BEGIN once (ledger begin); a setup failure never reaches it.
func (s sink) Phase(p provider.Phase, _ string) {
	if p == provider.Begun && s.j.Began.IsZero() {
		s.j.Began = time.Now()
		if OnBegin != nil {
			OnBegin(s.j)
		}
	}
}
func (s sink) Setup(line string) {
	fmt.Fprintf(s.j.P.InfoWriter(s.j.Stderr), "pod: %s\n", s.j.Red.String(line))
}
func (s sink) Out(st provider.Stream, b []byte) {
	if st == provider.Stderr {
		_, _ = s.j.Stderr.Write(b)
		return
	}
	_, _ = s.j.Stdout.Write(b)
}
