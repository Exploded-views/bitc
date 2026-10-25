package state

import (
	"encoding/json"
	"os"
	"time"

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
	for {
		b, err := json.Marshal(e)
		if err != nil {
			return nil, err
		}
		if len(b)+1 <= maxLine || len(e.Argv) == 0 {
			return append(b, '\n'), nil
		}
		longest := 0
		for i, a := range e.Argv {
			if len(a) > len(e.Argv[longest]) {
				longest = i
			}
		}
		if len(e.Argv[longest]) <= 16 {
			e.Argv = e.Argv[:len(e.Argv)-1]
		} else {
			e.Argv[longest] = e.Argv[longest][:len(e.Argv[longest])/2]
		}
		e.ArgvTruncated = true
	}
}
