package plugin

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/Exploded-views/bitc/internal/exit"
)

const (
	maxLine     = 1 << 20   // one JSON object per line, at most 1 MiB (A.4.1)
	maxChunk    = 512 << 10 // decoded bytes per Output or Fetch frame
	ringBytes   = 64 << 10  // stderr kept for protocol errors (A.3.1)
	tailLines   = 20
	cancelGrace = 5 * time.Second
	exitGrace   = 2 * time.Second
)

// errStopped is what do returns when the event handler ended the call (a marker settled it); the
// plugin has been sent a cancel.
var errStopped = errors.New("call stopped by its handler")

// timeoutErr: a call outlived its bound (the plugin got a cancel).
type timeoutErr struct {
	method string
	after  time.Duration
}

func (e *timeoutErr) Error() string { return fmt.Sprintf("%s timed out after %s", e.method, e.after) }

// diedErr: the plugin process ended or closed stdout while a call was open.
type diedErr struct{ why string }

func (e *diedErr) Error() string { return "plugin " + e.why }

type outcome struct {
	result json.RawMessage
	eobj   *errObj
	err    error
}

type call struct {
	id     uint64
	method string
	fin    chan outcome // buffered 1
	evMu   sync.Mutex
	ev     [][]byte
	evSig  chan struct{} // buffered 1
}

func (c *call) push(b []byte) {
	c.evMu.Lock()
	c.ev = append(c.ev, b)
	c.evMu.Unlock()
	select {
	case c.evSig <- struct{}{}:
	default:
	}
}

func (c *call) drain() [][]byte {
	c.evMu.Lock()
	defer c.evMu.Unlock()
	out := c.ev
	c.ev = nil
	return out
}

// ring keeps the last bytes of the plugin's stderr and feeds complete lines to onLine.
type ring struct {
	mu      sync.Mutex
	lines   []string
	partial []byte
	size    int
	onLine  func(string)
}

func (r *ring) Write(p []byte) (int, error) {
	r.mu.Lock()
	r.partial = append(r.partial, p...)
	var got []string
	for {
		i := bytes.IndexByte(r.partial, '\n')
		if i < 0 {
			break
		}
		got = append(got, string(bytes.TrimRight(r.partial[:i], "\r")))
		r.partial = r.partial[i+1:]
	}
	if len(r.partial) > ringBytes { // a line without a newline: keep the end
		r.partial = r.partial[len(r.partial)-ringBytes:]
	}
	for _, l := range got {
		r.lines = append(r.lines, l)
		r.size += len(l) + 1
	}
	for r.size > ringBytes && len(r.lines) > 1 {
		r.size -= len(r.lines[0]) + 1
		r.lines = r.lines[1:]
	}
	cb := r.onLine
	r.mu.Unlock()
	if cb != nil {
		for _, l := range got {
			cb(l)
		}
	}
	return len(p), nil
}

// tail is the last n complete lines.
func (r *ring) tail(n int) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	l := r.lines
	if len(l) > n {
		l = l[len(l)-n:]
	}
	return append([]string(nil), l...)
}

// session is one plugin process and the multiplexed request stream to it.
type session struct {
	typ      string
	cmd      *exec.Cmd
	stdin    *os.File
	wmu      sync.Mutex
	stderr   *ring
	exited   chan struct{} // closed once the process is reaped and its TMPDIR removed
	waitErr  error
	scale    func(time.Duration) time.Duration
	mu       sync.Mutex
	nextID   uint64
	calls    map[uint64]*call
	abandon  map[uint64]bool
	failed   error
	dead     chan struct{}
	deliber  bool // killed on purpose (a stuck plugin, an ambiguous launch): not a death
	closing  bool
	killOnce sync.Once
}

func (s *session) isDead() bool {
	select {
	case <-s.dead:
		return true
	default:
		return false
	}
}

// fail ends the session with err (first call wins): every open call gets err and the process
// group is killed.
func (s *session) fail(err error, deliberate bool) {
	s.mu.Lock()
	if s.failed != nil {
		s.mu.Unlock()
		return
	}
	s.failed, s.deliber = err, deliberate
	pending := s.calls
	s.calls = map[uint64]*call{}
	close(s.dead)
	s.mu.Unlock()
	for _, c := range pending {
		c.fin <- outcome{err: err}
	}
	s.kill()
}

