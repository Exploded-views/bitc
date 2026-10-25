package classify

import (
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/Exploded-views/bitc/internal/classify/classifytest"
	"github.com/Exploded-views/bitc/internal/config"
)

func TestUnwrap(t *testing.T) {
	cases := []struct {
		in   []string
		want [][]string
	}{
		{[]string{"eslint", "."}, [][]string{{"eslint", "."}}},
		{[]string{"A=1", "B=2", "npx", "eslint", "src"}, [][]string{{"eslint", "src"}}},
		{[]string{"timeout", "-k", "5", "600", "npx", "eslint", "."}, [][]string{{"eslint", "."}}},
		{[]string{"env", "-u", "X", "A=1", "nice", "-n", "5", "npx", "eslint"}, [][]string{{"eslint"}}},
		{[]string{"npx", "--yes", "eslint@9.30.0", "."}, [][]string{{"eslint", "."}}},
		{[]string{"npx", "-p", "typescript", "tsc", "--version"}, [][]string{{"tsc", "--version"}}},
		{[]string{"npm", "exec", "--", "eslint", "."}, [][]string{{"eslint", "."}}},
		{[]string{"pnpm", "dlx", "tsc"}, [][]string{{"tsc"}}},
		{[]string{"node", "-r", "dotenv/config", "node_modules/eslint/bin/eslint.js", "."}, [][]string{{"eslint", "."}}},
		{[]string{"node", "--version"}, [][]string{{"node", "--version"}}},
		{[]string{"bash", "-lc", "A=1 npx eslint . > lint.log 2>&1; (npm run lint)"}, [][]string{{"eslint", "."}, {"npm", "run", "lint"}}},
		{[]string{"sh", "-c", "if true; then bash -c 'npx tsc'; fi"}, [][]string{{"true"}, {"tsc"}}},
		{[]string{"bash", "-o", "pipefail", "-c", "npx eslint ."}, [][]string{{"eslint", "."}}},
		{[]string{"bash", "--rcfile", "/dev/null", "+O", "extglob", "-c", "npx tsc"}, [][]string{{"tsc"}}},
		{[]string{"env", "-S", "npx eslint", "src/a.ts"}, [][]string{{"eslint", "src/a.ts"}}},
		{[]string{"env", "-u", "X", "--split-string=A=1 npx tsc"}, [][]string{{"tsc"}}},
		{[]string{"command", "-v", "eslint"}, nil},
	}
	for _, c := range cases {
		got, _ := Unwrap(c.in)
		var argvs [][]string
		for _, s := range got {
			argvs = append(argvs, s.Argv)
		}
		if !reflect.DeepEqual(argvs, c.want) {
			t.Errorf("Unwrap(%q) = %q, want %q", c.in, argvs, c.want)
		}
	}
}

