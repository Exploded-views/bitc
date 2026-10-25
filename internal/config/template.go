package config

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Part is one piece of a template: a literal, or {Var}, {Var+Arg} or {Var-Arg}.
// {sha256:<path>} is Var "sha256" with Arg <path> and Op ":".
type Part struct {
	Lit string
	Var string
	Op  string // "", "+", "-", ":"
	Arg string
}

// HostOnlyVars exist only on the host; RemoteOnlyVars only in a remote job (spec 5.7).
var (
	HostOnlyVars   = map[string]bool{"slot": true, "slots": true, "hold": true, "state_dir": true, "worktree": true, "cwd": true}
	RemoteOnlyVars = map[string]bool{"week": true, "sha256": true}
)

var (
	varRE    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	exprRE   = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]*)(?:([+-])((?:0|[1-9][0-9]*)|[A-Za-z_][A-Za-z0-9_]*))?$`)
	shaVarRE = regexp.MustCompile(`^sha256:(.+)$`)
)

// ParseTemplate splits raw into parts; {{ and }} are literal braces.
func ParseTemplate(raw string) ([]Part, error) {
	var parts []Part
	var lit strings.Builder
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		switch {
		case c == '{' && i+1 < len(raw) && raw[i+1] == '{':
			lit.WriteByte('{')
			i++
		case c == '}' && i+1 < len(raw) && raw[i+1] == '}':
			lit.WriteByte('}')
			i++
		case c == '}':
			return nil, fmt.Errorf("unmatched } at byte %d", i)
		case c == '{':
			end := strings.IndexByte(raw[i+1:], '}')
			if end < 0 {
				return nil, fmt.Errorf("unclosed { at byte %d", i)
			}
			expr := raw[i+1 : i+1+end]
			p, err := parseExpr(expr)
			if err != nil {
				return nil, err
			}
			if lit.Len() > 0 {
				parts = append(parts, Part{Lit: lit.String()})
				lit.Reset()
			}
			parts = append(parts, p)
			i += end + 1
		default:
			lit.WriteByte(c)
		}
	}
	if lit.Len() > 0 {
		parts = append(parts, Part{Lit: lit.String()})
	}
	return parts, nil
}

func parseExpr(expr string) (Part, error) {
	if expr == "" {
		return Part{}, errors.New("empty {} in template")
	}
	if m := shaVarRE.FindStringSubmatch(expr); m != nil {
		return Part{Var: "sha256", Op: ":", Arg: m[1]}, nil
	}
	m := exprRE.FindStringSubmatch(expr)
	if m == nil {
		return Part{}, fmt.Errorf("malformed template expression {%s} (use {name}, {name+N} or {name-N})", expr)
	}
	return Part{Var: m[1], Op: m[2], Arg: m[3]}, nil
}

// Names lists every variable a template uses, in order (the right side of arithmetic too).
func (t Template) Names() ([]string, error) {
	parts, err := ParseTemplate(t.Raw)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, p := range parts {
		if p.Var == "" {
			continue
		}
		names = append(names, p.Var)
		if p.Op != ":" && p.Arg != "" && varRE.MatchString(p.Arg) {
			names = append(names, p.Arg)
		}
	}
	return names, nil
}

// Expand substitutes vars. Remote-only variables are an error here (the agent expands them).
func (t Template) Expand(v Vars) (string, error) {
	parts, err := ParseTemplate(t.Raw)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, p := range parts {
		if p.Var == "" {
			b.WriteString(p.Lit)
			continue
		}
		if RemoteOnlyVars[p.Var] {
			return "", fmt.Errorf("{%s} is a remote-only template variable", p.Var)
		}
		val, ok := v[p.Var]
		if !ok {
			return "", fmt.Errorf("unknown template variable %s", p.Var)
		}
		if p.Op == "" {
			b.WriteString(val)
			continue
		}
		left, err := strconv.ParseInt(val, 10, 64)
		if err != nil || !intRE.MatchString(val) {
			return "", fmt.Errorf("{%s%s%s}: %s is not an integer (%q)", p.Var, p.Op, p.Arg, p.Var, val)
		}
		rightTxt := p.Arg
		if varRE.MatchString(p.Arg) {
			r, ok := v[p.Arg]
			if !ok {
				return "", fmt.Errorf("unknown template variable %s", p.Arg)
			}
			rightTxt = r
		}
		right, err := strconv.ParseInt(rightTxt, 10, 64)
		if err != nil || !intRE.MatchString(rightTxt) {
			return "", fmt.Errorf("{%s%s%s}: %s is not an integer (%q)", p.Var, p.Op, p.Arg, p.Arg, rightTxt)
		}
		if p.Op == "+" {
			b.WriteString(strconv.FormatInt(left+right, 10))
		} else {
			b.WriteString(strconv.FormatInt(left-right, 10))
		}
	}
	return b.String(), nil
}

var (
	packageRE = regexp.MustCompile(`^[a-z0-9][a-z0-9+.-]*$`)
	sha256RE  = regexp.MustCompile(`^[0-9a-f]{64}$`)
	// toolNameRE is a path component the agent joins under /opt/bitc/tools as root: no '/', no leading '.'.
	// internal/agent keeps a copy (toolNameRE); keep them equal.
	toolNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
)
