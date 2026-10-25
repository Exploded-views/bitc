package conformance

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/Exploded-views/bitc/conformance/harness"
	"github.com/Exploded-views/bitc/internal/agent"
	"github.com/Exploded-views/bitc/internal/agent/agenttest"
)

// Family XD: the exec delivery and the exec results path (providers delta X.1), shared by the
// Sprites and ssh providers: no-listener mode, __agent --boot-stdin, __agent read/tail/list/teardown.

func TestXD1_NoListenerModeRunsOnceAndExits(t *testing.T) {
	f := agenttest.NewFixture(t, "#!/bin/sh\necho to-out\n")
	f.NoListener = true
	log, code := f.Run(t) // returns by itself: no results server keeps the agent alive
	dir := filepath.Join(f.Root, "j1")
	if code != 0 || !strings.Contains(log, "__BITC_EXIT__ n0nce 0") {
		t.Fatalf("%d %q", code, log)
	}
	if got := string(must(os.ReadFile(filepath.Join(dir, "agent.log")))); got != log {
		t.Fatalf("agent.log is not the agent's stdout:\n%q\nvs\n%q", got, log)
	}
	if _, err := agent.VerifyResult(f.K, must(os.ReadFile(filepath.Join(dir, "result.json")))); err != nil {
		t.Fatal(err)
	}
	pid := strings.Fields(string(must(os.ReadFile(filepath.Join(dir, "agent.pid")))))
	if len(pid) != 2 || pid[0] != strconv.Itoa(os.Getpid()) {
		t.Fatalf("agent.pid %q", pid)
	}
	// a second boot of the same job is LOST, never a second run, and ends the same way
	log2, code2 := f.Run(t)
	if code2 != 75 || !strings.Contains(log2, "__BITC_LOST__ n0nce") || strings.Contains(log2, "__BITC_BEGIN__") {
		t.Fatalf("restart: %d %q", code2, log2)
	}
	if got := string(must(os.ReadFile(filepath.Join(dir, "agent.log")))); got != log {
		t.Fatalf("agent.log after the restart (the loser writes only to its own stdout): %q", got)
	}
}

func bootFrame(t *testing.T, p agent.BootPayload) []byte {
	t.Helper()
	b, err := p.Frame()
	if err != nil || len(b) < 9 {
		t.Fatalf("Frame: %d bytes, %v", len(b), err)
	}
	return b
}