func TestLeadingWords(t *testing.T) {
	cases := map[string][]string{
		"npx eslint << EOF":                        {"npx", "eslint"},
		"npx eslint --fix<< EOF":                   {"npx", "eslint", "--fix"},
		"npx eslint <<EOF\nconsole.log(1)":         {"npx", "eslint"},
		"A=1 npx eslint 2>&1 <<EOF":                {"A=1", "npx", "eslint"},
		"npx eslint 'src/a b.ts' \"x\\\"y\" <<EOF": {"npx", "eslint", "src/a b.ts", `x"y`},
		`npx eslint "$PWD" <<EOF`:                  {"npx", "eslint", pwdMarker},
		"npx eslint $(pwd) <<EOF":                  {"npx", "eslint", pwdMarker},
		"npx eslint `pwd` <<EOF":                   {"npx", "eslint", pwdMarker},
		"npx eslint $(git ls-files) <<EOF":         {"npx", "eslint", opaqueMarker},
		"if ! npx eslint . <<EOF":                  {"npx", "eslint", "."},
		"echo a\\ b; x":                            {"echo", "a b"},
		"echo 'never closed":                       {"echo"},
		"echo start (((( npx eslint .":             {"echo", "start"},
		"":                                         nil,
	}
	for in, want := range cases {
		if got := leadingWords(in); !reflect.DeepEqual(got, want) {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}

// An unparsable string is not parsed further, but its leading simple command is unwrapped and
// checked (spec 6.7); later commands are not.
func TestUnparsableStringChecksItsLeadingCommand(t *testing.T) {
	for _, c := range []struct {
		in   []string
		want []string
	}{
		{[]string{"bash", "-c", "A=1 nice -n 5 npx eslint 2>&1 << EOF"}, []string{"eslint"}},
		{[]string{"env", "-S", "npx tsc <<EOF"}, []string{"tsc"}},
		{[]string{"bash", "-c", "if then fi ((("}, []string{"fi"}},
	} {
		cmds, notes := Unwrap(c.in)
		if len(notes) != 1 || !strings.Contains(notes[0], "checked its leading command only") ||
			len(cmds) != 1 || !reflect.DeepEqual(cmds[0].Argv, c.want) {
			t.Errorf("%q: %+v %q", c.in, cmds, notes)
		}
	}
}

func TestScriptRuns(t *testing.T) {
	for in, want := range map[string]string{
		"npm run lint": "lint", "npm run-script lint": "lint", "npm --prefix . run -s lint": "lint",
		"npm run --if-present check:types": "check:types", "pnpm lint": "lint", "pnpm run lint": "lint",
		"yarn typecheck": "typecheck", "yarn run test": "test", "npm test": "test", "npm tst": "test", "npm restart": "restart", "npm install": "",
	} {
		cmds, _ := Unwrap(splitWords(in))
		if len(cmds) != 1 || cmds[0].Script != want {
			t.Errorf("%q: %+v, want script %q", in, cmds, want)
		}
	}
}

func splitWords(s string) []string {
	var out []string
	for _, w := range regexpSpaces.Split(s, -1) {
		if w != "" {
			out = append(out, w)
		}
	}
	return out
}

var regexpSpaces = regexp.MustCompile(`\s+`)

// FuzzUnwrapAndCheck: any argv (words split on NUL) unwraps and classifies without a panic or
// a hang; nesting is bounded however deep the wrappers go.
func FuzzUnwrapAndCheck(f *testing.F) {
	for _, s := range []string{
		"sh\x00-c\x00npx eslint .",
		"env\x00A=1\x00timeout\x00-k\x005\x0060\x00npm\x00run\x00lint",
		"bash\x00-c\x00'unterminated",
		"nice\x00-n",
		"sh\x00-c\x00sh -c 'sh -c \"sh -c eslint\"'",
		"pnpm\x00exec\x00--\x00tsc",
	} {
		f.Add(s)
	}
	rules := []config.RefuseRule{
		{Tool: "eslint", When: "repo-wide", Reason: "r1"},
		{Tool: "tsc", When: "always", Reason: "r2"},
		{NpmScript: []string{"lint"}, Reason: "r3"},
	}
	f.Fuzz(func(t *testing.T, s string) {
		argv := strings.Split(s, "\x00")
		_, _ = Unwrap(argv)
		_, _ = Check(rules, argv, Context{Cwd: "/w/sub", LogicalCwd: "/w/sub", Worktree: "/w"})
	})
}

func TestPackageManagerSpellingsAreScripts(t *testing.T) {
	for _, a := range classifytest.PackageManagerSpellings {
		cmds, _ := Unwrap(a)
		if len(cmds) != 1 || cmds[0].Script != "test" {
			t.Errorf("Unwrap(%q) = %+v, want script test", a, cmds)
		}
		if _, ok := Check(classifytest.TestScriptRules(), a, Context{Cwd: "/w", LogicalCwd: "/w", Worktree: "/w"}); !ok {
			t.Errorf("not refused: %q", a)
		}
	}
	for _, a := range classifytest.ToolSpellings {
		if _, ok := Check(classifytest.NodeMonorepoRules(), a, Context{Cwd: "/w", LogicalCwd: "/w", Worktree: "/w"}); !ok {
			t.Errorf("tool not refused: %q", a)
		}
	}
}

func TestShellEscapesAreRemoved(t *testing.T) {
	for in, want := range map[string]string{
		`\eslint .`:      "eslint .",
		`eslint \.`:      "eslint .",
		`git cl\ean -n`:  "git clean -n",
		`$'\x65slint' .`: "eslint .",
		`"es\lint" .`:    `es\lint .`,
		`"\$PWD" x`:      "$PWD x",
		"es\\\nlint .":   "eslint .",
	} {
		cmds, err := parseShell(in)
		if err != nil || len(cmds) != 1 {
			t.Fatalf("%q: %v %v", in, cmds, err)
		}
		if got := strings.Join(cmds[0], " "); got != want {
			t.Errorf("parseShell(%q) = %q, want %q", in, got, want)
		}
	}
}

func rootCtx(t *testing.T) Context {
	t.Helper()
	root := classifytest.FixtureTree(t)
	return Context{Cwd: root, LogicalCwd: root, Worktree: root}
}

func TestIoniceIdOptionsTakeValues(t *testing.T) {
	c := rootCtx(t)
	for _, o := range []string{"-p", "-P", "-u", "--pid", "--pgid", "--uid"} {
		argv := []string{"ionice", o, "7", "npx", "eslint", "."}
		if _, refused := Check(classifytest.NodeMonorepoRules(), argv, c); !refused {
			t.Errorf("not refused: %q", argv)
		}
	}
}

func TestNodeEnvFileAndInspectPortTakeValues(t *testing.T) {
	c := rootCtx(t)
	for _, o := range []string{"--env-file", "--inspect-port"} {
		argv := []string{"node", o, "x", "node_modules/eslint/bin/eslint.js", "."}
		if _, refused := Check(classifytest.NodeMonorepoRules(), argv, c); !refused {
			t.Errorf("not refused: %q", argv)
		}
	}
}

func TestNodeEvalClusterIsNotAScript(t *testing.T) {
	c := rootCtx(t)
	for _, o := range []string{"-pe", "-ep"} {
		argv := []string{"node", o, "eslint", "."}
		if reason, refused := Check(classifytest.NodeMonorepoRules(), argv, c); refused {
			t.Errorf("refused (%s): %q", reason, argv)
		}
	}
	// -title is not a cluster of node's flags: the script after it is still the tool
	if _, refused := Check(classifytest.NodeMonorepoRules(), []string{"node", "-title", "node_modules/eslint/bin/eslint.js", "."}, c); !refused {
		t.Error("-title swallowed the script")
	}
}

func TestCommandLookupClustersOnlyPrint(t *testing.T) {
	c := rootCtx(t)
	for _, o := range []string{"-v", "-V", "-pv", "-pV", "-Vp"} {
		argv := []string{"command", o, "eslint"}
		if reason, refused := Check(classifytest.NodeMonorepoRules(), argv, c); refused {
			t.Errorf("refused (%s): %q", reason, argv)
		}
	}
	if _, refused := Check(classifytest.NodeMonorepoRules(), []string{"command", "-p", "eslint"}, c); !refused {
		t.Error("command -p eslint runs eslint and must be refused")
	}
}

func TestEslintValueOptionsAndInformationFlags(t *testing.T) {
	c := rootCtx(t)
	rules := classifytest.NodeMonorepoRules()
	for _, o := range classifytest.EslintValueOptions {
		if _, refused := Check(rules, []string{"npx", "eslint", o, "x"}, c); !refused {
			t.Errorf("%s: its value counted as a file", o)
		}
		if reason, refused := Check(rules, []string{"npx", "eslint", o, "x", "src/a.ts"}, c); refused {
			t.Errorf("%s with a file refused (%s)", o, reason)
		}
	}
	for _, f := range classifytest.EslintInfoFlags {
		if reason, refused := Check(rules, []string{"npx", "eslint", f}, c); refused {
			t.Errorf("%s refused (%s)", f, reason)
		}
	}
}
