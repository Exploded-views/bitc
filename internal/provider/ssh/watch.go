package ssh

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"
	"time"

	"github.com/Exploded-views/bitc/internal/agent"
	"github.com/Exploded-views/bitc/internal/exit"
	"github.com/Exploded-views/bitc/internal/provider"
)

// postBeginGrace is how long past min(BEGIN + timeout, D) the host waits for the agent's own kill
// and its EXIT marker before it gives up on a begun job (as RunPod's, spec 8.4 step 3).
const postBeginGrace = 2 * time.Minute

var files = []string{"agent.log", "out.log", "err.log"}

// watcher follows one job: the agent's log for the markers, the output files for the live
// output, and the host itself (sprites-provider.md 4, J5).
type watcher struct {
	p     *Provider
	st    *jobState
	id    string
	out   provider.Sink
	nonce string
	scale func(time.Duration) time.Duration

	offs    map[string]int64
	partial []byte // the last, incomplete line of agent.log

	mu        sync.Mutex
	began     bool
	beganAt   time.Time
	boots     int
	exitSeen  bool
	exitCode  int
	why       string
	monitored error // set by the monitor: the host is gone, or a deadline passed
}

// fatal ends the watch with this error and result (a setup failure, a restart).
type fatal struct {
	err error
	res provider.Result
}

func (f *fatal) Error() string { return f.err.Error() }
func (f *fatal) Unwrap() error { return f.err }

// errGap: a frame started past the offset the host has; the tail is reopened from the offsets.
var errGap = &provider.ErrTransient{Why: "tail frame past the expected offset"}

// Watch follows the job until its EXIT marker and the end of the agent (spec 8.4 host side). A
// dropped tail is not an error: it is reopened from the last offsets, and when reopening fails
// twice within 30 s the files are read every 5 s until a reopen succeeds.
func (p *Provider) Watch(ctx context.Context, h provider.Handle, out provider.Sink) (provider.Result, error) {
	st := p.job(h.ID)
	if st == nil {
		return provider.Result{}, fmt.Errorf("ssh job %s was not launched by this process", h.ID)
	}
	for _, l := range st.prov {
		out.Setup(l) // the provisioning output, printed as `pod:` lines before anything of the job
	}
	w := &watcher{p: p, st: st, id: h.ID, out: out, nonce: hex.EncodeToString(st.spec.Nonce[:]), scale: p.eff.Scale, offs: map[string]int64{"agent.log": 0, "out.log": 0, "err.log": 0}}
	return w.run(ctx)
}

func (w *watcher) run(parent context.Context) (provider.Result, error) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	monDone := make(chan struct{})
	exit.Go(func() { defer close(monDone); w.monitor(ctx, cancel) })
	defer func() { cancel(); <-monDone }()

	var fails []time.Time
	poll := false
	for {
		if ctx.Err() != nil {
			return w.ended(parent)
		}
		var got bool
		var err error
		if poll {
			done, perr := w.pollOnce(ctx)
			if done && perr == nil {
				return w.finish(ctx)
			}
			var pf *fatal
			if errors.As(perr, &pf) {
				return pf.res, pf.err
			}
			select { // between reads, and before trying the tail again
			case <-ctx.Done():
			case <-time.After(w.scale(5 * time.Second)):
			}
			if ctx.Err() != nil {
				continue
			}
			if got, err = w.tailOnce(ctx); err == nil {
				return w.finish(ctx)
			}
			if got {
				poll, fails = false, nil
			}
		} else if got, err = w.tailOnce(ctx); err == nil {
			return w.finish(ctx)
		}
		var f *fatal
		if errors.As(err, &f) {
			return f.res, f.err
		}
		if ctx.Err() != nil {
			continue
		}
		var te *TailExitError
		if errors.As(err, &te) && te.Code == 66 && !got && w.notBegun() {
			// the agent has not made the job's directory yet: not a failure, look again shortly
			select {
			case <-ctx.Done():
			case <-time.After(max(w.scale(250*time.Millisecond), 5*time.Millisecond)):
			}
			continue
		}
		now := time.Now()
		if got {
			fails = nil
		} else {
			fails = append(fails, now)
		}
		for len(fails) > 0 && now.Sub(fails[0]) > w.scale(30*time.Second) {
			fails = fails[1:]
		}
		if len(fails) >= 2 {
			poll = true
		}
		if !poll {
			select {
			case <-ctx.Done():
			case <-time.After(max(w.scale(200*time.Millisecond), 5*time.Millisecond)):
			}
		}
	}
}

func (w *watcher) notBegun() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return !w.began
}

// ended is the result when the watch's context ended: the monitor's reason, else the caller's.
func (w *watcher) ended(parent context.Context) (provider.Result, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.monitored != nil {
		return provider.Result{Began: w.began}, w.monitored
	}
	return provider.Result{Began: w.began}, parent.Err()
}

