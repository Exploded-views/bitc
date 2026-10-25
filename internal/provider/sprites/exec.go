package sprites

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Exploded-views/bitc/internal/agent"
	"github.com/Exploded-views/bitc/internal/exit"
	"github.com/Exploded-views/bitc/internal/provider"
	"github.com/Exploded-views/bitc/internal/wsmini"
)

// The exec channel is WSS /v1/sprites/{name}/exec. Assumed protocol (OpenAPI 0.1.12 and
// sprites-go v0.2.2 as the research sheet records them; probe 9.12/9.15 settle the rest):
//
//	query: cmd=<arg> repeated, path=<argv0>, dir, stdin=true, tty=false,
//	       max_run_after_disconnect=<seconds>s
//	client -> server, binary: 0x00 + stdin bytes; 0x04 = stdin EOF
//	server -> client, binary: 0x01 + stdout; 0x02 + stderr; 0x03 + one byte, the exit status
//
// A message with another first byte is a protocol error. No secret ever goes in the URL.
const (
	chStdin  = 0x00
	chStdout = 0x01
	chStderr = 0x02
	chExit   = 0x03
	chEOF    = 0x04

	stdinChunk = 32 << 10
	stderrKeep = 64 << 10
)

// ErrStreamEnded: the exec connection ended without an exit status (a dropped socket, a Sprite
// gone cold, a close frame before the exit message).
var ErrStreamEnded = &provider.ErrTransient{Why: "exec stream ended without an exit status"}

// ErrNotProvisioned: a bitc bug, not an API answer. Exec refuses a Sprite this client created
// before its policies were set and verified (SP8), and Attach one whose job policy was not.
var ErrNotProvisioned = errors.New("sprites: policies not verified before exec")

// ExecRequest is one exec. Cmd is the argv (no secret: it travels in the URL). Stdin opens the
// stdin channel. MaxRunAfterDisconnect, when set, lets the process outlive the connection for
// that long (assumed absent = it dies with the connection).
type ExecRequest struct {
	Cmd                   []string
	Dir                   string
	Stdin                 bool
	MaxRunAfterDisconnect time.Duration
}

const maxCmdBytes = 8 << 10

func (r ExecRequest) query() (url.Values, error) {
	if len(r.Cmd) == 0 {
		return nil, errors.New("exec: empty command")
	}
	n := 0
	q := url.Values{}
	for _, a := range r.Cmd {
		n += len(a)
		q.Add("cmd", a)
	}
	if n > maxCmdBytes {
		return nil, errors.New("exec: command over 8 KiB")
	}
	q.Set("path", r.Cmd[0])
	q.Set("tty", "false")
	q.Set("stdin", strconv.FormatBool(r.Stdin))
	if r.Dir != "" {
		q.Set("dir", r.Dir)
	}
	if r.MaxRunAfterDisconnect > 0 {
		q.Set("max_run_after_disconnect", strconv.FormatInt(int64((r.MaxRunAfterDisconnect+time.Second-1)/time.Second), 10)+"s")
	}
	return q, nil
}

// Session is one live exec. Read Stdout until EOF (or Close); Wait returns the exit status.
type Session struct {
	conn    *wsmini.Conn
	outR    *io.PipeReader
	outW    *io.PipeWriter
	done    chan struct{}
	mu      sync.Mutex
	exit    int
	err     error
	stderr  []byte
	closed  bool
	eofSent bool
}

// Stdout is the process's stdout. Backpressure applies: the session stops reading the socket
// while Stdout is not read.
func (s *Session) Stdout() io.Reader { return s.outR }

// Stderr is the last 64 KiB of the process's stderr seen so far.
func (s *Session) Stderr() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.stderr...)
}

// Write sends p on stdin in chunks.
func (s *Session) Write(p []byte) (int, error) {
	n := 0
	for len(p) > 0 {
		k := min(len(p), stdinChunk)
		msg := make([]byte, 1+k)
		msg[0] = chStdin
		copy(msg[1:], p[:k])
		if err := s.conn.WriteMessage(wsmini.OpBinary, msg); err != nil {
			return n, err
		}
		n += k
		p = p[k:]
	}
	return n, nil
}

// CloseStdin sends the stdin EOF (once).
func (s *Session) CloseStdin() error {
	s.mu.Lock()
	if s.eofSent {
		s.mu.Unlock()
		return nil
	}
	s.eofSent = true
	s.mu.Unlock()
	return s.conn.WriteMessage(wsmini.OpBinary, []byte{chEOF})
}

// Wait blocks until the session ended and returns the exit status; ErrStreamEnded when the
// connection ended without one. Stdout must be drained (or the session closed) for it to return.
func (s *Session) Wait() (int, error) {
	<-s.done
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.exit, s.err
}

