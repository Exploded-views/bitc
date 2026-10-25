package sprites

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Exploded-views/bitc/internal/agent"
	"github.com/Exploded-views/bitc/internal/agent/agenttest"
	"github.com/Exploded-views/bitc/internal/provider"
	"github.com/Exploded-views/bitc/internal/provider/sprites/spritesfake"
)

// recSink records what Watch reports.
type recSink struct {
	mu     sync.Mutex
	phases []provider.Phase
	setup  []string
	out    map[provider.Stream]*bytes.Buffer
}

func newSink() *recSink {
	return &recSink{out: map[provider.Stream]*bytes.Buffer{provider.Stdout: {}, provider.Stderr: {}}}
}
func (s *recSink) Phase(p provider.Phase, _ string) {
	s.mu.Lock()
	s.phases = append(s.phases, p)
	s.mu.Unlock()
}
func (s *recSink) Setup(l string) { s.mu.Lock(); s.setup = append(s.setup, l); s.mu.Unlock() }
func (s *recSink) Out(st provider.Stream, b []byte) {
	s.mu.Lock()
	s.out[st].Write(b)
	s.mu.Unlock()
}
func (s *recSink) stdout() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.out[provider.Stdout].String()
}
func (s *recSink) stderr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.out[provider.Stderr].String()
}

// rig is a provider with a launched job on a fake.
type rig struct {
	t    *testing.T
	f    *spritesfake.Fake
	p    *Provider
	j    *provider.JobSpec
	h    provider.Handle
	sink *recSink
}

// launchRig launches script as a job: the agent's spec is the agenttest fixture's, with the job's
// nonce and K. edit changes the fixture and the job before the launch.
func launchRig(t *testing.T, script string, edit func(fx *agenttest.Fixture, j *provider.JobSpec)) *rig {
	t.Helper()
	f := spritesfake.New(t, spritesfake.Options{Bin: spritesfake.BuildBitc(t), AgentSHA: testSHA, Exec: func(spritesfake.ExecContext) int { return 0 }})
	p := newProv(t, f, nil)
	fx := agenttest.NewFixture(t, script)
	now := time.Now()
	j := &provider.JobSpec{Job: fx.Spec.Job, Pool: "heavy", SizeKey: "cpu8", Created: now, Timeout: time.Minute, Deadline: now.Add(2*time.Minute + time.Minute),
		AgentURL: "https://downloads.example.com/bitc", AgentSHA256: testSHA, Unprivileged: true, CloneURL: fx.Repo, Install: testInstall}
	j.Nonce[0], j.Nonce[31] = 7, 9
	copy(j.MacKey[:], fx.K)
	j.Expiry = j.Deadline.Add(15 * time.Minute)
	if edit != nil {
		edit(fx, j)
	}
	j.Name = p.ResourceName(testSecret, "bitc", testInstall, "heavy", "cpu8", j.Job, j.Expiry, 63)
	old := AgentSpec
	AgentSpec = func(j *provider.JobSpec) agent.Spec {
		s := fx.Spec
		s.Nonce, s.MacKey, s.Job = hex.EncodeToString(j.Nonce[:]), hex.EncodeToString(j.MacKey[:]), j.Job
		s.Unprivileged = true
		return s
	}
	t.Cleanup(func() { AgentSpec = old })
	h, err := p.Launch(context.Background(), j)
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	t.Cleanup(func() { _ = p.Teardown(context.Background(), h.ID) })
	return &rig{t: t, f: f, p: p, j: j, h: h, sink: newSink()}
}

func (r *rig) watch() (provider.Result, error) {
	r.t.Helper()
	type res struct {
		r provider.Result
		e error
	}
	ch := make(chan res, 1)
	go func() { x, e := r.p.Watch(context.Background(), r.h, r.sink); ch <- res{x, e} }()
	select {
	case x := <-ch:
		return x.r, x.e
	case <-time.After(60 * time.Second):
		r.t.Fatal("Watch did not end")
		return provider.Result{}, nil
	}
}

