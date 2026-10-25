package remote

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Exploded-views/bitc/internal/agent"
	"github.com/Exploded-views/bitc/internal/config"
	"github.com/Exploded-views/bitc/internal/provider"
	"github.com/Exploded-views/bitc/internal/redact"
	"github.com/Exploded-views/bitc/internal/run"
	"github.com/Exploded-views/bitc/internal/state"
)

type scriptProvider struct {
	streamed  string // printed live before the stream "missed the end"
	full      string // the whole out.log
	exit      int
	forge     bool
	k         []byte
	watchExit *int   // what Watch reports when it read the exit from state.json
	why       string // Result.Why of that path
}

func (s *scriptProvider) Info() provider.Info {
	return provider.Info{Type: "script", Caps: provider.Caps{LiveLogs: true}}
}
func (s *scriptProvider) Preflight(context.Context, *provider.JobSpec) error { return nil }
func (s *scriptProvider) Estimate(context.Context, *provider.JobSpec) (config.Micros, error) {
	return 0, nil
}
func (s *scriptProvider) Launch(_ context.Context, j *provider.JobSpec) (provider.Handle, error) {
	s.k = j.MacKey[:]
	return provider.Handle{ID: "r1", Name: j.Name}, nil
}
func (s *scriptProvider) Watch(_ context.Context, _ provider.Handle, out provider.Sink) (provider.Result, error) {
	out.Phase(provider.Begun, "")
	out.Out(provider.Stdout, []byte(s.streamed))
	if s.watchExit != nil {
		return provider.Result{Exit: *s.watchExit, Began: true, Why: s.why}, nil
	}
	return provider.Result{Exit: s.exit, Began: true}, nil
}
func (s *scriptProvider) Output(_ context.Context, _ provider.Handle, st provider.Stream, off int64, w io.Writer) error {
	switch st {
	case provider.Stdout:
		_, _ = w.Write([]byte(s.full[off:]))
	case provider.ResultFile:
		rf := agent.Sign(s.k, agent.ResultFile{Exit: s.exit, OutBytes: int64(len(s.full)), OutSHA256: sha256hex([]byte(s.full)),
			ErrSHA256: sha256hex(nil), ArtifactsSHA256: ""})
		if s.forge {
			rf.Exit = 0
		}
		b, _ := json.Marshal(rf)
		_, _ = w.Write(b)
	}
	return nil
}
func (s *scriptProvider) Fetch(context.Context, provider.Handle, provider.ArtifactSpec, io.Writer) error {
	return nil
}
func (s *scriptProvider) Owner(context.Context, string) (string, error) { return "", nil }
func (s *scriptProvider) Teardown(context.Context, string) error        { return nil }
func (s *scriptProvider) List(context.Context, []string) ([]provider.Resource, error) {
	return nil, nil
}
func (s *scriptProvider) Spend(context.Context) (config.Micros, error) { return 0, nil }

func TestOutputCompletionAndVerification(t *testing.T) {
	var out, errb bytes.Buffer
	sp := &scriptProvider{streamed: "line1\n", full: "line1\nline2\n", exit: 3}
	j := testJob(t, sp, &out, &errb)
	code, began, err := Orchestrate(context.Background(), j)
	if err != nil || !began || code != 3 || out.String() != "line1\nline2\n" ||
		!strings.Contains(errb.String(), "the log stream missed the end of the job; the rest of its output follows") {
		t.Fatalf("%d %v %v %q %q", code, began, err, out.String(), errb.String())
	}
	sp = &scriptProvider{streamed: "x\n", full: "x\n", exit: 1, forge: true}
	j = testJob(t, sp, &out, &errb)
	if _, began, err := Orchestrate(context.Background(), j); err == nil || !began || !strings.Contains(err.Error(), "result failed verification") {
		t.Fatal(err)
	}
}

// An exit that Watch took from state.json never decides the job: the MAC-verified result.json
// must agree, on both state paths, or the job is a verification failure and not a success.
func TestStateExitNeverOverridesResult(t *testing.T) {
	zero := 0
	for _, why := range []string{"stream-missed-begin", "stream-missed-end"} {
		var out, errb bytes.Buffer
		sp := &scriptProvider{streamed: "x\n", full: "x\n", exit: 3, watchExit: &zero, why: why}
		j := testJob(t, sp, &out, &errb)
		code, _, err := Orchestrate(context.Background(), j)
		if err == nil || !strings.Contains(err.Error(), "result failed verification") {
			t.Fatalf("%s: code=%d err=%v", why, code, err)
		}
	}
}

// A TestOnly provider (the in-process fake) keeps basicRun: no agent source, no result.json.
func TestOrchestrateKeepsBasicRunForTestOnly(t *testing.T) {
	provider.Register("script-testonly", provider.Factory{TestOnly: true})
	var out, errb bytes.Buffer
	sp := &scriptProvider{streamed: "only\n", full: "", exit: 4} // Output serves no result.json
	j := testJob(t, sp, &out, &errb)
	j.Type, j.P.Eff.Test.AgentURL = "script-testonly", "" // no agent seam: ResolveAgent would be ErrIneligible
	code, began, err := Orchestrate(context.Background(), j)
	if err != nil || !began || code != 4 || out.String() != "only\n" {
		t.Fatalf("%d %v %v %q %q", code, began, err, out.String(), errb.String())
	}
}

