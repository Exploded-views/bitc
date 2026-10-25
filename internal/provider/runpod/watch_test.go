package runpod

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Exploded-views/bitc/internal/agent"
	"github.com/Exploded-views/bitc/internal/provider"
)

// A non-2xx answer from the log endpoint must be an error so Watch backs off, not a
// tight request loop.
func TestWatchBacksOffOnLogErrors(t *testing.T) {
	var logCalls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/pods/pod-429/logs" {
			logCalls.Add(1)
			w.WriteHeader(429)
			return
		}
		w.WriteHeader(401) // status() fails too
	}))
	defer srv.Close()
	p := &Provider{api: &client{base: srv.URL, key: "k", http: srv.Client()}, scale: func(d time.Duration) time.Duration { return d }}
	now := time.Now()
	jobsMu.Lock()
	jobs["pod-429"] = &provider.JobSpec{Timeout: 30 * time.Minute, Created: now, Deadline: now.Add(time.Hour)}
	jobsMu.Unlock()
	t.Cleanup(func() { jobsMu.Lock(); delete(jobs, "pod-429"); jobsMu.Unlock() })

	if _, _, err := p.logSlice(context.Background(), "pod-429", time.Second); err == nil {
		t.Fatal("a 429 from the log endpoint returned no error")
	}
	logCalls.Store(0)
	ctx, cancel := context.WithTimeout(context.Background(), 2500*time.Millisecond)
	defer cancel()
	_, _ = p.Watch(ctx, provider.Handle{ID: "pod-429"}, nil)
	if n := logCalls.Load(); n < 1 || n > 4 {
		t.Fatalf("%d log requests in 2.5s, want a backed-off 1..4", n)
	}
}

func watchJob(id string, startIn time.Duration) {
	now := time.Now()
	jobsMu.Lock()
	// startBy = Deadline - Timeout = now + startIn
	jobs[id] = &provider.JobSpec{Timeout: time.Minute, Created: now, Deadline: now.Add(time.Minute + startIn)}
	jobsMu.Unlock()
}

// A status poll that never answers must not keep Watch from its start-timeout check.
func TestWatchEnforcesStartDeadlineWhenStatusStalls(t *testing.T) {
	stop := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/pods/pod-stall/logs": // an idle SSE stream
			w.WriteHeader(200)
			w.(http.Flusher).Flush()
			select {
			case <-r.Context().Done():
			case <-stop:
			}
		default: // the status endpoint holds its answer
			select {
			case <-r.Context().Done():
			case <-stop:
			}
		}
	}))
	defer srv.Close()
	defer close(stop) // runs first: a held handler would keep Close waiting
	scale := func(d time.Duration) time.Duration { return d / 20 }
	p := &Provider{api: &client{base: srv.URL, key: "k", http: srv.Client(), timeout: requestBound(scale)}, http: srv.Client(), scale: scale}
	watchJob("pod-stall", 1500*time.Millisecond)
	t.Cleanup(func() { jobsMu.Lock(); delete(jobs, "pod-stall"); jobsMu.Unlock() })
	done := make(chan error, 1)
	go func() { _, err := p.Watch(context.Background(), provider.Handle{ID: "pod-stall"}, nil); done <- err }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "did not start the command") {
			t.Fatalf("%v", err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("Watch did not enforce its start deadline while the status request stalled")
	}
}

// A results-server read that stalls is cut off, so tail() cannot block Watch.
func TestOutputStallIsBounded(t *testing.T) {
	stop := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		select { // headers sent, body never arrives
		case <-r.Context().Done():
		case <-stop:
		}
	}))
	defer srv.Close()
	defer close(stop) // runs first: a held handler would keep Close waiting
	scale := func(d time.Duration) time.Duration { return d / 20 }
	p := &Provider{api: &client{base: srv.URL, key: "k", http: srv.Client()}, http: srv.Client(), scale: scale}
	watchJob("pod-out", time.Minute)
	t.Cleanup(func() { jobsMu.Lock(); delete(jobs, "pod-out"); jobsMu.Unlock() })
	done := make(chan error, 1)
	go func() {
		done <- p.Output(context.Background(), provider.Handle{ID: "pod-out", ResultsURL: srv.URL + "/"}, provider.Stdout, 0, io.Discard)
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a stalled read returned no error")
		}
	case <-time.After(8 * time.Second):
		t.Fatal("a stalled results read blocked")
	}
}

