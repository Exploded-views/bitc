package run

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Exploded-views/bitc/internal/proctest"
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
	head, clean := treeState(context.Background(), t.TempDir(), os.Environ(), time.Minute)
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
	head, clean := treeState(context.Background(), t.TempDir(), os.Environ(), 500*time.Millisecond)
	if head != "0123456789abcdef0123456789abcdef01234567" || clean != nil {
		t.Fatalf("%q %v", head, clean)
	}
}

// A descendant of git that keeps its output open (a hook, fsmonitor) must not stall the capture
// past the bound: git and its group are killed and the call gives up on the pipes.
func TestTreeStateBoundIsNotDefeatedByAPipeHolder(t *testing.T) {
	dir := t.TempDir()
	pidFile, mark := filepath.Join(dir, "pid"), filepath.Join(dir, "finished")
	fakeGit(t, `case "$*" in *rev-parse*) echo 0123456789abcdef0123456789abcdef01234567;; *)
( sleep 20; : > `+mark+` ) &
echo $! > `+pidFile+`
wait;;
esac`)
	head, clean := treeState(context.Background(), t.TempDir(), os.Environ(), 500*time.Millisecond)
	if head != "0123456789abcdef0123456789abcdef01234567" || clean != nil {
		t.Fatalf("%q %v", head, clean)
	}
	if _, err := os.Stat(mark); err == nil { // treeState returned only when the holder ended by itself
		t.Fatal("the capture waited for the process holding git's output")
	}
	b, _ := os.ReadFile(pidFile)
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	deadline := time.Now().Add(5 * time.Second)
	for pid > 0 && !proctest.Gone(pid) && time.Now().Before(deadline) {
		runtime.Gosched()
	}
	if pid == 0 || !proctest.Gone(pid) {
		t.Fatalf("git's child %d outlived the capture", pid)
	}
}

// A signal (a cancelled context) during the capture cuts git short.
func TestTreeStateIsCutShortByTheContext(t *testing.T) {
	log := fakeGit(t, `exec sleep 60`)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if head, clean := treeState(ctx, t.TempDir(), os.Environ(), 2*time.Second); head != "" || clean != nil {
		t.Fatalf("%q %v", head, clean)
	}
	if b, _ := os.ReadFile(log); len(b) != 0 {
		t.Fatalf("git ran under a cancelled context: %s", b)
	}
}
