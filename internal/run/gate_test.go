package run

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/Exploded-views/bitc/internal/execgate"
	"github.com/Exploded-views/bitc/internal/proc"
)

// -bypass-gate starts every command directly, as before the gate (bypassGate): the gate tests
// must fail with it, which shows they fail without the fix.
func init() {
	flag.BoolVar(&bypassGate, "bypass-gate", false, "start commands without the exec gate (the gate tests must then fail)")
}

// hold is the FIFO a test gate blocks on (execgate.HoldEnv). The test keeps its own read-write
// descriptor on it, so the gate's open never waits for a writer and never sees EOF; one byte
// written by release lets the gate through its stage.
type hold struct {
	path string
	f    *os.File
}

func newHold(t *testing.T) *hold {
	t.Helper()
	p := filepath.Join(t.TempDir(), "hold")
	if err := unix.Mkfifo(p, 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(p, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return &hold{path: p, f: f}
}

// env is the environment that makes the gate wait at stage.
func (h *hold) env(stage string) []string {
	return append(os.Environ(), execgate.HoldEnv+"="+h.path, execgate.HoldAtEnv+"="+stage)
}

func (h *hold) release() { _, _ = h.f.Write([]byte{1}) }

// groupSurvivors lists the live (not zombie) processes of process group pgid that started at or
// after start, the incarnation the pgid was recorded for.
func groupSurvivors(pgid int, start uint64) []int {
	var out []int
	for _, p := range proc.Pids() {
		st, err := proc.ReadStat(p)
		if err == nil && st.Pgrp == pgid && st.Start >= start && st.State != 'Z' {
			out = append(out, p)
		}
	}
	return out
}

// stragglers lists the live processes of group pgid (started at or after start) that are not the
// exec gate, once every gate left in the group has exited. A gate has no business outliving its
// supervisor, but it may still be on its way out: it is waited for (a pidfd on Linux, a kqueue on
// darwin), never slept on.
func stragglers(t *testing.T, pgid int, start uint64) []int {
	t.Helper()
	for {
		var gates, others []int
		for _, p := range groupSurvivors(pgid, start) {
			if isGate(p) {
				gates = append(gates, p)
				continue
			}
			// Not the gate: a process that exited since the scan reads as not the gate too.
			if st, err := proc.ReadStat(p); err == nil && st.Pgrp == pgid && st.State != 'Z' {
				others = append(others, p)
			}
		}
		if len(gates) == 0 {
			return others
		}
		for _, g := range gates {
			if err := waitExit(g); err != nil {
				t.Fatalf("waiting for the gate %d to exit: %v", g, err)
			}
		}
	}
}

// T1: nothing of the command runs before it is recorded: when Started runs, the started process
// is still the gate, not the command. After Exec returns the command has been reaped, so the
// marker is read once.
func TestExecRunsNothingBeforeTheCommandIsRecorded(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	s := spec(t, "sh", "-c", `echo x > "$1"`, "sh", marker)
	var started, atGate, early bool
	s.Started = func(pid, pgid int, start uint64) error {
		started = true
		atGate = isGate(pid)
		_, err := os.Stat(marker)
		early = err == nil
		return nil
	}
	r, err := Exec(context.Background(), s)
	if err != nil || r.Code != 0 {
		t.Fatalf("%+v %v", r, err)
	}
	if !started {
		t.Fatal("Started was not called")
	}
	if !atGate {
		t.Error("the started process was already the command when it was recorded")
	}
	if early {
		t.Error("the command ran before it was recorded")
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("the command never ran: %v", err)
	}
}

// T6: a signal sent to the command's group before the command is recorded and released ends the
// run with that signal, and the command never runs.
func TestSignalToTheGroupBeforeReleaseEndsTheRun(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	s := spec(t, "sh", "-c", `echo x > "$1"`, "sh", marker)
	var atGate, early bool
	s.Started = func(pid, pgid int, start uint64) error {
		atGate = isGate(pid)
		_, err := os.Stat(marker)
		early = err == nil
		if pgid <= 0 { // never kill(-0): fail closed, which kills the gate by its pid
			t.Errorf("pgid %d: the run has no group of its own", pgid)
			return errors.New("no group")
		}
		_ = syscall.Kill(-pgid, syscall.SIGTERM)
		return nil
	}
	r, err := Exec(context.Background(), s)
	if err != nil || r.Code != 143 {
		t.Errorf("%+v %v, want code 143", r, err)
	}
	if !atGate {
		t.Error("the started process was already the command when its group was signalled")
	}
	if early {
		t.Error("the command ran before it was recorded")
	}
	if _, err := os.Stat(marker); err == nil { // Exec has reaped the group's leader: final
		t.Error("the command ran after its group was signalled")
	}
}

// fifoUnwritten reports whether nothing was ever written to the FIFO at path and still waits to be
// read: a non-blocking open for reading, then one read that gives 0 bytes (no writer) or EAGAIN
// (a writer, no data).
func fifoUnwritten(path string) (bool, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return false, err
	}
	defer unix.Close(fd)
	var b [1]byte
	n, err := unix.Read(fd, b[:])
	if errors.Is(err, unix.EAGAIN) {
		return true, nil
	}
	return n == 0 && err == nil, err
}

// T2: a supervisor that dies after the command started but before it recorded it leaves nothing
// of the command running. A helper process (this test binary) is the supervisor and kills itself
// in the beforeRecord seam. With the gate it does so at once, the started process being the gate.
// Without it the started process is sh, and the helper waits until sh has forked its background
// sleep: sh signals that by opening the FIFO, which the helper reads. That is a handshake, not a
// timer.
func TestAKilledSupervisorLeavesNoCommandRunning(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "ran")
	if err := unix.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	kids := filepath.Join(dir, "kids")
	out, err := os.Create(filepath.Join(dir, "helper.out"))
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	args := []string{"-test.run=^TestHelperKilledSupervisor$"}
	if bypassGate {
		args = append(args, "-bypass-gate")
	}
	h := exec.Command(os.Args[0], args...)
	h.Env = append(os.Environ(), "BITC_GATE_HELPER=killed-supervisor", "BITC_GATE_FIFO="+fifo, "BITC_GATE_KIDS="+kids)
	h.Stdout, h.Stderr = out, out // files, so Wait does not wait for the command's children
	err = h.Run()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
		b, _ := os.ReadFile(out.Name())
		t.Fatalf("the helper did not kill itself: %v\n%s", err, b)
	}
	b, _ := os.ReadFile(kids)
	var pid, pgid int
	var start uint64
	if n, _ := fmt.Sscanf(string(b), "%d %d %d", &pid, &pgid, &start); n != 3 || pid <= 0 || pgid <= 0 {
		t.Fatalf("the helper recorded no started process: %q", b)
	}
	left := stragglers(t, pgid, start)
	defer func() {
		for _, p := range left {
			if p > 0 {
				_ = syscall.Kill(p, syscall.SIGKILL) // exact pids, never the group
			}
		}
	}()
	if len(left) != 0 {
		t.Errorf("processes of group %d outlived the killed supervisor: %v", pgid, left)
	}
	if unwritten, err := fifoUnwritten(fifo); err != nil || !unwritten {
		t.Errorf("the command wrote to the FIFO although its supervisor died before recording it (%v)", err)
	}
}