// The idle bound covers a stalled server only: a consumer that is slow to take the bytes must not
// cost the request.
func TestOutputSlowWriterIsNotAStall(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, strings.Repeat("x", 100000))
	}))
	defer srv.Close()
	scale := func(d time.Duration) time.Duration { return d / 20 } // the bound floors at 3s
	p := &Provider{api: &client{base: srv.URL, key: "k", http: srv.Client()}, http: srv.Client(), scale: scale}
	watchJob("pod-slow", time.Minute)
	t.Cleanup(func() { jobsMu.Lock(); delete(jobs, "pod-slow"); jobsMu.Unlock() })
	var got strings.Builder
	err := p.Output(context.Background(), provider.Handle{ID: "pod-slow", ResultsURL: srv.URL + "/"}, provider.Stdout, 0, &slowWriter{w: &got, d: 4 * time.Second})
	if err != nil || got.Len() != 100000 {
		t.Fatalf("err=%v got %d bytes", err, got.Len())
	}
}

type slowWriter struct {
	w io.Writer
	d time.Duration
}

func (s *slowWriter) Write(b []byte) (int, error) { time.Sleep(s.d); return s.w.Write(b) }

type recSink struct {
	mu     sync.Mutex
	phases []provider.Phase
	setup  []string
}

func (s *recSink) Phase(p provider.Phase, _ string) {
	s.mu.Lock()
	s.phases = append(s.phases, p)
	s.mu.Unlock()
}
func (s *recSink) Setup(l string)              { s.mu.Lock(); s.setup = append(s.setup, l); s.mu.Unlock() }
func (s *recSink) Out(provider.Stream, []byte) {}
func (s *recSink) begun() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.phases {
		if p == provider.Begun {
			return true
		}
	}
	return false
}

const zeroNonce = "0000000000000000000000000000000000000000000000000000000000000000"

func sse(w http.ResponseWriter, lines ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	for i, l := range lines {
		b, _ := json.Marshal(map[string]string{"ts": fmt.Sprintf("2026-01-01T00:00:%02dZ", i), "line": l})
		fmt.Fprintf(w, "data: %s\n\n", b)
	}
}

// BEGIN must be detected from the results server's state.json when the log stream goes quiet
// after its first batch (live bug 2026-10-23).
func TestWatchBeginsFromStateWhenLogStreamStalls(t *testing.T) {
	var polls atomic.Int64
	seven := 7
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/logs"):
			if r.URL.Query().Get("since") == "" {
				sse(w, agent.MarkBoot+" "+zeroNonce)
				return
			}
			<-r.Context().Done() // quiet
		case strings.HasSuffix(r.URL.Path, "/state.json"):
			if n := polls.Add(1); n < 3 {
				_, _ = io.WriteString(w, signedState("begun", nil, stJob, stNonce, n))
			} else {
				_, _ = io.WriteString(w, signedState("ended", &seven, stJob, stNonce, n))
			}
		case strings.HasSuffix(r.URL.Path, ".log"):
			w.WriteHeader(404)
		default:
			_, _ = io.WriteString(w, `{"status":"RUNNING"}`)
		}
	}))
	defer srv.Close()
	scale := func(d time.Duration) time.Duration { return d / 20 }
	p := &Provider{api: &client{base: srv.URL, key: "k", http: srv.Client(), timeout: requestBound(scale)}, http: srv.Client(), scale: scale}
	watchJobK("pod-state", time.Minute)
	t.Cleanup(func() { jobsMu.Lock(); delete(jobs, "pod-state"); jobsMu.Unlock() })
	sink := &recSink{}
	res, err := p.Watch(context.Background(), provider.Handle{ID: "pod-state", ResultsURL: srv.URL + "/"}, sink)
	if err != nil || res.Exit != 7 || !res.Began || !sink.begun() {
		t.Fatalf("res=%+v err=%v begun=%v", res, err, sink.begun())
	}
}

