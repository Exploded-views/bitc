package conformance

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The WF rows: GitHub Actions hygiene (GitHub-hosted runners only, no pull_request_target, every
// action pinned to a full commit SHA). The cases are ported one for one from crewkit's
// tests/test_check_workflows.py (crewkit commit 186a6d0); the checker is in wflint_test.go.

var wfSHA = strings.Repeat("0123456789abcdef", 3)[:40] // a well-formed commit SHA, built so no hex literal sits here

var wfGood = strings.NewReplacer("{SHA}", wfSHA, "{DIGEST}", strings.Repeat("a", 64)).Replace(`
name: ci
on:
  push:
    branches: [main]
  pull_request:
permissions:
  contents: read
jobs:
  a:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@{SHA} # v7.0.1
      - uses: ./.github/actions/local
      - uses: docker://alpine@sha256:{DIGEST}
      - run: echo hi
  b:
    runs-on: ${{ matrix.os }}
    strategy:
      matrix:
        os: [ubuntu-24.04, macos-14, windows-2022]
    steps:
      - uses: "actions/setup-python@{SHA}"
  c:
    runs-on:
      - ubuntu-24.04-arm
    steps:
      - run: true
`)

func wfPasses(t *testing.T, name, text string) {
	t.Helper()
	if f := checkWorkflow("ci.yml", text); len(f) != 0 {
		t.Errorf("%s: want no findings, got %q", name, f)
	}
}

func wfFails(t *testing.T, name, text, needle string) {
	t.Helper()
	f := checkWorkflow("ci.yml", text)
	if len(f) == 0 {
		t.Errorf("%s: want a finding naming %q, got none", name, needle)
		return
	}
	if !strings.Contains(strings.Join(f, "\n"), needle) {
		t.Errorf("%s: no finding names %q: %q", name, needle, f)
	}
}

func TestWF1_RunsOnMustBeAGitHubHostedLabel(t *testing.T) {
	wfPasses(t, "good workflow", wfGood)
	r := func(old, new string) string { return strings.ReplaceAll(wfGood, old, new) }
	for _, c := range []struct{ name, text, needle string }{
		{"self-hosted label", r("runs-on: ubuntu-latest", "runs-on: self-hosted"), "runs-on"},
		{"self-hosted in a flow list", r("runs-on: ubuntu-latest", "runs-on: [self-hosted, linux]"), "self-hosted"},
		{"self-hosted in a block list", r("- ubuntu-24.04-arm", "- self-hosted"), "self-hosted"},
		{"custom label", r("runs-on: ubuntu-latest", "runs-on: my-big-box"), "my-big-box"},
		{"runner group", r("runs-on: ubuntu-latest", "runs-on:\n      group: private"), "group"},
		{"inline runner group", r("runs-on: ubuntu-latest", "runs-on: { group: private, labels: big }"), "group"},
		{"matrix value that is self-hosted", r("windows-2022", "self-hosted"), "self-hosted"},
		{"matrix include value", r("        os: [ubuntu-24.04, macos-14, windows-2022]",
			"        include:\n          - os: ubuntu-24.04\n          - os: build-farm"), "build-farm"},
		// beyond crewkit's cases: matrix values written as a block list
		{"matrix block-list value", r("        os: [ubuntu-24.04, macos-14, windows-2022]",
			"        os:\n          - ubuntu-24.04\n          - build-farm"), "build-farm"},
		{"unresolvable expression", r("runs-on: ubuntu-latest", "runs-on: ${{ inputs.runner }}"), "resolve"},
	} {
		wfFails(t, c.name, c.text, c.needle)
	}
}

func TestWF2_NoPullRequestTargetTrigger(t *testing.T) {
	r := func(old, new string) string { return strings.ReplaceAll(wfGood, old, new) }
	onBlock := "on:\n  push:\n    branches: [main]\n  pull_request:"
	wfFails(t, "block form", r("  pull_request:", "  pull_request_target:"), "pull_request_target")
	wfFails(t, "flow list form", r(onBlock, "on: [push, pull_request_target]"), "pull_request_target")
	wfFails(t, "scalar form", r(onBlock, "on: pull_request_target"), "pull_request_target")
	wfPasses(t, "in a run line or a comment", r("      - run: echo hi", "      - run: echo pull_request_target  # not a trigger"))
}

func TestWF3_UsesPinnedToAFullCommitSHA(t *testing.T) {
	r := func(old, new string) string { return strings.ReplaceAll(wfGood, old, new) }
	wfFails(t, "tag ref", r("actions/checkout@"+wfSHA, "actions/checkout@v4"), "SHA")
	wfFails(t, "branch ref", r("actions/checkout@"+wfSHA, "actions/checkout@main"), "SHA")
	wfFails(t, "short sha", r("actions/checkout@"+wfSHA, "actions/checkout@"+wfSHA[:12]), "SHA")
	wfFails(t, "reusable workflow", wfGood+"  d:\n    uses: org/repo/.github/workflows/x.yml@main\n", "SHA")
	wfFails(t, "docker tag", r("docker://alpine@sha256:"+strings.Repeat("a", 64), "docker://alpine:3"), "digest")
}

func TestWF4_FindingsNameFileAndLine(t *testing.T) {
	f := checkWorkflow("ci.yml", strings.ReplaceAll(wfGood, "actions/checkout@"+wfSHA, "actions/checkout@v4"))
	if len(f) == 0 || !regexp.MustCompile(`ci\.yml:\d+:`).MatchString(f[0]) {
		t.Fatalf("want ci.yml:<line>: findings, got %q", f)
	}
	// the checkout line is line 13 of the fixture (it starts with a newline)
	if !strings.HasPrefix(f[0], "ci.yml:13: ") {
		t.Errorf("want the finding on line 13, got %q", f[0])
	}
}

func TestWF5_TheRepoWorkflowsPass(t *testing.T) {
	// a directory scan reads *.yml and *.yaml
	d := t.TempDir()
	if err := os.WriteFile(filepath.Join(d, "x.yaml"), []byte(strings.Replace(wfGood, "ubuntu-latest", "self-hosted", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if n, f, err := checkWorkflowDir(d); err != nil || n != 1 || len(f) == 0 {
		t.Errorf("a bad x.yaml: files %d, findings %q, err %v", n, f, err)
	}
	// no workflows at all is an error, not a pass
	if _, _, err := checkWorkflowDir(t.TempDir()); err == nil {
		t.Error("an empty workflows directory passed")
	}
	// this repo's own workflows
	n, f, err := checkWorkflowDir("../.github/workflows")
	if err != nil {
		t.Fatal(err)
	}
	if n < 2 {
		t.Errorf("only %d workflow files found", n)
	}
	for _, x := range f {
		t.Error(x)
	}
}

// WF6: every push and pull request runs the secret scan (make secrets: gitleaks over the whole
// history with the committed .gitleaks.toml) in its own job, with the full history, no persisted
// credentials and gitleaks pinned to the version `make tools` installs.
func TestWF6_CIRunsTheSecretScanOnEveryPushAndPullRequest(t *testing.T) {
	ci := string(must(os.ReadFile("../.github/workflows/ci.yml")))
	mk := string(must(os.ReadFile("../Makefile")))
	for _, f := range secretsJobFindings(ci, mk) {
		t.Error(f)
	}
	// each way to keep the job while defeating it is a finding
	for _, m := range []struct{ name, old, new string }{
		{"continue-on-error on the job", "\n  secrets:\n    runs-on: ubuntu-latest\n", "\n  secrets:\n    runs-on: ubuntu-latest\n    continue-on-error: true\n"},
		{"a scan that cannot fail", "\n        run: make secrets\n", "\n        run: make secrets || true\n"},
		{"a skipped step", "\n      - name: make secrets\n", "\n      - name: make secrets\n        if: false\n"},
		{"a job that skips pull requests", "\n  secrets:\n    runs-on: ubuntu-latest\n", "\n  secrets:\n    runs-on: ubuntu-latest\n    if: github.event_name == 'push'\n"},
		{"write permission behind a comment", "\n    permissions:\n      contents: read\n    steps:\n", "\n    permissions:\n      contents: write # not contents: read\n    steps:\n"},
		{"a workflow env that changes make", "\nenv:\n  GOTOOLCHAIN: local\n", "\nenv:\n  GOTOOLCHAIN: local\n  MAKEFLAGS: -i\n"},
		{"a workflow env that changes gitleaks", "\nenv:\n  GOTOOLCHAIN: local\n", "\nenv:\n  GOTOOLCHAIN: local\n  GITLEAKS_CONFIG: /dev/null\n"},
		{"a workflow env that changes the toolchain", "\nenv:\n  GOTOOLCHAIN: local\n", "\nenv:\n  GOTOOLCHAIN: auto\n"},
		{"workflow defaults", "\nenv:\n  GOTOOLCHAIN: local\n", "\ndefaults:\n  run:\n    shell: bash {0}\n\nenv:\n  GOTOOLCHAIN: local\n"},
		{"a step shell", "\n      - name: make secrets\n", "\n      - name: make secrets\n        shell: true {0}\n"},
		{"a job env", "\n    timeout-minutes: 15\n    permissions:", "\n    timeout-minutes: 15\n    env:\n      MAKEFLAGS: -i\n    permissions:"},
		{"job defaults", "\n    timeout-minutes: 15\n    permissions:", "\n    timeout-minutes: 15\n    defaults:\n      run:\n        working-directory: scripts\n    permissions:"},
		{"job defaults with a shell", "\n    timeout-minutes: 15\n    permissions:", "\n    timeout-minutes: 15\n    defaults:\n      run:\n        shell: bash {0}\n    permissions:"},
		{"a step env", "\n      - name: make secrets\n", "\n      - name: make secrets\n        env:\n          MAKEFLAGS: -i\n"},
		{"an inline job env", "\n    timeout-minutes: 15\n    permissions:", "\n    timeout-minutes: 15\n    env: {MAKEFLAGS: -i}\n    permissions:"},
		{"an inline step env", "\n      - name: make secrets\n", "\n      - name: make secrets\n        env: {MAKEFLAGS: -i}\n"},
		{"inline job defaults", "\n    timeout-minutes: 15\n    permissions:", "\n    timeout-minutes: 15\n    defaults: {run: {working-directory: scripts}}\n    permissions:"},
		{"a cached setup-go", "\n          cache: false\n      - name: install gitleaks\n", "\n          cache: true\n      - name: install gitleaks\n"},
	} {
		bad := strings.Replace(ci, m.old, m.new, 1)
		if bad == ci {
			t.Fatalf("%s: the mutation does not apply to ci.yml", m.name)
		}
		if len(secretsJobFindings(bad, mk)) == 0 {
			t.Errorf("%s: passed", m.name)
		}
	}
	// setup-go's cache input defaults to true, so leaving it out still caches: cache: false is required
	omitted := strings.Replace(ci, "\n          cache: false\n      - name: install gitleaks\n", "\n      - name: install gitleaks\n", 1)
	if len(secretsJobFindings(omitted, mk)) == 0 {
		t.Error("a setup-go step without cache: false: passed")
	}
}

// secretsJobFindings checks ci.yml's triggers and its secrets job. There is no YAML parser here, so
// the job is read line by line with comments dropped: what must be there is an exact line, and no
// if:, continue-on-error, shell:, env:, defaults: or || may appear anywhere in the job, since each
// keeps the job but defeats it. setup-go must say cache: false (its default is true). The workflow
// level may set only env GOTOOLCHAIN: local (anything else can change what make or gitleaks does)
// and no defaults:.
func secretsJobFindings(ci, mk string) []string {
	var f []string
	on := ci[:strings.Index(ci, "\njobs:")]
	if !strings.Contains(on, "push:") || !strings.Contains(on, "pull_request:") || strings.Contains(on, "paths:") {
		f = append(f, "ci.yml does not run on every push to main and every pull request")
	}
	var top []string // the workflow-level env block's keys, comments dropped
	inEnv := false
	for _, l := range strings.Split(on, "\n") {
		if k := strings.Index(l, " #"); k >= 0 {
			l = l[:k]
		}
		switch {
		case strings.HasPrefix(l, "defaults:"):
			f = append(f, "ci.yml sets workflow-level defaults:")
		case l == "env:":
			inEnv = true
		case strings.TrimSpace(l) == "":
		case strings.HasPrefix(l, " ") && inEnv:
			top = append(top, strings.TrimSpace(l))
		default:
			inEnv = false
		}
	}
	if len(top) != 1 || top[0] != "GOTOOLCHAIN: local" {
		f = append(f, fmt.Sprintf("ci.yml's workflow env is %q, not exactly GOTOOLCHAIN: local", top))
	}
	i := strings.Index(ci, "\n  secrets:\n")
	if i < 0 {
		return append(f, "ci.yml has no secrets job")
	}
	job := ci[i+1:]
	if j := regexp.MustCompile(`\n  [a-z][a-z-]*:\n`).FindStringIndex(job[1:]); j != nil {
		job = job[:j[0]+1]
	}
	type line struct {
		indent int
		text   string // trimmed, without a comment or a list dash
	}
	var lines []line
	for _, l := range strings.Split(job, "\n") {
		if k := strings.Index(l, " #"); k >= 0 {
			l = l[:k]
		}
		x := strings.TrimSpace(l)
		if x == "" {
			continue
		}
		lines = append(lines, line{len(l) - len(strings.TrimLeft(l, " ")), strings.TrimSpace(strings.TrimPrefix(x, "- "))})
	}
	has := map[string]bool{}
	for _, l := range lines {
		has[l.text] = true
		if strings.HasPrefix(l.text, "if:") || strings.HasPrefix(l.text, "continue-on-error:") || strings.HasPrefix(l.text, "shell:") || strings.HasPrefix(l.text, "env:") || strings.HasPrefix(l.text, "defaults:") || strings.Contains(l.text, "||") {
			f = append(f, "the secrets job can be skipped or cannot fail: "+l.text)
		}
	}
	pin := regexp.MustCompile(`github\.com/zricethezav/gitleaks/v8@v[0-9.]+`).FindString(mk)
	for _, want := range []string{"fetch-depth: 0", "persist-credentials: false", "cache: false", "run: make secrets", "go install " + pin} {
		if pin == "" || !has[want] {
			f = append(f, "the secrets job lacks the line "+want)
		}
	}
	// the job's permissions block is exactly contents: read
	var perms []string
	for k, l := range lines {
		if l.indent == 4 && l.text == "permissions:" {
			for _, p := range lines[k+1:] {
				if p.indent <= 4 {
					break
				}
				perms = append(perms, p.text)
			}
			break
		}
	}
	if len(perms) != 1 || perms[0] != "contents: read" {
		f = append(f, fmt.Sprintf("the secrets job's permissions are %q, not exactly contents: read", perms))
	}
	return f
}

// WF7: the merge queue and the sharded CI. Both workflows run on merge_group; ci.yml is split into
// the jobs below so the required checks run in parallel (every job one make command, locally too).

var wfCIJobs = []string{"lint", "unit", "coverage", "conformance", "conformance-race", "docs", "build", "secrets", "macos-smoke"}

// wfJob returns the comment-stripped text of one job of a workflow file.
func wfJob(t *testing.T, file, name string) string {
	t.Helper()
	lines := relLines(string(must(os.ReadFile("../.github/workflows/" + file))))
	ji, _ := relSection(lines, 0, len(lines), 0, "jobs")
	if ji < 0 {
		t.Fatalf("%s has no jobs", file)
	}
	i, body := relSection(lines, ji+1, len(lines), 2, name)
	if i < 0 || len(body) == 0 {
		t.Fatalf("%s has no %s job", file, name)
	}
	return strings.Join(lines[i:body[len(body)-1].n+1], "\n")
}

// wfList reads a flow-list matrix axis (`key: [a, b]`) of a job, sorted.
func wfList(t *testing.T, job, key string) []string {
	t.Helper()
	m := regexp.MustCompile(`(?m)^\s+` + key + `: \[([^\]]*)\]$`).FindStringSubmatch(job)
	if m == nil {
		t.Fatalf("no `%s: [..]` matrix axis in:\n%s", key, job)
	}
	var out []string
	for _, x := range strings.Split(m[1], ",") {
		out = append(out, strings.Trim(strings.TrimSpace(x), `"'`))
	}
	sort.Strings(out)
	return out
}

func wfScript(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("sh", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("sh %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// WF7a: the merge queue runs the same workflows.
func TestWF7_BothWorkflowsRunOnMergeGroup(t *testing.T) {
	for _, f := range []string{"ci.yml", "release.yml"} {
		text := string(must(os.ReadFile("../.github/workflows/" + f)))
		on := text[:strings.Index(text, "\njobs:")]
		if !regexp.MustCompile(`(?m)^  merge_group:`).MatchString(on) {
			t.Errorf("%s does not list merge_group under on:", f)
		}
	}
}

// WF7b: the job names are the required checks of the branch ruleset, so they are fixed.
func TestWF7_CIJobsAndLegs(t *testing.T) {
	text := string(must(os.ReadFile("../.github/workflows/ci.yml")))
	var got []string
	for _, m := range regexp.MustCompile(`(?m)^  ([a-z][a-z-]*):$`).FindAllStringSubmatch(text[strings.Index(text, "\njobs:"):], -1) {
		got = append(got, m[1])
	}
	want := append([]string(nil), wfCIJobs...)
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("ci.yml jobs are %v, want exactly %v", got, want)
	}
	unit := wfJob(t, "ci.yml", "unit")
	if l := wfList(t, unit, "leg"); strings.Join(l, " ") != "core providers" {
		t.Errorf("unit legs are %v, want core and providers", l)
	}
	shards := strings.Fields(wfScript(t, "../scripts/conformance-shards.sh", "shards"))
	sort.Strings(shards)
	for _, j := range []string{"conformance", "conformance-race"} {
		if l := wfList(t, wfJob(t, "ci.yml", j), "shard"); strings.Join(l, " ") != strings.Join(shards, " ") {
			t.Errorf("%s shards are %v, scripts/conformance-shards.sh says %v", j, l, shards)
		}
	}
	for _, j := range wfCIJobs {
		b := wfJob(t, "ci.yml", j)
		if !strings.Contains(b, "persist-credentials: false") || !regexp.MustCompile(`(?m)^    timeout-minutes: [0-9]+$`).MatchString(b) {
			t.Errorf("%s: needs persist-credentials: false and a timeout", j)
		}
	}
	// the Makefile gives each job one command
	mk := string(must(os.ReadFile("../Makefile")))
	for _, tgt := range []string{"\ntest-unit:", "\ncoverage-merge:", "\nconformance-shard:", "\nconformance-race-shard:", "\ndocs:"} {
		if !strings.Contains(mk, tgt) {
			t.Errorf("Makefile lacks %q", strings.TrimSpace(tgt))
		}
	}
	if !regexp.MustCompile(`(?m)^ci: lint test coverage conformance conformance-race examples linkcheck$`).MatchString(mk) {
		t.Error("make ci changed meaning")
	}
}

// WF7c: coverage is checked on the merged output of every unit leg, and a missing leg fails it
// (the workflow names the expected legs; download-artifact alone would not notice a missing one).
func TestWF7_CoverageNeedsEveryUnitLeg(t *testing.T) {
	cov := wfJob(t, "ci.yml", "coverage")
	if !regexp.MustCompile(`(?m)^    needs: \[unit\]$`).MatchString(cov) {
		t.Error("coverage does not have needs: [unit]")
	}
	for _, j := range []string{"unit", "coverage"} {
		b := wfJob(t, "ci.yml", j)
		a := map[string]string{"unit": "upload-artifact", "coverage": "download-artifact"}[j]
		if !regexp.MustCompile(`uses: actions/` + a + `@[0-9a-f]{40}`).MatchString(b) {
			t.Errorf("%s does not use a sha-pinned actions/%s", j, a)
		}
	}
	m := regexp.MustCompile(`(?m)^\s*run: make coverage-merge UNIT_LEGS="([a-z ]+)"$`).FindStringSubmatch(cov)
	if m == nil {
		t.Fatal(`coverage does not run: make coverage-merge UNIT_LEGS="<legs>"`)
	}
	legs := strings.Fields(m[1])
	sort.Strings(legs)
	if want := wfList(t, wfJob(t, "ci.yml", "unit"), "leg"); strings.Join(legs, " ") != strings.Join(want, " ") {
		t.Errorf("coverage expects legs %v, the unit matrix has %v", legs, want)
	}
	// a failed unit leg must fail the coverage check, not skip it (a skipped required check passes):
	// needs: alone skips the job, so it runs unless the run was cancelled
	if !regexp.MustCompile(`(?m)^    if: \$\{\{ !cancelled\(\) \}\}$`).MatchString(cov) {
		t.Error("coverage is skipped when a unit leg fails: it needs if: ${{ !cancelled() }}")
	}
	if strings.Contains(cov, "continue-on-error") || len(regexp.MustCompile(`(?m)^\s*(- )?if:`).FindAllString(cov, -1)) != 1 {
		t.Error("coverage can be skipped or cannot fail")
	}
	wfScript(t, "../scripts/coverage-merge_test.sh")
}

// WF7d: every conformance test belongs to exactly one shard; the check runs in make lint.
func TestWF7_ShardCompletenessRunsInLint(t *testing.T) {
	mk := string(must(os.ReadFile("../Makefile")))
	i := strings.Index(mk, "\nlint:")
	if i < 0 {
		t.Fatal("no lint target")
	}
	lint := mk[i+1:]
	lint = lint[:strings.Index(lint, "\n\n")]
	for _, s := range []string{"scripts/conformance-shards_test.sh", "scripts/coverage-merge_test.sh", "scripts/retry_test.sh"} {
		if !strings.Contains(lint, s) {
			t.Errorf("make lint does not run %s", s)
		}
	}
	// the shard table: every conformance package is covered, every shard has a package and a regex
	for _, sh := range strings.Fields(wfScript(t, "../scripts/conformance-shards.sh", "shards")) {
		if strings.TrimSpace(wfScript(t, "../scripts/conformance-shards.sh", "pkg", sh)) == "" {
			t.Errorf("shard %s has no package", sh)
		}
	}
	// a name that matches two shards, or none, is reported (run against a made-up table)
	tbl := filepath.Join(t.TempDir(), "table")
	if err := os.WriteFile(tbl, []byte("a ./conformance ^TestX\nb ./conformance ^TestXY\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run := func(in string) (string, error) {
		c := exec.Command("sh", "../scripts/conformance-shards.sh", "verify")
		c.Env = append(os.Environ(), "CONFORMANCE_SHARDS_TABLE="+tbl)
		c.Stdin = strings.NewReader(in)
		out, err := c.CombinedOutput()
		return string(out), err
	}
	if out, err := run("./conformance TestXA\n"); err != nil {
		t.Errorf("one owner: %v\n%s", err, out)
	}
	if out, err := run("./conformance TestXYZ\n"); err == nil || !strings.Contains(out, "TestXYZ") {
		t.Errorf("two owners passed or are unnamed: %v\n%s", err, out)
	}
	if out, err := run("./conformance TestQ\n"); err == nil || !strings.Contains(out, "TestQ") {
		t.Errorf("no owner passed or is unnamed: %v\n%s", err, out)
	}
	if out, err := run("./other TestXA\n"); err == nil || !strings.Contains(out, "./other") {
		t.Errorf("an unknown package passed or is unnamed: %v\n%s", err, out)
	}
}

// WF7e: the tool install is retried, boundedly, in both workflows (a release failed on one flaky
// download); nothing else is retried.
func TestWF7_ToolInstallRetriesBoundedly(t *testing.T) {
	re := regexp.MustCompile(`^\s*(run: )?sh scripts/retry\.sh ([0-9]+) ([0-9]+) make tools-ci$`)
	for f, jobs := range map[string][]string{"ci.yml": {"lint", "build"}, "release.yml": {"dry-run", "release"}} {
		for _, j := range jobs {
			n := 0
			for _, l := range strings.Split(wfJob(t, f, j), "\n") {
				if !strings.Contains(l, "make tools-ci") {
					continue
				}
				m := re.FindStringSubmatch(l)
				if m == nil {
					t.Errorf("%s %s: the tool install is not retried: %q", f, j, l)
					continue
				}
				tries, delay := atoiOr(m[2]), atoiOr(m[3])
				if tries < 2 || tries > 5 || delay < 10 || delay > 60 {
					t.Errorf("%s %s: retry of %d tries, %d s apart is not bounded sensibly", f, j, tries, delay)
				}
				n++
			}
			if n != 1 {
				t.Errorf("%s %s: %d tool installs, want 1", f, j, n)
			}
		}
	}
	wfScript(t, "../scripts/retry_test.sh")
	// nothing else is retried: retry.sh appears in the workflows only on make tools-ci lines
	files, _ := filepath.Glob("../.github/workflows/*.yml")
	for _, f := range files {
		for i, l := range strings.Split(string(must(os.ReadFile(f))), "\n") {
			if strings.Contains(l, "retry.sh") && !strings.HasSuffix(strings.TrimSpace(l), "make tools-ci") {
				t.Errorf("%s:%d: retry.sh wraps something other than make tools-ci: %q", f, i+1, l)
			}
		}
	}
}

// The TIM1 cap ratchet compares with main, so the conformance shard that runs it needs the whole
// history. fetch-depth is a literal 0: in an expression 0 is falsy, so `x && 0 || 1` is always 1.
func TestWF7_TheTIM1ShardChecksOutTheWholeHistory(t *testing.T) {
	owner := strings.TrimSpace(wfScript(t, "../scripts/conformance-shards.sh", "owners", "./conformance", "TestTIM1_CapOnlyGoesDown"))
	if owner == "" || strings.Contains(owner, "\n") {
		t.Fatalf("TestTIM1_CapOnlyGoesDown has owners %q, want exactly one", owner)
	}
	for _, j := range []string{"conformance", "conformance-race"} {
		b := wfJob(t, "ci.yml", j)
		if !regexp.MustCompile(`(?m)^\s+fetch-depth: 0$`).MatchString(b) {
			t.Errorf("%s (shard %s runs TIM1): fetch-depth is not the literal 0:\n%s", j, owner, b)
		}
		if strings.Contains(b, "&& 0 ||") {
			t.Errorf("%s: a fetch-depth expression with a falsy 0 in the middle is always the other value", j)
		}
	}
}

func atoiOr(s string) int {
	n := 0
	for _, c := range s {
		n = n*10 + int(c-'0')
	}
	return n
}

func TestWF7_ReleasingDocNamesTheJobs(t *testing.T) {
	doc := string(must(os.ReadFile("../docs/RELEASING.md")))
	for _, j := range wfCIJobs {
		if !strings.Contains(doc, "`"+j+"`") {
			t.Errorf("docs/RELEASING.md does not name the `%s` job", j)
		}
	}
	if strings.Contains(doc, "each one `make` command") {
		t.Error("docs/RELEASING.md says every job is one make command; build, macos-smoke and secrets are not")
	}
	if strings.Contains(doc, "A missing, empty or failed leg fails it by name") || !strings.Contains(doc, "dry run runs in the merge queue") {
		t.Error("docs/RELEASING.md misstates what a failed leg does to coverage, or omits the dry run in the merge queue")
	}
	if !strings.Contains(doc, "merge queue") || !strings.Contains(doc, "merge_group") {
		t.Error("docs/RELEASING.md does not say the merge queue runs the same jobs (merge_group)")
	}
}