func TestXD2_BootPayloadIsLengthPrefixedAndTruncationRefused(t *testing.T) {
	want := agent.BootPayload{Spec: "c3BlYw==", Deadline: 1700000000, DeployKey: "KEY", Provider: "sprites", ResourceID: "sp-1"}
	frame := bootFrame(t, want)
	if n := binary.BigEndian.Uint64(frame[:8]); int(n) != len(frame)-8 {
		t.Fatalf("length prefix %d for a %d byte body", n, len(frame)-8)
	}
	got, err := agent.ReadBootPayload(bytes.NewReader(append(frame, "trailing"...)))
	if err != nil || got != want {
		t.Fatalf("%+v %v", got, err)
	}
	huge := make([]byte, 8)
	binary.BigEndian.PutUint64(huge, agent.MaxBootPayload+1)
	bad := map[string][]byte{"empty": nil, "short prefix": frame[:5], "prefix only": frame[:8], "short body": frame[:len(frame)-1],
		"bad json": append([]byte{0, 0, 0, 0, 0, 0, 0, 3}, "{{{"...), "zero length": make([]byte, 8)}
	for name, b := range bad {
		if _, err := agent.ReadBootPayload(bytes.NewReader(b)); !errors.Is(err, agent.ErrTruncated) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := agent.ReadBootPayload(bytes.NewReader(huge)); !errors.Is(err, agent.ErrTooLarge) {
		t.Errorf("over the limit: %v", err)
	}
	if _, err := (agent.BootPayload{Spec: strings.Repeat("a", agent.MaxBootPayload)}).Frame(); !errors.Is(err, agent.ErrTooLarge) {
		t.Errorf("host side: %v", err)
	}
}

func TestXD3_BootStdinThroughTheBinary(t *testing.T) {
	root := t.TempDir()
	f := agenttest.NewFixture(t, "#!/bin/sh\necho via-stdin\n")
	f.Spec.Port, f.Spec.TestRoot = -1, root
	enc, err := f.Spec.Encode()
	if err != nil {
		t.Fatal(err)
	}
	run := func(stdin []byte) (string, int) {
		c := exec.Command(harness.Bin, "__agent", "--boot-stdin")
		c.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + t.TempDir()}
		c.Stdin = bytes.NewReader(stdin)
		var out bytes.Buffer
		c.Stdout = &out
		if err := c.Start(); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- c.Wait() }()
		select {
		case <-done:
		case <-time.After(60 * time.Second):
			_ = c.Process.Kill()
			t.Fatalf("the agent did not exit: %s", out.String())
		}
		return out.String(), c.ProcessState.ExitCode()
	}
	// refusals first: nothing is started, nothing is created
	full := bootFrame(t, agent.BootPayload{Spec: enc, Deadline: time.Now().Add(time.Hour).Unix()})
	for name, in := range map[string][]byte{"truncated": full[:len(full)-7], "empty": nil} {
		if out, code := run(in); code != 65 || out != "refused: truncated payload\n" {
			t.Errorf("%s: %d %q", name, code, out)
		}
	}
	listener := f.Spec
	listener.Port = 8080
	lenc, _ := listener.Encode()
	if out, code := run(bootFrame(t, agent.BootPayload{Spec: lenc})); code != 65 || out != "refused: --boot-stdin needs port -1 in the spec\n" {
		t.Errorf("listener spec: %d %q", code, out)
	}
	if _, err := os.Stat(filepath.Join(root, "run")); err == nil {
		t.Fatal("a refused payload created results")
	}
	// the real thing: stage 1, the memfd re-exec, stage 2, a job, exit 0 by itself
	out, code := run(full)
	if code != 0 || !strings.Contains(out, "__BITC_BOOT__ n0nce") || !strings.Contains(out, "__BITC_EXIT__ n0nce 0") {
		t.Fatalf("%d %q", code, out)
	}
	dir := filepath.Join(root, "run", "j1")
	if got := string(must(os.ReadFile(filepath.Join(dir, "out.log")))); got != "via-stdin\n" {
		t.Fatalf("out.log %q", got)
	}
	if got := string(must(os.ReadFile(filepath.Join(dir, "agent.log")))); got != out {
		t.Fatalf("agent.log %q vs stdout %q", got, out)
	}
	if _, err := os.Stat(filepath.Join(root, "port")); err == nil {
		t.Fatal("a listener was bound")
	}
}

func TestXD4_DeployKeyIsStrippedFromThePayloadStageTwoReads(t *testing.T) {
	p := agent.BootPayload{Spec: "c3BlYw==", DeployKey: "KEYMATERIAL-0123", ScopedKey: "scoped", Deadline: 5}
	rest, key := p.WithoutKey()
	if key != "KEYMATERIAL-0123" || rest.DeployKey != "" || rest.Spec != p.Spec || rest.Deadline != 5 || rest.ScopedKey != "scoped" {
		t.Fatalf("%+v %q", rest, key)
	}
	if b := bootFrame(t, rest); bytes.Contains(b, []byte("KEYMATERIAL")) {
		t.Fatalf("the stripped payload still carries the key: %s", b)
	}
	if p.DeployKey == "" {
		t.Fatal("WithoutKey changed the original")
	}
	// through the binary: while the job runs, no memfd of the agent holds the key and its
	// environment is empty
	root := t.TempDir()
	f := agenttest.NewFixture(t, "#!/bin/sh\n")
	f.Spec.Port, f.Spec.TestRoot = -1, root
	f.Spec.Argv = []string{"sh", "-c", "echo up > " + filepath.Join(root, "up") + "; sleep 30"}
	enc, _ := f.Spec.Encode()
	const secret = "PRIVATE-KEY-MATERIAL-7f3a"
	c := exec.Command(harness.Bin, "__agent", "--boot-stdin")
	c.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + t.TempDir()}
	c.Stdin = bytes.NewReader(bootFrame(t, agent.BootPayload{Spec: enc, DeployKey: secret, Deadline: time.Now().Add(time.Hour).Unix()}))
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Process.Kill(); _ = c.Wait() })
	waitFor(t, "the job to run", func() bool { _, err := os.Stat(filepath.Join(root, "up")); return err == nil })
	proc := "/proc/" + strconv.Itoa(c.Process.Pid)
	fds, _ := os.ReadDir(proc + "/fd")
	for _, fd := range fds {
		target, err := os.Readlink(proc + "/fd/" + fd.Name())
		if err != nil || !strings.HasPrefix(target, "/memfd:") {
			continue
		}
		if b, _ := os.ReadFile(proc + "/fd/" + fd.Name()); bytes.Contains(b, []byte(secret)) {
			t.Errorf("fd %s (%s) holds the deploy key", fd.Name(), target)
		}
	}
	if env := must(os.ReadFile(proc + "/environ")); len(env) != 0 {
		t.Errorf("the agent's environment is not empty: %q", env)
	}
}

