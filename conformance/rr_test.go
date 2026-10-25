package conformance

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Exploded-views/bitc/conformance/harness"
)

func fellBack(t *testing.T, r harness.Result, why string) {
	t.Helper()
	if r.Code != 0 || !strings.Contains(r.Stderr, "bitc: fake: running heavy locally: "+why) || strings.Count(r.Stderr, "running heavy locally") != 1 {
		t.Fatalf("want a fallback for %q: %+v", why, r)
	}
}

func TestRR3_StdinWithDataStaysLocal(t *testing.T) {
	h := harness.New(t)
	fakeRepo(t, h, "run", "")
	data := filepath.Join(h.Root, "data.txt")
	h.Write(data, "data\n", 0o600)
	f, err := os.Open(data)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	r := runStdin(h, f, "run", "heavy", "--", "sh", "-c", "cat")
	if !strings.Contains(r.Stderr, "running heavy locally: the command's stdin has data waiting") {
		t.Fatalf("%+v", r)
	}
	fellBack(t, h.Run("run", "heavy", "--", "echo", "-"), "the command reads stdin (--stdin or -)")
	if r := h.Run("run", "heavy", "--", "echo", "idle"); strings.Contains(r.Stderr, "locally") {
		t.Fatalf("/dev/null stdin fell back: %+v", r)
	}
}

func TestRR4_DirtyTreeStaysLocal(t *testing.T) {
	h := harness.New(t)
	wt := fakeRepo(t, h, "run", "")
	h.Write(filepath.Join(wt, "new.txt"), "x", 0o644)
	fellBack(t, h.Run("run", "heavy", "--", "echo", "x"), "the work tree has uncommitted or untracked changes")
}

func TestRR5_UnlistedCommandStaysLocal(t *testing.T) {
	h := harness.New(t)
	fakeRepo(t, h, "run", "")
	fellBack(t, h.Run("run", "heavy", "--", "true"), "the command matches no allow entry of [pools.heavy.remote]")
}

func TestRR6_HostOrIgnoredPathArgumentStaysLocal(t *testing.T) {
	h := harness.New(t)
	fakeRepo(t, h, "run", "")
	fellBack(t, h.Run("run", "heavy", "--", "echo", "/usr/share/doc"), "the command names '/usr/share/doc', a host path outside the work tree")
}

func TestRR7_ForbiddenIgnoredFilesStayLocal(t *testing.T) {
	h := harness.New(t)
	h.HostConfig("[remote_limits]\nworktrees = [\"**/app\"]\n[pools.heavy]\nproviders = [\"fake\", \"local\"]\nremote_limits = { size = { fake = \"small\" } }\n[providers.fake]\ndaily_usd = 1\n")
	h.Cwd = h.Repo("app", map[string]string{".gitignore": ".env*\n", ".bitc.toml": "version = 1\n[remote]\nforbid_ignored = [\".env\", \".env.*\"]\nallow_ignored = [\".env.example\"]\n[pools.heavy.remote]\nallow = [[\"echo\", \"*\"]]\n"})
	h.Write(filepath.Join(h.Cwd, ".env.example"), "x", 0o644)
	if r := h.Run("run", "heavy", "--", "echo", "x"); strings.Contains(r.Stderr, "locally") {
		t.Fatalf("allow_ignored file blocked: %+v", r)
	}
	h.Write(filepath.Join(h.Cwd, ".env.local"), "x", 0o644)
	fellBack(t, h.Run("run", "heavy", "--", "echo", "x"), "the work tree has git-ignored files a remote job would not have (.env.local)")
}

func TestRR8_BadArtifactPathsStayLocal(t *testing.T) {
	for path, why := range map[string]string{
		"README.md": "artifact path 'README.md' holds tracked files",
		"../out":    "artifact path '../out' must be relative and inside the work tree",
		".git":      "artifact path '.git' is under .git",
		"link/out":  "artifact path 'link/out' passes through the symbolic link 'link'",
	} {
		h := harness.New(t)
		h.HostConfig("[remote_limits]\nworktrees = [\"**/app\"]\n[pools.heavy]\nproviders = [\"fake\", \"local\"]\nremote_limits = { size = { fake = \"small\" } }\n[providers.fake]\ndaily_usd = 1\n")
		h.Cwd = h.Repo("app", map[string]string{"README.md": "x", ".gitignore": "link\n",
			".bitc.toml": "version = 1\n[pools.heavy.remote]\nallow = [[\"echo\", \"*\"]]\nartifacts = [{ path = " + harness.Q(path) + " }]\n"})
		_ = os.Symlink(h.Root, filepath.Join(h.Cwd, "link"))
		fellBack(t, h.Run("run", "heavy", "--", "echo", "x"), why)
	}
}

