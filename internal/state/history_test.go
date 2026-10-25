package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Exploded-views/bitc/internal/redact"
)

func TestAppendHistory(t *testing.T) {
	p := filepath.Join(t.TempDir(), "history.jsonl")
	red := redact.New()
	red.Add("s3cret")
	code := 3
	if err := AppendHistory(p, Event{Event: "end", Pool: "lint", Slot: 1, Job: "j", Pid: 42,
		Argv: []string{"sh", "-c", "echo s3cret", "a b"}, Cwd: "/w", Provider: "local", Exit: &code}, red); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	if strings.Contains(string(b), "s3cret") || !strings.HasSuffix(string(b), "\n") {
		t.Fatalf("%q", b)
	}
	var e Event
	if err := json.Unmarshal(b, &e); err != nil || e.Argv[3] != "a b" || *e.Exit != 3 || !strings.HasSuffix(e.TS, "Z") {
		t.Fatalf("%+v %v", e, err)
	}
	st, _ := os.Stat(p)
	if st.Mode().Perm() != 0o600 {
		t.Fatal(st.Mode())
	}
}

func TestHistoryLinesStayUnder4KiB(t *testing.T) {
	p := filepath.Join(t.TempDir(), "h.jsonl")
	_ = AppendHistory(p, Event{Event: "start", Pool: "x", Argv: []string{"cmd", strings.Repeat("y", 10000)}}, redact.New())
	b, _ := os.ReadFile(p)
	if len(b) >= 4096 || !strings.Contains(string(b), `"argv_truncated":true`) {
		t.Fatalf("len %d", len(b))
	}
}

// The caller's argv is what execve gets next: truncation must work on a copy.
func TestHistoryTruncationLeavesTheCallersArgv(t *testing.T) {
	long := strings.Repeat("y", 10000)
	argv := []string{"sh", "-c", long}
	_ = AppendHistory(filepath.Join(t.TempDir(), "h.jsonl"), Event{Event: "start", Argv: argv}, redact.New())
	if len(argv) != 3 || argv[2] != long {
		t.Fatalf("argv changed: len %d, word %d bytes", len(argv), len(argv[2]))
	}
}

// Redaction runs on the fields, before JSON escaping and before truncation halves a word.
func TestHistoryRedactsBeforeEncodingAndTruncation(t *testing.T) {
	const secret = `k3y<&"x>9`
	red := redact.New()
	red.Add(secret)
	p := filepath.Join(t.TempDir(), "h.jsonl")
	_ = AppendHistory(p, Event{Event: "start", Argv: []string{"cmd", secret}, Cwd: "/w/" + secret, Reason: secret}, red)
	_ = AppendHistory(p, Event{Event: "start", Argv: []string{"cmd", strings.Repeat("z", 3000) + secret + strings.Repeat("z", 3000)}}, red)
	b, _ := os.ReadFile(p)
	if strings.Contains(string(b), "k3y") { // any part of the secret, escaped or halved
		t.Fatalf("leaked: %s", b)
	}
}
