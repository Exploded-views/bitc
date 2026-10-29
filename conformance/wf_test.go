package conformance

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
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
	// this repo's own workflows, privileged.yml (private tree only) included
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
