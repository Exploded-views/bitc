package runpod

import (
	"bufio"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Exploded-views/bitc/internal/agent"
	"github.com/Exploded-views/bitc/internal/provider"
)

// postBeginGrace is how long past min(BEGIN + timeout, D) the host waits for the agent's own kill
// and its EXIT marker before it gives up on a begun job (spec 8.4 step 3, 8.7).
const postBeginGrace = 2 * time.Minute

// Watch follows the container log until BEGIN, then the results server, polling status every
// 15s (spec 8.7). Markers count only with this job's nonce.
func (p *Provider) Watch(ctx context.Context, h provider.Handle, out provider.Sink) (provider.Result, error) {
	j := jobOf(h.ID)
	if j == nil {
		return provider.Result{}, fmt.Errorf("pod %s was not launched by this process", h.ID)
	}
	nonce := hex.EncodeToString(j.Nonce[:])
	seen := map[string]int{}
	began, boots := false, 0
	var maxSeq int64
	sawBegin := false // BEGIN came from the log stream
	nextStatus := time.Now()
	var beganAt time.Time // when the host first saw BEGIN
	startedAt := ""       // the pod's startedAt at the first status read that had one
	var nextTail, nextLog time.Time
	var logBackoff time.Duration // the last 429 wait (unscaled); 0 after a good read
	dbg := debugWatch()
	offs := map[provider.Stream]int64{}
	// tail copies what the results server has past the forwarded offsets into the sink (spec 8.7:
	// after BEGIN the command's output comes from out.log and err.log by Range, about every 2s).
	tail := func() {
		for _, st := range []provider.Stream{provider.Stdout, provider.Stderr} {
			w := &tailWriter{out: out, s: st, off: offs}
			_ = p.Output(ctx, h, st, offs[st], w) // a failed or empty read is retried on the next tick
		}
	}
	for {
		if ctx.Err() != nil {
			return provider.Result{Began: began}, ctx.Err()
		}
		// start timeout: no BEGIN by create + start_timeout (= D - timeout). The pod exists, so the
		// host tears it down and exits 75, never a fallback (spec 8.2).
		if startBy := j.Deadline.Add(-j.Timeout); !began && time.Now().After(startBy) {
			return provider.Result{}, fmt.Errorf("pod %s did not start the command within %d min", h.ID, int(startBy.Sub(j.Created).Minutes()))
		}
		// the agent kills the command at min(BEGIN + timeout, D); the host waits a grace for that
		// kill and the EXIT marker, counting from when it saw BEGIN (spec 8.4 step 3)
		if began && time.Now().After(minTime(beganAt.Add(j.Timeout), j.Deadline).Add(p.scale(postBeginGrace))) {
			return provider.Result{Began: true}, fmt.Errorf("pod %s timed out after %d min", h.ID, int(j.Timeout.Minutes()))
		}
		// Log reads are tail=5000 slices (the since= query returned nothing on the live API,
		// 2026-10-23; the dedupe below drops what was already seen). One slice lasts at most 60s and
		// never past the next status poll; reads are spaced (2s before BEGIN, 5s after, when the
		// results server carries the output and the log only has to deliver markers) and a 429
		// backs them off, so the status, tail and start/deadline checks run on their own clocks.
		var lines []logLine
		if !time.Now().Before(nextLog) {
			slice := min(p.scale(60*time.Second), max(time.Until(nextStatus), p.scale(time.Second)))
			if began {
				slice = min(slice, p.scale(2*time.Second))
			}
			began0 := time.Now()
			ls, st, err := p.logSlice(ctx, h.ID, slice)
			if dbg {
				p.traceSlice(st, ls, began0, err)
			}
			lines = ls
			wait := 2 * time.Second
			if began {
				wait = 5 * time.Second
			}
			var le *logErr
			switch {
			case err == nil:
				logBackoff = 0
			case errors.As(err, &le) && le.status == 429:
				switch {
				case le.retryAfter > 0:
					wait = min(le.retryAfter, 2*time.Minute)
				case logBackoff == 0:
					wait = 2 * time.Second
				default:
					wait = min(2*logBackoff, 30*time.Second)
				}
				logBackoff = wait
				if dbg {
					fmt.Fprintf(debugOut, "watch: log read throttled (429), next read in %s\n", wait)
				}
			default:
				wait = time.Second // do not spin while the log endpoint is unreachable
			}
			nextLog = time.Now().Add(p.scale(wait))
		} else {
			wake := minTime(nextLog, nextStatus)
			if began {
				wake = minTime(wake, nextTail)
			}
			select {
			case <-ctx.Done():
			case <-time.After(min(max(time.Until(wake), time.Millisecond), p.scale(time.Second))):
			}
		}
		for _, l := range lines {
			key := l.ts + " " + l.line
			if seen[key] > 0 {
				continue
			}
			seen[key]++
			m, ok := agent.ParseMarker(l.line, nonce)
			switch {
			case !ok:
				if !began {
					out.Setup(l.line)
				}
			case m.Kind == agent.MarkBoot:
				boots++
				if began && boots > 1 {
					return provider.Result{Began: true}, fmt.Errorf("pod %s restarted while the command ran", h.ID)
				}
			case m.Kind == agent.MarkSetupFailed:
				// the agent's class decides 69 (repo) or 75 (infra, missing or unparsable: m.Class
				// is "" then); the host tears the pod down either way (spec 8.2, 8.4, 15.9)
				return provider.Result{}, &provider.ErrSetup{Step: m.Arg, Class: m.Class, Line: m.Line}
			case m.Kind == agent.MarkLost:
				if began {
					return provider.Result{Began: true}, fmt.Errorf("pod %s restarted while the command ran", h.ID)
				}
				return provider.Result{}, fmt.Errorf("pod %s restarted before the command started", h.ID)
			case m.Kind == agent.MarkBegin:
				began, sawBegin = true, true
				beganAt = time.Now()
				nextTail = time.Now().Add(p.scale(2 * time.Second))
				out.Phase(provider.Begun, "")
			case m.Kind == agent.MarkExit:
				code, _ := strconv.Atoi(m.Arg) // ParseMarker validated it
				if began {
					tail() // the logs are complete once the agent printed EXIT: forward the rest
				}
				return provider.Result{Exit: code, Began: true}, nil
			}
		}
		if began && !time.Now().Before(nextTail) {
			tail()
			nextTail = time.Now().Add(p.scale(2 * time.Second))
		}
		if time.Now().After(nextStatus) {
			nextStatus = time.Now().Add(p.scale(15 * time.Second))
			st, sa, err := p.statusStart(ctx, h.ID)
			// a new start of the pod is a restart even when the container never printed a second
			// BOOT (build mode: the stub's fetch fails once the host deleted the agent ref)
			if err == nil && sa != "" {
				if startedAt == "" {
					startedAt = sa
				} else if sa != startedAt {
					if began {
						return provider.Result{Began: true}, fmt.Errorf("pod %s restarted while the command ran", h.ID)
					}
					return provider.Result{}, fmt.Errorf("pod %s restarted before the command started", h.ID)
				}
			}
			if err == nil && (st == "gone" || st == "EXITED" || st == "TERMINATED" || st == "ERROR") {
				if began {
					return provider.Result{Began: true}, fmt.Errorf("pod %s was lost (%s)", h.ID, st)
				}
				return provider.Result{}, fmt.Errorf("pod %s stopped before the command started (%s)", h.ID, st)
			}
			// the log stream can miss BEGIN or the end: ask the results server, whose state.json
			// counts only when its MAC, job and attempt verify. The exit it carries is a hint; the
			// job's outcome is decided by the verified result.json (orchestrate complete).
			if st, ok := p.verifiedState(ctx, h, j, &maxSeq, dbg); ok {
				ended := st.Phase == "ended" && st.Exit != nil
				if !began && (st.Phase == "begun" || st.Phase == "ended") {
					began = true
					beganAt = time.Now()
					nextTail = time.Now().Add(p.scale(2 * time.Second))
					out.Phase(provider.Begun, "")
				}
				if began && ended {
					why := "stream-missed-end"
					if !sawBegin {
						why = "stream-missed-begin"
					}
					tail()
					return provider.Result{Exit: *st.Exit, Began: true, Why: why}, nil
				}
			}
		}
	}
}

