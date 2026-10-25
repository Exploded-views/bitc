package status

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Exploded-views/bitc/internal/admit"
	"github.com/Exploded-views/bitc/internal/config"
	"github.com/Exploded-views/bitc/internal/jobs"
	"github.com/Exploded-views/bitc/internal/lock"
	"github.com/Exploded-views/bitc/internal/redact"
	"github.com/Exploded-views/bitc/internal/state"
)

// RemoteSweep is the remote part of sweep.
var RemoteSweep func(ctx context.Context, e *config.Effective, dryRun bool, provider string, foreign bool, out io.Writer)

// Sweep cleans local state (spec 3.3): dead tickets and their process groups, dead
// supervisors' records (lost), history rotation, retention and stale artifact staging dirs. It never fails on one bad item.
func Sweep(e *config.Effective, l state.Layout, dryRun bool, out io.Writer) error {
	swept := func(kind, id, why string) { fmt.Fprintf(out, "swept %s %s (%s)\n", kind, id, why) }
	pools, _ := os.ReadDir(filepath.Join(l.Root, "pools"))
	for _, p := range pools {
		if dryRun {
			for _, t := range admit.ReadTickets(l, p.Name()) {
				if !lock.HeldRO(filepath.Join(l.Tickets(p.Name()), t.Name)) {
					swept("ticket", p.Name()+"/"+t.Name, "owner is dead; dry run")
				}
			}
			continue
		}
		pool := p.Name()
		admit.PruneDead(l, pool, func(name string) { swept("ticket", pool+"/"+name, "owner is dead") },
			func(job string) {
				swept("job-group", pool+"/"+job, "supervisor died; process group killed")
				_ = state.AppendHistory(l.History(), state.Event{Event: "reaped", Pool: pool, Job: job, Provider: "local"}, redact.New())
			})
	}
	sweepRecords(l, e, dryRun, swept)
	sweepStaging(e, l, dryRun, swept)
	rotate(l, e, dryRun, swept)
	return nil
}

func sweepRecords(l state.Layout, e *config.Effective, dryRun bool, swept func(kind, id, why string)) {
	wts, _ := os.ReadDir(l.JobsDir())
	for _, wt := range wts {
		names, _ := os.ReadDir(filepath.Join(l.JobsDir(), wt.Name()))
		for _, n := range names {
			rec := jobs.Open(l, wt.Name(), n.Name())
			gen := rec.Gen()
			st, err := rec.ReadStatus(gen)
			if err != nil {
				continue
			}
			var res map[string]any
			ended := rec.ReadResult(gen, &res) == nil
			if !ended && st.Pid > 0 {
				// the same test `wait` uses (1.8): pid gone, reused or a zombie
				if jobs.SupervisorDead(st) {
					swept("job", wt.Name()+"/"+n.Name(), "supervisor died; marked lost")
					if !dryRun {
						_ = rec.WriteResult(gen, map[string]any{"origin": "bitc", "outcome": "lost", "exit_code": 75,
							"reason": "the supervisor died without a result", "job": n.Name(), "generation": gen})
						// ended_at starts the retention clock for the lost record
						st.State, st.EndedAt = "ended", time.Now().UTC().Format(time.RFC3339Nano)
						_ = rec.WriteStatus(st)
					}
				}
				continue
			}
			if ended && st.EndedAt != "" {
				if t, err := time.Parse(time.RFC3339Nano, st.EndedAt); err == nil && time.Since(t) > e.Host.Retention {
					swept("job", wt.Name()+"/"+n.Name(), "older than retention "+e.Host.Retention.String())
					if !dryRun {
						removeIfStillExpired(rec, e.Host.Retention)
					}
				}
			}
		}
	}
}