// The log is read by tail=5000 only: a query never carries since=, and the same tail read again
// delivers each marker and line exactly once.
func TestWatchReadsTailOnlyAndDedupesRepeatedReads(t *testing.T) {
	var queries atomic.Int64
	var mu sync.Mutex
	var sinceSeen bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/logs"):
			if r.URL.Query().Get("since") != "" || r.URL.Query().Get("tail") != "5000" {
				mu.Lock()
				sinceSeen = true
				mu.Unlock()
			}
			// every read replays the whole tail; EXIT shows up on the fourth read
			if queries.Add(1) >= 4 {
				sse(w, "setup one", agent.MarkBoot+" "+zeroNonce, agent.MarkBegin+" "+zeroNonce, agent.MarkExit+" "+zeroNonce+" 0")
				return
			}
			sse(w, "setup one", agent.MarkBoot+" "+zeroNonce)
		default:
			_, _ = io.WriteString(w, `{"status":"RUNNING"}`)
		}
	}))
	defer srv.Close()
	scale := func(d time.Duration) time.Duration { return d / 20 }
	p := &Provider{api: &client{base: srv.URL, key: "k", http: srv.Client(), timeout: requestBound(scale)}, http: srv.Client(), scale: scale}
	watchJob("pod-tailonly", time.Minute)
	t.Cleanup(func() { jobsMu.Lock(); delete(jobs, "pod-tailonly"); jobsMu.Unlock() })
	sink := &recSink{}
	res, err := p.Watch(context.Background(), provider.Handle{ID: "pod-tailonly", ResultsURL: srv.URL + "/"}, sink)
	if err != nil || res.Exit != 0 || !res.Began {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if sinceSeen {
		t.Fatal("a log read used since= or a tail other than 5000")
	}
	if queries.Load() < 4 {
		t.Fatalf("only %d reads: the replay was not exercised", queries.Load())
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.setup) != 1 || sink.setup[0] != "setup one" {
		t.Fatalf("setup line delivered %v, want once", sink.setup)
	}
	begun := 0
	for _, ph := range sink.phases {
		if ph == provider.Begun {
			begun++
		}
	}
	if begun != 1 {
		t.Fatalf("BEGIN delivered %d times", begun)
	}
}

// A 429 from the log endpoint backs reads off (Retry-After when sent, else 2s doubling to 30s) and
// does not stall the job: status, BEGIN from state.json and the end still arrive.
func TestWatchBacksOffOn429AndHonoursRetryAfter(t *testing.T) {
	var logCalls atomic.Int64
	var stamps sync.Map
	seven := 7
	var polls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/logs"):
			n := logCalls.Add(1)
			stamps.Store(n, time.Now())
			if n == 1 {
				w.Header().Set("Retry-After", "20") // scaled by 1/20: about 1s
			}
			w.WriteHeader(429)
		case strings.HasSuffix(r.URL.Path, "/state.json"):
			if n := polls.Add(1); n < 3 {
				_, _ = io.WriteString(w, signedState("begun", nil, stJob, stNonce, n))
			} else {
				_, _ = io.WriteString(w, signedState("ended", &seven, stJob, stNonce, n))
			}
		case strings.HasSuffix(r.URL.Path, ".log"):
			w.WriteHeader(404)
		default:
			_, _ = io.WriteString(w, `{"status":"RUNNING"}`)
		}
	}))
	defer srv.Close()
	scale := func(d time.Duration) time.Duration { return d / 20 }
	p := &Provider{api: &client{base: srv.URL, key: "k", http: srv.Client(), timeout: requestBound(scale)}, http: srv.Client(), scale: scale}
	watchJobK("pod-429b", time.Minute)
	t.Cleanup(func() { jobsMu.Lock(); delete(jobs, "pod-429b"); jobsMu.Unlock() })
	res, err := p.Watch(context.Background(), provider.Handle{ID: "pod-429b", ResultsURL: srv.URL + "/"}, &recSink{})
	if err != nil || res.Exit != 7 || !res.Began {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if n := logCalls.Load(); n > 4 {
		t.Fatalf("%d log reads against a 429: not backed off", n)
	}
	a, _ := stamps.Load(int64(1))
	b, ok := stamps.Load(int64(2))
	if ok && b.(time.Time).Sub(a.(time.Time)) < 900*time.Millisecond {
		t.Fatalf("second read %s after a 429 with Retry-After", b.(time.Time).Sub(a.(time.Time)))
	}
}

