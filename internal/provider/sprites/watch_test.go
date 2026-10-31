package sprites

import (
	"bytes"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Exploded-views/bitc/internal/agent"
	"github.com/Exploded-views/bitc/internal/provider"
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
