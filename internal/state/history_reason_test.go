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

// A long reason (a build's output, an API message) is cut before argv is shortened, so every line
// stays one atomic write under 4 KiB even with no argv to shorten.
func TestHistoryLongReasonIsTruncatedWithAMarker(t *testing.T) {
	p := filepath.Join(t.TempDir(), "h.jsonl")
	reason := strings.Repeat("é", 6000) // multi-byte: the cut must stay valid UTF-8
	if err := AppendHistory(p, Event{Event: "fallback", Pool: "x", Reason: reason}, redact.New()); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	if len(b) >= 4096 {
		t.Fatalf("line of %d bytes", len(b))
	}
	var e Event
	if err := json.Unmarshal(b, &e); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(e.Reason, "[truncated]") || len(e.Reason) > 512+len(" [truncated]") || !utf8.ValidString(e.Reason) {
		t.Fatalf("reason %d bytes: %q", len(e.Reason), e.Reason[len(e.Reason)-20:])
	}
	// argv is not shortened for a long reason
	_ = AppendHistory(p, Event{Event: "fallback", Argv: []string{"echo", "x"}, Reason: reason}, redact.New())
	if strings.Contains(string(must(os.ReadFile(p))), `"argv_truncated":true`) {
		t.Fatal("argv was shortened for a long reason")
	}
}

func must(b []byte, _ error) []byte { return b }
