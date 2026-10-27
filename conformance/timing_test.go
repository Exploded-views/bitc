package conformance

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The timing lint (TIM1): test code must not pass or fail on how fast a machine schedules a
// timer. It flags, in every _test.go file and every test-only helper package (the harness, the
// fakes, the stub plugin, the *test packages), outside conformance/timing-allow.txt:
//
//	sleep    a time.Sleep, of any duration;
//	timer    any time.After, time.Tick, time.NewTimer, time.NewTicker or time.AfterFunc (a read of
//	         one in a select, a blocking <-time.After, or one kept in a variable alike);
//	ctx      a context.WithTimeout or context.WithDeadline;
//	scale    a TimeScale below 1 in a test config or rig (a TimeScale field, a time_scale key in a
//	         config string, or a ("time_scale", "0.x") call such as h.SetSeam);
//	elapsed  a verdict on elapsed time: a time.Since or a Time.Sub compared with a bound, directly,
//	         through a method on it (.Seconds()) or through a variable it was assigned to.
//
// The time and context functions are found under any import name, a dot import included.
//
// Polling loops with a fixed deadline whose expiry only fails the test (a safety net) are allowed
// by convention and review, not flagged: telling them from a loop whose expiry changes the
// verdict needs data flow this lint does not do.
//
// Each allowlist line is "<file>:<func> <pattern> <reason>", one per site. A site is a function
// and a pattern, not a line: a function with several matches of one pattern is one site, so a new
// match of a pattern inside a function already listed for it is not flagged (review catches that).
// The list may only shrink: timingAllowCap is its size, an entry whose site is gone fails, a list
// shorter than the cap asks for the cap to be lowered, and the cap may not rise above the cap at
// the merge base with main (TestTIM1_CapOnlyGoesDown).
const timingAllowCap = 151 // the allowlist's size; it may only go down

// timingHelperDirs are the non-test packages that exist only for tests.
var timingHelperDirs = []string{
	"conformance/", "internal/proctest/", "internal/classify/classifytest/", "internal/agent/agenttest/",
	"internal/provider/fake/", "internal/provider/plugin/stubplugin/", "internal/provider/sprites/spritesfake/",
	"internal/provider/ssh/sshfake/", "internal/provider/runpod/runpodfake/",
}

type timingSite struct{ file, fn, pattern string }

func (s timingSite) key() string { return s.file + ":" + s.fn + " " + s.pattern }

