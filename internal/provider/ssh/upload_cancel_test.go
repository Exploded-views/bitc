package ssh

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Exploded-views/bitc/internal/config"
)

// SH35: a cancelled context (Ctrl-C during `git bundle create`) must still delete the temporary
// ref from the user's repository.
func TestSH35_TheTempRefIsGoneAfterTheContextIsCancelledDuringBundleCreate(t *testing.T) {
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no git")
	}
	root := t.TempDir()
	wt := filepath.Join(root, "wt")
	run := func(args ...string) string {
		t.Helper()
		c := exec.Command(realGit, append([]string{"-C", wt, "-c", "user.name=t", "-c", "user.email=t@x"}, args...)...)
		c.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + root, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null"}
		out, err := c.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	if err := os.MkdirAll(wt, 0o700); err != nil {
		t.Fatal(err)
	}
	run("init", "-q")
	run("commit", "-q", "--allow-empty", "-m", "i")
	sha := run("rev-parse", "HEAD")

	// a git that hangs in `bundle create` once it has started, and is the real git otherwise
	bin := filepath.Join(root, "bin")
	started := filepath.Join(root, "started")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nfor a in \"$@\"; do if [ \"$a\" = bundle ]; then : > '" + started + "'; exec sleep 60; fi; done\nexec '" + realGit + "' \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		for i := 0; i < 600; i++ {
			if _, err := os.Stat(started); err == nil {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		cancel()
	}()
	if b, err := makeBundle(ctx, wt, filepath.Join(root, "up"), "job1", sha, config.Size(1<<30)); err == nil {
		b.Close()
		t.Fatalf("makeBundle succeeded although git bundle was cancelled")
	}
	if _, err := os.Stat(started); err != nil {
		t.Fatalf("git bundle never started")
	}
	if refs := run("for-each-ref", "refs/bitc/upload/"); refs != "" {
		t.Errorf("the temporary ref is still there after the cancel: %s", refs)
	}
}