// xdRoot makes a results root with job j1 and the given files (mode 0600).
func xdRoot(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "j1")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for n, b := range files {
		if err := os.WriteFile(filepath.Join(dir, n), []byte(b), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestXD5_ReadServesOnlyListedFilesOfAValidJob(t *testing.T) {
	root := xdRoot(t, map[string]string{"agent.log": "a", "out.log": "0123456789", "err.log": "e", "result.json": "{}", "state.json": "{}",
		"artifacts.tar.gz": "z", "x.txt": "x", "agent.pid": "1 2", "result.json.part": "p"})
	dir := filepath.Join(root, "j1")
	_ = os.WriteFile(filepath.Join(dir, "err.log.part"), []byte("partial"), 0o600)
	_ = os.Remove(filepath.Join(dir, "err.log"))
	_ = os.Symlink("/etc/hostname", filepath.Join(dir, "artifacts.tar.gz.link"))
	_ = os.Remove(filepath.Join(dir, "artifacts.tar.gz"))
	_ = os.Symlink("/etc/hostname", filepath.Join(dir, "artifacts.tar.gz"))
	_ = os.Symlink("j1", filepath.Join(root, "linkjob"))
	read := func(job, file string, off int64) (string, error) {
		var w bytes.Buffer
		err := agent.ReadFile(root, job, file, off, &w)
		return w.String(), err
	}
	for _, n := range []string{"agent.log", "out.log", "result.json", "state.json"} {
		if got, err := read("j1", n, 0); err != nil || got == "" {
			t.Errorf("%s: %q %v", n, got, err)
		}
	}
	if got, err := read("j1", "out.log", 4); err != nil || got != "456789" {
		t.Errorf("offset: %q %v", got, err)
	}
	if got, err := read("j1", "out.log", 10); err != nil || got != "" {
		t.Errorf("offset at the end: %q %v", got, err)
	}
	if got, err := read("j1", "out.log", 11); !errors.Is(err, agent.ErrOffset) || got != "" {
		t.Errorf("offset past the end: %q %v", got, err)
	}
	for _, c := range []struct{ job, file string }{{"j1", "x.txt"}, {"j1", "agent.pid"}, {"j1", "err.log.part"}, {"j1", "result.json.part"},
		{"j1", "../j1/out.log"}, {"j1", "sub/out.log"}, {"j1", ""}, {"j1", "."}, {"j1", "./out.log"}, {"j1", "out.log/"}, {"j1", "OUT.LOG"},
		{"", "out.log"}, {".", "out.log"}, {"..", "out.log"}, {"../j1", "out.log"}, {"j1/x", "out.log"}, {".hidden", "out.log"},
		{"a b", "out.log"}, {"j1\n", "out.log"}, {strings.Repeat("a", 65), "out.log"}} {
		if got, err := read(c.job, c.file, 0); !errors.Is(err, agent.ErrBadArg) || got != "" {
			t.Errorf("%q %q: %q %v", c.job, c.file, got, err)
		}
	}
	// well-formed names that are not there or are not regular files: not found, nothing written
	for _, c := range []struct{ job, file string }{{"j1", "err.log"}, {"j1", "artifacts.tar.gz"}, {"nojob", "out.log"}, {"linkjob", "out.log"}} {
		if got, err := read(c.job, c.file, 0); !errors.Is(err, agent.ErrNotFound) || got != "" {
			t.Errorf("%q %q: %q %v", c.job, c.file, got, err)
		}
	}
}

func TestXD6_ReadAndListCommandsExitCodes(t *testing.T) {
	root := xdRoot(t, map[string]string{"out.log": "0123456789", "state.json": `{"phase":"ended","exit":0}`})
	_ = os.MkdirAll(filepath.Join(root, "j2"), 0o700)
	_ = os.MkdirAll(filepath.Join(root, ".stray"), 0o700)
	run := func(args ...string) (string, int) {
		c := exec.Command(harness.Bin, append([]string{"__agent", "--root", root}, args...)...)
		c.Env = []string{"PATH=/usr/bin:/bin"}
		out, err := c.Output()
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return string(out), ee.ExitCode()
		}
		return string(out), 0
	}
	for args, want := range map[string]struct {
		out  string
		code int
	}{
		"read j1 out.log 3":    {"3456789", 0},
		"read j1 out.log 0":    {"0123456789", 0},
		"read j1 out.log 99":   {"", 65},
		"read j1 err.log 0":    {"", 66},
		"read j1 x.txt 0":      {"", 64},
		"read ../j1 out.log 0": {"", 64},
		"read j1 out.log -1":   {"", 64},
		"read j1 out.log 1x":   {"", 64},
		"read j1 out.log":      {"", 64},
		"read j1 out.log 0 0":  {"", 64},
		"frobnicate":           {"", 64},
		"tail j1 1 2":          {"", 64},
		"tail j1 a b c":        {"", 64},
		"teardown ../j1":       {"", 64},
		"teardown":             {"", 64},
		"list extra":           {"", 64},
	} {
		if out, code := run(strings.Fields(args)...); out != want.out || code != want.code {
			t.Errorf("%s: %d %q, want %d %q", args, code, out, want.code, want.out)
		}
	}
	out, code := run("list")
	var got []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("%q: %v", l, err)
		}
		got = append(got, m)
	}
	if code != 0 || len(got) != 2 || got[0]["job"] != "j1" || got[0]["phase"] != "ended" || got[0]["alive"] != false || got[0]["exit"] != float64(0) ||
		got[1]["job"] != "j2" || got[1]["phase"] != "" {
		t.Fatalf("%d %q", code, out)
	}
}

