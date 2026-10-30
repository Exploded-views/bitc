package conformance

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/Exploded-views/bitc/conformance/harness"
	"github.com/Exploded-views/bitc/internal/proc"
)

// queuedRecord runs a job that touches marker to its end, then rewrites its status.json to
// "queued" with the given supervisor pid, so the record looks like a supervisor still starting up.
func queuedRecord(t *testing.T, h *harness.H, marker string, pid int, extra map[string]any) (job, dir string, gen int) {
	t.Helper()
	name, _, _ := detached(t, h, "lint", "--", "touch", marker)
	if r := h.Run("wait", name); r.Code != 0 {
		t.Fatalf("%+v", r)
	}
	dirs, _ := filepath.Glob(filepath.Join(h.State, "jobs", "*", name))
	if len(dirs) != 1 {
		t.Fatalf("job dir: %v", dirs)
	}
	dir = dirs[0]
	gen = h.Atoi(strings.TrimSpace(string(must(os.ReadFile(filepath.Join(dir, "gen"))))))
	var st map[string]any
	if err := json.Unmarshal(must(os.ReadFile(filepath.Join(dir, "status.json"))), &st); err != nil {
		t.Fatal(err)
	}
	st["state"], st["pid"], st["start"] = "queued", pid, 0
	delete(st, "ended_at")
	if pid != 0 {
		ps, err := proc.ReadStat(pid)
		if err != nil {
			t.Fatal(err)
		}
		st["start"] = ps.Start
	}
	for k, v := range extra {
		st[k] = v
	}
	b, _ := json.Marshal(st)
	if err := os.WriteFile(filepath.Join(dir, "status.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(filepath.Join(dir, "result.json"))
	return name, dir, gen
}

func statusMap(t *testing.T, dir string) map[string]any {
	t.Helper()
	var st map[string]any
	if err := json.Unmarshal(must(os.ReadFile(filepath.Join(dir, "status.json"))), &st); err != nil {
		t.Fatal(err)
	}
	return st
}

// A cancel of a queued job never signals its supervisor, which may have no handlers yet (the
// default action would kill it, and the job would be lost): it leaves the request in status.json.
func TestDJ12_CancelOfAQueuedJobSignalsNothing(t *testing.T) {
	h := harness.New(t)
	scaledHost(h, "[pools.lint]\n")
	child := exec.Command("sleep", "30") // stands in for a supervisor with no signal handlers
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	pid := child.Process.Pid
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL); _ = child.Wait() })
	name, dir, _ := queuedRecord(t, h, filepath.Join(h.Root, "marker"), pid, nil)
	r := h.Run("cancel", name)
	if r.Code != 0 || !strings.HasPrefix(r.Stdout, "cancelled ") {
		t.Fatalf("%+v", r)
	}
	if ps, err := proc.ReadStat(pid); err != nil || ps.State == 'Z' {
		t.Fatalf("cancel signalled the queued supervisor (%v)", err)
	}
	if st := statusMap(t, dir); st["cancel_requested"] != "TERM" || st["state"] != "queued" {
		t.Fatalf("status.json: %v", st)
	}
	if r := h.Run("cancel", name, "--kill"); r.Code != 0 || !strings.HasPrefix(r.Stdout, "cancelled ") {
		t.Fatalf("%+v", r)
	}
	if st := statusMap(t, dir); st["cancel_requested"] != "KILL" {
		t.Fatalf("--kill: status.json: %v", st)
	}
}

// A supervisor that finds a cancel request when it flips to running acts as if the signal had
// arrived before the command: interrupted, 128+N, and the command never runs.
func TestDJ13_SupervisorHonoursAStartupCancelRequest(t *testing.T) {
	for _, req := range []string{"TERM", "KILL"} {
		h := harness.New(t)
		scaledHost(h, "[pools.lint]\n")
		marker := filepath.Join(h.Root, "marker")
		name, dir, gen := queuedRecord(t, h, marker, 0, map[string]any{"cancel_requested": req})
		if err := os.Remove(marker); err != nil {
			t.Fatal(err)
		}
		wt8 := filepath.Base(filepath.Dir(dir))
		r := h.Run("__supervise", wt8+"/"+name, strconv.Itoa(gen))
		if r.Code != 143 {
			t.Fatalf("%s: %+v", req, r)
		}
		if _, err := os.Stat(marker); err == nil {
			t.Fatalf("%s: the command ran after a start-up cancel", req)
		}
		if res := string(must(os.ReadFile(filepath.Join(dir, "result.json")))); !strings.Contains(res, `"interrupted"`) || !strings.Contains(res, "143") {
			t.Fatalf("%s: result.json: %s", req, res)
		}
	}
}