// BITC_DEBUG=watch prints one stderr line per slice and never the bearer or key.
func TestWatchDebugTrace(t *testing.T) {
	t.Setenv("BITC_DEBUG", "x,watch")
	var buf syncBuf
	old := debugOut
	debugOut = &buf
	t.Cleanup(func() { debugOut = old })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/logs") {
			sse(w, agent.MarkBoot+" "+zeroNonce, agent.MarkBegin+" "+zeroNonce, agent.MarkExit+" "+zeroNonce+" 0")
			return
		}
		_, _ = io.WriteString(w, `{"status":"RUNNING"}`)
	}))
	defer srv.Close()
	scale := func(d time.Duration) time.Duration { return d / 20 }
	p := &Provider{api: &client{base: srv.URL, key: "sk-secret-key", http: srv.Client(), timeout: requestBound(scale)}, http: srv.Client(), scale: scale}
	watchJob("pod-dbg", time.Minute)
	t.Cleanup(func() { jobsMu.Lock(); delete(jobs, "pod-dbg"); jobsMu.Unlock() })
	if _, err := p.Watch(context.Background(), provider.Handle{ID: "pod-dbg", ResultsURL: srv.URL + "/"}, &recSink{}); err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	if !strings.Contains(got, "watch: slice mode=tail status=200 lines=3 markers=BOOT,BEGIN,EXIT elapsed=") || strings.Contains(got, "sk-secret-key") {
		t.Fatalf("trace %q", got)
	}
}

type syncBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

var stK = []byte("0123456789abcdef0123456789abcdef")

const stJob = "job-1"

var stNonce = zeroNonce

func signedState(phase string, exit *int, job, nonce string, seq int64) string {
	b, _ := json.Marshal(agent.SignState(stK, agent.State{Phase: phase, Exit: exit, Job: job, Nonce: nonce, Seq: seq, TS: 1}))
	return string(b)
}

// watchJobK is watchJob with the job id, K and nonce the signed states are checked against.
func watchJobK(id string, startIn time.Duration) {
	watchJob(id, startIn)
	jobsMu.Lock()
	j := jobs[id]
	j.Job = stJob
	copy(j.MacKey[:], stK)
	jobsMu.Unlock()
}

// stateWatch runs Watch against a quiet log stream and the given state.json bodies (one per poll,
// the last repeating).
func stateWatch(t *testing.T, id string, startIn time.Duration, bodies func(n int64) string) (provider.Result, *recSink, error) {
	t.Helper()
	var polls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/logs"):
			if r.URL.Query().Get("since") == "" {
				sse(w, agent.MarkBoot+" "+zeroNonce)
				return
			}
			<-r.Context().Done()
		case strings.HasSuffix(r.URL.Path, "/state.json"):
			_, _ = io.WriteString(w, bodies(polls.Add(1)))
		case strings.HasSuffix(r.URL.Path, ".log"):
			w.WriteHeader(404)
		default:
			_, _ = io.WriteString(w, `{"status":"RUNNING"}`)
		}
	}))
	defer srv.Close()
	scale := func(d time.Duration) time.Duration { return d / 20 }
	p := &Provider{api: &client{base: srv.URL, key: "k", http: srv.Client(), timeout: requestBound(scale)}, http: srv.Client(), scale: scale}
	watchJobK(id, startIn)
	t.Cleanup(func() { jobsMu.Lock(); delete(jobs, id); jobsMu.Unlock() })
	sink := &recSink{}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	res, err := p.Watch(ctx, provider.Handle{ID: id, ResultsURL: srv.URL + "/"}, sink)
	return res, sink, err
}

// A state.json that is forged, for another job or attempt, unsigned or a replay is never BEGIN
// or END: Watch keeps waiting until the start timeout.
func TestWatchIgnoresUnauthenticatedState(t *testing.T) {
	nine := 9
	forged := agent.SignState([]byte("another key another key another k"), agent.State{Phase: "ended", Exit: &nine, Job: stJob, Nonce: stNonce, Seq: 1, TS: 1})
	fb, _ := json.Marshal(forged)
	badMAC := signedState("ended", &nine, stJob, stNonce, 1)
	badMAC = strings.Replace(badMAC, `"exit":9`, `"exit":0`, 1)
	for name, body := range map[string]string{
		"bad mac":        badMAC,
		"wrong key":      string(fb),
		"other job":      signedState("ended", &nine, "job-2", stNonce, 1),
		"earlier nonce":  signedState("begun", nil, stJob, strings.Repeat("1", 64), 1),
		"unsigned state": `{"phase":"ended","exit":9}`,
		"legacy begun":   `{"phase":"begun"}`,
	} {
		t.Run(name, func(t *testing.T) {
			res, sink, err := stateWatch(t, "pod-ign", 2*time.Second, func(int64) string { return body })
			if err == nil || !strings.Contains(err.Error(), "did not start") || res.Began || sink.begun() {
				t.Fatalf("res=%+v err=%v begun=%v", res, err, sink.begun())
			}
		})
	}
}