// ownStart is this process's identity as agent.pid records it: "<pid> <start time ticks>".
func ownIdentity(t *testing.T, pid int) string {
	t.Helper()
	b := must(os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat"))
	rest := string(b[bytes.LastIndexByte(b, ')')+2:])
	return fmt.Sprintf("%d %s", pid, strings.Fields(rest)[19])
}

type frameLog struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *frameLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}
func (l *frameLog) frames(t *testing.T) []agent.Frame {
	l.mu.Lock()
	defer l.mu.Unlock()
	var fs []agent.Frame
	for _, ln := range strings.Split(strings.TrimSpace(l.b.String()), "\n") {
		if ln == "" {
			continue
		}
		var f agent.Frame
		if err := json.Unmarshal([]byte(ln), &f); err != nil {
			t.Fatalf("%q: %v", ln, err)
		}
		fs = append(fs, f)
	}
	return fs
}

// collected is what the data frames of one file add up to, checking offsets are contiguous.
func collected(t *testing.T, fs []agent.Frame, file string, from int64) string {
	t.Helper()
	var sb strings.Builder
	next := from
	for _, f := range fs {
		if f.File != file {
			continue
		}
		if f.Off != next {
			t.Fatalf("%s: frame at %d, want %d", file, f.Off, next)
		}
		sb.Write(f.Data)
		next += int64(len(f.Data))
	}
	return sb.String()
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for end := time.Now().Add(10 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestXD7_TailStreamsByOffsetWithHeartbeatsAndEnds(t *testing.T) {
	agentProc := exec.Command("sleep", "60") // the running agent
	if err := agentProc.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = agentProc.Process.Kill(); _ = agentProc.Wait() })
	root := xdRoot(t, map[string]string{"agent.log": "boot\n", "out.log.part": "o1", "agent.pid": ownIdentity(t, agentProc.Process.Pid)})
	dir := filepath.Join(root, "j1")
	log := &frameLog{}
	done := make(chan error, 1)
	go func() {
		done <- agent.Tail(context.Background(), root, "j1", agent.TailOptions{Offsets: map[string]int64{"agent.log": 0, "out.log": 0, "err.log": 0},
			Heartbeat: 30 * time.Millisecond, Poll: 10 * time.Millisecond}, log)
	}()
	waitFor(t, "the first data", func() bool { return collected(t, log.frames(t), "out.log", 0) == "o1" })
	if got := collected(t, log.frames(t), "agent.log", 0); got != "boot\n" {
		t.Fatalf("agent.log %q", got)
	}
	appendTo := func(n, s string) {
		f, err := os.OpenFile(filepath.Join(dir, n), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = f.WriteString(s)
		f.Close()
	}
	appendTo("out.log.part", "o2")
	waitFor(t, "the appended data", func() bool { return collected(t, log.frames(t), "out.log", 0) == "o1o2" })
	waitFor(t, "a heartbeat", func() bool {
		for _, f := range log.frames(t) {
			if f.HB && f.Alive != nil && *f.Alive {
				return true
			}
		}
		return false
	})
	// the job ends: .part files are committed, result.json appears; tail drains and says done
	appendTo("err.log.part", "bad\n")
	_ = os.Rename(filepath.Join(dir, "out.log.part"), filepath.Join(dir, "out.log"))
	_ = os.Rename(filepath.Join(dir, "err.log.part"), filepath.Join(dir, "err.log"))
	_ = os.WriteFile(filepath.Join(dir, "result.json"), []byte("{}"), 0o600)
	_ = agentProc.Process.Kill() // the agent exits right after it has written EXIT
	_ = agentProc.Wait()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("tail did not end after result.json")
	}
	fs := log.frames(t)
	if !fs[len(fs)-1].Done || collected(t, fs, "out.log", 0) != "o1o2" || collected(t, fs, "err.log", 0) != "bad\n" {
		t.Fatalf("%+v", fs)
	}
	// resuming from the host's offsets sends only what is after them
	log2 := &frameLog{}
	err := agent.Tail(context.Background(), root, "j1", agent.TailOptions{Offsets: map[string]int64{"agent.log": 2, "out.log": 3, "err.log": 4},
		Heartbeat: time.Hour, Poll: 10 * time.Millisecond}, log2)
	fs2 := log2.frames(t)
	if err != nil || collected(t, fs2, "agent.log", 2) != "ot\n" || collected(t, fs2, "out.log", 3) != "2" || collected(t, fs2, "err.log", 4) != "" || !fs2[len(fs2)-1].Done {
		t.Fatalf("%v %+v", err, fs2)
	}
}

