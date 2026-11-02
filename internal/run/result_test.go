package run

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeGit points gitProgram at a script that logs its argv and runs body (sh) for each call.
func fakeGit(t *testing.T, body string) (log string) {
	t.Helper()
	dir := t.TempDir()
	log = filepath.Join(dir, "argv")
	script := "#!/bin/sh\necho \"$*\" >> " + log + "\n" + body + "\n"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	old := gitProgram
	gitProgram = filepath.Join(dir, "git")
	t.Cleanup(func() { gitProgram = old })
	return log
}

// The capture never takes .git/index.lock and does not depend on the repo's or user's config
// for what counts as clean.
func TestTreeStateGitArgv(t *testing.T) {
	log := fakeGit(t, `case "$*" in *rev-parse*) echo 0123456789abcdef0123456789abcdef01234567;; esac`)
	head, clean := treeState(t.TempDir(), os.Environ(), time.Minute)
	if head != "0123456789abcdef0123456789abcdef01234567" || clean == nil || !*clean {
		t.Fatalf("%q %v", head, clean)
	}
	b, _ := os.ReadFile(log)
	var status string
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if strings.Contains(l, "status") {
			status = l
		}
	}
	for _, want := range []string{"--no-optional-locks", "status", "--porcelain", "--untracked-files=normal", "--ignore-submodules=none"} {
		if !strings.Contains(status, want) {
			t.Errorf("git call %q lacks %s", status, want)
		}
	}
}

// A git that hangs costs the run a bounded wait: head is kept and tree_clean omitted.
func TestTreeStateBoundedByTheLimit(t *testing.T) {
	fakeGit(t, `case "$*" in *rev-parse*) echo 0123456789abcdef0123456789abcdef01234567;; *) exec sleep 60;; esac`)
	head, clean := treeState(t.TempDir(), os.Environ(), 500*time.Millisecond)
	if head != "0123456789abcdef0123456789abcdef01234567" || clean != nil {
		t.Fatalf("%q %v", head, clean)
	}
}
