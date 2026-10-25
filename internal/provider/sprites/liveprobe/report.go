package liveprobe

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
)

// Redactor replaces every secret string with [redacted]. Short strings are ignored (they would
// shred ordinary text).
type Redactor struct{ secrets []string }

// NewRedactor keeps the secrets of 8 or more characters, longest first.
func NewRedactor(secrets ...string) *Redactor {
	r := &Redactor{}
	for _, s := range secrets {
		if len(s) >= 8 {
			r.secrets = append(r.secrets, s)
		}
	}
	sort.Slice(r.secrets, func(i, j int) bool { return len(r.secrets[i]) > len(r.secrets[j]) })
	return r
}

// String redacts s.
func (r *Redactor) String(s string) string {
	for _, sec := range r.secrets {
		s = strings.ReplaceAll(s, sec, "[redacted]")
	}
	return s
}

// Writer wraps w so that every write is redacted (a secret split across two writes is not caught,
// so callers write whole lines).
func (r *Redactor) Writer(w io.Writer) io.Writer { return redactWriter{r, w} }

type redactWriter struct {
	r *Redactor
	w io.Writer
}

func (rw redactWriter) Write(p []byte) (int, error) {
	if _, err := io.WriteString(rw.w, rw.r.String(string(p))); err != nil {
		return 0, err
	}
	return len(p), nil
}

// value redacts every string inside an evidence value, by a JSON round trip.
func (r *Redactor) value(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	b, err := json.Marshal(m)
	if err != nil {
		return map[string]any{"evidence": "[unmarshalable]"}
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(r.String(string(b))), &out); err != nil {
		// a secret with a JSON-special character broke the text: redact the strings one by one
		_ = json.Unmarshal(b, &out)
		out = r.walk(out).(map[string]any)
	}
	return out
}

func (r *Redactor) walk(v any) any {
	switch t := v.(type) {
	case string:
		return r.String(t)
	case map[string]any:
		for k, x := range t {
			t[k] = r.walk(x)
		}
		return t
	case []any:
		for i, x := range t {
			t[i] = r.walk(x)
		}
		return t
	}
	return v
}

// Clean returns res with the finding and the evidence redacted.
func (r *Redactor) Clean(res Result) Result {
	res.Finding = r.String(res.Finding)
	res.Evidence = r.value(res.Evidence)
	return res
}

// WriteJSONL writes one JSON line per probe, in the order the probes ran.
func WriteJSONL(w io.Writer, results []Result) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	for _, r := range results {
		if r.Evidence == nil {
			r.Evidence = map[string]any{}
		}
		if err := enc.Encode(r); err != nil {
			return err
		}
	}
	return nil
}

func cell(s string) string {
	s = strings.NewReplacer("|", "\\|", "\n", " ", "\r", " ").Replace(s)
	return strings.TrimSpace(s)
}

// WriteMarkdown writes the runbook's Results table (one row per probe, by item number), the
// Sprites the run made and the totals.
func WriteMarkdown(w io.Writer, rep Report) {
	rs := append([]Result(nil), rep.Results...)
	sort.Slice(rs, func(i, j int) bool { return rs[i].ID < rs[j].ID })
	fmt.Fprintf(w, "| Item | Status | Finding | Sprite-s |\n| --- | --- | --- | --- |\n")
	for _, r := range rs {
		fmt.Fprintf(w, "| %d | %s | %s | %.0f |\n", r.ID, r.Status, cell(r.Finding), r.SpriteSeconds)
	}
	fmt.Fprintf(w, "\nSprites created (run %s):\n\n| Name | Created | Deleted | Confirmed | Seconds |\n| --- | --- | --- | --- | --- |\n", rep.Run)
	for _, s := range rep.Sprites {
		del := "-"
		if !s.Deleted.IsZero() {
			del = s.Deleted.UTC().Format("15:04:05")
		}
		fmt.Fprintf(w, "| %s | %s | %s | %v | %.0f |\n", s.Name, s.Created.UTC().Format("15:04:05"), del, s.DeleteOK, s.Seconds)
	}
	fmt.Fprintf(w, "\nTotal spend: $%.4f\n", rep.Spent)
	if len(rep.Preexisting) > 0 {
		fmt.Fprintf(w, "\nEarlier-run Sprites found and deleted before the first create: %s\n", strings.Join(rep.Preexisting, ", "))
	}
	if len(rep.Leftovers) > 0 {
		fmt.Fprintf(w, "\nLEFTOVER SPRITES (loud failure, exit 3): %s\n", strings.Join(rep.Leftovers, ", "))
	}
	if rep.Fatal != "" {
		fmt.Fprintf(w, "\nFATAL: %s\n", rep.Fatal)
	}
}

// Markdown renders the report to a string.
func Markdown(rep Report) string {
	var b bytes.Buffer
	WriteMarkdown(&b, rep)
	return b.String()
}