func TestRR9_AbsoluteArgv0MatchedByBasename(t *testing.T) {
	h := harness.New(t)
	fakeRepo(t, h, "run", "")
	echo, _ := exec.LookPath("echo")
	if r := h.Run("run", "heavy", "--", echo, "remote"); strings.Contains(r.Stderr, "locally") || r.Stdout != "remote\n" {
		t.Fatalf("%+v", r)
	}
}

// remoteOnlyRepo: heavy may go only to the fake (no local entry in providers); forbid_ignored
// and .gitignore let every preflight class be provoked. poolExtra is appended to [pools.heavy.remote].
func remoteOnlyRepo(t *testing.T, h *harness.H, mode, poolExtra string) string {
	t.Helper()
	h.HostConfig("[remote_limits]\nworktrees = [\"**/app\"]\n[pools.heavy]\nproviders = [\"fake\"]\n" +
		"remote_limits = { size = { fake = \"small\" } }\nwait = \"5s\"\n[providers.fake]\ndaily_usd = 1\nmode = " + harness.Q(mode) + "\n")
	h.Cwd = h.Repo("app", map[string]string{"README.md": "x", ".gitignore": ".env*\ndist/\n",
		".bitc.toml": "version = 1\n[remote]\nforbid_ignored = [\".env\", \".env.*\"]\n" +
			"[pools.heavy.remote]\nallow = [[\"sh\", \"*\"], [\"echo\", \"*\"]]\n" + poolExtra})
	return h.Cwd
}

// notEligible: 69 with the not-eligible line alone; nothing ran here or on the fake (which would
// have printed the launch line and the command's output).
func notEligible(t *testing.T, r harness.Result, why string) {
	t.Helper()
	if r.Code != 69 || r.Stdout != "" || r.Stderr != "bitc: fake: heavy: not eligible for remote: "+why+"\n" {
		t.Fatalf("want 69 for %q: %+v", why, r)
	}
}

// runStdin runs bitc in h.Cwd with f as its stdin.
func runStdin(h *harness.H, f *os.File, args ...string) harness.Result {
	cmd := exec.Command(harness.Bin, args...)
	var so, se strings.Builder
	cmd.Env, cmd.Dir, cmd.Stdin, cmd.Stdout, cmd.Stderr = h.Env, h.Cwd, f, &so, &se
	err := cmd.Run()
	return harness.Result{Code: harness.ExitCode(err, cmd.ProcessState), Stdout: so.String(), Stderr: se.String()}
}

func wentRemote(t *testing.T, r harness.Result, what string) {
	t.Helper()
	if r.Code != 0 || r.Stdout != "remote\n" || !strings.Contains(r.Stderr, "bitc: fake: heavy job ") || strings.Contains(r.Stderr, "not eligible") {
		t.Fatalf("%s did not go remote: %+v", what, r)
	}
}