// TestHelperKilledSupervisor is the supervisor process of T2; it does nothing in a normal run.
func TestHelperKilledSupervisor(t *testing.T) {
	if os.Getenv("BITC_GATE_HELPER") != "killed-supervisor" {
		return
	}
	fifo, kids := os.Getenv("BITC_GATE_FIFO"), os.Getenv("BITC_GATE_KIDS")
	beforeRecord = func(pid int) {
		if st, err := proc.ReadStat(pid); err == nil {
			_ = os.WriteFile(kids, []byte(fmt.Sprintf("%d %d %d\n", st.Pid, st.Pgrp, st.Start)), 0o600)
		}
		if !isGate(pid) { // the command itself: let it fork its background sleep first
			if f, err := os.Open(fifo); err == nil { // blocks until sh opens the FIFO for writing
				var b [1]byte
				_, _ = f.Read(b[:])
			}
		}
		_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
		select {} // not reached: the kill ends this process
	}
	s := spec(t, "sh", "-c", `sleep 3600 & echo x > "$1"; wait`, "sh", fifo)
	s.Started = func(pid, pgid int, start uint64) error { return nil }
	_, _ = Exec(context.Background(), s)
	t.Fatal("the helper outlived its own SIGKILL")
}

// T3: start errors keep today's codes and texts.
func TestStartErrAnswersAreUnchanged(t *testing.T) {
	dir := t.TempDir()
	noexec := filepath.Join(dir, "noexec")
	if err := os.WriteFile(noexec, []byte("#!/bin/sh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	garbage := filepath.Join(dir, "garbage")
	if err := os.WriteFile(garbage, []byte("this is not a program\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		argv []string
		dir  string
		code int
	}{
		{"bare name not found", []string{"bitc-no-such-command-xyz"}, "", 127},
		{"path not found", []string{filepath.Join(dir, "missing", "cmd")}, "", 127},
		{"no x bit", []string{noexec}, "", 126},
		{"not an executable format", []string{garbage}, "", 126},
		{"missing dir", []string{"sh", "-c", "true"}, filepath.Join(dir, "missing"), 127},
	}
	for _, c := range cases {
		s := spec(t, c.argv...)
		if c.dir != "" {
			s.Dir = c.dir
		}
		d := exec.Command(c.argv[0], c.argv[1:]...)
		d.Env, d.Dir, d.SysProcAttr = s.Env, s.Dir, childAttr(true)
		derr := d.Start()
		if derr == nil {
			_ = d.Process.Kill()
			_ = d.Wait()
			t.Fatalf("%s: a direct start succeeded", c.name)
		}
		r, err := Exec(context.Background(), s)
		if err != nil || r.Code != c.code || r.StartErr == nil {
			t.Errorf("%s: %+v %v, want code %d", c.name, r, err, c.code)
			continue
		}
		if r.StartErr.Error() != derr.Error() {
			t.Errorf("%s: StartErr %q, a direct start says %q", c.name, r.StartErr, derr)
		}
	}
}

func sortedFdList(t *testing.T, b []byte) []string {
	t.Helper()
	f := strings.Fields(string(b))
	sort.Strings(f)
	if len(f) < 3 {
		t.Fatalf("no fd list: %q", b)
	}
	return f
}

// T4a: the command's open files are exactly those of the same command started directly.
func TestNoExtraFdReachesTheCommand(t *testing.T) {
	dir := t.TempDir()
	viaFile, directFile := filepath.Join(dir, "via"), filepath.Join(dir, "direct")
	via, err := os.Create(viaFile)
	if err != nil {
		t.Fatal(err)
	}
	defer via.Close()
	direct, err := os.Create(directFile)
	if err != nil {
		t.Fatal(err)
	}
	defer direct.Close()
	s := spec(t, "sh", "-c", "ls /dev/fd")
	s.Stdout = via
	if r, err := Exec(context.Background(), s); err != nil || r.Code != 0 {
		t.Fatalf("%+v %v", r, err)
	}
	d := exec.Command("sh", "-c", "ls /dev/fd")
	d.Env, d.Dir, d.Stdin, d.Stdout, d.Stderr = s.Env, s.Dir, s.Stdin, direct, os.Stderr
	if err := d.Run(); err != nil {
		t.Fatal(err)
	}
	a, _ := os.ReadFile(viaFile)
	b, _ := os.ReadFile(directFile)
	got, want := sortedFdList(t, a), sortedFdList(t, b)
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("the command has fds %v, started directly it has %v", got, want)
	}
}

// T4b: a non-CLOEXEC fd the supervisor inherited reaches the command at the same number. A
// helper process (this test binary) is given a file as its fd 3.
func TestInheritedFdIsKept(t *testing.T) {
	f, err := os.Create(filepath.Join(t.TempDir(), "fd3"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	h := exec.Command(os.Args[0], "-test.run=^TestHelperInheritedFd$")
	h.Env = append(os.Environ(), "BITC_GATE_HELPER=inherited-fd")
	h.ExtraFiles = []*os.File{f}
	if out, err := h.CombinedOutput(); err != nil {
		t.Fatalf("the helper failed: %v\n%s", err, out)
	}
	b, _ := os.ReadFile(f.Name())
	if strings.TrimSpace(string(b)) != "hi" {
		t.Errorf("the command did not write to the inherited fd 3: %q", b)
	}
}

// TestHelperInheritedFd runs a command that writes to fd 3; it does nothing in a normal run.
func TestHelperInheritedFd(t *testing.T) {
	if os.Getenv("BITC_GATE_HELPER") != "inherited-fd" {
		return
	}
	r, err := Exec(context.Background(), spec(t, "sh", "-c", "echo hi >&3"))
	if err != nil || r.Code != 0 {
		t.Fatalf("%+v %v", r, err)
	}
}

// T4c: inherited fds above a hole in the supervisor's own fd numbers reach the command at the same
// numbers, and starting the gate does not hang. The helper has fds 3 up to the inherited one closed
// except those the runtime and the test open, which is the layout where Go's child-side fd shuffle
// could meet a copy bitc made.
func TestInheritedFdAboveAHoleIsKept(t *testing.T) {
	cases := []struct {
		name  string
		words map[int]string // inherited fd -> the word the command writes to it
	}{
		{"fd 9", map[int]string{9: "hi"}},
		{"fd 20", map[int]string{20: "hi"}},
		{"fds 9 and 20", map[int]string{9: "nine", 20: "twenty"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			top := 0
			files := map[int]*os.File{}
			var spec []string
			for fd, w := range c.words {
				f, err := os.Create(filepath.Join(t.TempDir(), "fd"+strconv.Itoa(fd)))
				if err != nil {
					t.Fatal(err)
				}
				defer f.Close()
				files[fd] = f
				top = max(top, fd)
				spec = append(spec, strconv.Itoa(fd)+":"+w)
			}
			extra := make([]*os.File, top-2) // slot i is descriptor 3+i; the rest stay nil (closed)
			for fd, f := range files {
				extra[fd-3] = f
			}
			out, err := os.Create(filepath.Join(t.TempDir(), "helper.out"))
			if err != nil {
				t.Fatal(err)
			}
			defer out.Close()
			h := exec.Command(os.Args[0], "-test.run=^TestHelperInheritedFdMulti$")
			h.Env = append(os.Environ(), "BITC_GATE_HELPER=inherited-fd-multi", "BITC_GATE_FDS="+strings.Join(spec, ","))
			h.ExtraFiles = extra
			h.Stdout, h.Stderr = out, out
			if err := h.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if h.ProcessState == nil { // still running: by its exact pid
					_ = h.Process.Kill()
				}
			})
			// A gate that hangs on these descriptors hangs here: go test's -timeout reports it.
			err = h.Wait()
			if err != nil {
				b, _ := os.ReadFile(out.Name())
				t.Fatalf("the helper failed: %v\n%s", err, b)
			}
			for fd, w := range c.words {
				b, _ := os.ReadFile(files[fd].Name())
				if string(b) != w+"\n" {
					t.Errorf("fd %d holds %q, want %q", fd, b, w+"\n")
				}
			}
		})
	}
}

// TestHelperInheritedFdMulti runs a command that writes a word to each inherited fd; it does
// nothing in a normal run.
func TestHelperInheritedFdMulti(t *testing.T) {
	if os.Getenv("BITC_GATE_HELPER") != "inherited-fd-multi" {
		return
	}
	var script strings.Builder
	for _, e := range strings.Split(os.Getenv("BITC_GATE_FDS"), ",") {
		fd, word, _ := strings.Cut(e, ":")
		if n, _ := strconv.Atoi(fd); n > 9 { // dash cannot redirect to a two-digit fd
			fmt.Fprintf(&script, "echo %s > /dev/fd/%s; ", word, fd)
		} else {
			fmt.Fprintf(&script, "echo %s >&%s; ", word, fd)
		}
	}
	r, err := Exec(context.Background(), spec(t, "sh", "-c", script.String()))
	if err != nil || r.Code != 0 {
		t.Fatalf("%+v %v", r, err)
	}
	for _, e := range strings.Split(os.Getenv("BITC_GATE_FDS"), ",") { // bitc's own fds stay open
		fd, _, _ := strings.Cut(e, ":")
		n, _ := strconv.Atoi(fd)
		if _, err := unix.FcntlInt(uintptr(n), unix.F_GETFD, 0); err != nil {
			t.Errorf("inherited fd %d is closed after Exec: %v", n, err)
		}
	}
}

// T5: the recorded process is the command, and its limits are those of a direct start.
func TestRecordedLeaderIsTheCommand(t *testing.T) {
	dir := t.TempDir()
	pidFile, limFile, directLimFile := filepath.Join(dir, "pid"), filepath.Join(dir, "lim"), filepath.Join(dir, "dlim")
	s := spec(t, "sh", "-c", `echo $$ > "$1"; ulimit -n > "$2"; sleep 0.3`, "sh", pidFile, limFile)
	var gotPid, gotPgid int
	var gotStart, seenStart uint64
	var seenErr error
	s.Started = func(pid, pgid int, start uint64) error {
		gotPid, gotPgid, gotStart = pid, pgid, start
		var st proc.Stat
		st, seenErr = proc.ReadStat(pid)
		seenStart = st.Start
		return nil
	}
	if r, err := Exec(context.Background(), s); err != nil || r.Code != 0 {
		t.Fatalf("%+v %v", r, err)
	}
	b, _ := os.ReadFile(pidFile)
	if p, _ := strconv.Atoi(strings.TrimSpace(string(b))); p != gotPid || gotPid <= 0 {
		t.Errorf("the command's pid is %q, Started got %d", b, gotPid)
	}
	if gotPgid != gotPid {
		t.Errorf("pgid %d, want the command's pid %d", gotPgid, gotPid)
	}
	if seenErr != nil || gotStart == 0 || gotStart != seenStart {
		t.Errorf("Started got start %d, the process table says %d (%v)", gotStart, seenStart, seenErr)
	}
	d := exec.Command("sh", "-c", `ulimit -n > "$1"`, "sh", directLimFile)
	d.Env, d.Dir, d.Stdin = s.Env, s.Dir, s.Stdin
	if err := d.Run(); err != nil {
		t.Fatal(err)
	}
	a, _ := os.ReadFile(limFile)
	w, _ := os.ReadFile(directLimFile)
	if len(a) == 0 || string(a) != string(w) {
		t.Errorf("ulimit -n is %q, a direct start has %q", a, w)
	}
}
