package conformance

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode"
)

// A Go port of crewkit's tools/check-workflows.py (crewkit commit 186a6d0), kept rule for rule:
// bitc's toolchain has no Python and go.mod has no YAML parser, so it reads workflows line by
// line exactly as the original does, and reports what it can't follow instead of skipping it.
//
// Fails on:
//   - a runs-on that isn't a GitHub-hosted label (ubuntu-*, macos-*, windows-*). Lists, block
//     lists and ${{ matrix.X }} are resolved; a runner group or any other expression is refused,
//     never guessed;
//   - a pull_request_target trigger, in any on: form;
//   - a uses: not pinned to a full 40-hex commit SHA (local ./ actions are fine; docker images
//     need an @sha256: digest).

var (
	wfHosted    = regexp.MustCompile(`^(ubuntu|macos|windows)-[A-Za-z0-9.-]+$`)
	wfPinned    = regexp.MustCompile(`^[^@\s]+@[0-9a-f]{40}$`)
	wfDigest    = regexp.MustCompile(`^docker://[^@\s]+@sha256:[0-9a-f]{64}$`)
	wfMatrixRef = regexp.MustCompile(`^\$\{\{\s*matrix\.([A-Za-z0-9_-]+)\s*\}\}$`)

	wfMatrixLine = regexp.MustCompile(`^\s*matrix:\s*$`)
	wfMatrixKey  = regexp.MustCompile(`^\s*(?:-\s+)?([A-Za-z0-9_-]+):\s*(.*)$`)
	wfItem       = regexp.MustCompile(`^\s*-\s+(.+)$`)
	wfOn         = regexp.MustCompile(`^["']?on["']?:\s*(.*)$`)
	wfPRTarget   = regexp.MustCompile(`(^|[\s\[,'"-])pull_request_target([\s\]:,'"]|$)`)
	wfUses       = regexp.MustCompile(`^\s*(?:-\s+)?uses:\s*(.+)$`)
	wfRunsOn     = regexp.MustCompile(`^\s*runs-on:\s*(.*)$`)
)

// wfStripComment drops a trailing # comment (outside quotes).
func wfStripComment(line string) string {
	r := []rune(line)
	var out []rune
	var quote rune
	for i, ch := range r {
		if quote != 0 {
			if ch == quote {
				quote = 0
			}
		} else if ch == '\'' || ch == '"' {
			quote = ch
		} else if ch == '#' && (i == 0 || unicode.IsSpace(r[i-1])) {
			break
		}
		out = append(out, ch)
	}
	return strings.TrimRightFunc(string(out), unicode.IsSpace)
}

func wfUnquote(v string) string {
	v = strings.TrimSpace(v)
	if len(v) >= 2 && v[0] == v[len(v)-1] && (v[0] == '\'' || v[0] == '"') {
		return v[1 : len(v)-1]
	}
	return v
}

func wfIndent(line string) int { return len(line) - len(strings.TrimLeft(line, " ")) }

func wfFlowList(v string) []string {
	r := []rune(strings.TrimSpace(v))
	inner := ""
	if len(r) >= 2 { // Python v[1:-1]: "" for a lone "["
		inner = string(r[1 : len(r)-1])
	}
	var out []string
	for _, x := range strings.Split(inner, ",") {
		if strings.TrimSpace(x) != "" {
			out = append(out, wfUnquote(x))
		}
	}
	return out
}

type wfLine struct {
	n    int
	text string
}

// wfBlock returns the lines indented deeper than lines[i], until the indentation returns.
func wfBlock(lines []string, i int) []wfLine {
	base := wfIndent(lines[i])
	var out []wfLine
	for j := i + 1; j < len(lines); j++ {
		if strings.TrimSpace(lines[j]) == "" {
			continue
		}
		if wfIndent(lines[j]) <= base {
			break
		}
		out = append(out, wfLine{j, lines[j]})
	}
	return out
}

// wfMatrixValues returns {key: [values]} from every matrix: block in the file, include: entries too.
func wfMatrixValues(lines []string) map[string][]string {
	vals := map[string][]string{}
	for i, line := range lines {
		if !wfMatrixLine.MatchString(line) {
			continue
		}
		body := wfBlock(lines, i)
		for _, b := range body {
			m := wfMatrixKey.FindStringSubmatch(b.text)
			if m == nil || m[1] == "include" || m[1] == "exclude" {
				continue
			}
			key, v := m[1], strings.TrimSpace(m[2])
			switch {
			case strings.HasPrefix(v, "["):
				vals[key] = append(vals[key], wfFlowList(v)...)
			case v != "":
				vals[key] = append(vals[key], wfUnquote(v))
			default:
				for _, item := range wfBlock(lines, b.n) {
					if im := wfItem.FindStringSubmatch(item.text); im != nil {
						vals[key] = append(vals[key], wfUnquote(im[1]))
					}
				}
			}
		}
	}
	return vals
}

