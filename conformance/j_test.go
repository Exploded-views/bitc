package conformance

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/Exploded-views/bitc/conformance/harness"
)

var startedRE = regexp.MustCompile(`^started (\S+) pid (\d+) log (/\S+)\n$`)

func detached(t *testing.T, h *harness.H, args ...string) (name string, pid int, log string) {
	t.Helper()
	r := h.Run(append([]string{"run", "--detach"}, args...)...)
	m := startedRE.FindStringSubmatch(r.Stdout)
	if r.Code != 0 || m == nil {
		t.Fatalf("%+v", r)
	}
	pid, _ = strconv.Atoi(m[2])
	h.Track(pid)
	return m[1], pid, m[3]
}

func scaledHost(h *harness.H, body string) {
	h.Seam("time_scale = 0.01")
	h.HostConfig(body)
}

func TestJ1_StartedLineAndLogHoldsBothStreams(t *testing.T) {
	h := harness.New(t)
	scaledHost(h, "[pools.lint]\n")
	name, _, log := detached(t, h, "--name", "j1", "lint", "--", "sh", "-c", "echo out; echo err >&2")
	if name != "j1" || !filepath.IsAbs(log) {
		t.Fatal(name, log)
	}
	r := h.Run("wait", "j1")
	if r.Code != 0 || !strings.HasPrefix(r.Stdout, "RESULT exit 0\n") || !strings.Contains(r.Stdout, "out\nerr\n__EXIT__ 0\n") {
		t.Fatalf("%+v", r)
	}
	// only the flag before <pool> is dropped: the command's own --detach stays (spec 3.1)
	detached(t, h, "--name", "argv", "lint", "--", "sh", "-c", `printf '[%s]' "$@"; echo`, "_", "--detach", "x")
	if r := h.Run("wait", "argv"); !strings.Contains(r.Stdout, "\n[--detach][x]\n") {
		t.Fatalf("%+v", r)
	}
}

func TestJ2_ExitLineOnItsOwnLine(t *testing.T) {
	h := harness.New(t)
	scaledHost(h, "[pools.lint]\n")
	for code, script := range map[int]string{3: "printf partial; exit 3", 4: `printf 'cr\r'; exit 4`, 5: `printf 'nul\0'; exit 5`} {
		name, _, log := detached(t, h, "lint", "--", "sh", "-c", script)
		r := h.Run("wait", name)
		if !strings.HasPrefix(r.Stdout, "RESULT exit "+strconv.Itoa(code)+"\n") {
			t.Fatalf("%q: %+v", script, r)
		}
		if b := must(os.ReadFile(log)); !strings.HasSuffix(string(b), "\n__EXIT__ "+strconv.Itoa(code)+"\n") {
			t.Fatalf("%q", b)
		}
	}
}

func TestJ3_JobNames(t *testing.T) {
	h := harness.New(t)
	scaledHost(h, "[pools.lint]\n")
	for _, bad := range []string{"", "a/b", "../escaped", "a b", "a;b", "$(id)", "a*", "a:b", "a\nb", "a@b", "ä", "a,b", "a$b"} {
		r := h.Run("run", "--detach", "--name", bad, "lint", "--", "true")
		if r.Code != 64 || r.Stdout != "" {
			t.Errorf("%q: %+v", bad, r)
		}
	}
	if _, err := os.Stat(filepath.Join(h.State, "jobs")); err == nil {
		t.Fatal("bad names created a record")
	}
	detached(t, h, "--name", "Az09._-", "lint", "--", "true")
}

func TestJ4_LogExistsBeforeStartedAndUncreatableLogIs64(t *testing.T) {
	h := harness.New(t)
	scaledHost(h, "[pools.lint]\n")
	_, _, log := detached(t, h, "lint", "--", "sleep", "1")
	if _, err := os.Stat(log); err != nil {
		t.Fatal("log missing right after started")
	}
	notDir := filepath.Join(h.Root, "not-a-dir") // ENOTDIR under any uid, root included
	h.Write(notDir, "x", 0o600)
	r := h.Run("run", "--detach", "--log", filepath.Join(notDir, "x.log"), "lint", "--", "true")
	if r.Code != 64 || r.Stdout != "" {
		t.Fatalf("%+v", r)
	}
}

func TestJ5_RerunNeverReportsTheFirstJobsCode(t *testing.T) {
	h := harness.New(t)
	scaledHost(h, "[pools.lint]\nslots = 2\n")
	log := filepath.Join(h.Root, "j.log")
	detached(t, h, "--name", "r", "--log", log, "lint", "--", "sh", "-c", "sleep 1; echo first; exit 7")
	detached(t, h, "--name", "r", "--log", log, "lint", "--", "sh", "-c", "echo second; exit 0")
	r := h.Run("wait", "r")
	if !strings.HasPrefix(r.Stdout, "RESULT exit 0\n") || strings.Contains(r.Stdout, "first") {
		t.Fatalf("%+v", r)
	}
	time.Sleep(1500 * time.Millisecond)
	b := string(must(os.ReadFile(log)))
	if strings.Contains(b, "first") || strings.Count(b, "__EXIT__") != 1 {
		t.Fatalf("the superseded job wrote into the fresh log: %q", b)
	}
}

