package remote

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Exploded-views/bitc/internal/config"
	"github.com/Exploded-views/bitc/internal/provider"
	"github.com/Exploded-views/bitc/internal/run"
)

// pluginStub is a provider that needs starting and closing, as a plugin does.
type pluginStub struct {
	scriptProvider
	startErr   error
	maxName    int
	launchErr  error
	events     *[]string
	names      []string
	teardowns  []string
	closed     bool
	sawRuntime provider.Runtime
}

func (s *pluginStub) Info() provider.Info {
	return provider.Info{Type: "plug", Caps: provider.Caps{FallbackUntil: provider.UntilAttach}, MaxName: s.maxName}
}
func (s *pluginStub) Start(_ context.Context, rt provider.Runtime) error {
	s.sawRuntime = rt
	return s.startErr
}
func (s *pluginStub) Launch(_ context.Context, j *provider.JobSpec) (provider.Handle, error) {
	s.names = append(s.names, j.Name)
	return provider.Handle{}, s.launchErr
}
func (s *pluginStub) Teardown(_ context.Context, id string) error {
	s.teardowns = append(s.teardowns, id)
	*s.events = append(*s.events, "teardown")
	return nil
}
func (s *pluginStub) Close() error {
	s.closed = true
	*s.events = append(*s.events, "close")
	return nil
}

// walkPlug runs Walk over a pluginStub with the rails stubbed out and returns what it saw.
type plugRun struct {
	oc       run.Outcome
	handled  bool
	err      error
	stderr   string
	failures int
	orphans  int
	ends     []string
	events   []string
}

func walkPlug(t *testing.T, stub *pluginStub) *plugRun { return walkPlugNamed(t, stub, false) }

// walkPlugNamed with long set uses the longest prefix, size key and a long pool name, so a name
// must be cut to fit.
func walkPlugNamed(t *testing.T, stub *pluginStub, long bool) *plugRun {
	t.Helper()
	pr := &plugRun{}
	stub.events = &pr.events
	provider.Register("plug", provider.Factory{
		New: func(*config.Provider, *config.Effective) (provider.Provider, error) { return stub, nil }})
	var out, errb bytes.Buffer
	j := testJob(t, stub, &out, &errb)
	eff, p := j.P.Eff, j.P
	eff.HostFile = "/etc/bitc.toml"
	eff.Test.TimeScale = 0.01
	core := &config.Provider{Type: "plug", NamePrefix: "bitc", FallbackUntil: "attach", StartTimeout: time.Minute, Raw: map[string]any{}}
	eff.Providers = map[string]*config.Provider{"plug": core}
	eff.RemoteLimits.Worktrees = []string{"**"}
	p.Pool.Providers = []string{"plug"}
	p.Pool.RemoteSize = map[string]string{"plug": "s"}
	p.Pool.Remote = &config.PoolRemote{}
	p.Pool.RemoteTimeout = time.Hour
	p.Args.Argv = []string{"true"}
	if long {
		core.NamePrefix, p.Pool.Name = "abcdefghij", strings.Repeat("p", 40)
		p.Pool.RemoteSize = map[string]string{"plug": "ssssssssss"}
	}

	oPre, oTr, oRails, oEnd, oFail, oLaunch := Preflight, Transport, Rails, OnEnd, OnFailure, OnLaunch
	defer func() {
		Preflight, Transport, Rails, OnEnd, OnFailure, OnLaunch = oPre, oTr, oRails, oEnd, oFail, oLaunch
	}()
	Preflight = func(context.Context, *Job) error { return nil }
	Transport = func(context.Context, *Job) (func(), error) { return func() {}, nil }
	Rails = func(_ context.Context, jj *Job) (func(), error) {
		jj.Orphan = func() { pr.orphans++ }
		return func() {}, nil
	}
	OnEnd = func(_ *Job, outcome string) { pr.ends = append(pr.ends, outcome); pr.events = append(pr.events, "end") }
	OnFailure = func(*Job) { pr.failures++ }
	OnLaunch = nil
	pr.oc, pr.handled, pr.err = Walk(context.Background(), run.Invocation{}, p, "plug", &errb)
	pr.stderr = errb.String()
	return pr
}