// rotate: history.jsonl over history_max becomes .1 (.1 -> .2 -> .3), keeping three.
func rotate(l state.Layout, e *config.Effective, dryRun bool, swept func(kind, id, why string)) {
	st, err := os.Stat(l.History())
	if err != nil || st.Size() < int64(e.Host.HistoryMax) {
		return
	}
	swept("history", l.History(), "over history_max "+strconv.FormatInt(int64(e.Host.HistoryMax), 10)+" bytes")
	if dryRun {
		return
	}
	f, err := lock.Open(filepath.Join(l.Root, "history.lock"))
	if err != nil {
		return
	}
	defer f.Close()
	if ok, _ := f.TryLock(); !ok {
		return
	}
	defer f.Unlock()
	for i := 3; i >= 1; i-- {
		src := l.History()
		if i > 1 {
			src = l.History() + "." + strconv.Itoa(i-1)
		}
		_ = os.Rename(src, l.History()+"."+strconv.Itoa(i))
	}
}

// beforeRetentionLock is a test seam between the unlocked read and the locked re-check.
var beforeRetentionLock func()

// removeIfStillExpired empties a record under its own lock after re-checking that the current
// generation is still ended and past retention (a concurrent Bump may have started a new one).
// record.lock is never deleted (spec 9): a concurrent Bump or Lock keeps contending on its inode.
func removeIfStillExpired(rec jobs.Record, retention time.Duration) {
	if beforeRetentionLock != nil {
		beforeRetentionLock()
	}
	unlock, err := rec.Lock()
	if err != nil {
		return
	}
	defer unlock()
	gen := rec.Gen()
	st, err := rec.ReadStatus(gen)
	if err != nil {
		return
	}
	var res map[string]any
	if rec.ReadResult(gen, &res) != nil || st.EndedAt == "" {
		return
	}
	t, err := time.Parse(time.RFC3339Nano, st.EndedAt)
	if err != nil || time.Since(t) <= retention {
		return
	}
	entries, err := os.ReadDir(rec.Dir)
	if err != nil {
		return
	}
	for _, en := range entries {
		if en.Name() == "record.lock" {
			continue
		}
		_ = os.RemoveAll(filepath.Join(rec.Dir, en.Name()))
	}
}

// StaleStaging is the age after which sweep removes an artifact staging dir (spec 3.3).
const StaleStaging = 6 * time.Hour

// sweepStaging removes <wt>/.bitc/staging/<job> entries older than StaleStaging.
func sweepStaging(e *config.Effective, l state.Layout, dryRun bool, swept func(kind, id, why string)) {
	for _, dir := range stagingDirs(e, l) {
		ents, _ := os.ReadDir(dir)
		for _, en := range ents {
			p := filepath.Join(dir, en.Name())
			info, err := os.Lstat(p)
			if err != nil || time.Since(info.ModTime()) < StaleStaging {
				continue
			}
			swept("staging", p, "older than 6h")
			if !dryRun {
				_ = os.RemoveAll(p) // a symlink entry is removed, never followed
			}
		}
	}
}

// stagingDirs: the .bitc/staging dir of the work tree sweep runs in, and the nearest one above
// each remote start event's cwd in history.jsonl.
func stagingDirs(e *config.Effective, l state.Layout) []string {
	var out []string
	seen := map[string]bool{}
	add := func(dir string) {
		for d := filepath.Clean(dir); ; d = filepath.Dir(d) {
			s := filepath.Join(d, ".bitc", "staging")
			if st, err := os.Lstat(s); err == nil && st.IsDir() {
				if !seen[s] {
					seen[s] = true
					out = append(out, s)
				}
				return
			}
			if d == filepath.Dir(d) {
				return
			}
		}
	}
	if e.Worktree != "" {
		add(e.Worktree)
	}
	b, _ := os.ReadFile(l.History())
	for _, line := range strings.Split(string(b), "\n") {
		var ev state.Event
		if json.Unmarshal([]byte(line), &ev) == nil && ev.Event == "start" && ev.Provider != "" &&
			ev.Provider != "local" && filepath.IsAbs(ev.Cwd) {
			add(ev.Cwd)
		}
	}
	return out
}
