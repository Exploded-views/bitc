package liveprobe

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestRedactorReplacesSecretsAndIgnoresShortOnes(t *testing.T) {
	r := NewRedactor("sandbox-org/abcdef123456", "short", "0123456789abcdef")
	got := r.String("token sandbox-org/abcdef123456 marker 0123456789abcdef short")
	if got != "token [redacted] marker [redacted] short" {
		t.Fatalf("redacted: %q", got)
	}
	var b bytes.Buffer
	_, _ = r.Writer(&b).Write([]byte("x sandbox-org/abcdef123456 y\n"))
	if b.String() != "x [redacted] y\n" {
		t.Fatalf("writer: %q", b.String())
	}
}

func TestCleanRedactsEvidenceDeep(t *testing.T) {
	r := NewRedactor("sandbox-org/abcdef123456")
	res := r.Clean(Result{ID: 1, Finding: "saw sandbox-org/abcdef123456", Evidence: map[string]any{
		"a": "sandbox-org/abcdef123456", "b": []string{"x", "sandbox-org/abcdef123456"}, "c": map[string]any{"d": "sandbox-org/abcdef123456"}, "n": 3}})
	b, _ := json.Marshal(res)
	if strings.Contains(string(b), "abcdef123456") {
		t.Fatalf("the secret survived: %s", b)
	}
	if n, _ := res.Evidence["n"].(float64); n != 3 {
		t.Fatalf("evidence changed: %v", res.Evidence)
	}
	if Cleaned := r.Clean(Result{}); Cleaned.Evidence == nil {
		t.Fatal("a nil evidence must become an empty object")
	}
}

func TestWriteJSONLOneLinePerProbe(t *testing.T) {
	var b bytes.Buffer
	if err := WriteJSONL(&b, []Result{{ID: 24, Status: Answered, Finding: "x86_64 <ok> & more", SpriteSeconds: 12.5}, {ID: 3, Status: Skipped}}); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(b.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("%d lines: %q", len(lines), b.String())
	}
	var r Result
	if err := json.Unmarshal([]byte(lines[0]), &r); err != nil || r.ID != 24 || r.Status != "answered" || r.SpriteSeconds != 12.5 || !strings.Contains(r.Finding, "<ok> &") {
		t.Fatalf("line 0: %v %+v", err, r)
	}
	for _, key := range []string{`"id"`, `"status"`, `"finding"`, `"evidence"`, `"sprite_seconds"`} {
		if !strings.Contains(lines[1], key) {
			t.Errorf("line 1 lacks %s: %s", key, lines[1])
		}
	}
}

func TestWriteMarkdownTable(t *testing.T) {
	t0 := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	rep := Report{Run: "r1", Spent: 0.0123, Results: []Result{{ID: 15, Status: Answered, Finding: "a | b\nc", SpriteSeconds: 20}, {ID: 3, Status: AssumeWorst, Finding: "w"}},
		Sprites:   []SpriteRec{{Name: "bitcproof-s0-r1-1", Created: t0, Deleted: t0.Add(time.Minute), DeleteOK: true, Seconds: 60}},
		Leftovers: []string{"bitcproof-s0-r1-9"}, Preexisting: []string{"bitcproof-s0-old-1"}}
	md := Markdown(rep)
	i3, i15 := strings.Index(md, "| 3 |"), strings.Index(md, "| 15 |")
	if i3 < 0 || i15 < i3 {
		t.Fatalf("rows are not sorted by item:\n%s", md)
	}
	for _, want := range []string{"| 15 | answered | a \\| b c | 20 |", "| bitcproof-s0-r1-1 | 12:00:00 | 12:01:00 | true | 60 |", "Total spend: $0.0123", "LEFTOVER SPRITES (loud failure, exit 3): bitcproof-s0-r1-9", "bitcproof-s0-old-1"} {
		if !strings.Contains(md, want) {
			t.Errorf("the table lacks %q:\n%s", want, md)
		}
	}
}
