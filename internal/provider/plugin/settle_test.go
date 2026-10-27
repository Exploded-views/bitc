package plugin

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Exploded-views/bitc/internal/provider"
	"github.com/Exploded-views/bitc/internal/redact"
)

// These cover, without a plugin process or a timer, the settle paths that the rig tests reach
// only when a race goes one way: the stderr tail in a debug error, the answer to a cancelled
// call, and a handler's plain error. Under load those were hit in some runs and not others, so
// the package's coverage swung across its floor.

// testSession is a session with no process: stdin is a pipe the test reads frames from.
func testSession(t *testing.T) (*session, *bufio.Reader) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	s := &session{typ: "toy", scale: func(d time.Duration) time.Duration { return d }, calls: map[uint64]*call{}, abandon: map[uint64]bool{}, dead: make(chan struct{}),
		exited: make(chan struct{}), stdin: w, stderr: &ring{}}
	t.Cleanup(func() {
		select {
		case <-s.dead:
		default:
			close(s.dead) // releases cancel's grace watcher
		}
		w.Close()
		r.Close()
	})
	return s, bufio.NewReader(r)
}

func TestTailRendersTheLastStderrLinesRedacted(t *testing.T) {
	s, _ := testSession(t)
	red := redact.New()
	red.Add("hunter2")
	_, _ = s.stderr.Write([]byte("first\nthe token is hunter2\n"))
	p := &Provider{rt: provider.Runtime{Debug: true, Red: red}}
	got := p.tail(s)
	if want := "\n  plugin stderr: first\n  plugin stderr: the token is <redacted>"; got != want {
		t.Fatalf("tail = %q, want %q", got, want)
	}
	if (&Provider{rt: provider.Runtime{Red: red}}).tail(s) != "" {
		t.Fatal("tail without debug is not empty")
	}
}

func TestRouteDropsTheAnswerToACancelledCall(t *testing.T) {
	s, _ := testSession(t)
	s.abandon[7] = true
	if pe := s.route(7, "", json.RawMessage(`{"ok":true}`), nil, []byte(`{"id":7,"result":{"ok":true}}`)); pe != nil {
		t.Fatalf("the answer to a cancelled call is a protocol error: %v", pe)
	}
	if s.abandon[7] {
		t.Fatal("the cancelled id is still waiting for its answer")
	}
	if pe := s.route(7, "", json.RawMessage(`{"ok":true}`), nil, nil); pe == nil {
		t.Fatal("a second answer to the same id is not a protocol error")
	}
}

func TestStoppedCancelsTheCallAndReturnsAPlainError(t *testing.T) {
	s, in := testSession(t)
	c := &call{id: 3, method: "status", fin: make(chan outcome, 1), evSig: make(chan struct{}, 1)}
	s.calls[c.id] = c
	boom := errors.New("handler failed")
	raw, eo, err := s.stopped(c, false, boom)
	if raw != nil || eo != nil || !errors.Is(err, boom) {
		t.Fatalf("stopped = %s %v %v, want the handler's error", raw, eo, err)
	}
	if _, open := s.calls[c.id]; open || !s.abandon[c.id] {
		t.Fatal("the call was not cancelled")
	}
	line, err := in.ReadString('\n')
	if err != nil || !strings.Contains(line, `"method":"cancel"`) || !strings.Contains(line, `"id":3`) {
		t.Fatalf("cancel frame %q (%v)", line, err)
	}
	if s.isDead() {
		t.Fatal("a plain handler error killed the session")
	}
}
