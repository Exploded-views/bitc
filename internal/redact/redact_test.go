package redact

import (
	"bytes"
	"strings"
	"testing"
)

func TestString(t *testing.T) {
	s := New()
	s.Add("sk-abc123")
	s.Add("")
	s.Add("tok")
	got := s.String("key sk-abc123 and tok and sk-abc123tok")
	if got != "key <redacted> and <redacted> and <redacted><redacted>" {
		t.Fatal(got)
	}
	if !s.Contains("x sk-abc123") || s.Contains("nothing") {
		t.Fatal("Contains")
	}
}

func TestLongestSecretFirst(t *testing.T) {
	s := New()
	s.Add("abc")
	s.Add("abcdef")
	if got := s.String("abcdef"); got != "<redacted>" {
		t.Fatal(got)
	}
}

func TestWriterSplitsAcrossWrites(t *testing.T) {
	var buf bytes.Buffer
	s := New()
	s.Add("SECRET-VALUE")
	w := s.Writer(&buf)
	for _, chunk := range []string{"line one SEC", "RET-VAL", "UE end\nsecond SECRET", "-VALUE"} {
		if _, err := w.Write([]byte(chunk)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	if got := buf.String(); got != "line one <redacted> end\nsecond <redacted>" {
		t.Fatalf("%q", got)
	}
}

func TestWriterFlushesLongLinesWithoutLeaking(t *testing.T) {
	var buf bytes.Buffer
	s := New()
	s.Add("0123456789")
	w := s.Writer(&buf)
	big := strings.Repeat("x", 70_000) + "0123456789"
	_, _ = w.Write([]byte(big[:69_995]))
	_, _ = w.Write([]byte(big[69_995:]))
	_ = w.Flush()
	if strings.Contains(buf.String(), "0123456789") || !strings.HasSuffix(buf.String(), "<redacted>") {
		t.Fatal("leaked or lost")
	}
}

// One write over 64 KiB ends with the whole secret plus fewer than len(secret) bytes, so the
// naive hold-back cut (len - 10) falls inside the secret: safeCut must move it to the secret's start.
func TestWriterNeverCutsInsideASecret(t *testing.T) {
	var buf bytes.Buffer
	s := New()
	s.Add("0123456789")
	w := s.Writer(&buf)
	x := strings.Repeat("x", 70_000)
	_, _ = w.Write([]byte(x + "0123456789" + "abc"))
	if got := buf.String(); got != x {
		t.Fatalf("before Flush: %d bytes, tail %q", len(got), got[max(0, len(got)-12):])
	}
	_ = w.Flush()
	if got := buf.String(); got != x+"<redacted>abc" || strings.Contains(got, "0123456789") {
		t.Fatalf("after Flush: tail %q", got[max(0, len(got)-20):])
	}
}

func TestEnvDropsSecretValues(t *testing.T) {
	s := New()
	s.Add("k1")
	got := s.Env([]string{"A=1", "TOKEN=k1", "B=xk1x"})
	if strings.Join(got, ",") != "A=1" {
		t.Fatal(got)
	}
}