type logLine struct{ ts, line string }

// logErr is a non-2xx answer from the log endpoint; retryAfter is its Retry-After (0 when absent).
type logErr struct {
	status     int
	retryAfter time.Duration
}

func (e *logErr) Error() string { return fmt.Sprintf("logs: HTTP %d", e.status) }

// retryAfter parses a Retry-After header given in seconds or as an HTTP date; 0 when absent or bad.
func retryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if n, err := strconv.Atoi(v); err == nil && n > 0 {
		return time.Duration(n) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		return max(time.Until(t), 0)
	}
	return 0
}

// logSlice reads one SSE connection of at most d from tail=5000 and returns its lines and the HTTP
// status (0 when none came).
func (p *Provider) logSlice(ctx context.Context, id string, d time.Duration) ([]logLine, int, error) {
	status := 0
	ctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	req, err := p.api.newReq(ctx, "GET", "/v2/pods/"+id+"/logs?source=container&tail=5000")
	if err != nil {
		return nil, status, err
	}
	resp, err := p.api.http.Do(req)
	if err != nil {
		return nil, status, err
	}
	status = resp.StatusCode
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return nil, status, &logErr{status: status, retryAfter: retryAfter(resp.Header.Get("Retry-After"))}
	}
	var out []logLine
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		data, ok := strings.CutPrefix(sc.Text(), "data:")
		if !ok {
			continue
		}
		var ev struct{ TS, Line string }
		if json.Unmarshal([]byte(strings.TrimSpace(data)), &ev) != nil {
			continue
		}
		out = append(out, logLine{ts: ev.TS, line: ev.Line})
		if strings.HasPrefix(ev.Line, agent.MarkExit+" ") {
			break // a likely end: Watch checks the nonce
		}
	}
	return out, status, nil
}