// monitor enforces the start timeout, the command's deadline and the host's existence (every
// 60 s): a 404 or another organization is the end of the job.
func (w *watcher) monitor(ctx context.Context, cancel context.CancelFunc) {
	j := w.st.spec
	tick := time.NewTicker(max(w.scale(time.Second), 20*time.Millisecond))
	defer tick.Stop()
	fail := func(err error) {
		w.mu.Lock()
		if w.monitored == nil {
			w.monitored = err
		}
		w.mu.Unlock()
		cancel()
	}
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-tick.C:
			w.mu.Lock()
			began, beganAt, exitSeen := w.began, w.beganAt, w.exitSeen
			w.mu.Unlock()
			if startBy := j.Deadline.Add(-j.Timeout); !began && !exitSeen && now.After(startBy) {
				fail(fmt.Errorf("ssh job %s did not start the command within %s", w.id, startBy.Sub(j.Created).Round(time.Second)))
				return
			}
			if began && !exitSeen && now.After(minTime(beganAt.Add(j.Timeout), j.Deadline).Add(w.scale(postBeginGrace))) {
				fail(fmt.Errorf("ssh job %s timed out after %s", w.id, j.Timeout.Round(time.Second)))
				return
			}
		}
	}
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

// tailOnce opens `__agent tail` from the offsets and handles its frames until the final done
// frame (nil) or an error. got reports whether any frame arrived.
func (w *watcher) tailOnce(ctx context.Context) (got bool, err error) {
	offs := map[string]int64{}
	for _, f := range files {
		offs[f] = w.offs[f]
	}
	err = w.p.tail(ctx, w.st.sha, w.st.spec.Job, offs, func(f agent.Frame) error {
		got = true
		return w.frame(f)
	})
	return got, err
}

// pollOnce reads the three files from the offsets with `__agent read`. done: the agent's EXIT
// marker was seen, so out.log and err.log are committed and were read too.
func (w *watcher) pollOnce(ctx context.Context) (done bool, err error) {
	read := func(name string) error {
		var b bytes.Buffer
		code, err := w.p.read(ctx, w.st.sha, w.st.spec.Job, name, w.offs[name], &b)
		if err != nil {
			return err
		}
		if code != 0 || b.Len() == 0 {
			return nil // not there yet (66), or nothing new
		}
		return w.frame(agent.Frame{File: name, Off: w.offs[name], Data: b.Bytes()})
	}
	if err := read("agent.log"); err != nil {
		return false, err
	}
	w.mu.Lock()
	exitSeen := w.exitSeen
	w.mu.Unlock()
	if !exitSeen {
		return false, nil
	}
	for _, n := range []string{"out.log", "err.log"} {
		if err := read(n); err != nil {
			return false, err
		}
	}
	return true, nil
}

// frame applies one frame of the tail (or one read).
func (w *watcher) frame(f agent.Frame) error {
	if f.File == "" {
		return nil // a heartbeat or the done frame
	}
	have, ok := w.offs[f.File]
	if !ok {
		return nil
	}
	data := f.Data
	switch {
	case f.Off > have:
		return errGap
	case f.Off < have:
		skip := have - f.Off
		if skip >= int64(len(data)) {
			return nil
		}
		data = data[skip:]
	}
	w.offs[f.File] = have + int64(len(data))
	switch f.File {
	case "agent.log":
		return w.agentLog(data)
	case "out.log":
		w.out.Out(provider.Stdout, append([]byte(nil), data...))
	case "err.log":
		w.out.Out(provider.Stderr, append([]byte(nil), data...))
	}
	return nil
}

func (w *watcher) agentLog(data []byte) error {
	w.partial = append(w.partial, data...)
	for {
		i := bytes.IndexByte(w.partial, '\n')
		if i < 0 {
			return nil
		}
		line := string(bytes.TrimRight(w.partial[:i], "\r"))
		w.partial = w.partial[i+1:]
		if err := w.line(line); err != nil {
			return err
		}
	}
}

func (w *watcher) setBegan() {
	w.mu.Lock()
	first := !w.began
	if first {
		w.began, w.beganAt = true, time.Now()
	}
	w.mu.Unlock()
	if first {
		w.out.Phase(provider.Begun, "")
	}
}