func wfCheckLabel(label string, matrix map[string][]string, report func(string)) {
	if m := wfMatrixRef.FindStringSubmatch(label); m != nil {
		values := matrix[m[1]]
		if len(values) == 0 {
			report(fmt.Sprintf("runs-on %s: can't resolve matrix.%s; list its values in strategy.matrix", label, m[1]))
			return
		}
		for _, v := range values {
			wfCheckLabel(v, map[string][]string{}, report)
		}
		return
	}
	if strings.Contains(label, "${{") {
		report(fmt.Sprintf("runs-on %s: can't resolve this expression; use a GitHub-hosted label", label))
	} else if !wfHosted.MatchString(label) {
		report(fmt.Sprintf("runs-on %s: not a GitHub-hosted label (ubuntu-*, macos-*, windows-*); self-hosted runners are not allowed", label))
	}
}

// checkWorkflow returns the findings for one workflow file's text, each as path:line: message.
func checkWorkflow(path, text string) []string {
	var findings []string
	raw := strings.Split(strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n"), "\n")
	if len(raw) > 0 && raw[len(raw)-1] == "" { // splitlines() drops the final empty piece
		raw = raw[:len(raw)-1]
	}
	lines := make([]string, len(raw))
	for i, l := range raw {
		lines[i] = wfStripComment(l)
	}
	matrix := wfMatrixValues(lines)
	at := func(n int) func(string) {
		return func(msg string) { findings = append(findings, fmt.Sprintf("%s:%d: %s", path, n+1, msg)) }
	}

	for i, line := range lines {
		report := at(i)

		// Triggers: the top-level on: key, inline or as a block.
		if m := wfOn.FindStringSubmatch(line); m != nil {
			region := []string{m[1]}
			for _, b := range wfBlock(lines, i) {
				region = append(region, b.text)
			}
			for _, t := range region {
				if wfPRTarget.MatchString(t) {
					report("pull_request_target runs fork code with this repo's secrets; use pull_request")
					break
				}
			}
		}

		if m := wfUses.FindStringSubmatch(line); m != nil {
			ref := wfUnquote(m[1])
			switch {
			case strings.HasPrefix(ref, "./"):
			case strings.HasPrefix(ref, "docker://"):
				if !wfDigest.MatchString(ref) {
					report(fmt.Sprintf("uses %s: pin the image by digest (@sha256:<64 hex>)", ref))
				}
			case !wfPinned.MatchString(ref):
				report(fmt.Sprintf("uses %s: not pinned to a full commit SHA", ref))
			}
		}

		if m := wfRunsOn.FindStringSubmatch(line); m != nil {
			v := strings.TrimSpace(m[1])
			switch {
			case strings.HasPrefix(v, "{"):
				report("runs-on uses a runner group or mapping; only GitHub-hosted labels are allowed")
			case strings.HasPrefix(v, "["):
				for _, label := range wfFlowList(v) {
					wfCheckLabel(label, matrix, report)
				}
			case v != "":
				wfCheckLabel(wfUnquote(v), matrix, report)
			default:
				body := wfBlock(lines, i)
				if len(body) == 0 {
					report("runs-on is empty")
				}
				for _, b := range body {
					if im := wfItem.FindStringSubmatch(b.text); im != nil {
						wfCheckLabel(wfUnquote(im[1]), matrix, at(b.n))
					} else {
						report("runs-on uses a runner group or mapping; only GitHub-hosted labels are allowed")
						break
					}
				}
			}
		}
	}
	return findings
}

// checkWorkflowDir lints dir/*.yml and dir/*.yaml; no workflow files at all is an error.
func checkWorkflowDir(dir string) (int, []string, error) {
	var paths []string
	for _, pat := range []string{"*.yml", "*.yaml"} {
		m, err := filepath.Glob(filepath.Join(dir, pat))
		if err != nil {
			return 0, nil, err
		}
		paths = append(paths, m...)
	}
	sort.Strings(paths)
	if len(paths) == 0 {
		return 0, nil, errors.New("check-workflows: no workflow files found in " + dir)
	}
	var findings []string
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			return 0, nil, fmt.Errorf("check-workflows: cannot read %s: %w", p, err)
		}
		findings = append(findings, checkWorkflow(p, string(b))...)
	}
	return len(paths), findings, nil
}