func (s *session) kill() {
	s.killOnce.Do(func() {
		select {
		case <-s.exited: // reaped: the pid may belong to someone else by now
			return
		default:
		}
		if s.cmd != nil && s.cmd.Process != nil {
			_ = syscall.Kill(-s.cmd.Process.Pid, syscall.SIGKILL)
		}
	})
}

func (s *session) writeFrame(b []byte) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_, err := s.stdin.Write(append(b, '\n'))
	return err
}

// readLine reads one line of at most maxLine bytes.
func readLine(r *bufio.Reader) ([]byte, error) {
	var buf []byte
	for {
		chunk, err := r.ReadSlice('\n')
		buf = append(buf, chunk...)
		if len(buf) > maxLine+1 {
			return nil, protoErrf("a line is longer than 1 MiB")
		}
		switch {
		case err == nil:
			return bytes.TrimRight(buf, "\r\n"), nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		default:
			return nil, err
		}
	}
}

// readLoop routes frames to their calls until the stream ends.
func (s *session) readLoop(r io.Reader) {
	br := bufio.NewReaderSize(r, 64<<10)
	for {
		line, err := readLine(br)
		if err != nil {
			var pe *protoErr
			if errors.As(err, &pe) {
				s.fail(pe, false)
				return
			}
			<-s.waitedOrTimeout()
			s.mu.Lock()
			closing, werr := s.closing, s.waitErr
			s.mu.Unlock()
			why := "exited"
			if werr != nil {
				why = "exited: " + werr.Error()
			}
			if closing {
				why = "closed"
			}
			s.fail(&diedErr{why}, closing)
			return
		}
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var f struct {
			ID     *uint64         `json:"id"`
			Event  string          `json:"event"`
			Result json.RawMessage `json:"result"`
			Error  json.RawMessage `json:"error"`
		}
		if err := json.Unmarshal(line, &f); err != nil {
			s.fail(protoErrf("malformed frame: %v", err), false)
			return
		}
		if f.ID == nil {
			s.fail(protoErrf("frame without an id"), false)
			return
		}
		if string(f.Error) == "null" { // "error": null beside a result is no error
			f.Error = nil
		}
		if string(f.Result) == "null" && len(f.Error) == 0 && f.Event == "" { // a void method may answer null
			f.Result = json.RawMessage("{}")
		}
		if perr := s.route(*f.ID, f.Event, f.Result, f.Error, line); perr != nil {
			s.fail(perr, false)
			return
		}
	}
}

func (s *session) waitedOrTimeout() <-chan struct{} {
	ch := make(chan struct{})
	exit.Go(func() {
		select {
		case <-s.exited:
		case <-time.After(500 * time.Millisecond):
		}
		close(ch)
	})
	return ch
}

// route delivers one frame; a non-nil result is a protocol error.
func (s *session) route(id uint64, event string, result, errRaw json.RawMessage, line []byte) *protoErr {
	s.mu.Lock()
	c := s.calls[id]
	gone := s.abandon[id]
	hasFinal := len(result) > 0 || len(errRaw) > 0
	if c == nil && !gone {
		s.mu.Unlock()
		return protoErrf("frame for unknown or finished id %d", id)
	}
	if hasFinal && gone {
		delete(s.abandon, id) // the answer to a cancelled call: dropped
	}
	if hasFinal && c != nil {
		delete(s.calls, id)
	}
	s.mu.Unlock()
	switch {
	case c == nil:
		return nil
	case event != "" && hasFinal, len(result) > 0 && len(errRaw) > 0:
		return protoErrf("frame %d is both an event and a final, or has a result and an error", id)
	case event != "":
		c.push(append([]byte(nil), line...))
	case len(result) > 0:
		c.fin <- outcome{result: result}
	case len(errRaw) > 0:
		eo, err := parseErrObj(errRaw)
		if err != nil {
			c.fin <- outcome{err: err}
			return nil
		}
		c.fin <- outcome{eobj: eo}
	default:
		return protoErrf("frame %d has no event, result or error", id)
	}
	return nil
}

type callOpts struct {
	timeout time.Duration
	idle    bool // the bound is a silence, reset by every event
	// onEvent sees each event frame in order. stop ends the call (the plugin is sent a cancel); a
	// *protoErr fails the whole session.
	onEvent func(raw []byte) (stop bool, err error)
}