// line applies one line of the agent's log: a marker with this job's nonce, or text.
func (w *watcher) line(l string) error {
	m, ok := agent.ParseMarker(l, w.nonce)
	w.mu.Lock()
	began := w.began
	w.mu.Unlock()
	switch {
	case !ok:
		if !began {
			w.out.Setup(l)
		}
	case m.Kind == agent.MarkBoot:
		w.mu.Lock()
		w.boots++
		again := w.boots > 1
		w.mu.Unlock()
		if began && again {
			return &fatal{err: fmt.Errorf("ssh job %s restarted while the command ran", w.id), res: provider.Result{Began: true}}
		}
	case m.Kind == agent.MarkSetupFailed:
		// the agent's class decides 69 (repo) or 75 (infra, missing or unparsable)
		return &fatal{err: &provider.ErrSetup{Step: m.Arg, Class: m.Class, Line: m.Line}}
	case m.Kind == agent.MarkLost:
		if began {
			return &fatal{err: fmt.Errorf("ssh job %s restarted while the command ran", w.id), res: provider.Result{Began: true}}
		}
		return &fatal{err: fmt.Errorf("ssh job %s restarted before the command started", w.id)}
	case m.Kind == agent.MarkBegin:
		w.setBegan()
	case m.Kind == agent.MarkExit:
		code, _ := strconv.Atoi(m.Arg) // ParseMarker validated it
		w.mu.Lock()
		w.exitSeen, w.exitCode = true, code
		w.mu.Unlock()
	}
	return nil
}

// finish ends a watch whose tail ran to its done frame: the agent has exited.
func (w *watcher) finish(ctx context.Context) (provider.Result, error) {
	if len(w.partial) > 0 {
		_ = w.line(string(bytes.TrimRight(w.partial, "\r\n")))
		w.partial = nil
	}
	w.mu.Lock()
	exitSeen, code, began := w.exitSeen, w.exitCode, w.began
	w.mu.Unlock()
	if exitSeen {
		if !began {
			w.setBegan()
			w.why = "stream-missed-begin"
		}
		return provider.Result{Exit: code, Began: true, Why: w.why}, nil
	}
	// the agent is gone without an EXIT marker: its result.json says whether it finished
	var raw bytes.Buffer
	if rc, err := w.p.read(ctx, w.st.sha, w.st.spec.Job, "result.json", 0, &raw); err == nil && rc == 0 {
		if rf, err := agent.VerifyResult(w.st.spec.MacKey[:], raw.Bytes()); err == nil {
			if !began {
				w.setBegan()
			}
			return provider.Result{Exit: rf.Exit, Began: true, Why: "stream-missed-end"}, nil
		}
	}
	if began {
		return provider.Result{Began: true}, fmt.Errorf("ssh job %s was lost (the agent exited without a result)", w.id)
	}
	return provider.Result{}, fmt.Errorf("ssh job %s was lost before the command started", w.id)
}

// fileOf names the result file of a stream.
func fileOf(s provider.Stream) string {
	switch s {
	case provider.Stdout:
		return "out.log"
	case provider.Stderr:
		return "err.log"
	case provider.ResultFile:
		return "result.json"
	case provider.StateFile:
		return "state.json"
	}
	return ""
}

// readError is the error for a nonzero exit of `__agent read`.
func readError(file string, code int) error {
	switch code {
	case 65:
		return fmt.Errorf("%s: the offset is beyond the end of the file", file)
	case 66:
		return fmt.Errorf("%s: not found", file)
	}
	return fmt.Errorf("%s: the agent read exited %d", file, code)
}

// Output copies a result file from off (spec 8.4 "Host side"), over exec.
func (p *Provider) Output(ctx context.Context, h provider.Handle, s provider.Stream, off int64, w io.Writer) error {
	file := fileOf(s)
	if file == "" {
		return fmt.Errorf("unknown stream %d", s)
	}
	st := p.job(h.ID)
	if st == nil {
		return fmt.Errorf("ssh job %s was not launched by this process", h.ID)
	}
	code, err := p.read(ctx, st.sha, st.spec.Job, file, off, w)
	if err != nil {
		return err
	}
	if code != 0 {
		return readError(file, code)
	}
	return nil
}

// capWriter writes at most max bytes and then fails, so an oversized archive stops the read.
type capWriter struct {
	w    io.Writer
	left int64
	over bool
}

func (c *capWriter) Write(b []byte) (int, error) {
	if int64(len(b)) > c.left {
		c.over = true
		n, _ := c.w.Write(b[:c.left])
		c.left = 0
		return n, errors.New("over max_compressed")
	}
	c.left -= int64(len(b))
	return c.w.Write(b)
}

// Fetch downloads artifacts.tar.gz from a.Offset over exec, capped (resumable by offset).
func (p *Provider) Fetch(ctx context.Context, h provider.Handle, a provider.ArtifactSpec, w io.Writer) error {
	st := p.job(h.ID)
	if st == nil {
		return fmt.Errorf("ssh job %s was not launched by this process", h.ID)
	}
	cw := &capWriter{w: w, left: a.MaxCompressed - a.Offset}
	code, err := p.read(ctx, st.sha, st.spec.Job, "artifacts.tar.gz", a.Offset, cw)
	if cw.over {
		return errors.New("the artifact archive is over max_compressed")
	}
	if err != nil {
		return err
	}
	if code != 0 {
		return readError("artifacts", code)
	}
	return nil
}