func TestXD7_TailReportsADeadAgentAndStops(t *testing.T) {
	sleeper := exec.Command("sleep", "60")
	if err := sleeper.Start(); err != nil {
		t.Fatal(err)
	}
	id := ownIdentity(t, sleeper.Process.Pid)
	_ = sleeper.Process.Kill()
	_ = sleeper.Wait()
	root := xdRoot(t, map[string]string{"agent.log": "boot\n", "agent.pid": id})
	log := &frameLog{}
	done := make(chan error, 1)
	go func() {
		done <- agent.Tail(context.Background(), root, "j1", agent.TailOptions{Offsets: map[string]int64{}, Heartbeat: time.Hour, Poll: 10 * time.Millisecond}, log)
	}()
	select {
	case err := <-done:
		fs := log.frames(t)
		var dead bool
		for _, f := range fs {
			dead = dead || (f.HB && f.Alive != nil && !*f.Alive)
		}
		if err != nil || !dead || !fs[len(fs)-1].Done || collected(t, fs, "agent.log", 0) != "boot\n" {
			t.Fatalf("%v %+v", err, fs)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("tail kept running for an agent that is gone and left no result")
	}
	if err := agent.Tail(context.Background(), t.TempDir(), "j1", agent.TailOptions{}, &bytes.Buffer{}); !errors.Is(err, agent.ErrNotFound) {
		t.Fatalf("unknown job: %v", err)
	}
	if err := agent.Tail(context.Background(), root, "../x", agent.TailOptions{}, &bytes.Buffer{}); !errors.Is(err, agent.ErrBadArg) {
		t.Fatalf("bad job: %v", err)
	}
}

func TestXD8_TeardownKillsTheRecordedProcessesOnlyAndRemovesTheJob(t *testing.T) {
	start := func() *exec.Cmd {
		c := exec.Command("sleep", "60")
		c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := c.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Process.Kill(); _ = c.Wait() })
		return c
	}
	ag, job, stranger := start(), start(), start()
	root := xdRoot(t, map[string]string{"agent.log": "x", "agent.pid": ownIdentity(t, ag.Process.Pid), "job.pid": ownIdentity(t, job.Process.Pid)})
	var w bytes.Buffer
	if err := agent.Teardown(root, "j1", &w); err != nil || w.String() != "torn j1\n" {
		t.Fatalf("%v %q", err, w.String())
	}
	for name, c := range map[string]*exec.Cmd{"agent": ag, "job": job} {
		done := make(chan error, 1)
		go func() { done <- c.Wait() }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Errorf("the %s survived teardown", name)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "j1")); err == nil {
		t.Fatal("the job directory is still there")
	}
	w.Reset()
	if err := agent.Teardown(root, "j1", &w); err != nil || w.String() != "gone\n" {
		t.Fatalf("second teardown: %v %q", err, w.String())
	}
	// a recorded pid that now belongs to another process (the start time differs) is not killed
	root = xdRoot(t, map[string]string{"agent.pid": fmt.Sprintf("%d 1", stranger.Process.Pid)})
	w.Reset()
	if err := agent.Teardown(root, "j1", &w); err != nil || w.String() != "torn j1\n" {
		t.Fatalf("%v %q", err, w.String())
	}
	if err := stranger.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("teardown killed a process it does not own: %v", err)
	}
	if err := agent.Teardown(root, "../j1", &w); !errors.Is(err, agent.ErrBadArg) {
		t.Fatalf("bad job: %v", err)
	}
	_ = strconv.Itoa
}

