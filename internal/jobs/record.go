// Package jobs is bitc's detached-job record (spec 7).
package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Exploded-views/bitc/internal/config"
	"github.com/Exploded-views/bitc/internal/exit"
	"github.com/Exploded-views/bitc/internal/lock"
	"github.com/Exploded-views/bitc/internal/state"
)

var nameRE = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// Record is jobs/<wt8>/<name>/.
type Record struct {
	Dir, WT8, Name string
	layout         state.Layout
}

// Spec is spec.json: what the supervisor replays. The caller's env is passed through the
// supervisor's process environment, never written to disk.
type Spec struct {
	Gen    int          `json:"gen"`
	Name   string       `json:"name"`
	WT8    string       `json:"wt8"`
	Log    string       `json:"log"`
	Args   []string     `json:"args"` // run args without --detach
	Cwd    string       `json:"cwd"`
	Flags  config.Flags `json:"flags"`
	Result string       `json:"result,omitempty"` // --result PATH
}

// Status is status.json.
type Status struct {
	State     string `json:"state"` // queued | running | ended
	Gen       int    `json:"gen"`
	Pid       int    `json:"pid"`
	Start     uint64 `json:"start"`
	Log       string `json:"log"`
	StartedAt string `json:"started_at"`
	EndedAt   string `json:"ended_at,omitempty"`
	// CancelRequested is a cancel that arrived while the job was queued ("TERM" or "KILL"): the
	// supervisor reads it when it flips to running, under the record lock.
	CancelRequested string `json:"cancel_requested,omitempty"`
}

func Open(l state.Layout, wt8, name string) Record {
	return Record{Dir: l.JobDir(wt8, name), WT8: wt8, Name: name, layout: l}
}

// Resolve finds <name> in the cwd's work tree, or <wt8>/<name>; 66 when absent.
func Resolve(l state.Layout, wt8, job string) (Record, error) {
	if a, b, ok := strings.Cut(job, "/"); ok {
		wt8, job = a, b
	}
	if !nameRE.MatchString(job) || !nameRE.MatchString(wt8) {
		return Record{}, exit.Usagef("invalid job %q", job)
	}
	r := Open(l, wt8, job)
	if _, err := os.Stat(filepath.Join(r.Dir, "gen")); err != nil {
		return Record{}, exit.NoInputf("no job %s in this work tree (%s)", job, wt8)
	}
	return r, nil
}

// Lock takes the record flock (bounded).
func (r Record) Lock() (func(), error) {
	f, err := lock.Open(filepath.Join(r.Dir, "record.lock"))
	if err != nil {
		return nil, err
	}
	if err := f.LockWait(context.Background(), 10*time.Second); err != nil {
		f.Close()
		return nil, err
	}
	return func() { _ = f.Unlock(); f.Close() }, nil
}

// Gen is the current generation (0 if none).
func (r Record) Gen() int {
	b, err := os.ReadFile(filepath.Join(r.Dir, "gen"))
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return n
}

// Bump starts a new generation under the record lock, moving the old one to prev-<gen>/.
func (r Record) Bump() (int, error) {
	if err := os.MkdirAll(r.Dir, 0o700); err != nil {
		return 0, err
	}
	unlock, err := r.Lock()
	if err != nil {
		return 0, err
	}
	defer unlock()
	old := r.Gen()
	if old > 0 {
		prev := filepath.Join(r.Dir, "prev-"+strconv.Itoa(old))
		_ = os.MkdirAll(prev, 0o700)
		for _, f := range []string{"spec.json", "status.json", "result.json", "out.log"} {
			_ = os.Rename(filepath.Join(r.Dir, f), filepath.Join(prev, f))
		}
	}
	if err := state.WriteFileAtomic(filepath.Join(r.Dir, "gen"), []byte(strconv.Itoa(old+1)+"\n")); err != nil {
		return 0, err
	}
	return old + 1, nil
}

// LogFor is where generation gen's log is now: a default log (<record>/out.log) moves to
// prev-<gen>/ when a rerun supersedes the generation, so a reader bound to gen follows it.
func (r Record) LogFor(gen int, specLog string) string {
	if specLog == filepath.Join(r.Dir, "out.log") {
		return filepath.Join(r.genDir(gen), "out.log")
	}
	return specLog
}

// writeFor returns the directory generation gen's files live in, under the record lock: the
// record itself while gen is current, prev-<gen>/ once superseded (Bump moved them there). A
// superseded supervisor writes its own generation, never the new one (spec 7); ok is false when
// that directory is gone.
func (r Record) writeFor(gen int) (dir string, ok bool) {
	if gen == r.Gen() {
		return r.Dir, true
	}
	d := filepath.Join(r.Dir, "prev-"+strconv.Itoa(gen))
	st, err := os.Stat(d)
	return d, err == nil && st.IsDir()
}

// writeIfCurrent writes f into generation gen's own directory under the record lock.
func (r Record) writeIfCurrent(gen int, f string, v any) error {
	unlock, err := r.Lock()
	if err != nil {
		return err
	}
	defer unlock()
	dir, ok := r.writeFor(gen)
	if !ok {
		return nil
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return state.WriteFileAtomic(filepath.Join(dir, f), append(b, '\n'))
}

func (r Record) WriteSpec(s Spec) error     { return r.writeIfCurrent(s.Gen, "spec.json", s) }
func (r Record) WriteStatus(s Status) error { return r.writeIfCurrent(s.Gen, "status.json", s) }

// UpdateStatus applies f to gen's status.json (zero if absent) under the record lock, so detach
// and the supervisor never drop each other's writes; a superseded gen updates its prev-<gen>/.
func (r Record) UpdateStatus(gen int, f func(*Status)) error {
	unlock, err := r.Lock()
	if err != nil {
		return err
	}
	defer unlock()
	dir, ok := r.writeFor(gen)
	if !ok {
		return nil
	}
	var s Status
	if err := readJSON(filepath.Join(dir, "status.json"), &s); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	f(&s)
	s.Gen = gen
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return state.WriteFileAtomic(filepath.Join(dir, "status.json"), append(b, '\n'))
}
func (r Record) WriteResult(gen int, v any) error {
	return r.writeIfCurrent(gen, "result.json", v)
}

func readJSON(p string, v any) error {
	b, err := os.ReadFile(p)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// ReadSpec, ReadStatus and ReadResult read the files of generation gen (current or prev-<gen>).
func (r Record) genDir(gen int) string {
	if gen == r.Gen() {
		return r.Dir
	}
	return filepath.Join(r.Dir, "prev-"+strconv.Itoa(gen))
}
func (r Record) ReadSpec(gen int) (Spec, error) {
	var s Spec
	return s, readJSON(filepath.Join(r.genDir(gen), "spec.json"), &s)
}
func (r Record) ReadStatus(gen int) (Status, error) {
	var s Status
	return s, readJSON(filepath.Join(r.genDir(gen), "status.json"), &s)
}

// ReadResult returns result.json of gen; os.ErrNotExist while the job runs.
func (r Record) ReadResult(gen int, v any) error {
	err := readJSON(filepath.Join(r.genDir(gen), "result.json"), v)
	if errors.Is(err, fs.ErrNotExist) {
		return os.ErrNotExist
	}
	return err
}