// With no pid recorded yet (detach has not written it) the cancel line names no pid.
func TestDJ14_CancelOfAQueuedJobWithNoPidNamesNone(t *testing.T) {
	h := harness.New(t)
	scaledHost(h, "[pools.lint]\n")
	name, dir, _ := queuedRecord(t, h, filepath.Join(h.Root, "marker"), 0, nil)
	if r := h.Run("cancel", name); r.Code != 0 || r.Stdout != "cancelled "+name+"\n" {
		t.Fatalf("%+v", r)
	}
	if st := statusMap(t, dir); st["cancel_requested"] != "TERM" {
		t.Fatalf("status.json: %v", st)
	}
}

// A status.json cancel cannot read or lock is an error (69), never "already ended" with 0.
func TestDJ15_CancelWithAnUnreadableStatusIsAnError(t *testing.T) {
	h := harness.New(t)
	scaledHost(h, "[pools.lint]\n")
	name, dir, _ := queuedRecord(t, h, filepath.Join(h.Root, "marker"), 0, nil)
	if err := os.WriteFile(filepath.Join(dir, "status.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if r := h.Run("cancel", name); r.Code != 69 || strings.Contains(r.Stdout, "already ended") || r.Stderr == "" {
		t.Fatalf("%+v", r)
	}
}

// A later plain cancel keeps a KILL request.
func TestDJ16_ACancelNeverDowngradesAKillRequest(t *testing.T) {
	h := harness.New(t)
	scaledHost(h, "[pools.lint]\n")
	child := exec.Command("sleep", "30") // stands in for a supervisor with no signal handlers
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	pid := child.Process.Pid
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL); _ = child.Wait() })
	name, dir, _ := queuedRecord(t, h, filepath.Join(h.Root, "marker"), pid, map[string]any{"cancel_requested": "KILL"})
	if r := h.Run("cancel", name); r.Code != 0 {
		t.Fatalf("%+v", r)
	}
	if st := statusMap(t, dir); st["cancel_requested"] != "KILL" {
		t.Fatalf("status.json: %v", st)
	}
}

// A queued job whose supervisor never started (detach's cmd.Start failed) has ended.
func TestDJ17_CancelOfAJobThatNeverStartedReportsItEnded(t *testing.T) {
	h := harness.New(t)
	scaledHost(h, "[pools.lint]\n")
	name, dir, _ := queuedRecord(t, h, filepath.Join(h.Root, "marker"), 0, map[string]any{"started_at": "2020-01-01T00:00:00Z"})
	if r := h.Run("cancel", name); r.Code != 0 || r.Stdout != name+" already ended\n" {
		t.Fatalf("%+v", r)
	}
	if st := statusMap(t, dir); st["cancel_requested"] != nil {
		t.Fatalf("status.json: %v", st)
	}
}

// Cancel never creates a status.json: a job with none has not started.
func TestDJ18_CancelDoesNotCreateAStatusFile(t *testing.T) {
	h := harness.New(t)
	scaledHost(h, "[pools.lint]\n")
	name, dir, _ := queuedRecord(t, h, filepath.Join(h.Root, "marker"), 0, nil)
	if err := os.Remove(filepath.Join(dir, "status.json")); err != nil {
		t.Fatal(err)
	}
	if r := h.Run("cancel", name); r.Code != 0 || r.Stdout != name+" already ended\n" {
		t.Fatalf("%+v", r)
	}
	if _, err := os.Stat(filepath.Join(dir, "status.json")); err == nil {
		t.Fatal("cancel created status.json")
	}
}

// A supervisor that cannot mark its job running (here: a corrupt status.json) ends 69 and runs
// nothing: otherwise no cancel could ever reach the job.
func TestDJ19_SupervisorThatCannotMarkItsJobRunningEnds69(t *testing.T) {
	h := harness.New(t)
	scaledHost(h, "[pools.lint]\n")
	marker := filepath.Join(h.Root, "marker")
	name, dir, gen := queuedRecord(t, h, marker, 0, nil)
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "status.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := h.Run("__supervise", filepath.Base(filepath.Dir(dir))+"/"+name, strconv.Itoa(gen))
	if r.Code != 69 || !strings.Contains(r.Stderr, "cannot mark the job running") {
		t.Fatalf("%+v", r)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the command ran")
	}
	if res := string(must(os.ReadFile(filepath.Join(dir, "result.json")))); !strings.Contains(res, `"internal"`) || !strings.Contains(res, "69") {
		t.Fatalf("result.json: %s", res)
	}
}
