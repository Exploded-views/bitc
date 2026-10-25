package conformance

// SH34 (the host's git items) and SH38 (Task H.5 integrator): `check-host --doctor` relays the two
// items the controller's ssh-git rule judges, and an upload job runs end to end on the ssh fake.

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Exploded-views/bitc/conformance/harness"
	"github.com/Exploded-views/bitc/internal/agent"
	"github.com/Exploded-views/bitc/internal/provider/ssh/sshfake"
)

func shChangelog(t *testing.T, root, text string) {
	t.Helper()
	p := filepath.Join(root, "usr/share/doc/git/changelog.Debian.gz")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	zw := gzip.NewWriter(&b)
	_, _ = zw.Write([]byte(text))
	_ = zw.Close()
	if err := os.WriteFile(p, b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func shGitItems(root string, version string, gitErr error) []string {
	var out []string
	for _, f := range agent.GitItems(agent.HostEnv{Root: root, Exec: func(stdin, name string, args ...string) ([]byte, error) {
		if name != "git" || len(args) != 1 || args[0] != "--version" {
			return nil, fmt.Errorf("unexpected command %s %v", name, args)
		}
		return []byte(version), gitErr
	}}) {
		out = append(out, f.String())
	}
	return out
}

func TestSH34_CheckHostDoctorRelaysTheHostsGitVersionAndChangelog(t *testing.T) {
	const cve = "CVE-2024-32002"
	t.Run("a package changelog that names the CVE", func(t *testing.T) {
		root := t.TempDir()
		shChangelog(t, root, "git (1:2.39.5-0+deb12u1) bookworm; urgency=high\n\n  * Fix "+cve+": symlinks in submodules.\n  * other\n")
		got := shGitItems(root, "git version 2.39.5\n", nil)
		want := []string{"ok ssh-git-version: git version 2.39.5", "ok ssh-git-changelog: * Fix " + cve + ": symlinks in submodules."}
		if strings.Join(got, "|") != strings.Join(want, "|") {
			t.Errorf("got %q, want %q", got, want)
		}
	})
	t.Run("no changelog or none naming the CVE gives an empty item", func(t *testing.T) {
		for _, text := range []string{"", "git (1:2.39.5-0) x\n  * CVE-2024-32004\n"} {
			root := t.TempDir()
			if text != "" {
				shChangelog(t, root, text)
			}
			got := shGitItems(root, "git version 2.46.0\n", nil)
			want := []string{"ok ssh-git-version: git version 2.46.0", "ok ssh-git-changelog"}
			if strings.Join(got, "|") != strings.Join(want, "|") {
				t.Errorf("changelog %q: got %q, want %q", text, got, want)
			}
		}
	})
	t.Run("a host with no git prints neither item", func(t *testing.T) {
		if got := shGitItems(t.TempDir(), "", fmt.Errorf("executable file not found")); len(got) != 0 {
			t.Errorf("got %q, want nothing", got)
		}
	})
	t.Run("an unreadable changelog is empty", func(t *testing.T) {
		root := t.TempDir()
		p := filepath.Join(root, "usr/share/doc/git/changelog.Debian.gz")
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		_ = os.WriteFile(p, []byte("not gzip"), 0o644)
		got := shGitItems(root, "git version 2.46.0\n", nil)
		if len(got) != 2 || got[1] != "ok ssh-git-changelog" {
			t.Errorf("got %q", got)
		}
	})
}

func TestSH38_AnUploadJobWithNoCloneURLRunsOnTheFakeHostAndItsOutputArrives(t *testing.T) {
	h := harness.New(t)
	e := shRepo(t, h, shOpts{extra: shUploadCfg})
	shGit(t, h.Cwd, "remote", "remove", "origin") // no clone_url and no forge: only the bundle can carry the code
	if err := os.WriteFile(filepath.Join(h.Cwd, "hello.txt"), []byte("uploaded-code\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	shGit(t, h.Cwd, "add", "-A")
	shGit(t, h.Cwd, "commit", "-q", "-m", "hello")
	r := h.Run("run", "heavy", "--", "sh", "-c", "cat hello.txt; echo on-ssh")
	if r.Code != 0 || r.Stdout != "uploaded-code\non-ssh\n" || strings.Contains(r.Stderr, "locally") {
		t.Fatalf("the upload job did not run on ssh with the committed file: %+v", r)
	}
	rc := e.runCall(t)
	if rc.payload.Code == nil || rc.payload.DeployKey != "" || rc.spec.CloneURL != "" || rc.note != fmt.Sprintf("upload %d bytes", rc.payload.Code.Len) {
		t.Errorf("payload code %+v, deploy key %q, clone_url %q, %q", rc.payload.Code, rc.payload.DeployKey, rc.spec.CloneURL, rc.note)
	}
	if e.count("run") != 1 || e.count("teardown") == 0 {
		t.Errorf("ops %v, want one run and a teardown", e.ops())
	}
	if _, err := os.Lstat(filepath.Join(rc.spec.TestRoot, "work", ".bitc-upload")); !os.IsNotExist(err) {
		t.Errorf("the bundle is still on the host after the job: %v", err)
	}
	if refs := shUploadRefs(t, h.Cwd); refs != "" {
		t.Errorf("a temporary upload ref is left: %q", refs)
	}
}

func TestSH34_TheGitItemsAreNotShownWithoutTheUploadTransport(t *testing.T) {
	h := harness.New(t)
	e := shRepo(t, h, shOpts{})
	e.script(t, sshfake.Rule{Op: "check-host", Times: -1, Exit: 0, Stdout: "ok ssh-arch: amd64\nok ssh-git-version: git version 2.39.5\nok ssh-git-changelog:\n"})
	out, code := shDoctor(h)
	if code != 0 || strings.Contains(out, "ssh-git") {
		t.Errorf("exit %d, the git items must stay out of a git-push doctor:\n%s", code, out)
	}
}