// A replayed older seq is ignored once a newer one was accepted; the newer END decides.
func TestWatchIgnoresReplayedStateSeq(t *testing.T) {
	nine, four := 9, 4
	res, sink, err := stateWatch(t, "pod-replay", time.Minute, func(n int64) string {
		switch n {
		case 1:
			return signedState("begun", nil, stJob, stNonce, 5)
		case 2:
			return signedState("ended", &nine, stJob, stNonce, 2) // replayed from earlier
		default:
			return signedState("ended", &four, stJob, stNonce, 6)
		}
	})
	if err != nil || res.Exit != 4 || !res.Began || !sink.begun() {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

// A valid signed begun starts the job (Sink.Phase Begun) before the signed ended returns.
func TestWatchBeginsFromSignedState(t *testing.T) {
	two := 2
	res, sink, err := stateWatch(t, "pod-signed", 2*time.Second, func(n int64) string {
		if n < 2 {
			return signedState("begun", nil, stJob, stNonce, n)
		}
		return signedState("ended", &two, stJob, stNonce, n)
	})
	if err != nil || res.Exit != 2 || !res.Began || !sink.begun() {
		t.Fatalf("res=%+v err=%v begun=%v", res, err, sink.begun())
	}
}

// The trace of ignored states carries no bearer, key, MAC, nonce or env.
func TestWatchDebugTraceHasNoSecretsFromState(t *testing.T) {
	t.Setenv("BITC_DEBUG", "watch")
	t.Setenv("BITC_TEST_ENV_SECRET", "env-secret-value")
	var buf syncBuf
	old := debugOut
	debugOut = &buf
	t.Cleanup(func() { debugOut = old })
	nine := 9
	good := signedState("ended", &nine, stJob, stNonce, 1)
	var st agent.State
	_ = json.Unmarshal([]byte(good), &st)
	forged := strings.Replace(good, `"exit":9`, `"exit":0`, 1)
	_, _, _ = stateWatch(t, "pod-dbg2", time.Second, func(int64) string { return forged })
	got := buf.String()
	if !strings.Contains(got, "watch: state ignored") {
		t.Fatalf("no ignored-state line in %q", got)
	}
	bearer := fmt.Sprintf("%x", [32]byte{}) // the test job's B (zero)
	for _, bad := range []string{st.MAC, string(stK), fmt.Sprintf("%x", stK), stNonce, bearer, "env-secret-value", "sk-secret-key"} {
		if bad != "" && strings.Contains(got, bad) {
			t.Fatalf("trace leaks %q: %q", bad, got)
		}
	}
}

// BEGIN seen, then nothing: the host gives up at BEGIN + timeout + grace, not at D (spec 8.4 step 3).
func TestWatchGivesUpAtBeginPlusTimeoutPlusGrace(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/logs"):
			if r.URL.Query().Get("since") == "" {
				sse(w, agent.MarkBoot+" "+zeroNonce, agent.MarkBegin+" "+zeroNonce)
				return
			}
			<-r.Context().Done()
		case strings.HasSuffix(r.URL.Path, ".log"), strings.HasSuffix(r.URL.Path, "/state.json"):
			w.WriteHeader(404)
		default:
			_, _ = io.WriteString(w, `{"status":"RUNNING"}`)
		}
	}))
	defer srv.Close()
	scale := func(d time.Duration) time.Duration { return d / 20 }
	p := &Provider{api: &client{base: srv.URL, key: "k", http: srv.Client(), timeout: requestBound(scale)}, http: srv.Client(), scale: scale}
	now := time.Now()
	jobsMu.Lock() // timeout 1s, grace 2m (6s scaled), D 10m out (30s scaled)
	jobs["pod-bt"] = &provider.JobSpec{Timeout: time.Second, Created: now, Deadline: now.Add(10 * time.Minute)}
	jobsMu.Unlock()
	t.Cleanup(func() { jobsMu.Lock(); delete(jobs, "pod-bt"); jobsMu.Unlock() })
	start := time.Now()
	res, err := p.Watch(context.Background(), provider.Handle{ID: "pod-bt", ResultsURL: srv.URL + "/"}, &recSink{})
	took := time.Since(start)
	if err == nil || !strings.Contains(err.Error(), "timed out") || !res.Began {
		t.Fatalf("%+v %v", res, err)
	}
	if took < 6*time.Second || took > 12*time.Second {
		t.Fatalf("gave up after %s, want about BEGIN+1s+6s grace (D is 30s out)", took)
	}
}