// timingScan reports the sites in one Go source file (path is repo-relative, for the report).
func timingScan(path string, src []byte) ([]timingSite, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, src, 0)
	if err != nil {
		return nil, err
	}
	timeName, ctxName := "", ""
	for _, im := range f.Imports {
		p, _ := strconv.Unquote(im.Path.Value)
		name := filepath.Base(p)
		if im.Name != nil {
			name = im.Name.Name
		}
		switch p {
		case "time":
			timeName = name
		case "context":
			ctxName = name
		}
	}
	seen := map[string]bool{}
	var out []timingSite
	add := func(fn, pattern string) {
		s := timingSite{path, fn, pattern}
		if !seen[s.key()] {
			seen[s.key()] = true
			out = append(out, s)
		}
	}
	pkgCall := func(n ast.Node, pkg string, names ...string) bool { // pkg "." is a dot import
		c, ok := n.(*ast.CallExpr)
		if !ok || pkg == "" {
			return false
		}
		var fun string
		switch f := c.Fun.(type) {
		case *ast.Ident:
			if pkg != "." {
				return false
			}
			fun = f.Name
		case *ast.SelectorExpr:
			id, ok := f.X.(*ast.Ident)
			if !ok || id.Name != pkg {
				return false
			}
			fun = f.Sel.Name
		default:
			return false
		}
		for _, n := range names {
			if fun == n {
				return true
			}
		}
		return false
	}
	scan := func(fn string, root ast.Node) {
		vars := map[string]bool{} // names assigned an elapsed time in this declaration
		var elapsed func(e ast.Expr) bool
		elapsed = func(e ast.Expr) bool { // time.Since(x), x.Sub(y), a method on one (.Seconds()), or a var holding one
			switch x := e.(type) {
			case *ast.ParenExpr:
				return elapsed(x.X)
			case *ast.Ident:
				return vars[x.Name]
			case *ast.CallExpr:
				if pkgCall(x, timeName, "Since") {
					return true
				}
				sel, ok := x.Fun.(*ast.SelectorExpr)
				return ok && (sel.Sel.Name == "Sub" && len(x.Args) == 1 || elapsed(sel.X))
			}
			return false
		}
		ast.Inspect(root, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.AssignStmt:
				for i, l := range x.Lhs {
					if id, ok := l.(*ast.Ident); ok && len(x.Lhs) == len(x.Rhs) && elapsed(x.Rhs[i]) {
						vars[id.Name] = true
					}
				}
			case *ast.ValueSpec:
				for i, id := range x.Names {
					if i < len(x.Values) && elapsed(x.Values[i]) {
						vars[id.Name] = true
					}
				}
			}
			return true
		})
		ast.Inspect(root, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.CallExpr:
				switch {
				case pkgCall(x, timeName, "Sleep"):
					add(fn, "sleep")
				case pkgCall(x, timeName, "After", "Tick", "NewTimer", "NewTicker", "AfterFunc"):
					add(fn, "timer")
				case pkgCall(x, ctxName, "WithTimeout", "WithDeadline", "WithTimeoutCause", "WithDeadlineCause"):
					add(fn, "ctx")
				case len(x.Args) == 2 && stringLit(x.Args[0]) == "time_scale" && scaleBelowOne(stringLit(x.Args[1])):
					add(fn, "scale") // h.SetSeam("time_scale", "0.05")
				}
			case *ast.KeyValueExpr:
				if k, ok := x.Key.(*ast.Ident); ok && k.Name == "TimeScale" && belowOne(x.Value) {
					add(fn, "scale")
				}
			case *ast.AssignStmt:
				for i, l := range x.Lhs {
					if sel, ok := l.(*ast.SelectorExpr); ok && sel.Sel.Name == "TimeScale" && i < len(x.Rhs) && belowOne(x.Rhs[i]) {
						add(fn, "scale")
					}
				}
			case *ast.BasicLit:
				if x.Kind == token.STRING {
					for _, m := range timeScaleKey.FindAllStringSubmatch(x.Value, -1) {
						if scaleBelowOne(m[1]) {
							add(fn, "scale")
						}
					}
				}
			case *ast.BinaryExpr:
				switch x.Op {
				case token.GTR, token.GEQ, token.LSS, token.LEQ:
					if elapsed(x.X) || elapsed(x.Y) {
						add(fn, "elapsed")
					}
				}
			}
			return true
		})
	}
	for _, d := range f.Decls {
		switch d := d.(type) {
		case *ast.FuncDecl:
			name := d.Name.Name
			if d.Recv != nil && len(d.Recv.List) == 1 {
				t := d.Recv.List[0].Type
				if st, ok := t.(*ast.StarExpr); ok {
					t = st.X
				}
				if id, ok := t.(*ast.Ident); ok {
					name = id.Name + "." + name
				}
			}
			scan(name, d)
		default:
			scan("_", d)
		}
	}
	return out, nil
}

// timeScaleKey matches a time_scale key's value in TOML text.
var timeScaleKey = regexp.MustCompile(`time_scale\s*=\s*([0-9]*\.?[0-9]+)`)

func scaleBelowOne(v string) bool {
	f, err := strconv.ParseFloat(v, 64)
	return err == nil && f < 1
}

// stringLit is a string literal's value, or "" for anything else.
func stringLit(e ast.Expr) string {
	b, ok := e.(*ast.BasicLit)
	if !ok || b.Kind != token.STRING {
		return ""
	}
	v, _ := strconv.Unquote(b.Value)
	return v
}

// belowOne: a float or integer literal below 1 (0.05, 0), or a constant expression of them.
func belowOne(e ast.Expr) bool {
	b, ok := e.(*ast.BasicLit)
	if !ok || (b.Kind != token.FLOAT && b.Kind != token.INT) {
		return false
	}
	v, err := strconv.ParseFloat(b.Value, 64)
	return err == nil && v < 1
}

