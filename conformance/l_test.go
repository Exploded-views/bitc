package conformance

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Exploded-views/bitc/conformance/harness"
	"github.com/Exploded-views/bitc/internal/classify"
	"github.com/Exploded-views/bitc/internal/classify/classifytest"
)

func TestL2_RepoWideToolsScriptsAndAlwaysToolsRefused(t *testing.T) {
	root := classifytest.FixtureTree(t)
	c := classify.Context{Cwd: root, LogicalCwd: root, Worktree: root}
	for _, a := range classifytest.RefusedFromRoot {
		if _, ok := classify.Check(classifytest.NodeMonorepoRules(), a, c); !ok {
			t.Errorf("not refused: %q", a)
		}
	}
}

func TestL3_UnwrapperSeesThroughWrappersAndShells(t *testing.T) {
	cmds, _ := classify.Unwrap([]string{"env", "-u", "X", "nice", "-n", "5", "timeout", "-k", "5", "60", "bash", "-lc", "A=1 npx -p typescript tsc > out.log 2>&1"})
	if len(cmds) != 1 || cmds[0].Argv[0] != "tsc" || len(cmds[0].Argv) != 1 {
		t.Fatalf("%+v", cmds)
	}
}

func TestL4_FilesDeeperDirsAndOtherToolsAllowed(t *testing.T) {
	root := classifytest.FixtureTree(t)
	c := classify.Context{Cwd: root, LogicalCwd: root, Worktree: root}
	for _, a := range classifytest.AllowedFromRoot {
		if reason, ok := classify.Check(classifytest.NodeMonorepoRules(), a, c); ok {
			t.Errorf("refused (%s): %q", reason, a)
		}
	}
	sub := filepath.Join(root, "src", "app")
	cs := classify.Context{Cwd: sub, LogicalCwd: sub, Worktree: root}
	for _, a := range classifytest.AllowedFromSubdir {
		if _, ok := classify.Check(classifytest.NodeMonorepoRules(), append([]string{"npx", "eslint"}, a...), cs); ok {
			t.Errorf("from src/app refused: %q", a)
		}
	}
	for _, a := range classifytest.RefusedFromSubdir {
		if _, ok := classify.Check(classifytest.NodeMonorepoRules(), append([]string{"npx", "eslint"}, a...), cs); !ok {
			t.Errorf("from src/app not refused: %q", a)
		}
	}
	pre := filepath.Join(root, "src", "lib", "auth-utils")
	cp := classify.Context{Cwd: pre, LogicalCwd: pre, Worktree: root}
	for _, a := range classifytest.AllowedFromPrefixSibling {
		if reason, ok := classify.Check(classifytest.NodeMonorepoRules(), append([]string{"npx", "eslint"}, a...), cp); ok {
			t.Errorf("from src/lib/auth-utils refused (%s): %q", reason, a)
		}
	}
	for _, a := range classifytest.RefusedFromPrefixSibling {
		if _, ok := classify.Check(classifytest.NodeMonorepoRules(), append([]string{"npx", "eslint"}, a...), cp); !ok {
			t.Errorf("from src/lib/auth-utils not refused: %q", a)
		}
	}
}

func TestL6_SkippedOptionValuesAreNotPaths(t *testing.T) {
	root := classifytest.FixtureTree(t)
	c := classify.Context{Cwd: root, LogicalCwd: root, Worktree: root}
	rules := classifytest.NodeMonorepoRules()
	if _, ok := classify.Check(rules, []string{"npx", "eslint", "--fix-type", "problem"}, c); !ok {
		t.Fatal("--fix-type value counted as a path")
	}
	if _, ok := classify.Check(rules, []string{"npx", "eslint", "--fix-type", "problem", "src/a.ts"}, c); ok {
		t.Fatal("file after a skipped value not counted")
	}
}

func TestCL1_UnparsableShellStringIsNotRefused(t *testing.T) {
	root := classifytest.FixtureTree(t)
	c := classify.Context{Cwd: root, LogicalCwd: root, Worktree: root}
	if _, ok := classify.Check(classifytest.NodeMonorepoRules(), []string{"bash", "-c", "echo start (((( npx eslint ."}, c); ok {
		t.Fatal("an unparsable -c string whose leading command matches no rule was refused")
	}
}