func TestJ6_OwnSessionSurvivesHangup(t *testing.T) {
	h := harness.New(t)
	scaledHost(h, "[pools.lint]\n")
	out := filepath.Join(h.Root, "started")
	// a caller shell starts the job, then hangs up its own process group and dies
	script := `"$1" run --detach --name hup lint -- sh -c 'sleep 0.5; echo survived' > "$2"; kill -HUP 0; sleep 5`
	sh := exec.Command("sh", "-c", script, "_", harness.Bin, out)
	sh.Env, sh.Dir = h.Env, h.Cwd
	sh.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	_ = sh.Run() // killed by its own SIGHUP
	m := startedRE.FindStringSubmatch(string(must(os.ReadFile(out))))
	if m == nil {
		t.Fatal("no started line")
	}
	pid, _ := strconv.Atoi(m[2])
	h.Track(pid)
	if sid, _ := unix.Getsid(pid); sid != pid {
		t.Fatalf("the job does not lead its own session (sid %d, pid %d)", sid, pid)
	}
	if r := h.Run("wait", "hup"); !strings.Contains(r.Stdout, "survived") {
		t.Fatalf("%+v", r)
	}
	// W:1380: a SIGHUP to the job's whole process group stops neither the job nor its command
	ready, gate := filepath.Join(h.Root, "ready"), filepath.Join(h.Root, "go")
	_, jpid, _ := detached(t, h, "--name", "hup2", "lint", "--", "sh", "-c",
		`: > "$1"; while [ ! -e "$2" ]; do sleep 0.05; done; echo survived2`, "_", ready, gate)
	h.Until(func() bool { _, err := os.Stat(ready); return err == nil }, 10*time.Second)
	if err := syscall.Kill(-jpid, syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(gate, nil, 0o644); err != nil { // the job outlives the HUP until now
		t.Fatal(err)
	}
	if r := h.Run("wait", "hup2", "--exit", "job"); r.Code != 0 || !strings.HasPrefix(r.Stdout, "RESULT exit 0\n") ||
		!strings.Contains(r.Stdout, "survived2\n") {
		t.Fatalf("a hangup to the job's group: %+v", r)
	}
}

func TestJ7_UsesTheCallersStateDirAbsolute(t *testing.T) {
	h := harness.New(t)
	scaledHost(h, "[pools.lint]\n")
	r := h.RunWith([]string{"BITC_DIR=relstate"}, "run", "--detach", "--name", "abs", "lint", "--", "sh", "-c", `echo "$BITC_DIR"`)
	if r.Code != 0 {
		t.Fatalf("%+v", r)
	}
	w := h.RunWith([]string{"BITC_DIR=relstate"}, "wait", "abs")
	if !strings.Contains(w.Stdout, filepath.Join(h.Cwd, "relstate")+"\n") {
		t.Fatalf("%+v", w)
	}
}

func TestJ8_WaitLogTakesTheLastExitLineAtLineStart(t *testing.T) {
	h := harness.New(t)
	scaledHost(h, "[pools.lint]\n")
	log := filepath.Join(h.Root, "legacy.log")
	h.Write(log, "retry\n__EXIT__ 75\nmid __EXIT__ 9\nnul\x00__EXIT__ 8\n__EXIT__ 0\n", 0o600)
	r := h.Run("wait", "--log", log)
	if r.Code != 0 || !strings.HasPrefix(r.Stdout, "RESULT exit 0\n") {
		t.Fatalf("%+v", r)
	}
	h.Write(log, "x\n__EXIT__ 75\n", 0o600)
	if r := h.Run("wait", "--log", log); r.Code != 0 || !strings.HasPrefix(r.Stdout, "RESULT starved\n") {
		t.Fatalf("legacy --log keeps J9: %+v", r)
	}
	// the log vanishes only after wait has read it once: 1 MiB read shows in /proc/<pid>/io
	h.Write(log, strings.Repeat("mid __EXIT__ 9\n", 1<<16), 0o600)
	w := h.Start("wait", "--log", log, "--timeout", "300s")
	h.Until(func() bool { return rchar(w.Cmd.Process.Pid) >= 15<<16 }, 10*time.Second)
	if err := os.Remove(log); err != nil {
		t.Fatal(err)
	}
	if r := w.Wait(); r.Code != 124 || r.Stderr != "" {
		t.Fatalf("a vanishing log: %+v", r)
	}
}

// rchar is the bytes pid has read so far (/proc/<pid>/io), 0 if unknown.
func rchar(pid int) int64 {
	b, _ := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/io")
	for _, l := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(l, "rchar: "); ok {
			n, _ := strconv.ParseInt(v, 10, 64)
			return n
		}
	}
	return 0
}