// bootBinary starts `bitc __agent --boot-stdin` with a framed payload and returns the command; the
// caller owns stdout (nil discards it).
func bootBinary(t *testing.T, enc string, stdout *os.File) *exec.Cmd {
	t.Helper()
	c := exec.Command(harness.Bin, "__agent", "--boot-stdin")
	c.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + t.TempDir()}
	c.Stdin = bytes.NewReader(bootFrame(t, agent.BootPayload{Spec: enc, Deadline: time.Now().Add(time.Hour).Unix()}))
	if stdout != nil {
		c.Stdout = stdout
	}
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Process.Kill(); _ = c.Wait() })
	return c
}

func TestXD9_TailDoesNotSayDoneBeforeTheAgentHasWrittenExit(t *testing.T) {
	sleeper := exec.Command("sleep", "60")
	if err := sleeper.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sleeper.Process.Kill(); _ = sleeper.Wait() })
	// result.json is committed, but the agent is still alive and has not written EXIT yet
	root := xdRoot(t, map[string]string{"agent.log": "__BITC_BEGIN__ n0nce\n", "out.log": "o", "err.log": "", "result.json": "{}",
		"agent.pid": ownIdentity(t, sleeper.Process.Pid)})
	dir := filepath.Join(root, "j1")
	log := &frameLog{}
	done := make(chan error, 1)
	go func() {
		done <- agent.Tail(context.Background(), root, "j1", agent.TailOptions{Heartbeat: time.Hour, Poll: 10 * time.Millisecond}, log)
	}()
	waitFor(t, "the agent.log data", func() bool { return collected(t, log.frames(t), "agent.log", 0) == "__BITC_BEGIN__ n0nce\n" })
	select {
	case err := <-done:
		t.Fatalf("tail ended (%v) while the agent had not written its EXIT marker: %+v", err, log.frames(t))
	case <-time.After(500 * time.Millisecond):
	}
	// the agent writes EXIT and then exits
	f, err := os.OpenFile(filepath.Join(dir, "agent.log"), os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("__BITC_EXIT__ n0nce 0\n")
	f.Close()
	_ = sleeper.Process.Kill()
	_ = sleeper.Wait()
	select {
	case err := <-done:
		fs := log.frames(t)
		if err != nil || !fs[len(fs)-1].Done || collected(t, fs, "agent.log", 0) != "__BITC_BEGIN__ n0nce\n__BITC_EXIT__ n0nce 0\n" {
			t.Fatalf("%v %+v", err, fs)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("tail did not end after the agent exited")
	}
}

func TestXD10_ASecondBootKeepsTheFirstAgentsIdentity(t *testing.T) {
	root := t.TempDir()
	f := agenttest.NewFixture(t, "#!/bin/sh\n")
	f.Spec.Port, f.Spec.TestRoot = -1, root
	f.Spec.Argv = []string{"sh", "-c", "echo up > " + filepath.Join(root, "up") + "; sleep 60"}
	enc, _ := f.Spec.Encode()
	first := bootBinary(t, enc, nil)
	waitFor(t, "the job to run", func() bool { _, err := os.Stat(filepath.Join(root, "up")); return err == nil })
	results := filepath.Join(root, "run")
	pidFile := filepath.Join(results, "j1", "agent.pid")
	before := string(must(os.ReadFile(pidFile)))
	if !strings.HasPrefix(before, strconv.Itoa(first.Process.Pid)+" ") {
		t.Fatalf("agent.pid %q does not name the first agent %d", before, first.Process.Pid)
	}
	logFile := filepath.Join(results, "j1", "agent.log")
	logBefore := must(os.ReadFile(logFile))
	var out bytes.Buffer
	second := exec.Command(harness.Bin, "__agent", "--boot-stdin")
	second.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + t.TempDir()}
	second.Stdin = bytes.NewReader(bootFrame(t, agent.BootPayload{Spec: enc, Deadline: time.Now().Add(time.Hour).Unix()}))
	second.Stdout = &out
	_ = second.Run()
	if code := second.ProcessState.ExitCode(); code != 75 || !strings.Contains(out.String(), "__BITC_LOST__ n0nce") {
		t.Fatalf("second boot: %d %q", code, out.String())
	}
	if !strings.Contains(out.String(), "__BITC_BOOT__ n0nce") {
		t.Fatalf("the second boot's own stdout lacks BOOT: %q", out.String())
	}
	if logAfter := must(os.ReadFile(logFile)); !bytes.Equal(logAfter, logBefore) {
		t.Fatalf("the second boot wrote to the first job's agent.log: %q -> %q", logBefore, logAfter)
	}
	if after := string(must(os.ReadFile(pidFile))); after != before {
		t.Fatalf("the second boot rewrote agent.pid: %q -> %q", before, after)
	}
	// the host's tail still sees the first agent alive
	log := &frameLog{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_ = agent.Tail(ctx, results, "j1", agent.TailOptions{Heartbeat: 10 * time.Millisecond, Poll: 10 * time.Millisecond}, log)
	}()
	waitFor(t, "a heartbeat", func() bool {
		for _, f := range log.frames(t) {
			if f.HB && f.Alive != nil {
				return true
			}
		}
		return false
	})
	for _, f := range log.frames(t) {
		if f.HB && f.Alive != nil && !*f.Alive {
			t.Fatalf("tail reports the running agent as gone: %+v", log.frames(t))
		}
	}
	// and teardown still kills it
	var w bytes.Buffer
	if err := agent.Teardown(results, "j1", &w); err != nil || w.String() != "torn j1\n" {
		t.Fatalf("%v %q", err, w.String())
	}
	gone := make(chan error, 1)
	go func() { gone <- first.Wait() }()
	select {
	case <-gone:
	case <-time.After(5 * time.Second):
		t.Fatal("teardown did not kill the first agent")
	}
}