// A here-document that is never closed makes the -c string unparsable, but a shell still runs
// its first command (spec 6.7): that command is checked.
func TestCL3_UnparsableStringStillChecksItsLeadingCommand(t *testing.T) {
	root := classifytest.FixtureTree(t)
	c := classify.Context{Cwd: root, LogicalCwd: root, Worktree: root}
	rules := classifytest.NodeMonorepoRules()
	for _, a := range [][]string{
		{"bash", "-c", "npx eslint << EOF"},
		{"bash", "-c", "npx eslint --fix<< EOF"},
		{"sh", "-c", "npx eslint <<EOF\nconsole.log(1)"},
		{"bash", "-c", "A=1 nice -n 5 npx eslint 2>&1 <<EOF"},
		{"bash", "-c", "if npx eslint . <<EOF"},
		{"bash", "-c", "npm run lint << EOF"},
		{"env", "-S", "npx eslint << EOF"},
	} {
		if _, ok := classify.Check(rules, a, c); !ok {
			t.Errorf("not refused: %q", a)
		}
	}
	for _, a := range [][]string{
		{"bash", "-c", "npx eslint src/a.ts << EOF"},
		{"bash", "-c", "npx eslint 'src/app' <<EOF"},
		{"bash", "-c", "echo start (((( npx eslint ."},
	} {
		if reason, ok := classify.Check(rules, a, c); ok {
			t.Errorf("refused (%s): %q", reason, a)
		}
	}
	h := harness.New(t)
	nodeRepo(t, h, "", nil)
	npx := fakeNpx(t, h)
	if r := h.Run("run", "lint", "--", "bash", "-c", npx+" eslint << EOF"); r.Code != 64 || !strings.Contains(r.Stderr, "bitc: lint: refused: ") {
		t.Fatalf("%+v", r)
	}
	if r := h.Run("run", "lint", "--", "bash", "-c", npx+" eslint src/a.ts << EOF"); r.Code != 0 {
		t.Fatalf("a scoped lint was refused or failed: %+v", r)
	}
}

func TestL2_PackageManagerSpellingsOfScriptsAndToolsRefused(t *testing.T) {
	root := classifytest.FixtureTree(t)
	c := classify.Context{Cwd: root, LogicalCwd: root, Worktree: root}
	for _, a := range classifytest.PackageManagerSpellings {
		if _, ok := classify.Check(classifytest.TestScriptRules(), a, c); !ok {
			t.Errorf("npm_script test not refused: %q", a)
		}
	}
	for _, a := range classifytest.ToolSpellings {
		if _, ok := classify.Check(classifytest.NodeMonorepoRules(), a, c); !ok {
			t.Errorf("tool not refused: %q", a)
		}
	}
}

