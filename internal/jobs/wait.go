package jobs

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/Exploded-views/bitc/internal/exit"
	"github.com/Exploded-views/bitc/internal/proc"
)

// WaitOptions configure a bounded wait (spec 3.2).
type WaitOptions struct {
	Record  *Record // nil with LogPath
	LogPath string  // --log PATH: a log with no record (legacy J8/J9)
	Timeout time.Duration
	Poll    time.Duration // 10s, scaled by [test] time_scale
	ExitJob bool          // --exit job
}

type result struct {
	Origin   string `json:"origin"`
	Outcome  string `json:"outcome"`
	ExitCode int    `json:"exit_code"`
}

// Wait polls until the job ends or the timeout passes; it returns stdout and the exit code.
func Wait(o WaitOptions) (string, int) {
	var gen int
	log, specLog := o.LogPath, ""
	if o.Record != nil {
		gen = o.Record.Gen() // bind to the generation at first read
		if sp, err := o.Record.ReadSpec(gen); err == nil {
			specLog = sp.Log
			log = o.Record.LogFor(gen, specLog)
		}
	}
	if _, err := os.Stat(log); err != nil {
		return fmt.Sprintf("NO LOG %s (no such job: check the name and the work tree)\n", log), exit.NoInput
	}
	deadline := time.Now().Add(o.Timeout)
	for {
		if o.Record != nil && specLog != "" {
			log = o.Record.LogFor(gen, specLog) // follows the log into prev-<gen>/ after a rerun
		}
		b, _ := os.ReadFile(log)
		if o.Record == nil {
			if code, ok := LastExit(b); ok {
				head := fmt.Sprintf("RESULT exit %d", code)
				if code == 75 {
					head = "RESULT starved"
				}
				return ended(head, log, b), 0
			}
		} else {
			var r result
			if err := o.Record.ReadResult(gen, &r); err == nil {
				return o.recordEnded(r, log, b)
			}
			if o.lost(gen) {
				_ = o.Record.WriteResult(gen, map[string]any{"origin": "bitc", "outcome": "lost", "exit_code": 75,
					"reason": "the supervisor died without a result"})
				return ended("RESULT starved", log, b), exit.TempFail
			}
		}
		if !time.Now().Before(deadline) {
			var sb strings.Builder
			fmt.Fprintf(&sb, "RUNNING (no exit line yet after %ds); last lines:\n", int(o.Timeout.Seconds()))
			for _, l := range Tail(b, 5) {
				sb.WriteString(l + "\n")
			}
			return sb.String(), exit.StillRunning
		}
		wait := time.Until(deadline)
		if wait > o.Poll {
			wait = o.Poll
		}
		time.Sleep(wait)
	}
}

func (o WaitOptions) recordEnded(r result, log string, b []byte) (string, int) {
	switch {
	case r.Origin == "bitc" && (r.Outcome == "starved" || r.Outcome == "lost"):
		return ended("RESULT starved", log, b), exit.TempFail
	case r.Origin == "bitc" && r.Outcome == "internal":
		return ended("RESULT internal error", log, b), exit.Unavailable
	}
	code := 0
	if o.ExitJob {
		code = r.ExitCode
	}
	return ended(fmt.Sprintf("RESULT exit %d", r.ExitCode), log, b), code
}

// SupervisorDead reports whether st's supervisor is gone: its pid is dead, reused (another start
// time) or a zombie (a non-reaping PID 1 keeps it as Z). No pid yet is not dead. wait, cancel and
// sweep (1.9) share it.
func SupervisorDead(st Status) bool {
	if st.Pid == 0 {
		return false
	}
	ps, err := proc.ReadStat(st.Pid)
	return err != nil || ps.Start != st.Start || ps.State == 'Z'
}

// lost: the supervisor of gen is gone and wrote no result.
func (o WaitOptions) lost(gen int) bool {
	st, err := o.Record.ReadStatus(gen)
	if err != nil || !SupervisorDead(st) {
		return false
	}
	var r result
	return o.Record.ReadResult(gen, &r) != nil
}

func ended(head, log string, b []byte) string {
	var sb strings.Builder
	sb.WriteString(head + "\n")
	fmt.Fprintf(&sb, "--- last 60 lines of %s ---\n", log)
	for _, l := range Tail(b, 60) {
		sb.WriteString(l + "\n")
	}
	return sb.String()
}