func TestXD11_TheJobSurvivesTheBootChannelDropping(t *testing.T) {
	root := t.TempDir()
	f := agenttest.NewFixture(t, "#!/bin/sh\necho finished\n")
	f.Spec.Port, f.Spec.TestRoot = -1, root
	f.Spec.Setup = []agent.Step{{Name: "wait", RunAs: "job", Argv: []string{"sleep", "1"}}} // BEGIN comes after the drop
	enc, _ := f.Spec.Encode()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	c := bootBinary(t, enc, w)
	w.Close()
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), "__BITC_BOOT__ n0nce") {
			break
		}
	}
	r.Close() // the exec channel is gone: every later write to stdout fails
	done := make(chan error, 1)
	go func() { done <- c.Wait() }()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("the agent did not exit")
	}
	dir := filepath.Join(root, "run", "j1")
	log := string(must(os.ReadFile(filepath.Join(dir, "agent.log"))))
	if _, err := os.Stat(filepath.Join(dir, "result.json")); err != nil || c.ProcessState.ExitCode() != 0 ||
		!strings.Contains(log, "__BITC_BEGIN__ n0nce") || !strings.Contains(log, "__BITC_EXIT__ n0nce 0") {
		t.Fatalf("exit %v (%v), result.json %v, agent.log:\n%s", c.ProcessState, c.ProcessState.ExitCode(), err, log)
	}
}