// do sends one request and waits for its final frame.
func (s *session) do(ctx context.Context, method string, params any, o callOpts) (json.RawMessage, *errObj, error) {
	body, err := json.Marshal(params)
	if err != nil {
		return nil, nil, err
	}
	c := &call{method: method, fin: make(chan outcome, 1), evSig: make(chan struct{}, 1)}
	s.mu.Lock()
	if s.failed != nil {
		err := s.failed
		s.mu.Unlock()
		return nil, nil, err
	}
	s.nextID++
	c.id = s.nextID
	s.calls[c.id] = c
	s.mu.Unlock()
	frame, _ := json.Marshal(map[string]any{"id": c.id, "method": method, "params": json.RawMessage(body)})
	if len(frame) > maxLine {
		s.mu.Lock()
		delete(s.calls, c.id)
		s.mu.Unlock()
		return nil, nil, errTooLarge
	}
	if err := s.writeFrame(frame); err != nil {
		s.fail(&diedErr{"closed its input: " + err.Error()}, false)
		return nil, nil, s.failedErr()
	}
	started := time.Now()
	var timer *time.Timer
	var tc <-chan time.Time
	if o.timeout > 0 {
		timer = time.NewTimer(o.timeout)
		defer timer.Stop()
		tc = timer.C
	}
	handle := func() (bool, error) {
		for _, raw := range c.drain() {
			if o.idle && timer != nil {
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(o.timeout)
			}
			if o.onEvent == nil {
				continue
			}
			stop, err := o.onEvent(raw)
			if err != nil || stop {
				return stop, err
			}
		}
		return false, nil
	}
	for {
		select {
		case out := <-c.fin:
			if stop, err := handle(); err != nil || stop {
				return s.stopped(c, stop, err)
			}
			return out.result, out.eobj, out.err
		case <-c.evSig:
			if stop, err := handle(); err != nil || stop {
				return s.stopped(c, stop, err)
			}
		case <-tc:
			s.cancel(c)
			return nil, nil, &timeoutErr{method, time.Since(started).Round(time.Millisecond)}
		case <-ctx.Done():
			s.cancel(c)
			return nil, nil, ctx.Err()
		}
	}
}

var errTooLarge = errors.New("request larger than 1 MiB")

func (s *session) failedErr() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.failed
}

// stopped ends a call its handler settled: a protocol error fails the session, anything else
// cancels the call.
func (s *session) stopped(c *call, stop bool, err error) (json.RawMessage, *errObj, error) {
	var pe *protoErr
	if errors.As(err, &pe) {
		s.fail(pe, false)
		return nil, nil, pe
	}
	s.cancel(c)
	if err != nil {
		return nil, nil, err
	}
	return nil, nil, errStopped
}

// cancel abandons a call and tells the plugin; a plugin that has not answered cancelGrace later
// is killed (the next call respawns it).
func (s *session) cancel(c *call) {
	s.mu.Lock()
	if _, open := s.calls[c.id]; !open {
		s.mu.Unlock()
		return
	}
	delete(s.calls, c.id)
	s.abandon[c.id] = true
	s.mu.Unlock()
	b, _ := json.Marshal(map[string]any{"id": 0, "method": "cancel", "params": map[string]any{"id": c.id}})
	_ = s.writeFrame(b)
	exit.Go(func() {
		select {
		case <-time.After(s.scale(cancelGrace)):
		case <-s.dead:
			return
		}
		s.mu.Lock()
		stuck := s.abandon[c.id]
		s.mu.Unlock()
		if stuck {
			s.fail(&diedErr{"did not answer the cancel of " + c.method}, true)
		}
	})
}

// closeGracefully closes stdin, then waits for the process, then SIGTERM, then SIGKILL.
func (s *session) closeGracefully() {
	s.mu.Lock()
	s.closing = true
	s.mu.Unlock()
	_ = s.stdin.Close()
	if s.waitFor(s.scale(exitGrace)) {
		return
	}
	if s.cmd.Process != nil {
		_ = syscall.Kill(-s.cmd.Process.Pid, syscall.SIGTERM)
	}
	if s.waitFor(s.scale(exitGrace)) {
		return
	}
	s.kill()
	s.waitFor(5 * time.Second)
}

func (s *session) waitFor(d time.Duration) bool {
	select {
	case <-s.exited:
		return true
	case <-time.After(d):
		return false
	}
}