// verifiedState reads the results server's state.json and returns it only if its MAC verifies
// under K for this job and attempt and its seq is not below the highest accepted (*maxSeq, which
// it advances). Anything else is ignored: never begun, never ended.
func (p *Provider) verifiedState(ctx context.Context, h provider.Handle, j *provider.JobSpec, maxSeq *int64, dbg bool) (agent.State, bool) {
	if p.http == nil || h.ResultsURL == "" {
		return agent.State{}, false
	}
	resp, err := p.results(ctx, h, "state.json", 0, hex.EncodeToString(j.Bearer[:]))
	if err != nil {
		return agent.State{}, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return agent.State{}, false
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return agent.State{}, false
	}
	st, err := agent.VerifyState(j.MacKey[:], raw, j.Job, hex.EncodeToString(j.Nonce[:]))
	if err != nil || st.Seq < *maxSeq {
		if dbg {
			why := "failed verification"
			if err == nil {
				why = "replayed seq"
			}
			fmt.Fprintf(debugOut, "watch: state ignored (%s)\n", why)
		}
		return agent.State{}, false
	}
	*maxSeq = st.Seq
	return st, true
}

// Output copies a results stream from off (spec 8.4 "Host side").
func (p *Provider) Output(ctx context.Context, h provider.Handle, s provider.Stream, off int64, w io.Writer) error {
	name := map[provider.Stream]string{provider.Stdout: "out.log", provider.Stderr: "err.log",
		provider.ResultFile: "result.json", provider.StateFile: "state.json"}[s]
	if name == "" {
		return fmt.Errorf("unknown stream %d", s)
	}
	j := jobOf(h.ID)
	if j == nil {
		return fmt.Errorf("pod %s was not launched by this process", h.ID)
	}
	resp, err := p.results(ctx, h, name, off, hex.EncodeToString(j.Bearer[:]))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 && resp.StatusCode != 206 {
		return fmt.Errorf("%s: HTTP %d", name, resp.StatusCode)
	}
	_, err = io.Copy(w, resp.Body)
	return err
}

// Fetch downloads artifacts.tar.gz from a.Offset, capped (resumable with Range).
func (p *Provider) Fetch(ctx context.Context, h provider.Handle, a provider.ArtifactSpec, w io.Writer) error {
	j := jobOf(h.ID)
	if j == nil {
		return fmt.Errorf("pod %s was not launched by this process", h.ID)
	}
	resp, err := p.results(ctx, h, "artifacts.tar.gz", a.Offset, hex.EncodeToString(j.Bearer[:]))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 && resp.StatusCode != 206 {
		return fmt.Errorf("artifacts: HTTP %d", resp.StatusCode)
	}
	n, err := io.Copy(w, io.LimitReader(resp.Body, a.MaxCompressed-a.Offset+1))
	if err == nil && a.Offset+n > a.MaxCompressed {
		return fmt.Errorf("the artifact archive is over max_compressed")
	}
	return err
}

// tailWriter forwards a results stream to the sink and advances that stream's offset.
type tailWriter struct {
	out provider.Sink
	s   provider.Stream
	off map[provider.Stream]int64
}

func (w *tailWriter) Write(b []byte) (int, error) {
	if len(b) > 0 {
		w.out.Out(w.s, append([]byte(nil), b...))
		w.off[w.s] += int64(len(b))
	}
	return len(b), nil
}

// debugOut receives the BITC_DEBUG=watch trace.
var debugOut io.Writer = os.Stderr

func debugWatch() bool {
	for _, w := range strings.FieldsFunc(os.Getenv("BITC_DEBUG"), func(r rune) bool { return r == ',' || r == ' ' }) {
		if w == "watch" {
			return true
		}
	}
	return false
}

// traceSlice prints one line per log slice: no bearer, key or env, only shapes.
func (p *Provider) traceSlice(status int, lines []logLine, started time.Time, err error) {
	var kinds []string
	for _, l := range lines {
		if f := strings.Fields(l.line); len(f) > 0 && strings.HasPrefix(f[0], "__BITC_") {
			kinds = append(kinds, strings.TrimSuffix(strings.TrimPrefix(f[0], "__BITC_"), "__"))
		}
	}
	e := ""
	if err != nil {
		e = " err=" + strconv.Quote(err.Error())
	}
	fmt.Fprintf(debugOut, "watch: slice mode=tail status=%d lines=%d markers=%s elapsed=%s%s\n",
		status, len(lines), strings.Join(kinds, ","), time.Since(started).Round(time.Millisecond), e)
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