// In a remote-only pool a deterministic preflight ineligibility exits 69 and never runs locally
// (spec 8.2, D3), for the reference's classes and bitc's wider set (an artifact path); a transient
// failure stays 75. An empty file or an idle pipe on stdin may still go remote (bitc's rule).
func TestRR16_RemoteOnlyIneligibleIs69NeverLocal(t *testing.T) {
	h := harness.New(t)
	wt := remoteOnlyRepo(t, h, "run", "")
	if err := os.MkdirAll(filepath.Join(wt, "dist"), 0o755); err != nil { // ignored: the tree stays clean
		t.Fatal(err)
	}
	h.Write(filepath.Join(wt, "new.txt"), "x", 0o644)
	notEligible(t, h.Run("run", "heavy", "--", "echo", "x"), "the work tree has uncommitted or untracked changes (a remote job runs a commit)")
	if err := os.Remove(filepath.Join(wt, "new.txt")); err != nil {
		t.Fatal(err)
	}
	notEligible(t, h.Run("run", "heavy", "--", "true"), "the command matches no allow entry of [pools.heavy.remote]")
	notEligible(t, h.Run("run", "heavy", "--", "echo", "/usr/share/doc"), "the command names '/usr/share/doc', a host path outside the work tree")
	notEligible(t, h.Run("run", "heavy", "--", "echo", "dist/x.js"), "the command names 'dist/x.js', which git ignores (a remote job has only tracked files)")
	// data on stdin: a non-empty regular file (deterministic, unlike a pipe still being filled)
	data, empty := filepath.Join(h.Root, "data.txt"), filepath.Join(h.Root, "empty.txt")
	h.Write(data, "data\n", 0o600)
	h.Write(empty, "", 0o600)
	for _, c := range []struct {
		path   string
		remote bool
	}{{data, false}, {empty, true}} {
		f, err := os.Open(c.path)
		if err != nil {
			t.Fatal(err)
		}
		if c.remote {
			wentRemote(t, runStdin(h, f, "run", "heavy", "--", "echo", "remote"), "an empty stdin file")
		} else {
			notEligible(t, runStdin(h, f, "run", "heavy", "--", "sh", "-c", "cat"), "the command's stdin has data waiting, which a remote job cannot read")
		}
		_ = f.Close()
	}
	pr, pw, err := os.Pipe() // an idle pipe: the writer stays open and writes nothing
	if err != nil {
		t.Fatal(err)
	}
	r := runStdin(h, pr, "run", "heavy", "--", "echo", "remote")
	_, _ = pw.Close(), pr.Close()
	wentRemote(t, r, "an idle pipe")
	h.Write(filepath.Join(wt, ".env.local"), "x", 0o644)
	notEligible(t, h.Run("run", "heavy", "--", "echo", "x"), "the work tree has git-ignored files a remote job would not have (.env.local)")
	// bitc's wider set: an artifact path holding tracked files (the reference keeps it at 75)
	h2 := harness.New(t)
	remoteOnlyRepo(t, h2, "run", "artifacts = [{ path = \"README.md\" }]\n")
	notEligible(t, h2.Run("run", "heavy", "--", "echo", "x"), "artifact path 'README.md' holds tracked files")
	// a transient failure stays 75: STARVED with the reason, no local note, nothing ran
	h3 := harness.New(t)
	remoteOnlyRepo(t, h3, "transient", "")
	if r := h3.Run("run", "heavy", "--", "echo", "x"); r.Code != 75 || r.Stdout != "" ||
		!strings.HasPrefix(r.Stderr, "STARVED heavy (no provider left: ") || strings.Contains(r.Stderr, "locally") {
		t.Fatalf("transient: %+v", r)
	}
}

func TestK4_OnlyForwardedVariablesCross(t *testing.T) {
	h := harness.New(t)
	h.HostConfig("[remote_limits]\nworktrees = [\"**/app\"]\nforward_env_allow = [\"CI\", \"TZ\"]\n[pools.heavy]\nproviders = [\"fake\", \"local\"]\nremote_limits = { size = { fake = \"small\" } }\n[providers.fake]\ndaily_usd = 1\n")
	h.Cwd = h.Repo("app", map[string]string{".bitc.toml": "version = 1\n[remote]\nforward_env = [\"CI\", \"HOME\"]\n[pools.heavy.remote]\nallow = [[\"sh\", \"*\"]]\n"})
	r := h.RunWith([]string{"CI=yes", "TZ=UTC", "OTHER=x"}, "run", "heavy", "--", "sh", "-c", `echo "CI=$CI TZ=$TZ OTHER=$OTHER HOME=$HOME"`)
	if r.Stdout != "CI=yes TZ= OTHER= HOME=\n" {
		t.Fatalf("%q", r.Stdout)
	}
}

func TestPF1_RootStepsNeedTheGrant(t *testing.T) {
	repo := "version = 1\n[remote]\nsetup = [{ name = \"prep\", run_as = \"root\", argv = [\"true\"] }]\n" +
		"[pools.heavy.remote]\nallow = [[\"echo\", \"*\"]]\n"
	for _, grant := range []bool{false, true} {
		h := harness.New(t)
		host := "[remote_limits]\nworktrees = [\"**/app\"]\n[pools.heavy]\nproviders = [\"fake\", \"local\"]\n" +
			"remote_limits = { size = { fake = \"small\" } }\n[providers.fake]\ndaily_usd = 1\n"
		if grant {
			host += "\n[[repos]]\nmatch = \"**/app\"\nroot_steps = true\n"
		}
		h.HostConfig(host)
		h.Cwd = h.Repo("app", map[string]string{".bitc.toml": repo})
		r := h.Run("run", "heavy", "--", "echo", "x")
		if !grant {
			fellBack(t, r, "the repo's remote setup runs as root but its [[repos]] grant has no root_steps")
			continue
		}
		if r.Code != 0 || r.Stdout != "x\n" || strings.Contains(r.Stderr, "locally") {
			t.Fatalf("with the grant: %+v", r)
		}
	}
}
