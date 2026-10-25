package run

import (
	"fmt"
	"strings"

	"github.com/Exploded-views/bitc/internal/config"
)

// ParseEnvOutput parses env-hook output (spec 6.5): one [export ]NAME=VALUE per line, blank
// and # lines skipped. VALUE concatenates unquoted chars (not blanks, control characters or
// ' " \ $ ` ; & | < > ( )), '...' literals and "..." with only \" \\ \` \$ escapes. No form
// admits a control character (< 0x20 or 0x7f). Never evaluated. A CR is a control character,
// so CRLF output is unparsable.
func ParseEnvOutput(b []byte) ([]KV, error) {
	var out []KV
	for n, line := range strings.Split(string(b), "\n") {
		trim := strings.TrimLeft(line, " \t")
		if trim == "" || strings.HasPrefix(trim, "#") {
			continue
		}
		kv, err := parseEnvLine(strings.TrimPrefix(trim, "export "))
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", n+1, err)
		}
		out = append(out, kv)
	}
	return out, nil
}

const forbidden = " \t'\"\\$`;&|<>()"

func parseEnvLine(s string) (KV, error) {
	name, rest, ok := strings.Cut(s, "=")
	if !ok || !config.ValidEnvName(name) {
		return KV{}, fmt.Errorf("not NAME=VALUE with a valid name (not BITC_*)")
	}
	var v strings.Builder
	for i := 0; i < len(rest); {
		c := rest[i]
		switch {
		case c == '\'':
			end := strings.IndexByte(rest[i+1:], '\'')
			if end < 0 {
				return KV{}, fmt.Errorf("unterminated '")
			}
			lit := rest[i+1 : i+1+end]
			if strings.IndexFunc(lit, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0 {
				return KV{}, fmt.Errorf("control character inside '...'")
			}
			v.WriteString(lit)
			i += end + 2
		case c == '"':
			i++
			closed := false
			for i < len(rest) {
				d := rest[i]
				if d == '"' {
					closed = true
					i++
					break
				}
				if d == '\\' && i+1 < len(rest) && strings.IndexByte("\"\\`$", rest[i+1]) >= 0 {
					v.WriteByte(rest[i+1])
					i += 2
					continue
				}
				if d == '$' || d == '`' || d < 0x20 || d == 0x7f {
					return KV{}, fmt.Errorf("a bare $ or ` (or a control character) inside \"...\"")
				}
				v.WriteByte(d)
				i++
			}
			if !closed {
				return KV{}, fmt.Errorf("unterminated \"")
			}
		case c == ' ' || c == '\t':
			if strings.TrimLeft(rest[i:], " \t") != "" {
				return KV{}, fmt.Errorf("unexpected text after the value")
			}
			i = len(rest)
		case c < 0x20 || c == 0x7f || strings.IndexByte(forbidden, c) >= 0:
			return KV{}, fmt.Errorf("character %q is not allowed unquoted", c)
		default:
			v.WriteByte(c)
			i++
		}
	}
	return KV{Name: name, Value: v.String()}, nil
}