// Close drops the connection (the process may live on, per MaxRunAfterDisconnect) and ends
// Stdout.
func (s *Session) Close() error {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	err := s.conn.Close()
	s.outR.Close()
	<-s.done
	return err
}

func (s *Session) pump() {
	defer close(s.done)
	finish := func(err error) {
		s.mu.Lock()
		if s.err == nil && s.exit < 0 {
			s.err = err
		}
		s.mu.Unlock()
	}
	for {
		op, p, err := s.conn.ReadMessage()
		if err != nil {
			s.mu.Lock()
			closed := s.closed
			s.mu.Unlock()
			if closed || s.exit >= 0 {
				_ = s.outW.Close()
				return
			}
			_ = s.outW.CloseWithError(ErrStreamEnded)
			finish(ErrStreamEnded)
			return
		}
		if op != wsmini.OpBinary || len(p) == 0 {
			continue
		}
		switch p[0] {
		case chStdout:
			if _, err := s.outW.Write(p[1:]); err != nil {
				s.mu.Lock()
				s.closed = true
				s.mu.Unlock()
				_ = s.conn.Close()
				return
			}
		case chStderr:
			s.mu.Lock()
			s.stderr = append(s.stderr, p[1:]...)
			if len(s.stderr) > stderrKeep {
				s.stderr = s.stderr[len(s.stderr)-stderrKeep:]
			}
			s.mu.Unlock()
		case chExit:
			code, ok := exitCode(p[1:])
			s.mu.Lock()
			if ok {
				s.exit = code
			} else {
				s.err = &provider.ErrTransient{Why: "exec: unusable exit status"}
				s.exit = 0
			}
			s.mu.Unlock()
			_ = s.outW.Close()
			_ = s.conn.Close()
			return
		default:
			s.mu.Lock()
			s.err = &provider.ErrTransient{Why: fmt.Sprintf("exec: unknown stream %#x", p[0])}
			s.exit = 0
			s.mu.Unlock()
			_ = s.outW.CloseWithError(s.err)
			_ = s.conn.Close()
			return
		}
	}
}

// exitCode reads the exit message: one byte (assumed), or a decimal number.
func exitCode(b []byte) (int, bool) {
	if len(b) == 1 {
		return int(b[0]), true
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(b)))
	return n, err == nil && n >= 0 && n < 256
}

