package conformance

// SH34 and SH35 (Task H.5, controller side): the ssh upload transport. The commit goes to the
// host as a git bundle on the gate session, right after the boot frame of `slot-boot --run`: no
// clone_url, no deploy key and no job ref on a forge. The agent side (SH36, SH37) and the end to
// end run (SH38) are other tasks.

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/Exploded-views/bitc/conformance/harness"
	"github.com/Exploded-views/bitc/internal/agent"
	"github.com/Exploded-views/bitc/internal/provider/ssh/sshfake"
)

const shUploadCfg = `transport = { kind = "upload", max_bundle = "1GiB" }` + "\n"

// shGit runs git in dir with a fixed identity and no system config.
func shGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	c := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.invalid", "-c", "commit.gpgsign=false"}, args...)...)
	c.Dir = dir
	c.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1")
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

// shCommitRandom commits one file of n incompressible bytes, so the bundle is about n bytes.
func shCommitRandom(t *testing.T, wt string, n int) {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, "blob.bin"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	shGit(t, wt, "add", "-A")
	shGit(t, wt, "commit", "-q", "-m", "blob")
}

// shRunCall is the recorded `slot-boot --run` call: the decoded payload, the spec and the
// bytes the fake saved after the frame.
type shRunCall struct {
	payload agent.BootPayload
	spec    agent.Spec
	frame   []byte
	bundle  []byte // what followed the frame, as the fake saved it
	note    string // the fake's own line about the stream
}

func (e *shEnv) runCall(t *testing.T) shRunCall {
	t.Helper()
	var c *sshfake.Call
	for _, x := range e.calls() {
		if x.Op == "run" {
			x := x
			c = &x
		}
	}
	if c == nil {
		t.Fatalf("no run call; ops %v", e.ops())
	}
	data := []byte(c.Data)
	if len(data) < 8 {
		t.Fatalf("run data %q", data)
	}
	n := int(binary.BigEndian.Uint64(data[:8]))
	var out shRunCall
	out.frame = data[:8+n]
	if err := json.Unmarshal(data[8:8+n], &out.payload); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if err := json.Unmarshal([]byte(strings.SplitN(string(data[8+n:]), "\n", 3)[1]), &out.spec); err != nil {
		t.Fatalf("spec: %v in %q", err, data[8+n:])
	}
	if i := bytes.Index(data[8+n:], []byte("\nupload ")); i >= 0 {
		out.note = strings.TrimSpace(string(data[8+n+i:]))
	}
	out.bundle, _ = os.ReadFile(filepath.Join(e.root, "uploads", out.spec.Job+".bundle"))
	return out
}

func shUploadRefs(t *testing.T, wt string) string {
	t.Helper()
	return strings.TrimSpace(shGit(t, wt, "for-each-ref", "refs/bitc/upload"))
}

// ---- SH34

func TestSH34_ConfigKindsUploadIsSSHOnlyAndMaxBundleBounds(t *testing.T) {
	for _, extra := range []string{shUploadCfg, `transport = { kind = "git-push" }` + "\n", `transport = { kind = "upload", max_bundle = "8GiB" }` + "\n",
		`transport = { kind = "upload", max_bundle = "1KiB" }` + "\n"} {
		h := harness.New(t)
		shRepo(t, h, shOpts{extra: extra})
		if r := h.Run("config", "check"); r.Code != 0 {
			t.Errorf("%q must be valid: %+v", extra, r)
		}
	}
	cases := []struct {
		name string
		o    shOpts
		want string
	}{
		{"kind", shOpts{extra: `transport = { kind = "zip" }` + "\n"}, `providers.ssh.transport.kind = "zip" must be "git-push" or "upload"`},
		{"kind not a string", shOpts{extra: `transport = { kind = 3 }` + "\n"}, `providers.ssh.transport.kind = 3 must be "git-push" or "upload"`},
		{"max_bundle over 8GiB", shOpts{extra: `transport = { kind = "upload", max_bundle = "9GiB" }` + "\n"},
			`providers.ssh.transport.max_bundle = "9GiB" must be more than 0 and at most 8GiB`},
		{"max_bundle zero", shOpts{extra: `transport = { kind = "upload", max_bundle = "0MiB" }` + "\n"}, `providers.ssh.transport.max_bundle = "0MiB"`},
		{"max_bundle unit", shOpts{extra: `transport = { kind = "upload", max_bundle = "lots" }` + "\n"}, `providers.ssh.transport.max_bundle = "lots" must be a size`},
		{"upload is ssh only", shOpts{fake: true, order: `"ssh", "fake", "local"`, host: `transport = { kind = "upload" }` + "\n"},
			`providers.fake.transport.kind = "upload" is valid on ssh only`},
	}
	for _, c := range cases {
		h := harness.New(t)
		shRepo(t, h, c.o)
		if r := h.Run("config", "check"); r.Code != 78 || !strings.Contains(r.Stderr, ": "+c.want) {
			t.Errorf("%s: want 78 with %q, got %+v", c.name, c.want, r)
		}
	}
}

