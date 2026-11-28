package run

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Exploded-views/bitc/internal/proc"
)

// appears polls for path for up to d and reports whether it showed up.
func appears(path string, d time.Duration) bool {
	for deadline := time.Now().Add(d); ; time.Sleep(10 * time.Millisecond) {
		if _, err := os.Stat(path); err == nil {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
	}
}

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

// T1: nothing of the command runs before it is recorded (Started has returned).
func TestExecRunsNothingBeforeTheCommandIsRecorded(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	s := spec(t, "sh", "-c", `echo x > "$1"`, "sh", marker)
	var started, early bool
	s.Started = func(pid, pgid int, start uint64) error {
		started = true
		early = appears(marker, 2*time.Second)
		return nil
	}
	r, err := Exec(context.Background(), s)
	if err != nil || r.Code != 0 {
		t.Fatalf("%+v %v", r, err)
	}
	if !started {
		t.Fatal("Started was not called")
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
	var early bool
	s.Started = func(pid, pgid int, start uint64) error {
		early = appears(marker, 2*time.Second)
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
	if early {
		t.Error("the command ran before it was recorded")
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("the command ran after its group was signalled")
	}
}

// T2: a supervisor that dies after the command started but before it recorded it leaves nothing
// of the command running. A helper process (this test binary) is the supervisor and kills itself
// in the beforeRecord seam, once the command has had every chance to fork.
func TestAKilledSupervisorLeavesNoCommandRunning(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "ran")
	kids := filepath.Join(dir, "kids")
	out, err := os.Create(filepath.Join(dir, "helper.out"))
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	h := exec.Command(os.Args[0], "-test.run=^TestHelperKilledSupervisor$")
	h.Env = append(os.Environ(), "BITC_GATE_HELPER=killed-supervisor", "BITC_GATE_MARKER="+marker, "BITC_GATE_KIDS="+kids)
	h.Stdout, h.Stderr = out, out // files, so Wait does not wait for the command's children
	err = h.Run()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
		b, _ := os.ReadFile(out.Name())
		t.Fatalf("the helper did not kill itself: %v\n%s", err, b)
	}
	b, _ := os.ReadFile(kids)
	var pgid int
	var start uint64
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		f := strings.Fields(line)
		if len(f) != 3 {
			continue
		}
		pid, _ := strconv.Atoi(f[0])
		pg, _ := strconv.Atoi(f[1])
		if pid > 0 && pid == pg { // the group leader: sh at base, the exec gate with the fix
			pgid = pid
			start, _ = strconv.ParseUint(f[2], 10, 64)
		}
	}
	if pgid <= 0 {
		t.Fatalf("the helper recorded no group leader: %q", b)
	}
	defer func() {
		for _, p := range groupSurvivors(pgid, start) {
			_ = syscall.Kill(p, syscall.SIGKILL) // exact pids, never the group
		}
	}()
	var left []int
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		if left = groupSurvivors(pgid, start); len(left) == 0 || time.Now().After(deadline) {
			break
		}
	}
	if len(left) != 0 {
		t.Errorf("processes of group %d outlived the killed supervisor: %v", pgid, left)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("the command ran although its supervisor died before recording it")
	}
}

// TestHelperKilledSupervisor is the supervisor process of T2; it does nothing in a normal run.
func TestHelperKilledSupervisor(t *testing.T) {
	if os.Getenv("BITC_GATE_HELPER") != "killed-supervisor" {
		return
	}
	marker, kids := os.Getenv("BITC_GATE_MARKER"), os.Getenv("BITC_GATE_KIDS")
	beforeRecord = func(int) {
		appears(marker, 3*time.Second)
		me := os.Getpid()
		var b strings.Builder
		for _, p := range proc.Pids() {
			if st, err := proc.ReadStat(p); err == nil && st.PPid == me {
				fmt.Fprintf(&b, "%d %d %d\n", st.Pid, st.Pgrp, st.Start)
			}
		}
		_ = os.WriteFile(kids, []byte(b.String()), 0o600)
		_ = syscall.Kill(me, syscall.SIGKILL)
		time.Sleep(time.Minute)
	}
	s := spec(t, "sh", "-c", `sleep 30 & echo x > "$1"; wait`, "sh", marker)
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