// TestL6_ExampleListsEveryEslintValueOption runs the real binary with the node-monorepo example:
// each value option without a file is refused (64), and the information flags run.
func TestL6_ExampleListsEveryEslintValueOption(t *testing.T) {
	h := harness.New(t)
	wt := nodeRepo(t, h, "", nil)
	fake := filepath.Join(wt, "fakebin")
	if err := os.MkdirAll(fake, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fake, "npx"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil { //nolint:gosec // test stub must be executable
		t.Fatal(err)
	}
	for _, o := range classifytest.EslintValueOptions {
		if r := h.Run("run", "lint", "--", "./fakebin/npx", "eslint", o, "x"); r.Code != 64 {
			t.Errorf("%s x: want 64, got %+v", o, r)
		}
		if r := h.Run("run", "lint", "--", "./fakebin/npx", "eslint", o, "x", "src/a.ts"); r.Code != 0 {
			t.Errorf("%s x src/a.ts: want 0, got %+v", o, r)
		}
	}
	for _, f := range classifytest.EslintInfoFlags {
		if r := h.Run("run", "lint", "--", "./fakebin/npx", "eslint", f); r.Code != 0 {
			t.Errorf("%s: want 0, got %+v", f, r)
		}
	}
}

// fakeNpx writes a stub npx (exit 0) outside the work trees and returns its absolute path, so
// an allowed command really runs and a refused one never does.
func fakeNpx(t *testing.T, h *harness.H) string {
	t.Helper()
	p := filepath.Join(h.Root, "fakebin", "npx")
	h.Write(p, "#!/bin/sh\nexit 0\n", 0o755)
	return p
}

// lintCodes runs `bitc run lint -- npx eslint <args>` in dir (with env extras) and returns the exit code.
func lintCodes(h *harness.H, npx, dir string, env []string, args ...string) int {
	h.Cwd = dir
	return h.RunWith(env, append([]string{"run", "lint", "--", npx, "eslint"}, args...)...).Code
}

func TestCL2_LinkedWorktreeNestedInACheckoutIsItsOwnRoot(t *testing.T) {
	h := harness.New(t)
	wt := nodeRepo(t, h, "", nil)
	npx := fakeNpx(t, h)
	nested := filepath.Join(wt, "nested")
	cmd := exec.Command("git", "-c", "user.name=t", "-c", "user.email=t@example.invalid", "worktree", "add", "-q", "-b", "nested", nested)
	cmd.Dir = wt
	cmd.Env = append(harness.MergeEnv(h.Env, nil), "GIT_CONFIG_NOSYSTEM=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git worktree add: %v\n%s", err, out)
	}
	for _, d := range []string{"src/app", "e2e/scripts", "scripts", "docs"} {
		if err := os.MkdirAll(filepath.Join(nested, d), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	if fi, err := os.Stat(filepath.Join(nested, ".git")); err != nil || fi.IsDir() {
		t.Fatalf("the nested worktree should have a .git file: %v %v", fi, err)
	}
	for _, d := range []string{"src", "e2e", "scripts", "docs", "."} {
		if c := lintCodes(h, npx, nested, nil, d); c != 64 {
			t.Errorf("from the nested root, eslint %s: want 64, got %d", d, c)
		}
	}
	if c := lintCodes(h, npx, nested, nil, "src/app"); c != 0 {
		t.Errorf("from the nested root, eslint src/app: want 0, got %d", c)
	}
	app := filepath.Join(nested, "src", "app")
	for _, d := range []string{"../..", "../../e2e", "../../src"} {
		if c := lintCodes(h, npx, app, nil, d); c != 64 {
			t.Errorf("from nested src/app, eslint %s: want 64, got %d", d, c)
		}
	}
	if c := lintCodes(h, npx, app, nil, "../../e2e/scripts"); c != 0 {
		t.Errorf("from nested src/app, eslint ../../e2e/scripts: want 0, got %d", c)
	}
}

func TestCL2_LogicalPwdThroughASymlinkedWorkTree(t *testing.T) {
	h := harness.New(t)
	wt := nodeRepo(t, h, "", nil)
	npx := fakeNpx(t, h)
	for _, d := range []string{"src/app", "e2e"} {
		if err := os.MkdirAll(filepath.Join(wt, d), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(h.Root, "link")
	if err := os.Symlink(wt, link); err != nil {
		t.Fatal(err)
	}
	env := func(dir string) []string { return []string{"PWD=" + dir} }
	for _, d := range []string{".", "src", "e2e"} {
		if c := lintCodes(h, npx, link, env(link), d); c != 64 {
			t.Errorf("logical cwd, eslint %s: want 64, got %d", d, c)
		}
	}
	if c := lintCodes(h, npx, link, env(link), "src/app"); c != 0 {
		t.Errorf("logical cwd, eslint src/app: want 0, got %d", c)
	}
	logicalApp := filepath.Join(link, "src", "app")
	if c := lintCodes(h, npx, logicalApp, env(logicalApp), "../.."); c != 64 {
		t.Errorf("logical src/app, eslint ../..: want 64, got %d", c)
	}
}

func TestCL2_UnenterableTopLevelDirIsStillRepoWide(t *testing.T) {
	h := harness.New(t)
	wt := nodeRepo(t, h, "", nil)
	npx := fakeNpx(t, h)
	locked := filepath.Join(wt, "locked")
	if err := os.Mkdir(locked, 0o000); err != nil { //nolint:gosec // the point of the test is a mode-0 directory
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) }) //nolint:gosec // restore so the temp dir can be removed
	if c := lintCodes(h, npx, wt, nil, "locked"); c != 64 {
		t.Errorf("eslint locked (mode 0): want 64, got %d", c)
	}
}
