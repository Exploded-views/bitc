package state

import (
	"encoding/json"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/sys/unix"

	"github.com/Exploded-views/bitc/internal/redact"
)

// Event is one history.jsonl line (spec 9). Refusals write nothing.
type Event struct {
	TS            string   `json:"ts"`
	Event         string   `json:"event"` // start | end | starved | fallback | reaped
	Pool          string   `json:"pool"`
	Slot          int      `json:"slot,omitempty"`
	Job           string   `json:"job"`
	Pid           int      `json:"pid"`
	Nested        bool     `json:"nested,omitempty"`
	Argv          []string `json:"argv"`
	ArgvTruncated bool     `json:"argv_truncated,omitempty"`
	Cwd           string   `json:"cwd"`
	Provider      string   `json:"provider"`
	Resource      string   `json:"resource,omitempty"`
	Sha           string   `json:"sha,omitempty"`
	WaitMS        *int64   `json:"wait_ms,omitempty"`
	RunMS         *int64   `json:"run_ms,omitempty"`
	Exit          *int     `json:"exit,omitempty"`
	Outcome       string   `json:"outcome,omitempty"`
	Reason        string   `json:"reason,omitempty"`
}

const maxLine = 4095

// maxReason bounds a history reason (a build's output or an API message can be long), so a line
// stays one atomic write before argv is shortened.
const maxReason = 512

// cutMark ends a field that was cut.
const cutMark = " [truncated]"

// cutReason cuts a reason over maxReason bytes at a UTF-8 boundary and marks it.
func cutReason(r string) string {
	if len(r) <= maxReason {
		return r
	}
	return cutTo(r, maxReason)
}

// cutTo cuts s (without an earlier mark) to at most n bytes at a UTF-8 boundary and marks it.
func cutTo(s string, n int) string {
	s = strings.TrimSuffix(s, cutMark)
	if len(s) > n {
		s = s[:runeCut(s, n)]
	}
	return s + cutMark
}

// runeCut is the largest index <= n that starts a character of s.
func runeCut(s string, n int) int {
	for n > 0 && n < len(s) && !utf8.RuneStart(s[n]) {
		n--
	}
	return n
}

// AppendHistory appends one redacted JSON line with a single O_APPEND write.
func AppendHistory(path string, e Event, red *redact.Set) error {
	if e.TS == "" {
		e.TS = time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	}
	line, err := encode(e, red)
	if err != nil {
		return err
	}
	fd, err := unix.Open(path, unix.O_WRONLY|unix.O_APPEND|unix.O_CREAT|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return &os.PathError{Op: "open", Path: path, Err: err}
	}
	defer unix.Close(fd)
	_, err = unix.Write(fd, line)
	return err
}

// encode redacts the string fields first (JSON escaping would hide a secret holding <, >, & or a
// quote, and a halved word would keep part of one), then shortens a copy of argv: the caller's
// slice is the argv execve gets next.
func encode(e Event, red *redact.Set) ([]byte, error) {
	e.Argv = append([]string(nil), e.Argv...)
	for i := range e.Argv {
		e.Argv[i] = red.String(e.Argv[i])
	}
	e.Cwd, e.Reason, e.Resource = red.String(e.Cwd), red.String(e.Reason), red.String(e.Resource)
	e.Reason = cutReason(e.Reason)
	// The bound is on the encoded line (JSON escaping can make a field six times longer): the
	// reason is halved first, then argv, then the cwd and the resource, and as a last resort the
	// pool, job, provider and sha, each cut at a UTF-8 boundary, until the line fits.
	for {
		b, err := json.Marshal(e)
		if err != nil {
			return nil, err
		}
		if len(b)+1 <= maxLine {
			return append(b, '\n'), nil
		}
		switch {
		case len(e.Reason) > 64:
			e.Reason = cutTo(e.Reason, len(strings.TrimSuffix(e.Reason, cutMark))/2)
		case len(e.Argv) > 0:
			longest := 0
			for i, a := range e.Argv {
				if len(a) > len(e.Argv[longest]) {
					longest = i
				}
			}
			if w := e.Argv[longest]; len(w) <= 16 {
				e.Argv = e.Argv[:len(e.Argv)-1]
			} else {
				e.Argv[longest] = w[:runeCut(w, len(w)/2)]
			}
			e.ArgvTruncated = true
		case len(e.Cwd) > 64:
			e.Cwd = cutTo(e.Cwd, len(strings.TrimSuffix(e.Cwd, cutMark))/2)
		case len(e.Resource) > 64:
			e.Resource = cutTo(e.Resource, len(strings.TrimSuffix(e.Resource, cutMark))/2)
		case len(e.Pool) > 64: // the last resort: names are not length-checked where they are written
			e.Pool = cutTo(e.Pool, len(strings.TrimSuffix(e.Pool, cutMark))/2)
		case len(e.Job) > 64:
			e.Job = cutTo(e.Job, len(strings.TrimSuffix(e.Job, cutMark))/2)
		case len(e.Provider) > 64:
			e.Provider = cutTo(e.Provider, len(strings.TrimSuffix(e.Provider, cutMark))/2)
		case len(e.Sha) > 64:
			e.Sha = cutTo(e.Sha, len(strings.TrimSuffix(e.Sha, cutMark))/2)
		default: // nothing long is left
			return append(b, '\n'), nil
		}
	}
}
