package classify

import (
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// parseShell returns the argv of every simple command in a POSIX/bash string: lists,
// pipelines, subshells, compounds and command substitutions. Redirections are not arguments.
func parseShell(src string) ([][]string, error) {
	f, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(src), "")
	if err != nil {
		return nil, err
	}
	var cmds [][]string
	syntax.Walk(f, func(n syntax.Node) bool {
		ce, ok := n.(*syntax.CallExpr)
		if !ok || len(ce.Args) == 0 {
			return true
		}
		argv := make([]string, 0, len(ce.Args)+len(ce.Assigns))
		for _, w := range ce.Args {
			argv = append(argv, wordString(w))
		}
		cmds = append(cmds, argv)
		return true
	})
	return cmds, nil
}

// wordString renders a word literally; $PWD, ${PWD}, $(pwd) and `pwd` become pwdMarker and
// any other expansion makes the whole word opaque.
func wordString(w *syntax.Word) string {
	var b strings.Builder
	opaque := false
	dbl := false
	var part func(p syntax.WordPart)
	part = func(p syntax.WordPart) {
		switch x := p.(type) {
		case *syntax.Lit:
			if dbl {
				b.WriteString(unescapeDouble(x.Value))
			} else {
				b.WriteString(unescapeBare(x.Value))
			}
		case *syntax.SglQuoted:
			if x.Dollar {
				b.WriteString(decodeANSIC(x.Value))
			} else {
				b.WriteString(x.Value)
			}
		case *syntax.DblQuoted:
			prev := dbl
			dbl = true
			for _, q := range x.Parts {
				part(q)
			}
			dbl = prev
		case *syntax.ParamExp:
			if x.Param != nil && x.Param.Value == "PWD" && x.Exp == nil && x.Slice == nil && x.Repl == nil && !x.Length && !x.Excl {
				b.WriteString(pwdMarker)
			} else {
				opaque = true
			}
		case *syntax.CmdSubst:
			if isPwdCall(x) {
				b.WriteString(pwdMarker)
			} else {
				opaque = true
			}
		default:
			opaque = true
		}
	}
	for _, p := range w.Parts {
		part(p)
	}
	if opaque {
		return opaqueMarker
	}
	return b.String()
}

func isPwdCall(cs *syntax.CmdSubst) bool {
	if len(cs.Stmts) != 1 {
		return false
	}
	ce, ok := cs.Stmts[0].Cmd.(*syntax.CallExpr)
	return ok && len(ce.Args) == 1 && ce.Args[0].Lit() == "pwd"
}

// unescapeBare removes the backslash before any character of an unquoted word (and the
// backslash-newline pair).
func unescapeBare(v string) string {
	if !strings.Contains(v, `\`) {
		return v
	}
	var b strings.Builder
	for i := 0; i < len(v); i++ {
		if v[i] == '\\' && i+1 < len(v) {
			i++
			if v[i] == '\n' {
				continue
			}
		}
		b.WriteByte(v[i])
	}
	return b.String()
}

// unescapeDouble removes the backslash only before $ ` " \ and newline, as inside "...".
func unescapeDouble(v string) string {
	if !strings.Contains(v, `\`) {
		return v
	}
	var b strings.Builder
	for i := 0; i < len(v); i++ {
		if v[i] == '\\' && i+1 < len(v) && strings.IndexByte("$`\"\\\n", v[i+1]) >= 0 {
			i++
			if v[i] == '\n' {
				continue
			}
		}
		b.WriteByte(v[i])
	}
	return b.String()
}

// decodeANSIC decodes the body of $'...' per bash: \n \t \xHH \NNN \uHHHH and so on; an
// unknown escape keeps its backslash.
func decodeANSIC(v string) string {
	var b strings.Builder
	simple := map[byte]byte{'a': 7, 'b': 8, 'e': 27, 'E': 27, 'f': 12, 'n': 10, 'r': 13, 't': 9, 'v': 11, '\\': '\\', '\'': '\'', '"': '"', '?': '?'}
	for i := 0; i < len(v); i++ {
		c := v[i]
		if c != '\\' || i+1 >= len(v) {
			b.WriteByte(c)
			continue
		}
		i++
		e := v[i]
		if r, ok := simple[e]; ok {
			b.WriteByte(r)
			continue
		}
		base, max := 0, 0
		switch {
		case e >= '0' && e <= '7':
			base, max = 8, 3
			i--
		case e == 'x':
			base, max = 16, 2
		case e == 'u':
			base, max = 16, 4
		case e == 'U':
			base, max = 16, 8
		default:
			b.WriteByte('\\')
			b.WriteByte(e)
			continue
		}
		n, got := 0, 0
		for j := i + 1; j < len(v) && got < max; j++ {
			d := hexVal(v[j])
			if d < 0 || d >= base {
				break
			}
			n = n*base + d
			got++
		}
		if got == 0 {
			b.WriteByte('\\')
			b.WriteByte(e)
			continue
		}
		i += got
		if e == 'x' || (e >= '0' && e <= '7') {
			b.WriteByte(byte(n))
		} else {
			b.WriteRune(rune(n))
		}
	}
	return b.String()
}

func hexVal(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10
	}
	return -1
}