// A changed startedAt is a restart even with no second BOOT, before or after BEGIN.
func TestWatchSeesRestartFromStartedAt(t *testing.T) {
	for _, begin := range []bool{false, true} {
		var polls atomic.Int64
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.HasSuffix(r.URL.Path, "/logs"):
				if r.URL.Query().Get("since") == "" {
					if begin {
						sse(w, agent.MarkBoot+" "+zeroNonce, agent.MarkBegin+" "+zeroNonce)
					} else {
						sse(w, agent.MarkBoot+" "+zeroNonce)
					}
					return
				}
				<-r.Context().Done()
			case strings.HasSuffix(r.URL.Path, ".log"), strings.HasSuffix(r.URL.Path, "/state.json"):
				w.WriteHeader(404)
			default:
				n := polls.Add(1)
				fmt.Fprintf(w, `{"status":"RUNNING","startedAt":"2026-10-23T10:00:0%dZ"}`, min(n, 2))
			}
		}))
		scale := func(d time.Duration) time.Duration { return d / 20 }
		p := &Provider{api: &client{base: srv.URL, key: "k", http: srv.Client(), timeout: requestBound(scale)}, http: srv.Client(), scale: scale}
		id := fmt.Sprintf("pod-sa-%v", begin)
		watchJob(id, time.Minute)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		res, err := p.Watch(ctx, provider.Handle{ID: id, ResultsURL: srv.URL + "/"}, &recSink{})
		cancel()
		srv.Close()
		jobsMu.Lock()
		delete(jobs, id)
		jobsMu.Unlock()
		want := "restarted before the command started"
		if begin {
			want = "restarted while the command ran"
		}
		if err == nil || !strings.Contains(err.Error(), want) || res.Began != begin {
			t.Errorf("begin=%v: %+v %v", begin, res, err)
		}
	}
}

// An empty startedAt and a failed status read are not a baseline: only two different non-empty
// values are a restart. Sequence: empty, HTTP 500, X, empty, X, then the job ends normally.
func TestWatchIgnoresEmptyAndFailedStartedAtReads(t *testing.T) {
	var polls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/logs"):
			if polls.Load() >= 6 {
				sse(w, agent.MarkBoot+" "+zeroNonce, agent.MarkBegin+" "+zeroNonce, agent.MarkExit+" "+zeroNonce+" 0")
				return
			}
			sse(w, agent.MarkBoot+" "+zeroNonce, agent.MarkBegin+" "+zeroNonce)
		case strings.HasSuffix(r.URL.Path, ".log"), strings.HasSuffix(r.URL.Path, "/state.json"):
			w.WriteHeader(404)
		default:
			switch n := polls.Add(1); n {
			case 1, 4:
				_, _ = io.WriteString(w, `{"status":"RUNNING","startedAt":""}`)
			case 2:
				w.WriteHeader(500)
			default:
				_, _ = io.WriteString(w, `{"status":"RUNNING","startedAt":"2026-10-23T10:00:00Z"}`)
			}
		}
	}))
	defer srv.Close()
	scale := func(d time.Duration) time.Duration { return d / 20 }
	p := &Provider{api: &client{base: srv.URL, key: "k", http: srv.Client(), timeout: requestBound(scale)}, http: srv.Client(), scale: scale}
	watchJob("pod-neg", time.Minute)
	t.Cleanup(func() { jobsMu.Lock(); delete(jobs, "pod-neg"); jobsMu.Unlock() })
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	res, err := p.Watch(ctx, provider.Handle{ID: "pod-neg", ResultsURL: srv.URL + "/"}, &recSink{})
	if err != nil || res.Exit != 0 || !res.Began {
		t.Fatalf("empty/failed startedAt reads must not look like a restart: %+v %v", res, err)
	}
	if polls.Load() < 5 {
		t.Fatalf("only %d status polls: the sequence did not run", polls.Load())
	}
}
