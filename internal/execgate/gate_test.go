package execgate

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// gateProc is a gate run directly: this test binary re-executed with the verb, whose init runs
// the gate. The sh command touches marker; the tests hold the release end themselves.
type gateProc struct {
	cmd     *exec.Cmd
	release *os.File // write end of the gate pipe
	status  *os.File // read end of the status pipe
	marker  string
}

// gateCommand is this test binary run with args. On Linux it dies with the test process
// (dieWithTest), so a -timeout panic, which runs no Cleanup, leaves no gate behind; on darwin it
// may be left running.
func gateCommand(args ...string) *exec.Cmd {
	c := exec.Command(os.Args[0], args...)
	dieWithTest(c)
	return c
}

func startGate(t *testing.T, env []string, path string, argv ...string) *gateProc {
	t.Helper()
	gr, gw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	sr, sw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	g := &gateProc{release: gw, status: sr}
	args := append([]string{Verb, "3", "4", path}, argv...)
	g.cmd = gateCommand(args...)
	g.cmd.Env = append(os.Environ(), env...)
	g.cmd.ExtraFiles = []*os.File{gr, sw}
	if err := g.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	gr.Close()
	sw.Close()
	t.Cleanup(func() { // not run by a -timeout panic: see gateCommand
		gw.Close()
		sr.Close()
		_ = g.cmd.Process.Kill()
		_ = g.cmd.Wait()
	})
	return g
}

func (g *gateProc) wait(t *testing.T) int {
	t.Helper()
	err := g.cmd.Wait()
	var ee *exec.ExitError
	if err != nil && !errors.As(err, &ee) {
		t.Fatal(err)
	}
	return g.cmd.ProcessState.ExitCode()
}

func shMarker(t *testing.T, env []string) *gateProc {
	t.Helper()
	dir := t.TempDir()
	marker := filepath.Join(dir, "ran")
	g := startGate(t, env, "/bin/sh", "sh", "-c", `echo x > "$1"`, "sh", marker)
	g.marker = marker
	return g
}

func ran(g *gateProc) bool {
	_, err := os.Stat(g.marker)
	return err == nil
}

// T2b: with the supervisor's end of the gate pipe closed, the gate says hello, never runs the
// command, and exits 125 with nothing else on the status pipe.
func TestGateExitsWithoutExecOnEOF(t *testing.T) {
	g := shMarker(t, nil)
	g.release.Close()
	if code := g.wait(t); code != 125 {
		t.Errorf("exit %d, want 125", code)
	}
	b, _ := io.ReadAll(g.status)
	if string(b) != string([]byte{Hello}) {
		t.Errorf("status pipe holds %q, want only the hello", b)
	}
	if ran(g) {
		t.Error("the gate ran the command on EOF")
	}
}

func TestGateReleasedExecsTheCommand(t *testing.T) {
	g := shMarker(t, nil)
	hello := make([]byte, 1)
	if _, err := io.ReadFull(g.status, hello); err != nil || hello[0] != Hello {
		t.Fatalf("hello: %q %v", hello, err)
	}
	if ran(g) {
		t.Fatal("the command ran before the release")
	}
	if _, err := g.release.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	if rest, _ := io.ReadAll(g.status); len(rest) != 0 { // EOF at the exec, nothing written
		t.Errorf("status pipe holds %q after the exec", rest)
	}
	if code := g.wait(t); code != 0 || !ran(g) {
		t.Errorf("exit %d, ran %v", code, ran(g))
	}
}

func TestGateReportsAFailedExecAsAnErrno(t *testing.T) {
	g := startGate(t, nil, filepath.Join(t.TempDir(), "missing"), "missing")
	if _, err := g.release.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(g.status)
	if len(b) != 5 || b[0] != Hello || syscall.Errno(uint32(b[1])|uint32(b[2])<<8|uint32(b[3])<<16|uint32(b[4])<<24) != syscall.ENOENT {
		t.Errorf("status pipe holds %v, want the hello and ENOENT", b)
	}
	if code := g.wait(t); code != 127 {
		t.Errorf("exit %d, want 127", code)
	}
}

func TestGateQuitBeforeTheReleaseExits131(t *testing.T) {
	g := shMarker(t, nil)
	hello := make([]byte, 1)
	if _, err := io.ReadFull(g.status, hello); err != nil {
		t.Fatal(err)
	}
	if err := g.cmd.Process.Signal(syscall.SIGQUIT); err != nil {
		t.Fatal(err)
	}
	if code := g.wait(t); code != 131 || ran(g) {
		t.Errorf("exit %d, ran %v; want 131 and no command", code, ran(g))
	}
}

// The hold is honoured at the stage it names (one byte is read from the FIFO there) and is not
// passed on to the command.
func TestGateHoldIsStrippedFromTheCommandEnvironment(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "hold")
	if err := unix.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	hf, err := os.OpenFile(fifo, os.O_RDWR, 0) // this test is a writer, so the gate's open never waits
	if err != nil {
		t.Fatal(err)
	}
	defer hf.Close()
	if _, err := hf.Write([]byte{1}); err != nil { // the hold byte the gate reads before its exec
		t.Fatal(err)
	}
	out := filepath.Join(dir, "env")
	g := startGate(t, []string{HoldEnv + "=" + fifo, HoldAtEnv + "=" + HoldExec}, "/bin/sh", "sh", "-c", `env > "$1"`, "sh", out)
	if _, err := g.release.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	if code := g.wait(t); code != 0 {
		t.Fatalf("exit %d", code)
	}
	b, _ := os.ReadFile(out)
	if len(b) == 0 || containsLine(string(b), HoldEnv+"=") || containsLine(string(b), HoldAtEnv+"=") {
		t.Errorf("the command's environment has the test hold: %q", b)
	}
	// The gate read the hold byte: the FIFO is empty now.
	fd, err := unix.Open(fifo, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0) // hf is a writer: EAGAIN, not EOF
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	var left [1]byte
	if n, err := unix.Read(fd, left[:]); !errors.Is(err, unix.EAGAIN) {
		t.Errorf("the hold byte was not consumed: read %d, %v", n, err)
	}
}

func containsLine(s, prefix string) bool {
	for _, l := range strings.Split(s, "\n") {
		if strings.HasPrefix(l, prefix) {
			return true
		}
	}
	return false
}

func TestGateRefusesAGateFdThatIsNotAPipe(t *testing.T) {
	f, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sr, sw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer sr.Close()
	c := gateCommand(Verb, "3", "4", "/bin/sh", "sh", "-c", "true")
	c.ExtraFiles = []*os.File{f, sw}
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	sw.Close()
	_ = c.Wait()
	if code := c.ProcessState.ExitCode(); code != 125 {
		t.Errorf("exit %d, want 125", code)
	}
}