func TestJ9_WaitOutcomes(t *testing.T) {
	h := harness.New(t)
	scaledHost(h, "[pools.lint]\nwait = \"100s\"\n")
	ready := filepath.Join(h.Root, "ready")
	name, _, _ := detached(t, h, "lint", "--", "sh", "-c", `seq 1 100; : > "$1"; sleep 5`, "_", ready)
	h.Until(func() bool { _, err := os.Stat(ready); return err == nil }, 10*time.Second)
	r := h.Run("wait", name, "--timeout", "100s")
	if r.Code != 124 || !strings.HasPrefix(r.Stdout, "RUNNING (no exit line yet after 1s); last lines:\n96\n97\n98\n99\n100\n") {
		t.Fatalf("%+v", r)
	}
	r = h.Run("wait", name, "--timeout", "1000s")
	lines := strings.Split(strings.TrimSuffix(r.Stdout, "\n"), "\n")
	if r.Code != 0 || lines[0] != "RESULT exit 0" || !strings.HasPrefix(lines[1], "--- last 60 lines of ") || len(lines) != 62 {
		t.Fatalf("%d lines: %+v", len(lines), r)
	}
	if r := h.Run("wait", "nope"); r.Code != 66 || !strings.HasPrefix(r.Stdout, "NO LOG ") {
		t.Fatalf("%+v", r)
	}
	if r := h.Run("wait"); r.Code != 64 {
		t.Fatalf("%+v", r)
	}
	if out := h.Run("config", "explain", "detach.wait_timeout").Stdout; !strings.Contains(out, "9m") {
		t.Fatal(out)
	}
	hold := h.Hold(nil, "--wait", "0s", "lint")
	s, _, _ := detached(t, h, "--wait", "0s", "lint", "--", "true")
	if r := h.Run("wait", s); r.Code != 75 || !strings.HasPrefix(r.Stdout, "RESULT starved\n") {
		t.Fatalf("starved: %+v", r)
	}
	hold.Release()
	own, _, _ := detached(t, h, "lint", "--", "sh", "-c", "exit 75")
	if r := h.Run("wait", own); r.Code != 0 || !strings.HasPrefix(r.Stdout, "RESULT exit 75\n") {
		t.Fatalf("a command's own 75: %+v", r)
	}
}

// B:1085: a job detached inside a heavy hold waits for the shared group unit and never overlaps
// the outer heavy or the next one.
func TestM8_DetachedJobInsideAHoldTakesItsOwnUnits(t *testing.T) {
	h := harness.New(t)
	scaledHost(h, "groups = { bigmem = { units = 1 } }\n[pools.heavy]\ngroups = { bigmem = 1 }\nwait = \"1000s\"\n[pools.coverage]\ngroups = { bigmem = 1 }\nwait = \"1000s\"\n")
	seq, step := filepath.Join(h.Root, "seq"), filepath.Join(h.Root, "step.sh")
	h.Write(step, "#!/bin/sh\necho \"start $1\" >> \"$2\"\nsleep 0.3\necho \"end $1\" >> \"$2\"\n", 0o700)
	r := h.Run("run", "heavy", "--", "sh", "-c",
		`"$1" run --detach --name inner coverage -- "$2" coverage "$3" > /dev/null && "$2" heavy1 "$3"`, "_", harness.Bin, step, seq)
	if r.Code != 0 {
		t.Fatalf("%+v", r)
	}
	if r := h.Run("run", "heavy", "--", step, "heavy2", seq); r.Code != 0 {
		t.Fatalf("%+v", r)
	}
	if w := h.Run("wait", "inner", "--timeout", "1000s"); !strings.HasPrefix(w.Stdout, "RESULT exit 0\n") {
		t.Fatalf("%+v", w)
	}
	lines := strings.Split(strings.TrimSpace(string(must(os.ReadFile(seq)))), "\n")
	if len(lines) != 6 || lines[0] != "start heavy1" || lines[1] != "end heavy1" {
		t.Fatalf("coverage did not wait for the outer heavy: %q", lines)
	}
	for i := 0; i < len(lines); i += 2 { // strictly start X, end X: no overlap
		if n := strings.TrimPrefix(lines[i], "start "); lines[i] == n || lines[i+1] != "end "+n {
			t.Fatalf("overlap: %q", lines)
		}
	}
}