func TestWalkStartConfigErrorEnds78WithTheHostFileLine(t *testing.T) {
	stub := &pluginStub{startErr: &provider.ErrConfig{Why: "providers.plug.plugin: max_name 59 is below 60, the shortest name bitc builds"}}
	r := walkPlug(t, stub)
	if !r.handled || r.oc.Code != 78 || !strings.Contains(r.stderr, "bitc: /etc/bitc.toml: providers.plug.plugin: max_name 59 is below 60") {
		t.Fatalf("handled %v code %d stderr %q", r.handled, r.oc.Code, r.stderr)
	}
	if len(stub.names) != 0 {
		t.Error("a job was launched after a config error")
	}
}

func TestWalkStartFailureIsAProviderFailureNotAConfigError(t *testing.T) {
	stub := &pluginStub{startErr: &provider.ErrUnavailable{Why: "plugin protocol error: x", Local: true}}
	r := walkPlug(t, stub)
	var una *provider.ErrUnavailable
	if r.handled || !errors.As(r.err, &una) {
		t.Fatalf("handled %v err %v: the walk should pass to the next provider", r.handled, r.err)
	}
}

func TestWalkClosesTheProviderAfterEveryCleanupStep(t *testing.T) {
	stub := &pluginStub{launchErr: &provider.ErrAttached{ID: "r1", Err: &provider.ErrTransient{Why: "boom"}}}
	r := walkPlug(t, stub)
	if !stub.closed || r.events[len(r.events)-1] != "close" {
		t.Fatalf("events %v: the plugin process must be stopped last", r.events)
	}
}

func TestWalkGivesTheProviderTheRedactorAndDebugFlag(t *testing.T) {
	stub := &pluginStub{launchErr: &provider.ErrTransient{Why: "x"}}
	walkPlug(t, stub)
	if stub.sawRuntime.Red == nil || stub.sawRuntime.Stderr == nil {
		t.Fatalf("runtime %+v", stub.sawRuntime)
	}
}

func TestWalkBuildsNamesWithinThePluginsMaxName(t *testing.T) {
	stub := &pluginStub{maxName: 60, launchErr: &provider.ErrTransient{Why: "x"}}
	walkPlugNamed(t, stub, true)
	if len(stub.names) == 0 {
		t.Fatal("no launch")
	}
	for _, n := range stub.names {
		if len(n) > 60 || !strings.HasPrefix(n, "abcdefghij-") {
			t.Errorf("name %q is %d characters, max_name is 60", n, len(n))
		}
	}
}

func TestWalkAmbiguousLaunchIsOneBreakerFailureAnd75(t *testing.T) {
	stub := &pluginStub{launchErr: &provider.ErrAttached{ID: "bitc-x", Ambiguous: true,
		Err: &provider.ErrTransient{Why: "launch ambiguous: launch timed out after 20m0s"}}}
	r := walkPlug(t, stub)
	if r.oc.Code != 75 || !strings.Contains(r.stderr, "STARVED heavy (plug: launch ambiguous: launch timed out after 20m0s)") {
		t.Fatalf("code %d stderr %q", r.oc.Code, r.stderr)
	}
	if r.failures != 1 {
		t.Errorf("%d breaker failures, want 1", r.failures)
	}
	if len(stub.teardowns) != 0 {
		t.Errorf("core tore down %v: the adapter reaped by name already", stub.teardowns)
	}
	if r.orphans != 0 {
		t.Errorf("a confirmed reap kept an orphan")
	}
	if len(stub.names) != 1 {
		t.Errorf("%d launches, an ambiguous launch is never retried", len(stub.names))
	}
}

func TestWalkUnconfirmedReapKeepsTheReservationAsAnOrphan(t *testing.T) {
	stub := &pluginStub{launchErr: &provider.ErrAttached{ID: "bitc-x", Ambiguous: true, Unconfirmed: true,
		Err: &provider.ErrTransient{Why: "launch ambiguous: x; bitc-x left for sweep"}}}
	r := walkPlug(t, stub)
	if r.orphans != 1 || r.oc.Code != 75 {
		t.Fatalf("orphans %d code %d", r.orphans, r.oc.Code)
	}
}

func TestWalkAttachedWithIDStillTearsDownByID(t *testing.T) {
	stub := &pluginStub{launchErr: &provider.ErrAttached{ID: "r9", Err: &provider.ErrTransient{Why: "capacity after create"}}}
	r := walkPlug(t, stub)
	if len(stub.teardowns) != 1 || stub.teardowns[0] != "r9" || r.failures != 0 {
		t.Fatalf("teardowns %v failures %d", stub.teardowns, r.failures)
	}
}