// timingTestCode reports whether a repo-relative .go path is test code.
func timingTestCode(path string) bool {
	if strings.HasSuffix(path, "_test.go") {
		return true
	}
	for _, d := range timingHelperDirs {
		if strings.HasPrefix(path, d) {
			return true
		}
	}
	return false
}

func timingRepoSites(t *testing.T) []timingSite {
	t.Helper()
	var sites []timingSite
	err := filepath.WalkDir("..", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel("..", p)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			switch {
			case rel != "." && strings.HasPrefix(d.Name(), "."), d.Name() == "testdata", rel == "dist", strings.HasPrefix(rel, "examples/"):
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(rel, ".go") || !timingTestCode(rel) || rel == "conformance/timing_test.go" { // its fixtures are strings
			return nil
		}
		src, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		s, err := timingScan(rel, src)
		sites = append(sites, s...)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return sites
}

func timingAllowlist(t *testing.T) map[string]string {
	t.Helper()
	f, err := os.Open("timing-allow.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	allow := map[string]string{}
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.SplitN(line, " ", 3)
		if len(fields) < 3 || strings.TrimSpace(fields[2]) == "" {
			t.Errorf("timing-allow.txt:%d: want \"<file>:<func> <pattern> <reason>\"", n)
			continue
		}
		key := fields[0] + " " + fields[1]
		if _, dup := allow[key]; dup {
			t.Errorf("timing-allow.txt:%d: %s is listed twice", n, key)
		}
		allow[key] = fields[2]
	}
	return allow
}

// timingFile, when set, lints that one file (any path) against the allowlist instead of the repo:
// `go test ./conformance -run TestTIM1_NoTimingMarginsInTests -args -timing-file=PATH -bin=x`.
var timingFile = flag.String("timing-file", "", "lint one Go file instead of the repository (TIM1)")

func TestTIM1_NoTimingMarginsInTests(t *testing.T) {
	if *timingFile != "" {
		src, err := os.ReadFile(*timingFile)
		if err != nil {
			t.Fatal(err)
		}
		sites, err := timingScan(filepath.Base(*timingFile), src)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range sites {
			t.Errorf("timing margin: %s", s.key())
		}
		return
	}
	allow := timingAllowlist(t)
	found := map[string]bool{}
	var bad []string
	for _, s := range timingRepoSites(t) {
		found[s.key()] = true
		if _, ok := allow[s.key()]; !ok {
			bad = append(bad, s.key())
		}
	}
	sort.Strings(bad)
	for _, b := range bad {
		t.Errorf("timing margin outside timing-allow.txt: %s (make it deterministic, or allowlist it with a reason)", b)
	}
	var stale []string
	for k := range allow {
		if !found[k] {
			stale = append(stale, k)
		}
	}
	sort.Strings(stale)
	for _, k := range stale {
		t.Errorf("timing-allow.txt: %s matches nothing any more: remove it and lower timingAllowCap", k)
	}
	switch n := len(allow); {
	case n > timingAllowCap:
		t.Errorf("timing-allow.txt has %d entries, over the cap %d: the list may only shrink", n, timingAllowCap)
	case n < timingAllowCap:
		t.Errorf("timing-allow.txt has %d entries: lower timingAllowCap from %d to %d (the cap only goes down)", n, timingAllowCap, n)
	}
}

// The lint itself, on fixtures: each pattern is caught, and a safety-net poll is not.
func TestTIM1_LintCatchesEachPattern(t *testing.T) {
	for want, src := range map[string]string{
		"sleep":   `package x; import "time"; func F() { time.Sleep(10 * time.Millisecond) }`,
		"timer":   `package x; import "time"; func F(c chan int) { select { case <-c: case <-time.After(time.Second): } }`,
		"ctx":     `package x; import ("context"; "time"); func F() { context.WithTimeout(context.Background(), 2*time.Second) }`,
		"scale":   `package x; type T struct{ TimeScale float64 }; func F() T { return T{TimeScale: 0.02} }`,
		"elapsed": `package x; import "time"; func F(s time.Time) bool { return time.Since(s) > 500*time.Millisecond }`,
	} {
		sites, err := timingScan("x_test.go", []byte(src))
		if err != nil {
			t.Fatal(err)
		}
		if len(sites) != 1 || sites[0].pattern != want || sites[0].fn != "F" {
			t.Errorf("%s: got %v", want, sites)
		}
	}
	for name, src := range map[string]string{
		"aliased time":           `package x; import tm "time"; func F() { tm.Sleep(1) }`,
		"timer":                  `package x; import "time"; func F() { time.NewTimer(time.Second) }`,
		"toml time_scale":        "package x; const C = `[test]\ntime_scale = 0.05`",
		"assigned scale":         `package x; type T struct{ TimeScale float64 }; func F(t *T) { t.TimeScale = 0.5 }`,
		"sub as a verdict":       `package x; import "time"; func F(a, b time.Time) bool { return a.Sub(b) < time.Second }`,
		"elapsed via a variable": `package x; import "time"; func F(s time.Time) bool { d := time.Since(s); return d > time.Second }`,
		"elapsed via var decl":   `package x; import "time"; func F(s time.Time) bool { var d = time.Now().Sub(s); return d > time.Second }`,
		"elapsed in seconds":     `package x; import "time"; func F(s time.Time) bool { return time.Since(s).Seconds() > 2 }`,
		"after func":             `package x; import "time"; func F(f func()) { time.AfterFunc(time.Second, f) }`,
		"ticker":                 `package x; import "time"; func F() { time.NewTicker(time.Second) }`,
		"blocking after":         `package x; import "time"; func F() { <-time.After(time.Second) }`,
		"after var in a select":  `package x; import "time"; func F(c chan int) { to := time.After(time.Second); select { case <-c: case <-to: } }`,
		"after, not in a select": `package x; import "time"; func F() <-chan time.Time { return time.After(time.Second) }`,
		"tick":                   `package x; import "time"; func F() { for range time.Tick(time.Second) {} }`,
		"set seam scale":         `package x; type H struct{}; func (H) SetSeam(k, v string) {}; func F(h H) { h.SetSeam("time_scale", "0.05") }`,
		"toml scale, no spaces":  "package x; const C = `[test]\ntime_scale=.5`",
		"dot-imported time":      `package x; import . "time"; func F() { Sleep(Second) }`,
		"dot-imported context":   `package x; import (. "context"; "time"); func F() { WithTimeout(Background(), time.Second) }`,
	} {
		if sites, _ := timingScan("x_test.go", []byte(src)); len(sites) != 1 {
			t.Errorf("%s: got %v", name, sites)
		}
	}
	for name, src := range map[string]string{
		"scale 1":           `package x; type T struct{ TimeScale float64 }; func F() T { return T{TimeScale: 1} }`,
		"set seam scale 1":  `package x; type H struct{}; func (H) SetSeam(k, v string) {}; func F(h H) { h.SetSeam("time_scale", "1") }`,
		"toml scale 1":      "package x; const C = `[test]\ntime_scale = 1.0`",
		"sub, not compared": `package x; import "time"; func F(a, b time.Time) time.Duration { d := a.Sub(b); return d }`,
		"safety-net poll":   `package x; import "time"; func F(ok func() bool) bool { d := time.Now().Add(30 * time.Second); for !ok() { if time.Now().After(d) { return false } }; return true }`,
		"local Sleep":       `package x; func Sleep(int) {}; func F() { Sleep(1) }`,
	} {
		if sites, _ := timingScan("x_test.go", []byte(src)); len(sites) != 0 {
			t.Errorf("%s: flagged %v", name, sites)
		}
	}
	if timingTestCode("internal/run/pipeline.go") || !timingTestCode("internal/run/x_test.go") || !timingTestCode("internal/provider/sprites/spritesfake/fake.go") {
		t.Error("test-code detection")
	}
}

// timingCapIn reads timingAllowCap's value from a timing_test.go source.
func timingCapIn(src []byte) (int, bool) {
	m := regexp.MustCompile(`(?m)^const timingAllowCap = (\d+)`).FindSubmatch(src)
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(string(m[1]))
	return n, err == nil
}

// timingCapCheck holds capNow against the cap in timing_test.go at the base: on a branch, its
// merge base with origin/main (or main); on main itself (the merge base is HEAD), HEAD's first
// parent. git runs one git command. Without that history (a shallow clone, an exported tree) the
// check is skipped, but fails in CI (ci true), whose checkout must fetch the history. A base that
// has no TIM1 yet is skipped. It returns why the check was skipped, or why it failed; both empty
// means it passed.
func timingCapCheck(git func(args ...string) ([]byte, error), capNow int, ci bool) (skip, fail string) {
	var base []byte
	for _, ref := range []string{"origin/main", "main"} {
		mb, err := git("merge-base", "HEAD", ref)
		if err != nil {
			continue
		}
		at := strings.TrimSpace(string(mb))
		if head, err := git("rev-parse", "HEAD"); err == nil && strings.TrimSpace(string(head)) == at {
			at += "^" // on main: hold it against the previous commit
		}
		base, err = git("show", at+":conformance/timing_test.go")
		if err != nil {
			return fmt.Sprintf("main has no TIM1 at %.12s", at), ""
		}
		break
	}
	if base == nil {
		if ci {
			return "", "no merge base with main in CI: the checkout needs the history (fetch-depth: 0)"
		}
		return "no merge base with main (shallow clone or exported tree)", ""
	}
	was, ok := timingCapIn(base)
	if !ok {
		return "", "no timingAllowCap in timing_test.go at the base"
	}
	if capNow > was {
		return "", fmt.Sprintf("timingAllowCap %d is above %d at the base: the cap only goes down", capNow, was)
	}
	return "", ""
}

// The cap may not rise above main's (TIM1's ratchet).
func TestTIM1_CapOnlyGoesDown(t *testing.T) {
	git := func(args ...string) ([]byte, error) { return exec.Command("git", args...).Output() }
	skip, fail := timingCapCheck(git, timingAllowCap, os.Getenv("CI") == "true")
	if fail != "" {
		t.Fatal(fail)
	}
	if skip != "" {
		t.Skip(skip)
	}
}

// The ratchet's decisions, on a fake git.
func TestTIM1_CapCheckDecides(t *testing.T) {
	file := func(n int) string { return fmt.Sprintf("package conformance\nconst timingAllowCap = %d // x\n", n) }
	if n, ok := timingCapIn([]byte(file(12))); !ok || n != 12 {
		t.Fatalf("timingCapIn: %d %v", n, ok)
	}
	fake := func(out map[string]string) func(args ...string) ([]byte, error) {
		return func(args ...string) ([]byte, error) {
			if v, ok := out[strings.Join(args, " ")]; ok {
				return []byte(v), nil
			}
			return nil, errors.New("fake git: no such ref")
		}
	}
	branch := map[string]string{ // a branch off main at mb (cap 10)
		"rev-parse HEAD": "head\n", "merge-base HEAD origin/main": "mb\n", "show mb:conformance/timing_test.go": file(10),
	}
	onMain := map[string]string{ // main itself: the merge base is HEAD; its first parent had cap 10
		"rev-parse HEAD": "head\n", "merge-base HEAD origin/main": "head\n",
		"show head:conformance/timing_test.go": file(11), "show head^:conformance/timing_test.go": file(10),
	}
	noTIM1 := map[string]string{"rev-parse HEAD": "head\n", "merge-base HEAD origin/main": "mb\n"}
	for _, c := range []struct {
		name       string
		git        map[string]string
		capNow     int
		ci         bool
		skip, fail bool
	}{
		{"branch, cap kept", branch, 10, true, false, false},
		{"branch, cap lowered", branch, 9, true, false, false},
		{"branch, cap raised", branch, 11, false, false, true},
		{"main, cap raised over its first parent", onMain, 11, true, false, true},
		{"main, cap kept", onMain, 10, true, false, false},
		{"no history, local", map[string]string{}, 99, false, true, false},
		{"no history, in CI", map[string]string{}, 99, true, false, true},
		{"main has no TIM1 yet", noTIM1, 99, true, true, false},
	} {
		skip, fail := timingCapCheck(fake(c.git), c.capNow, c.ci)
		if (skip != "") != c.skip || (fail != "") != c.fail {
			t.Errorf("%s: skip %q, fail %q", c.name, skip, fail)
		}
	}
}
