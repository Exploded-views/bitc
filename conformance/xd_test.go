package conformance

import (
	"bytes"
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
	"testing"
	"time"

	"github.com/Exploded-views/bitc/conformance/harness"
	"github.com/Exploded-views/bitc/internal/agent"
)

// Family XD: the exec delivery and the exec results path (providers delta X.1), shared by the
// Sprites and ssh providers: no-listener mode, __agent --boot-stdin, __agent read/tail/list/teardown.

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