// testJob is a Job over p with no artifacts, a test agent source and buffers for its output.
func testJob(t *testing.T, p provider.Provider, out, errb *bytes.Buffer) *Job {
	t.Helper()
	out.Reset()
	errb.Reset()
	dir := t.TempDir()
	eff := &config.Effective{Worktree: dir, Cwd: dir, Host: config.Host{StateDir: dir}, LogLevel: "info",
		Test: &config.Test{AgentURL: "http://127.0.0.1:9/bitc", AccountingDir: filepath.Join(dir, "account")}}
	prep := &run.Prepared{Eff: eff, Pool: &config.Pool{Name: "heavy"}, Layout: state.Layout{Root: dir}, Job: "j1",
		Redact: redact.New(), Args: run.RunArgs{Argv: []string{"true"}}}
	spec := &provider.JobSpec{Job: "j1", Pool: "heavy", Name: "bitc-test-heavy-j1", Bearer: rand32(), MacKey: rand32(), Nonce: rand32()}
	return &Job{P: prep, Type: "script", Core: &config.Provider{NamePrefix: "bitc"}, Prov: p, Spec: spec,
		Stdout: out, Stderr: errb, Red: prep.Redact}
}

func sha256hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// With KeepSignals (Walk) Orchestrate leaves its signal registration in place until StopSignals:
// a signal after it returned, during Walk's cleanup, is absorbed instead of killing the process.
func TestOrchestrateKeepsSignalsUntilStopSignals(t *testing.T) {
	var out, errb bytes.Buffer
	sp := &scriptProvider{streamed: "a\n", full: "a\n"}
	j := testJob(t, sp, &out, &errb)
	j.KeepSignals = true
	if _, _, err := Orchestrate(context.Background(), j); err != nil {
		t.Fatal(err)
	}
	if j.StopSignals == nil {
		t.Fatal("no StopSignals handed to Walk")
	}
	defer j.StopSignals()
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond) // the default action would have ended this test binary
}

// attachStub is a non-TestOnly provider whose every Launch leaves a resource that fails before
// attach with a transient error, under the "attach" boundary (spec 8.2).
type attachStub struct {
	scriptProvider
	launches int
}

func (s *attachStub) Info() provider.Info {
	return provider.Info{Type: "attach-stub", Caps: provider.Caps{FallbackUntil: provider.UntilAttach}}
}
func (s *attachStub) Launch(_ context.Context, j *provider.JobSpec) (provider.Handle, error) {
	s.launches++
	return provider.Handle{}, &provider.ErrPreAttach{ID: "r" + strconv.Itoa(s.launches),
		Err: &provider.ErrTransient{Why: "setup step packages failed"}}
}

// A signal in Walk's retry window (after a pre-attach discard, before the second Launch) is not
// lost: no second resource is created, the ledger end is "interrupted", no breaker record, 128+N,
// and no signal registration is left behind (spec 8.3).
func TestWalkSignalInRetryWindowIsNotLost(t *testing.T) {
	stub := &attachStub{}
	provider.Register("attach-stub", provider.Factory{
		New: func(*config.Provider, *config.Effective) (provider.Provider, error) { return stub, nil }})
	var out, errb bytes.Buffer
	j := testJob(t, stub, &out, &errb)
	eff, p := j.P.Eff, j.P
	core := &config.Provider{Type: "attach-stub", NamePrefix: "bitc", FallbackUntil: "attach",
		StartTimeout: time.Minute, Raw: map[string]any{}}
	eff.Providers = map[string]*config.Provider{"attach-stub": core}
	eff.RemoteLimits.Worktrees = []string{"**"}
	p.Pool.Providers = []string{"attach-stub"}
	p.Pool.RemoteSize = map[string]string{"attach-stub": "s"}
	p.Pool.Remote = &config.PoolRemote{}

	oPre, oTr, oRails, oEnd, oFail, oLaunch := Preflight, Transport, Rails, OnEnd, OnFailure, OnLaunch
	defer func() {
		Preflight, Transport, Rails, OnEnd, OnFailure, OnLaunch = oPre, oTr, oRails, oEnd, oFail, oLaunch
	}()
	Preflight = func(context.Context, *Job) error { return nil }
	Transport = func(context.Context, *Job) (func(), error) { return func() {}, nil }
	var railsCalls, failures int
	var ends []string
	Rails = func(context.Context, *Job) (func(), error) {
		railsCalls++
		if railsCalls == 2 { // the retry's rails: the signal lands while they run
			_ = syscall.Kill(os.Getpid(), syscall.SIGINT)
			time.Sleep(200 * time.Millisecond)
		}
		return func() {}, nil
	}
	OnEnd = func(_ *Job, outcome string) { ends = append(ends, outcome) }
	OnFailure = func(*Job) { failures++ }
	OnLaunch = nil

	oc, handled, _ := Walk(context.Background(), run.Invocation{}, p, "attach-stub", &errb)
	if !handled || oc.Code != 130 || oc.Result.Outcome != "interrupted" {
		t.Fatalf("handled=%v code=%d outcome=%q, want 130 interrupted; stderr %q", handled, oc.Code, oc.Result.Outcome, errb.String())
	}
	if stub.launches != 1 {
		t.Fatalf("%d Launch calls, want 1 (no second paid resource)", stub.launches)
	}
	if want := []string{"fallback", "interrupted"}; !slices.Equal(ends, want) {
		t.Fatalf("ledger ends %v, want %v", ends, want)
	}
	if failures != 1 { // the discarded first resource only
		t.Fatalf("%d breaker records, want 1", failures)
	}
	if n := sigRegs.Load(); n != 0 {
		t.Fatalf("%d signal registrations left", n)
	}
}
