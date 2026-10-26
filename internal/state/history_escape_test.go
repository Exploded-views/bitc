package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/Exploded-views/bitc/internal/redact"
)

// The 4 KiB bound holds on the encoded line: a reason of '<', '>' and '&' (six bytes each once
// escaped), a long cwd and multi-byte argv words still give one line under 4 KiB, valid JSON
// whose strings are valid UTF-8.
func TestHistoryLineBoundHoldsAfterEscaping(t *testing.T) {
	p := filepath.Join(t.TempDir(), "h.jsonl")
	e := Event{Event: "fallback", Pool: "x", Reason: strings.Repeat("<>&", 170),
		Cwd: "/" + strings.Repeat("ü", 1500), Argv: []string{"echo", strings.Repeat("é", 1999), strings.Repeat("€", 999)}}
	if err := AppendHistory(p, e, redact.New()); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	if len(b) > 4096 {
		t.Fatalf("line of %d bytes", len(b))
	}
	var got Event
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	for _, s := range append([]string{got.Reason, got.Cwd}, got.Argv...) {
		if !utf8.ValidString(s) || strings.ContainsRune(s, utf8.RuneError) {
			t.Fatalf("a field was cut inside a character: %q", s)
		}
	}
}

// With nothing in argv to shorten the bound still holds: a huge cwd is cut, and when only the
// reason can give way (a long pool name is never cut) the reason is cut on its escaped size.
func TestHistoryLineBoundWithoutArgv(t *testing.T) {
	for name, e := range map[string]Event{
		"cwd":    {Event: "fallback", Pool: "x", Cwd: "/" + strings.Repeat("ü", 2500), Reason: "<>&"},
		"reason": {Event: "fallback", Pool: strings.Repeat("p", 1200), Cwd: "/w", Reason: strings.Repeat("<", 510)},
	} {
		p := filepath.Join(t.TempDir(), "h.jsonl")
		if err := AppendHistory(p, e, redact.New()); err != nil {
			t.Fatal(err)
		}
		b, _ := os.ReadFile(p)
		var got Event
		if len(b) > 4096 || json.Unmarshal(b, &got) != nil || !utf8.ValidString(got.Cwd+got.Reason) {
			t.Fatalf("%s: line of %d bytes", name, len(b))
		}
	}
}

// Names are not length-checked where history is written (a pool or job name is one path element,
// a plugin type a short name), so the bound needs a last resort: huge pool, job, provider and sha
// values with nothing else to cut still give one line of at most 4 KiB.
func TestHistoryLineBoundWithHugeNames(t *testing.T) {
	huge := "job-" + strings.Repeat("n", 5000) // a prefix kept is told from a suffix kept
	for name, e := range map[string]Event{
		"job":      {Event: "start", Pool: "x", Job: huge, Cwd: "/w"},
		"pool":     {Event: "start", Pool: huge, Job: "j", Cwd: "/w"},
		"all four": {Event: "end", Pool: huge, Job: huge, Provider: huge, Sha: huge, Cwd: "/w", Argv: []string{"echo"}},
	} {
		p := filepath.Join(t.TempDir(), "h.jsonl")
		if err := AppendHistory(p, e, redact.New()); err != nil {
			t.Fatal(err)
		}
		b, _ := os.ReadFile(p)
		var got Event
		if len(b) > 4096 || json.Unmarshal(b, &got) != nil {
			t.Fatalf("%s: line of %d bytes", name, len(b))
		}
		if e.Job == huge && !strings.HasPrefix(huge, strings.TrimSuffix(got.Job, " [truncated]")) {
			t.Fatalf("%s: the job is not a prefix of the name: %q", name, got.Job)
		}
	}
}