func (r *rig) waitStdout(want string) {
	r.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !strings.Contains(r.sink.stdout(), want) {
		if time.Now().After(deadline) {
			r.t.Fatalf("stdout never held %q: %q", want, r.sink.stdout())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestWatchStreamsAJobToItsExit(t *testing.T) {
	r := launchRig(t, "echo to-out; echo to-err >&2; exit 3", nil)
	res, err := r.watch()
	if err != nil || res.Exit != 3 || !res.Began {
		t.Fatalf("%+v %v", res, err)
	}
	if r.sink.stdout() != "to-out\n" || r.sink.stderr() != "to-err\n" || len(r.sink.phases) != 1 || r.sink.phases[0] != provider.Begun {
		t.Fatalf("stdout %q stderr %q phases %v", r.sink.stdout(), r.sink.stderr(), r.sink.phases)
	}
	// the provisioning output comes first, as pod: lines, then the agent's own lines
	if len(r.sink.setup) < 3 || !strings.HasPrefix(r.sink.setup[0], "provision: installing") || r.sink.setup[1] != "provision: done" || !strings.Contains(strings.Join(r.sink.setup, "|"), "provisioned: skipping") {
		t.Fatalf("setup lines %q", r.sink.setup)
	}
	var raw bytes.Buffer
	if err := r.p.Output(context.Background(), r.h, provider.ResultFile, 0, &raw); err != nil {
		t.Fatal(err)
	}
	if rf, err := agent.VerifyResult(r.j.MacKey[:], raw.Bytes()); err != nil || rf.Exit != 3 {
		t.Fatalf("%+v %v", rf, err)
	}
	var tail bytes.Buffer
	if err := r.p.Output(context.Background(), r.h, provider.Stdout, 3, &tail); err != nil || tail.String() != "out\n" {
		t.Fatalf("output from an offset: %q %v", tail.String(), err)
	}
	if err := r.p.Output(context.Background(), r.h, provider.Stdout, 99, &tail); err == nil || !strings.Contains(err.Error(), "beyond the end") {
		t.Fatalf("offset past the end: %v", err)
	}
	if err := r.p.Output(context.Background(), r.h, provider.Stream(42), 0, &tail); err == nil {
		t.Fatal("an unknown stream was read")
	}
	if err := r.p.Output(context.Background(), provider.Handle{ID: "bitc-other"}, provider.Stdout, 0, &tail); err == nil {
		t.Fatal("a Sprite this process did not launch was read")
	}
}

func TestWatchResumesADroppedTailFromItsOffsets(t *testing.T) {
	gate := filepath.Join(t.TempDir(), "gate")
	r := launchRig(t, "echo first; while [ ! -e "+gate+" ]; do sleep 0.05; done; echo second", nil)
	done := make(chan struct{})
	var res provider.Result
	var err error
	go func() { res, err = r.watch(); close(done) }()
	r.waitStdout("first\n")
	r.f.CutSessions(r.h.ID) // the tail drops mid-stream
	time.Sleep(300 * time.Millisecond)
	if err := os.WriteFile(gate, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	<-done
	if err != nil || res.Exit != 0 || r.sink.stdout() != "first\nsecond\n" {
		t.Fatalf("%+v %v stdout %q: a resumed tail must not repeat or lose output", res, err, r.sink.stdout())
	}
}

func TestWatchPollsWithReadWhenTheTailKeepsFailing(t *testing.T) {
	r := launchRig(t, "echo polled; echo e >&2; exit 5", nil)
	r.f.BehaveN("drop-tail", -1) // every reopen is cut: two failures within 30 s switch to reads
	res, err := r.watch()
	if err != nil || res.Exit != 5 || !res.Began || r.sink.stdout() != "polled\n" || r.sink.stderr() != "e\n" {
		t.Fatalf("%+v %v stdout %q stderr %q", res, err, r.sink.stdout(), r.sink.stderr())
	}
	reads := 0
	for _, x := range r.f.Execs() {
		if strings.Contains(strings.Join(x.Query["cmd"], " "), "__agent read") {
			reads++
		}
	}
	if reads == 0 {
		t.Fatal("no __agent read: the watch never polled")
	}
}

func TestWatchSpriteGoneIsALostJob(t *testing.T) {
	r := launchRig(t, "echo up; sleep 60", nil)
	done := make(chan struct{})
	var res provider.Result
	var err error
	go func() { res, err = r.watch(); close(done) }()
	r.waitStdout("up\n")
	if e := r.p.cli.DeleteSprite(context.Background(), r.h.ID); e != nil {
		t.Fatal(e)
	}
	<-done
	if err == nil || !strings.Contains(err.Error(), "is gone") || !res.Began {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestWatchAnotherOrganizationEndsTheJob(t *testing.T) {
	r := launchRig(t, "echo up; sleep 60", nil)
	done := make(chan struct{})
	var res provider.Result
	var err error
	go func() { res, err = r.watch(); close(done) }()
	r.waitStdout("up\n")
	r.f.Behave("wrong-org")
	<-done
	var un *provider.ErrUnavailable
	var om *OrgMismatchError
	if !errors.As(err, &un) || !errors.As(err, &om) || !res.Began {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestWatchStartTimeoutIsAnErrorBeforeBegin(t *testing.T) {
	r := launchRig(t, "echo never", func(fx *agenttest.Fixture, j *provider.JobSpec) {
		fx.Spec.Setup = []agent.Step{{Name: "slow", RunAs: "job", Argv: []string{"sleep", "30"}}}
		j.Timeout = time.Second
		j.Deadline = time.Now().Add(4 * time.Second) // the start timeout is Deadline - Timeout: 3 s
	})
	res, err := r.watch()
	if err == nil || !strings.Contains(err.Error(), "did not start the command within") || res.Began {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestWatchSetupFailureIsAnErrSetupWithItsClass(t *testing.T) {
	for _, c := range []struct {
		name  string
		steps []agent.Step
		class string
	}{
		{"repo", []agent.Step{{Name: "deps", RunAs: "job", Argv: []string{"sh", "-c", "echo npm ERR >&2; exit 1"}}}, "repo"},
		{"infra", []agent.Step{{Name: "deps", RunAs: "job", Argv: []string{"sh", "-c", "kill -9 $$"}}}, "infra"},
	} {
		r := launchRig(t, "echo RAN", func(fx *agenttest.Fixture, j *provider.JobSpec) { fx.Spec.Setup = c.steps })
		_, err := r.watch()
		var se *provider.ErrSetup
		if !errors.As(err, &se) || se.Step != "deps" || se.Class != c.class || strings.Contains(r.sink.stdout(), "RAN") {
			t.Errorf("%s: %v", c.name, err)
		}
	}
}

func TestFetchIsCappedAndResumableByOffset(t *testing.T) {
	r := launchRig(t, "mkdir -p ../out; head -c 3000 /dev/urandom > ../out/blob", func(fx *agenttest.Fixture, j *provider.JobSpec) {
		fx.Spec.Artifacts = []string{"out"}
	})
	if _, err := r.watch(); err != nil {
		t.Fatal(err)
	}
	var all bytes.Buffer
	if err := r.p.Fetch(context.Background(), r.h, provider.ArtifactSpec{MaxCompressed: 1 << 20}, &all); err != nil || all.Len() < 3000 {
		t.Fatalf("%d bytes, %v", all.Len(), err)
	}
	var rest bytes.Buffer
	if err := r.p.Fetch(context.Background(), r.h, provider.ArtifactSpec{MaxCompressed: 1 << 20, Offset: 1000}, &rest); err != nil || !bytes.Equal(rest.Bytes(), all.Bytes()[1000:]) {
		t.Fatalf("resume from 1000: %d bytes, %v", rest.Len(), err)
	}
	var capped bytes.Buffer
	err := r.p.Fetch(context.Background(), r.h, provider.ArtifactSpec{MaxCompressed: 2000}, &capped)
	if err == nil || !strings.Contains(err.Error(), "over max_compressed") || capped.Len() > 2000 {
		t.Fatalf("cap: %v (%d bytes written)", err, capped.Len())
	}
	if err := r.p.Fetch(context.Background(), provider.Handle{ID: "bitc-other"}, provider.ArtifactSpec{MaxCompressed: 10}, &capped); err == nil {
		t.Fatal("fetched from a Sprite this process did not launch")
	}
}

// the marker and frame rules, without a Sprite
func newBareWatcher() (*watcher, *recSink) {
	s := newSink()
	return &watcher{out: s, nonce: "n", scale: func(d time.Duration) time.Duration { return d }, id: "bitc-x",
		offs: map[string]int64{"agent.log": 0, "out.log": 0, "err.log": 0}}, s
}

func TestWatcherMarkersFollowTheNonceAndTheBootRules(t *testing.T) {
	w, s := newBareWatcher()
	for _, l := range []string{"__BITC_BOOT__ other", "__BITC_BEGIN__ other", "pod text"} {
		if err := w.line(l); err != nil {
			t.Fatal(err)
		}
	}
	if w.began || len(s.setup) != 3 {
		t.Fatalf("another job's markers counted: began %v setup %q", w.began, s.setup)
	}
	for _, l := range []string{"__BITC_BOOT__ n", "__BITC_BEGIN__ n", "after begin text"} {
		if err := w.line(l); err != nil {
			t.Fatal(err)
		}
	}
	if !w.began || len(s.setup) != 3 || len(s.phases) != 1 {
		t.Fatalf("began %v setup %q phases %v", w.began, s.setup, s.phases)
	}
	var f *fatal
	if err := w.line("__BITC_BOOT__ n"); !errors.As(err, &f) || !f.res.Began || !strings.Contains(err.Error(), "restarted while the command ran") {
		t.Fatalf("a second BOOT after BEGIN: %v", err)
	}
	if err := w.line("__BITC_LOST__ n"); !errors.As(err, &f) || !f.res.Began {
		t.Fatalf("LOST after BEGIN: %v", err)
	}
	w2, _ := newBareWatcher()
	if err := w2.line("__BITC_LOST__ n"); !errors.As(err, &f) || f.res.Began || !strings.Contains(err.Error(), "before the command started") {
		t.Fatalf("LOST before BEGIN: %v", err)
	}
	if err := w2.line("__BITC_EXIT__ n 7"); err != nil || !w2.exitSeen || w2.exitCode != 7 {
		t.Fatalf("EXIT: %v %+v", err, w2)
	}
}

func TestWatcherFramesResumeOverlapsAndRefuseGaps(t *testing.T) {
	w, s := newBareWatcher()
	frame := func(off int64, data string) error {
		return w.frame(agent.Frame{File: "out.log", Off: off, Data: []byte(data)})
	}
	if err := frame(0, "abcd"); err != nil {
		t.Fatal(err)
	}
	if err := frame(2, "cdef"); err != nil { // a replay overlapping what the host has
		t.Fatal(err)
	}
	if err := frame(0, "ab"); err != nil { // wholly seen
		t.Fatal(err)
	}
	if s.stdout() != "abcdef" || w.offs["out.log"] != 6 {
		t.Fatalf("stdout %q offset %d", s.stdout(), w.offs["out.log"])
	}
	if err := frame(9, "zz"); !errors.Is(err, errGap) && err == nil {
		t.Fatalf("a frame past the offset: %v", err)
	}
	if s.stdout() != "abcdef" {
		t.Fatalf("a gap was forwarded: %q", s.stdout())
	}
	// a partial line waits for its newline
	if err := w.frame(agent.Frame{File: "agent.log", Off: 0, Data: []byte("par")}); err != nil || len(s.setup) != 0 {
		t.Fatalf("%v %q", err, s.setup)
	}
	if err := w.frame(agent.Frame{File: "agent.log", Off: 3, Data: []byte("tial\nnext")}); err != nil || strings.Join(s.setup, "|") != "partial" {
		t.Fatalf("%v %q", err, s.setup)
	}
}

func TestLaunchReportsTheCreateTimeAheadOfProvisioning(t *testing.T) {
	r := launchRig(t, "true", nil)
	if r.h.Created.IsZero() {
		t.Fatal("Handle.Created is not set")
	}
	if time.Since(r.h.Created) < 0 || time.Since(r.h.Created) > time.Minute {
		t.Fatalf("created %v", r.h.Created)
	}
}