func TestSH34_UploadNeedsNoCloneURLOrDeployKey(t *testing.T) {
	h := harness.New(t)
	e := shRepo(t, h, shOpts{extra: shUploadCfg})
	// no git remote at all: the git-push transport would call the job ineligible
	shGit(t, h.Cwd, "remote", "remove", "origin")
	r := h.Run(shEcho...)
	if strings.Contains(r.Stderr, "no git remote") || strings.Contains(r.Stderr, "clone_url") || strings.Contains(r.Stderr, "deploy_key") {
		t.Fatalf("upload must not need a clone URL or a key: %+v", r)
	}
	rc := e.runCall(t)
	if rc.spec.CloneURL != "" || rc.payload.DeployKey != "" {
		t.Errorf("clone_url %q, deploy_key %q: want neither", rc.spec.CloneURL, rc.payload.DeployKey)
	}
}

func TestSH34_DoctorShowsTheTransportAndJudgesTheHostsGit(t *testing.T) {
	doctor := func(t *testing.T, extra, hostOut string) (string, int) {
		t.Helper()
		h := harness.New(t)
		e := shRepo(t, h, shOpts{extra: extra})
		if hostOut != "" {
			e.script(t, sshfake.Rule{Op: "check-host", Times: -1, Exit: 0, Stdout: "ok ssh-arch: amd64\n" + hostOut})
		}
		return shDoctor(h)
	}
	t.Run("git-push prints no transport line", func(t *testing.T) {
		if out, _ := doctor(t, "", ""); strings.Contains(out, "ssh transport") || strings.Contains(out, "ssh-git") {
			t.Errorf("default transport:\n%s", out)
		}
	})
	t.Run("a new enough git", func(t *testing.T) {
		out, code := doctor(t, shUploadCfg, "ok ssh-git-version: git version 2.46.0\nok ssh-git-changelog:\n")
		for _, want := range []string{"ok ssh transport: upload (max 1GiB)\n", "ok ssh-git: git 2.46.0\n"} {
			if !strings.Contains(out, want) {
				t.Errorf("doctor lacks %q:\n%s", want, out)
			}
		}
		if code != 0 || strings.Contains(out, "ssh-git-version") {
			t.Errorf("exit %d:\n%s", code, out)
		}
	})
	t.Run("an old git whose package changelog names the CVE", func(t *testing.T) {
		out, code := doctor(t, `transport = { kind = "upload", max_bundle = "512MiB" }`+"\n",
			"ok ssh-git-version: git version 2.39.5\nok ssh-git-changelog: CVE-2024-32002\n")
		for _, want := range []string{"ok ssh transport: upload (max 512MiB)\n", "ok ssh-git: git 2.39.5 (the package changelog names CVE-2024-32002)\n"} {
			if !strings.Contains(out, want) {
				t.Errorf("doctor lacks %q:\n%s", want, out)
			}
		}
		if code != 0 {
			t.Errorf("exit %d:\n%s", code, out)
		}
	})
	t.Run("an old git without the fix fails", func(t *testing.T) {
		out, code := doctor(t, shUploadCfg, "ok ssh-git-version: git version 2.39.5\nok ssh-git-changelog:\n")
		if want := "fail ssh-git: git 2.39.5 is older than 2.45.1 and its package changelog does not name CVE-2024-32002\n"; !strings.Contains(out, want) || code != 69 {
			t.Errorf("want %q and exit 69, got %d:\n%s", want, code, out)
		}
	})
	t.Run("a host that reports no git version fails", func(t *testing.T) {
		out, code := doctor(t, shUploadCfg, "")
		if want := "fail ssh-git: the host did not report its git version"; !strings.Contains(out, want) || code != 69 {
			t.Errorf("want %q and exit 69, got %d:\n%s", want, code, out)
		}
	})
}

// ---- SH35

