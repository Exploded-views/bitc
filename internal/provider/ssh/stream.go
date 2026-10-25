package ssh

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"

	"github.com/Exploded-views/bitc/internal/agent"
	"github.com/Exploded-views/bitc/internal/provider"
)

// ErrStreamEnded: the tail's session ended before the agent's done frame (a dropped link); the
// caller reopens it from the last offsets.
var ErrStreamEnded = &provider.ErrTransient{Why: "tail stream ended without a done frame"}

// TailExitError: `agent tail` ended with a nonzero exit status (66: the job's directory is not
// there yet, which the first tail after the attach can meet). It is transient.
type TailExitError struct{ Code int }

func (e *TailExitError) Error() string { return fmt.Sprintf("agent tail exited %d", e.Code) }
func (e *TailExitError) As(target any) bool {
	if t, ok := target.(**provider.ErrTransient); ok {
		*t = &provider.ErrTransient{Why: e.Error()}
		return true
	}
	return false
}

// frameWriter decodes the NDJSON frames of `agent tail` as they arrive.
type frameWriter struct {
	buf  []byte
	fn   func(agent.Frame) error
	done bool
	err  error // what fn answered: the copy to the process hides it behind its own error
}

func (f *frameWriter) Write(b []byte) (int, error) {
	f.buf = append(f.buf, b...)
	for {
		i := bytes.IndexByte(f.buf, '\n')
		if i < 0 {
			return len(b), nil
		}
		line := f.buf[:i]
		f.buf = f.buf[i+1:]
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var fr agent.Frame
		if err := json.Unmarshal(line, &fr); err != nil {
			return 0, &provider.ErrTransient{Why: "unreadable tail frame"}
		}
		if fr.Done {
			f.done = true
		}
		if err := f.fn(fr); err != nil {
			f.err = err
			return 0, err
		}
	}
}

// tail opens `agent <sha> tail <job> <offsets>` over a fresh session and calls fn for each frame
// until the done frame (nil), the end of the stream (ErrStreamEnded) or an error.
func (p *Provider) tail(ctx context.Context, sha, job string, offs map[string]int64, fn func(agent.Frame) error) error {
	verb := "agent " + sha + " tail " + job
	for _, f := range []string{"agent.log", "out.log", "err.log"} {
		if offs[f] < 0 {
			return &provider.ErrIneligible{Why: "negative tail offset"}
		}
		verb += " " + strconv.FormatInt(offs[f], 10)
	}
	fw := &frameWriter{fn: fn}
	res, err := p.run(ctx, verb, nil, fw)
	switch {
	case fw.done:
		return nil
	case fw.err != nil:
		return fw.err
	case ctx.Err() != nil:
		return ctx.Err()
	case err != nil:
		return err
	case res.Code != 0 && res.Code != 255:
		return &TailExitError{Code: res.Code}
	}
	return ErrStreamEnded
}

// read streams `agent <sha> read <job> <file> <off>` into w and returns the agent's exit status
// (0 ok, 64 bad argument, 65 offset past the end, 66 not found, 69 agent error). A session that
// failed (exit 255) is an error, not a status.
func (p *Provider) read(ctx context.Context, sha, job, file string, off int64, w io.Writer) (int, error) {
	if off < 0 {
		return 0, &provider.ErrIneligible{Why: "negative read offset"}
	}
	res, err := p.run(ctx, "agent "+sha+" read "+job+" "+file+" "+strconv.FormatInt(off, 10), nil, w)
	if err != nil {
		return 0, err
	}
	if res.Code == 255 || res.Code < 0 {
		return 0, &provider.ErrTransient{Why: fmt.Sprintf("ssh %s: read failed: %s", p.set.Host, firstNonEmpty(lastLine(res.Stderr), "the connection ended"))}
	}
	return res.Code, nil
}