func (c *Client) execURL(name string, r ExecRequest) (string, error) {
	q, err := r.query()
	if err != nil {
		return "", err
	}
	u := *c.base
	u.Path = "/v1/sprites/" + name + "/exec"
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func (c *Client) state(name string) (int, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	st, ok := c.fresh[name]
	return st, ok
}

// Exec opens an exec session. Errors are the raw classes of 1.2: an upgrade refused with an HTTP
// status maps like any API answer (*wsmini.StatusError never escapes); a failure after the
// request was written is ErrTransient. Use Attach for the attach exec.
func (c *Client) Exec(ctx context.Context, name string, r ExecRequest) (*Session, error) {
	s, err := c.exec(ctx, name, r, polBuild)
	var d *dialFailure
	if errors.As(err, &d) {
		return nil, d.err
	}
	var h *sentFailure
	if errors.As(err, &h) {
		return nil, h.err
	}
	return s, err
}

type dialFailure struct{ err error } // nothing was sent
type sentFailure struct{ err error } // the request was written; the server may have acted

func (e *dialFailure) Error() string { return e.err.Error() }
func (e *dialFailure) Unwrap() error { return e.err }
func (e *sentFailure) Error() string { return e.err.Error() }
func (e *sentFailure) Unwrap() error { return e.err }

func (c *Client) exec(ctx context.Context, name string, r ExecRequest, need int) (*Session, error) {
	if err := checkName(name); err != nil {
		return nil, err
	}
	if st, fresh := c.state(name); fresh && st < need {
		return nil, ErrNotProvisioned
	}
	raw, err := c.execURL(name, r)
	if err != nil {
		return nil, &provider.ErrIneligible{Why: err.Error()}
	}
	h := http.Header{"Authorization": {"Bearer " + string(c.token)}}
	to := c.handshake
	conn, err := wsmini.Dial(ctx, raw, wsmini.DialOptions{Header: h, HandshakeTimeout: to})
	if err != nil {
		var se *wsmini.StatusError
		var de *wsmini.DialError
		switch {
		case errors.As(err, &se):
			return nil, mapHTTP(se.Code, se.Body, "exec upgrade")
		case errors.As(err, &de):
			return nil, &dialFailure{&provider.ErrTransient{Why: "exec: " + de.Error()}}
		}
		return nil, &sentFailure{&provider.ErrTransient{Why: "exec upgrade: no usable answer"}}
	}
	s := &Session{conn: conn, done: make(chan struct{}), exit: -1}
	s.outR, s.outW = io.Pipe()
	exit.Go(s.pump)
	return s, nil
}

var (
	shaRE   = regexp.MustCompile(`^[0-9a-f]{64}$`)
	argRE   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	agentAt = "/opt/bitc/bin/bitc"
)

// AgentCmd is the argv of an agent exec through the J4 sha check (4, J4 step 1):
//
//	sh -c 'echo "<sha>  /opt/bitc/bin/bitc" | sha256sum -c - >/dev/null && exec [sudo -n] /opt/bitc/bin/bitc __agent <args>'
//
// sha is the agent's sha256 (not a secret). Every arg must be a plain word (job ids, file names,
// offsets, verbs and "--boot-stdin"), so nothing can reach the shell as syntax.
func AgentCmd(sha string, sudo bool, args ...string) ([]string, error) {
	if !shaRE.MatchString(sha) {
		return nil, errors.New("agent sha256 must be 64 lowercase hex characters")
	}
	for _, a := range args {
		if !argRE.MatchString(strings.TrimPrefix(a, "--")) {
			return nil, fmt.Errorf("agent argument %q is not a plain word", a)
		}
	}
	run := "exec "
	if sudo {
		run += "sudo -n "
	}
	run += agentAt + " __agent " + strings.Join(args, " ")
	return []string{"sh", "-c", fmt.Sprintf("echo \"%s  %s\" | sha256sum -c - >/dev/null && %s", sha, agentAt, run)}, nil
}

// AttachOptions: AgentSHA is the agent's sha256; Sudo is false when exec runs as root (probe
// 9.15); Deadline is D, from which max_run_after_disconnect = D + 10m - now is set; Payload is
// the framed boot payload (agent.BootPayload.Frame), sent on stdin and never in a URL.
type AttachOptions struct {
	AgentSHA string
	Sudo     bool
	Deadline time.Time
	Payload  []byte
}

// Attach starts `__agent --boot-stdin` and delivers the boot payload (J4). The error classes are
// the attach classification (1.3): an HTTP error status before 101 is ErrPreAttach (the class of
// the status, 1.2; the Sprite is the caller's to delete); a failure to connect at all (nothing
// sent) is ErrPreAttach{ErrTransient}; anything after the upgrade request was written (101, a
// timeout, a network error, a failed stdin write) is ErrAttached. A Sprite this client created
// must have its job policy verified first (ErrNotProvisioned).
func (c *Client) Attach(ctx context.Context, name string, o AttachOptions) (*Session, error) {
	cmd, err := AgentCmd(o.AgentSHA, o.Sudo, "--boot-stdin")
	if err != nil {
		return nil, &provider.ErrIneligible{Why: err.Error()}
	}
	if len(o.Payload) == 0 || len(o.Payload) > agent.MaxBootPayload+8 {
		return nil, &provider.ErrIneligible{Why: "boot payload empty or over 8 MiB"}
	}
	req := ExecRequest{Cmd: cmd, Stdin: true}
	if !o.Deadline.IsZero() {
		req.MaxRunAfterDisconnect = max(time.Until(o.Deadline)+10*time.Minute, time.Minute)
	}
	s, err := c.exec(ctx, name, req, polJob)
	if err != nil {
		if errors.Is(err, ErrNotProvisioned) {
			return nil, err
		}
		var d *dialFailure
		var h *sentFailure
		switch {
		case errors.As(err, &d):
			return nil, PreAttach(name, d.err)
		case errors.As(err, &h):
			return nil, &provider.ErrAttached{ID: name, Err: h.err}
		}
		return nil, PreAttach(name, err) // an HTTP error status: definitely refused
	}
	if _, err := s.Write(o.Payload); err != nil {
		_ = s.Close()
		return nil, &provider.ErrAttached{ID: name, Err: &provider.ErrTransient{Why: "exec: writing the boot payload failed"}}
	}
	if err := s.CloseStdin(); err != nil {
		_ = s.Close()
		return nil, &provider.ErrAttached{ID: name, Err: &provider.ErrTransient{Why: "exec: closing stdin failed"}}
	}
	return s, nil
}

// ExecResult is a finished exec.
type ExecResult struct {
	Exit   int
	Stdout []byte // at most MaxOutput
	Stderr []byte // the last 64 KiB
}

// MaxOutput bounds what Run keeps of stdout.
const MaxOutput = 8 << 20

// Run execs r, sends stdin (when r.Stdin) and collects the result. It is for provisioning
// commands and small reads; use Exec, Tail or Read for streams.
func (c *Client) Run(ctx context.Context, name string, r ExecRequest, stdin []byte) (ExecResult, error) {
	r.Stdin = r.Stdin || len(stdin) > 0
	s, err := c.Exec(ctx, name, r)
	if err != nil {
		return ExecResult{}, err
	}
	defer s.Close()
	stop := context.AfterFunc(ctx, func() { _ = s.Close() })
	defer stop()
	// A process that exits before it has read all of stdin (an agent that refuses its input) cuts
	// the connection under the writer: the exit status that was sent first still counts.
	var stdinErr error
	if r.Stdin {
		if _, err := s.Write(stdin); err != nil {
			stdinErr = &provider.ErrTransient{Why: "exec: writing stdin failed"}
		} else if err := s.CloseStdin(); err != nil {
			stdinErr = &provider.ErrTransient{Why: "exec: closing stdin failed"}
		}
	}
	var out bytes.Buffer
	_, cerr := io.Copy(&out, io.LimitReader(s.Stdout(), MaxOutput+1))
	if out.Len() > MaxOutput {
		return ExecResult{}, &provider.ErrTransient{Why: "exec: output over 8 MiB"}
	}
	code, werr := s.Wait()
	if werr != nil && stdinErr != nil {
		return ExecResult{}, stdinErr
	}
	if werr == nil && cerr != nil && ctx.Err() != nil {
		werr = ctx.Err()
	}
	if werr != nil {
		return ExecResult{}, werr
	}
	return ExecResult{Exit: code, Stdout: out.Bytes(), Stderr: s.Stderr()}, nil
}

// ReadFrames decodes the NDJSON frames of `__agent tail` from r and calls fn for each, until the
// stream ends (nil) or fn returns an error. A line that is not a frame is an error.
func ReadFrames(r io.Reader, fn func(agent.Frame) error) error {
	dec := json.NewDecoder(r)
	for {
		var f agent.Frame
		if err := dec.Decode(&f); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		if err := fn(f); err != nil {
			return err
		}
	}
}

// Tail opens `__agent tail <job> <agent> <out> <err>` through the sha check and calls fn for each
// frame until the agent's final done frame, the end of the stream, or an error. A stream that
// ends before the done frame returns ErrStreamEnded (the caller reopens from the last offsets).
func (c *Client) Tail(ctx context.Context, name, agentSHA, job string, offs map[string]int64, fn func(agent.Frame) error) error {
	args := []string{"tail", job}
	for _, f := range []string{"agent.log", "out.log", "err.log"} {
		off := offs[f]
		if off < 0 {
			return &provider.ErrIneligible{Why: "negative tail offset"}
		}
		args = append(args, strconv.FormatInt(off, 10))
	}
	cmd, err := AgentCmd(agentSHA, c.sudo, args...)
	if err != nil {
		return &provider.ErrIneligible{Why: err.Error()}
	}
	s, err := c.Exec(ctx, name, ExecRequest{Cmd: cmd})
	if err != nil {
		return err
	}
	defer s.Close()
	stop := context.AfterFunc(ctx, func() { _ = s.Close() })
	defer stop()
	done := false
	ferr := ReadFrames(s.Stdout(), func(f agent.Frame) error {
		if f.Done {
			done = true
		}
		return fn(f)
	})
	if ferr != nil && !errors.Is(ferr, ErrStreamEnded) && !done {
		_ = s.Close() // the handler gave up: the agent's tail would run to the job's end
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return ferr
	}
	code, werr := s.Wait()
	switch {
	case ctx.Err() != nil:
		return ctx.Err()
	case ferr != nil && !errors.Is(ferr, ErrStreamEnded):
		return ferr
	case done:
		return nil
	case werr != nil:
		return werr
	case ferr != nil:
		return ferr
	case code != 0:
		return &TailExitError{Code: code}
	}
	return ErrStreamEnded
}

// Read streams `__agent read <job> <file> <off>` into w and returns the agent's exit status
// (0 ok, 64 bad argument, 65 offset past the end, 66 not found, 69 agent error).
func (c *Client) Read(ctx context.Context, name, agentSHA, job, file string, off int64, w io.Writer) (int, error) {
	if off < 0 {
		return 0, &provider.ErrIneligible{Why: "negative read offset"}
	}
	cmd, err := AgentCmd(agentSHA, c.sudo, "read", job, file, strconv.FormatInt(off, 10))
	if err != nil {
		return 0, &provider.ErrIneligible{Why: err.Error()}
	}
	s, err := c.Exec(ctx, name, ExecRequest{Cmd: cmd})
	if err != nil {
		return 0, err
	}
	defer s.Close()
	stop := context.AfterFunc(ctx, func() { _ = s.Close() })
	defer stop()
	_, cerr := io.Copy(w, s.Stdout())
	code, werr := s.Wait()
	switch {
	case ctx.Err() != nil:
		return 0, ctx.Err()
	case werr != nil:
		return 0, werr
	case cerr != nil:
		return 0, cerr
	}
	return code, nil
}
