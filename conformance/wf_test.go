package conformance

import (
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