func TestSH35_TheTempRefIsMadeAndGoneAfterSuccessAndAfterFailure(t *testing.T) {
	for _, c := range []struct {
		name string
		rule *sshfake.Rule
	}{
		{"success", nil},
		{"the run fails", &sshfake.Rule{Op: "run", Times: -1, Read: true, Exit: 255, Stderr: "client_loop: send disconnect: Broken pipe\n"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := harness.New(t)
			e := shRepo(t, h, shOpts{extra: shUploadCfg})
			if c.rule != nil {
				e.script(t, *c.rule)
			}
			h.Run(shEcho...)
			rc := e.runCall(t)
			// made: the bundle the host received carries exactly the job's temporary ref
			bf := filepath.Join(h.Root, "got.bundle")
			if err := os.WriteFile(bf, rc.bundle, 0o600); err != nil {
				t.Fatal(err)
			}
			heads := shGit(t, h.Cwd, "bundle", "list-heads", bf)
			head := strings.TrimSpace(shGit(t, h.Cwd, "rev-parse", "HEAD"))
			if want := head + " refs/bitc/upload/" + rc.spec.Job; strings.TrimSpace(heads) != want {
				t.Errorf("bundle heads %q, want %q", heads, want)
			}
			if rc.spec.Sha != head || rc.spec.Ref != "refs/bitc/upload/"+rc.spec.Job {
				t.Errorf("spec sha %q ref %q", rc.spec.Sha, rc.spec.Ref)
			}
			// gone: no upload ref in the work tree and no temp file left in the account dir
			if refs := shUploadRefs(t, h.Cwd); refs != "" {
				t.Errorf("refs left behind: %s", refs)
			}
			left, _ := filepath.Glob(filepath.Join(h.Root, "account", "remote", "ssh", "upload", "*", "*"))
			if len(left) != 0 {
				t.Errorf("temp bundle left behind: %v", left)
			}
		})
	}
}

var shOverRE = regexp.MustCompile(`bundle is [0-9]+ MiB, over max_bundle 1MiB`)

func TestSH35_OverMaxBundleIsIneligibleAndNothingIsCreated(t *testing.T) {
	h := harness.New(t)
	e := shRepo(t, h, shOpts{extra: `transport = { kind = "upload", max_bundle = "1MiB" }` + "\n"})
	shCommitRandom(t, h.Cwd, 1536<<10)
	r := h.Run(shEcho...)
	if r.Code != 0 || r.Stdout != "on-ssh\n" || !strings.Contains(r.Stderr, "bitc: ssh: running heavy locally: ") || !shOverRE.MatchString(r.Stderr) {
		t.Fatalf("want a local fallback naming the bundle size: %+v", r)
	}
	for _, op := range e.ops() { // the walk's live count (list) is not a resource
		if op != "list" {
			t.Errorf("nothing may be created on the host, got ops %v", e.ops())
			break
		}
	}
	for i := 1; i <= e.slots; i++ {
		if !e.free(t, i) {
			t.Errorf("slot lock %d is held", i)
		}
	}
	if refs := shUploadRefs(t, h.Cwd); refs != "" {
		t.Errorf("refs left behind: %s", refs)
	}
	if left, _ := filepath.Glob(filepath.Join(h.Root, "account", "remote", "ssh", "upload", "*", "*")); len(left) != 0 {
		t.Errorf("temp bundle left behind: %v", left)
	}
}

func TestSH35_ThePayloadHasCodeNoKeyAndTheBytesFollowTheFrame(t *testing.T) {
	h := harness.New(t)
	e := shRepo(t, h, shOpts{extra: shUploadCfg})
	shCommitRandom(t, h.Cwd, 300<<10)
	h.Run(shEcho...)
	rc := e.runCall(t)
	c := rc.payload.Code
	if c == nil || c.Kind != "upload" || c.Ref != "refs/bitc/upload/"+rc.spec.Job {
		t.Fatalf("payload code %+v", c)
	}
	if rc.payload.DeployKey != "" || bytes.Contains(rc.frame, []byte("BEGIN")) {
		t.Errorf("an upload payload carries no deploy key: %q", rc.payload.DeployKey)
	}
	if c.Len < 300<<10 || int64(len(rc.bundle)) != c.Len || rc.note != fmt.Sprintf("upload %d bytes", c.Len) {
		t.Errorf("len %d, the host received %d bytes (%q)", c.Len, len(rc.bundle), rc.note)
	}
	if sum := sha256.Sum256(rc.bundle); hex.EncodeToString(sum[:]) != c.SHA256 {
		t.Errorf("sha256 of the received bytes is not the payload's %s", c.SHA256)
	}
}

func TestSH35_TheCoreMakesNoPush(t *testing.T) {
	h := harness.New(t)
	e := shRepo(t, h, shOpts{extra: shUploadCfg})
	// a push could not succeed; neither could deleting a ref afterwards
	shGit(t, h.Cwd, "remote", "set-url", "origin", "/nonexistent/origin.git")
	r := h.Run(shEcho...)
	if strings.Contains(r.Stderr, "could not push") {
		t.Fatalf("the core pushed: %+v", r)
	}
	if e.count("run") != 1 {
		t.Fatalf("ops %v, want the job attached", e.ops())
	}
	if refs := strings.TrimSpace(shGit(t, h.Origin("app"), "for-each-ref", "--format=%(refname)")); refs != "refs/heads/main" {
		t.Errorf("origin refs %q, want only refs/heads/main", refs)
	}
}
