// Package redact keeps secret values out of every bitc log sink (spec 10).
package redact

import (
	"bytes"
	"io"
	"sort"
	"strings"
	"sync"
)

// Placeholder replaces every secret.
const Placeholder = "<redacted>"

// Set is the set of secret values to redact. It is safe for concurrent use.
type Set struct {
	mu      sync.RWMutex
	secrets []string // longest first
}

func New() *Set { return &Set{} }

// Add registers a secret; empty strings are ignored.
func (s *Set) Add(secret string) {
	if secret == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.secrets = append(s.secrets, secret)
	sort.Slice(s.secrets, func(i, j int) bool { return len(s.secrets[i]) > len(s.secrets[j]) })
}

// String replaces every occurrence of every secret.
func (s *Set) String(x string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, sec := range s.secrets {
		x = strings.ReplaceAll(x, sec, Placeholder)
	}
	return x
}

// Contains reports whether any secret occurs in x.
func (s *Set) Contains(x string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, sec := range s.secrets {
		if strings.Contains(x, sec) {
			return true
		}
	}
	return false
}

// Env drops entries whose value contains a secret.
func (s *Set) Env(env []string) []string {
	var out []string
	for _, kv := range env {
		_, v, _ := strings.Cut(kv, "=")
		if !s.Contains(v) {
			out = append(out, kv)
		}
	}
	return out
}

func (s *Set) maxLen() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.secrets) == 0 {
		return 0
	}
	return len(s.secrets[0])
}

// Writer redacts a stream. It writes whole lines as they complete; a line longer than 64 KiB
// is written in pieces, holding back enough bytes that a secret split across a piece boundary
// is still caught. Call Flush at the end.
type Writer struct {
	set     *Set
	w       io.Writer
	mu      sync.Mutex
	pending []byte
}

func (s *Set) Writer(w io.Writer) *Writer { return &Writer{set: s, w: w} }

const maxPending = 64 << 10

func (w *Writer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.pending = append(w.pending, p...)
	if i := bytes.LastIndexByte(w.pending, '\n'); i >= 0 {
		if err := w.emit(w.pending[:i+1]); err != nil {
			return 0, err
		}
		w.pending = append([]byte(nil), w.pending[i+1:]...)
	}
	if len(w.pending) > maxPending {
		// hold back the tail, and never cut inside a secret occurrence
		cut := safeCut(w.pending, len(w.pending)-w.set.maxLen(), w.set)
		if err := w.emit(w.pending[:cut]); err != nil {
			return 0, err
		}
		w.pending = append([]byte(nil), w.pending[cut:]...)
	}
	return len(p), nil
}

// Flush writes what is pending, redacted.
func (w *Writer) Flush() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	err := w.emit(w.pending)
	w.pending = nil
	return err
}

func (w *Writer) emit(b []byte) error {
	if len(b) == 0 {
		return nil
	}
	_, err := w.w.Write([]byte(w.set.String(string(b))))
	return err
}

// safeCut moves cut back to the start of any secret occurrence that straddles it.
func safeCut(buf []byte, cut int, s *Set) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, sec := range s.secrets {
		lo := cut - len(sec) + 1
		if lo < 0 {
			lo = 0
		}
		if i := bytes.Index(buf[lo:], []byte(sec)); i >= 0 && lo+i < cut {
			cut = lo + i
		}
	}
	return cut
}
