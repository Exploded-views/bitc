package classify

import "strings"

// leadingKeywords are reserved words a shell reads before a simple command.
var leadingKeywords = map[string]bool{"!": true, "{": true, "if": true, "then": true, "elif": true,
	"else": true, "while": true, "until": true, "do": true}

// leadingWords is the fallback for a -c or env -S string the parser rejects (spec 6.7): the
// words of its first simple command. It reads up to the first unquoted operator or redirection
// character (; & | < > ( ) or a newline), splits on blanks, removes quotes and backslashes,
// drops leading reserved words and a digit-only word glued to < or > (a file descriptor), and
// stops before a word whose quote or substitution is never closed. $PWD, ${PWD}, $(pwd) and
// `pwd` become pwdMarker as in normalizeLiteral; a word with any other $ or ` is opaque.
func leadingWords(src string) []string {
	var words []string
	var b strings.Builder
	inWord, quoted, expands := false, false, false
	flush := func() {
		if inWord {
			w := b.String()
			if expands {
				if n := normalizeLiteral([]string{w})[0]; n != w {
					w = n
				} else {
					w = opaqueMarker
				}
			}
			words = append(words, w)
		}
		b.Reset()
		inWord, quoted, expands = false, false, false
	}
loop:
	for i := 0; i < len(src); i++ {
		c := src[i]
		switch {
		case c == ' ' || c == '\t':
			flush()
		case strings.IndexByte(";&|<>()\n", c) >= 0:
			if inWord && !quoted && (c == '<' || c == '>') && strings.Trim(b.String(), "0123456789") == "" {
				inWord = false // 2>&1, 10<x: a file descriptor, not a word
			}
			flush()
			break loop
		case c == '\\':
			if i+1 < len(src) {
				i++
				if src[i] != '\n' { // a backslash-newline joins two lines
					b.WriteByte(src[i])
				}
			}
			inWord = true
		case c == '\'':
			j := strings.IndexByte(src[i+1:], '\'')
			if j < 0 {
				inWord = false
				break loop
			}
			b.WriteString(src[i+1 : i+1+j])
			i += j + 1
			inWord, quoted = true, true
		case c == '"':
			j := i + 1
			for j < len(src) && src[j] != '"' {
				if src[j] == '\\' && j+1 < len(src) {
					j++
				}
				j++
			}
			if j >= len(src) {
				inWord = false
				break loop
			}
			body := src[i+1 : j]
			if strings.ContainsAny(body, "$`") {
				expands = true
			}
			b.WriteString(unescapeDouble(body))
			i = j
			inWord, quoted = true, true
		case c == '$' && i+1 < len(src) && (src[i+1] == '(' || src[i+1] == '{'):
			end := byte(')')
			if src[i+1] == '{' {
				end = '}'
			}
			j := strings.IndexByte(src[i+2:], end)
			if j < 0 {
				inWord = false
				break loop
			}
			b.WriteString(src[i : i+3+j])
			i += 2 + j
			inWord, expands = true, true
		case c == '`':
			j := strings.IndexByte(src[i+1:], '`')
			if j < 0 {
				inWord = false
				break loop
			}
			b.WriteString(src[i : i+2+j])
			i += 1 + j
			inWord, expands = true, true
		default:
			if c == '$' {
				expands = true
			}
			b.WriteByte(c)
			inWord = true
		}
	}
	flush()
	for len(words) > 0 && leadingKeywords[words[0]] {
		words = words[1:]
	}
	return words
}
