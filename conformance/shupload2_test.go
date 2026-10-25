package conformance

// SH37: stage 2 of the ssh upload transport, driven in-process through agent.Run with a System
// that records every command (so "git runs as the job user, through the step path" is checked on
// the commands themselves) and real git on a local bundle.

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Exploded-views/bitc/internal/agent"
)

const (
	s2UID = 4242
	s2GID = 4343
)

// s2System is PlainSystem plus a record of what the agent asked of it.
type s2System struct {
	agent.PlainSystem
	mu     sync.Mutex
	calls  []string // ordered: "ensure", "chown <path>", "cmd <argv0> root=<b> uid=<n> gid=<n>"
	cmds   []*exec.Cmd
	finish func(c *exec.Cmd)
}

func (s *s2System) rec(f string, a ...any) {
	s.mu.Lock()
	s.calls = append(s.calls, fmt.Sprintf(f, a...))
	s.mu.Unlock()
}
func (s *s2System) EnsureJobUser() (int, int, string, error) {
	s.rec("ensure")
	return s2UID, s2GID, os.Getenv("HOME"), nil
}
func (s *s2System) Chown(path string, uid, gid int) error {
	s.rec("chown %s %d:%d", path, uid, gid)
	return nil
}
func (s *s2System) Cmd(argv []string, asRoot bool, uid, gid int) *exec.Cmd {
	s.rec("cmd %s root=%v uid=%d gid=%d", argv[0], asRoot, uid, gid)
	c := s.PlainSystem.Cmd(argv, asRoot, uid, gid)
	s.mu.Lock()
	s.cmds = append(s.cmds, c)
	s.mu.Unlock()
	return c
}

// FinishCmd is the hook a host System has to rewrite a command once Dir and Env are set.
func (s *s2System) FinishCmd(c *exec.Cmd) {
	if s.finish != nil {
		s.finish(c)
	}
}

type s2Rig struct {
	t       *testing.T
	results string
	work    string // the slot's work dir (Options.WorkRoot)
	lock    string
	sys     *s2System
	spec    agent.Spec
	opts    func(o *agent.Options)
	avail   uint64
	ffree   uint64
	out     bytes.Buffer
}

func newS2Rig(t *testing.T) *s2Rig {
	t.Helper()
	root := t.TempDir()
	r := &s2Rig{t: t, results: filepath.Join(root, "results"), work: filepath.Join(root, "work"), lock: filepath.Join(root, "upload.lock"),
		sys: &s2System{}, avail: 1 << 40, ffree: 1 << 30}
	for _, d := range []string{r.work, filepath.Join(r.work, ".bitc-upload")} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	r.spec = agent.Spec{Job: "j1", Pool: "heavy", Argv: []string{"sh", "-c", "echo ran"}, Cwd: "", Ref: "refs/bitc/upload/j1",
		Timeout: agent.Duration(time.Minute), Port: 0, Nonce: "n0nce", Env: []string{}, MacKey: strings.Repeat("05", 32), Bearer: strings.Repeat("04", 32)}
	return r
}

func (r *s2Rig) bundlePath() string { return filepath.Join(r.work, ".bitc-upload", "code.bundle") }

func s2Git(t *testing.T, dir string, stdin []byte, args ...string) string {
	t.Helper()
	c := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@x", "-c", "protocol.file.allow=always"}, args...)...)
	c.Dir = dir
	c.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null"}
	if stdin != nil {
		c.Stdin = bytes.NewReader(stdin)
	}
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// bundleOf commits files (a path -> content map) in a fresh repo, bundles refs/bitc/upload/j1 to
// the rig's bundle path and sets the spec's sha. extra runs in the repo before the commit.
func (r *s2Rig) bundleOf(files map[string]string, extra func(repo string)) {
	r.t.Helper()
	repo := r.t.TempDir()
	s2Git(r.t, repo, nil, "init", "-q")
	for p, c := range files {
		full := filepath.Join(repo, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			r.t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(c), 0o600); err != nil {
			r.t.Fatal(err)
		}
	}
	if extra != nil {
		extra(repo)
	}
	s2Git(r.t, repo, nil, "add", "-A")
	s2Git(r.t, repo, nil, "commit", "-q", "-m", "i")
	r.spec.Sha = s2Git(r.t, repo, nil, "rev-parse", "HEAD")
	s2Git(r.t, repo, nil, "update-ref", "refs/bitc/upload/j1", "HEAD")
	s2Git(r.t, repo, nil, "bundle", "create", r.bundlePath(), "refs/bitc/upload/j1")
}

// bundleOfTree bundles a commit whose single tree entry is named name (raw, unchecked).
func (r *s2Rig) bundleOfTree(name string) {
	r.t.Helper()
	repo := r.t.TempDir()
	s2Git(r.t, repo, nil, "init", "-q")
	blob := s2Git(r.t, repo, []byte("x"), "hash-object", "-w", "--stdin")
	raw, _ := hex.DecodeString(blob)
	tree := s2Git(r.t, repo, append([]byte("100644 "+name+"\x00"), raw...), "hash-object", "-w", "-t", "tree", "--literally", "--stdin")
	r.spec.Sha = s2Git(r.t, repo, nil, "commit-tree", tree, "-m", "evil")
	s2Git(r.t, repo, nil, "update-ref", "refs/bitc/upload/j1", r.spec.Sha)
	s2Git(r.t, repo, nil, "bundle", "create", r.bundlePath(), "refs/bitc/upload/j1")
}

func (r *s2Rig) run() int {
	r.t.Helper()
	o := agent.Options{Stdout: &r.out, ResultsRoot: r.results, ToolsRoot: filepath.Join(r.results, "tools"), WorkRoot: r.work,
		System: r.sys, NoServe: true, Deadline: time.Now().Add(time.Hour),
		Code:       &agent.CodeUpload{Kind: "upload", Len: 1, SHA256: strings.Repeat("0", 64), Ref: "refs/bitc/upload/j1"},
		UploadLock: r.lock, Upload: agent.UploadEnv{Statfs: func(string) (uint64, uint64, error) { return r.avail, r.ffree, nil }}}
	if r.opts != nil {
		r.opts(&o)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	return agent.Run(ctx, o, r.spec, agent.Secrets{})
}

// codeFailed checks a failed code step: the marker, the message, and that neither the bundle
// dir nor a partial work/<job> is left, and that the job's command never ran.
func (r *s2Rig) codeFailed(code int, name, msg string) {
	r.t.Helper()
	log := r.out.String()
	if code != 75 || !strings.Contains(log, "__BITC_SETUP_FAILED__ n0nce "+name+" ") || !strings.Contains(log, msg) || strings.Contains(log, "__BITC_BEGIN__") {
		r.t.Errorf("exit %d, want 75 with a %q failure naming %q; log:\n%s", code, name, msg, log)
	}
	for _, p := range []string{filepath.Join(r.work, ".bitc-upload"), filepath.Join(r.work, "j1")} {
		if _, err := os.Lstat(p); err == nil {
			r.t.Errorf("%s is still there after the failed code step", p)
		}
	}
	if ents, _ := os.ReadDir(r.work); len(ents) != 0 {
		r.t.Errorf("the work dir is not empty after the failed code step: %v", ents)
	}
}

func TestSH37_ABundleThatFailsVerifyIsACodeFailure(t *testing.T) {
	r := newS2Rig(t)
	r.spec.Sha = strings.Repeat("a", 40)
	if err := os.WriteFile(r.bundlePath(), []byte("# v2 git bundle\nthis is not a bundle\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	r.codeFailed(r.run(), "code", "")
}

func TestSH37_ATipThatIsNotTheShaIsARefMismatch(t *testing.T) {
	r := newS2Rig(t)
	r.bundleOf(map[string]string{"a.txt": "a"}, nil)
	r.spec.Sha = strings.Repeat("0", 40)
	r.codeFailed(r.run(), "ref-mismatch", "want "+r.spec.Sha)
}

func TestSH37_TheGuardRefusesAnOversizeCheckoutAndAnInodeBomb(t *testing.T) {
	big := strings.Repeat("b", 3*4096+1) // 4 blocks
	files := map[string]string{"big.bin": big, "d/one": "1", "d/two": "2"}
	// entries: big.bin, d, d/one, d/two = 4 x 4 KiB + 4 + 1 + 1 blocks of 4 KiB = 6 x 4096 + 16 KiB... computed below
	const sum = 4*4096 + 4096 + 4096 + 4*4096 // big (4 blocks) + one + two (1 block each) + 4 entries x 4 KiB
	r := newS2Rig(t)
	r.bundleOf(files, nil)
	r.avail = 1<<30 + sum // the checkout needs sum and the guard wants it under avail - 1 GiB
	r.codeFailed(r.run(), "code", "checkout would exceed disk")

	r = newS2Rig(t)
	r.bundleOf(files, nil)
	r.avail, r.ffree = 1<<40, 100_000+4 // entries + 100000 must be under the free inodes
	r.codeFailed(r.run(), "code", "checkout would exceed inodes")

	// one byte of room each way and it goes through
	r = newS2Rig(t)
	r.bundleOf(files, nil)
	r.avail, r.ffree = 1<<30+sum+1, 100_000+4+1
	if code := r.run(); code != 0 {
		t.Errorf("with just enough room: exit %d; log:\n%s", code, r.out.String())
	}
}

func TestSH37_MalformedTreeEntriesAreRefused(t *testing.T) {
	// the explicit git fsck is what names them (git before 2.48 does not fsck a bundle's objects)
	for name, want := range map[string]string{".git": "hasDotgit", "..": "hasDotdot", ".GIT": "hasDotgit"} {
		t.Run(name, func(t *testing.T) {
			r := newS2Rig(t)
			r.bundleOfTree(name)
			r.codeFailed(r.run(), "code", want)
			if _, err := os.Lstat(filepath.Join(r.work, "..", "x")); err == nil {
				t.Errorf("a file was written outside the work tree")
			}
		})
	}
}

func TestSH37_TheBundleIsGoneBeforeTheFirstSetupStepRuns(t *testing.T) {
	r := newS2Rig(t)
	r.bundleOf(map[string]string{"hello.txt": "hello\n", "sub/keep": ""}, nil)
	up := filepath.Join(r.work, ".bitc-upload")
	r.spec.Setup = []agent.Step{{Name: "gone", RunAs: "job", Argv: []string{"sh", "-c", "test ! -e '" + up + "'"}}}
	r.spec.Argv = []string{"sh", "-c", "test ! -e '" + up + "' && cat hello.txt"}
	if code := r.run(); code != 0 {
		t.Fatalf("exit %d; log:\n%s", code, r.out.String())
	}
	if b, _ := os.ReadFile(filepath.Join(r.work, "j1", "hello.txt")); string(b) != "hello\n" {
		t.Errorf("work/j1/hello.txt = %q", b)
	}
	if _, err := os.Stat(up); err == nil {
		t.Errorf(".bitc-upload survived a successful run")
	}
	out, _ := os.ReadFile(filepath.Join(r.results, "j1", "out.log"))
	if string(out) != "hello\n" {
		t.Errorf("job output %q", out)
	}
}

func TestSH37_GitRunsAsTheJobUserThroughTheStepPathWithAClearedEnv(t *testing.T) {
	r := newS2Rig(t)
	r.bundleOf(map[string]string{"a": "a"}, nil)
	var envs [][]string
	r.sys.finish = func(c *exec.Cmd) {
		if filepath.Base(c.Path) == "git" || (len(c.Args) > 0 && c.Args[0] == "git") {
			envs = append(envs, append([]string{"ARGS " + strings.Join(c.Args, " ")}, c.Env...))
		}
	}
	if code := r.run(); code != 0 {
		t.Fatalf("exit %d; log:\n%s", code, r.out.String())
	}
	first := -1
	for i, c := range r.sys.calls {
		if strings.HasPrefix(c, "cmd git") {
			if first < 0 {
				first = i
			}
			if c != "cmd git root=false uid=4242 gid=4343" {
				t.Errorf("git ran as %q, want the job user (root=false uid=4242 gid=4343)", c)
			}
		}
	}
	if first < 0 {
		t.Fatalf("git never ran through System.Cmd: %v", r.sys.calls)
	}
	ensure, chown := -1, -1
	for i, c := range r.sys.calls {
		switch {
		case c == "ensure" && ensure < 0:
			ensure = i
		case strings.HasPrefix(c, "chown "+filepath.Join(r.work, "j1")+" ") && chown < 0:
			chown = i
		}
	}
	if ensure < 0 || chown < 0 || ensure > first || chown > first {
		t.Errorf("the job user and the chown of work/j1 must come before the first git (ensure %d, chown %d, git %d): %v", ensure, chown, first, r.sys.calls)
	}
	var verbs []string
	for _, e := range envs {
		args := e[0]
		fields := strings.Fields(args)
		for i, f := range fields {
			if f == "init" || f == "bundle" || f == "fetch" || f == "fsck" || f == "ls-tree" || f == "checkout" {
				verbs = append(verbs, f)
				// git before 2.48 has no --end-of-options for checkout --detach: its operand is the verified hex tip
				if f != "checkout" && !strings.Contains(args, " --end-of-options ") {
					t.Errorf("%s has no --end-of-options before its operands: %s", f, args)
				}
				_ = i
				break
			}
		}
		keys := map[string]string{}
		for _, kv := range e[1:] {
			k, v, _ := strings.Cut(kv, "=")
			keys[k] = v
		}
		for k, want := range map[string]string{"GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_GLOBAL": "/dev/null", "GIT_TERMINAL_PROMPT": "0", "PATH": "/usr/sbin:/usr/bin:/sbin:/bin"} {
			if keys[k] != want {
				t.Errorf("%s: %s = %q, want %q", args, k, keys[k], want)
			}
		}
		if h := keys["HOME"]; !strings.HasPrefix(h, r.work+string(filepath.Separator)) {
			t.Errorf("%s: HOME = %q, want a dir under the work root", args, h)
		}
		for k := range keys {
			switch k {
			case "PATH", "HOME", "GIT_CONFIG_NOSYSTEM", "GIT_CONFIG_GLOBAL", "GIT_TERMINAL_PROMPT":
			case "GIT_LFS_SKIP_SMUDGE":
				if !strings.Contains(args, " checkout ") {
					t.Errorf("%s: GIT_LFS_SKIP_SMUDGE outside the checkout", args)
				}
			default:
				t.Errorf("%s: unexpected env key %s (the env must be cleared)", args, k)
			}
		}
		if strings.Contains(args, " fetch ") {
			for _, want := range []string{"transfer.fsckObjects=true", "fetch.fsckObjects=true", "core.symlinks=true", "submodule.recurse=false", "core.hooksPath=/dev/null"} {
				if !strings.Contains(args, "-c "+want) {
					t.Errorf("fetch lacks -c %s: %s", want, args)
				}
			}
		}
		if strings.Contains(args, " checkout ") {
			for _, want := range []string{"filter.lfs.smudge=", "filter.lfs.process=", "filter.lfs.required=false"} {
				if !strings.Contains(args, "-c "+want+" ") {
					t.Errorf("checkout lacks -c %s: %s", want, args)
				}
			}
			if keys["GIT_LFS_SKIP_SMUDGE"] != "1" {
				t.Errorf("checkout lacks GIT_LFS_SKIP_SMUDGE=1: %v", e)
			}
		}
	}
	if strings.Join(verbs, " ") != "init bundle fetch fsck ls-tree checkout" {
		t.Errorf("git verbs ran in the order %v, want init bundle fetch fsck ls-tree checkout", verbs)
	}
}

func TestSH37_LFSFiltersAndSubmodulesStayInertUnderAHostileSystemConfig(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "filter-ran")
	cfgDir := t.TempDir()
	hostile := filepath.Join(cfgDir, "gitconfig")
	smudge := filepath.Join(cfgDir, "smudge.sh")
	if err := os.WriteFile(smudge, []byte("#!/bin/sh\necho ran >> '"+marker+"'\necho SMUDGED\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := fmt.Sprintf("[filter \"lfs\"]\n\tsmudge = %s\n\trequired = true\n[submodule]\n\trecurse = true\n[protocol \"file\"]\n\tallow = always\n", smudge)
	if err := os.WriteFile(hostile, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{".gitattributes": "*.dat filter=lfs\n", "x.dat": "pointer-ish\n", "plain": "p\n",
		".gitmodules": "[submodule \"evil\"]\n\tpath = evil\n\turl = file:///nonexistent/evil\n"}
	sub := func(repo string) {
		s2Git(t, repo, nil, "update-index", "--add", "--cacheinfo", "160000,"+strings.Repeat("1", 40)+",evil")
	}
	// the control: without GIT_CONFIG_NOSYSTEM and without the -c overrides the hostile config runs
	ctl := t.TempDir()
	s2Git(t, ctl, nil, "init", "-q")
	for p, c := range files {
		_ = os.WriteFile(filepath.Join(ctl, p), []byte(c), 0o600)
	}
	s2Git(t, ctl, nil, "add", "-A")
	s2Git(t, ctl, nil, "commit", "-q", "-m", "c")
	_ = os.Remove(filepath.Join(ctl, "x.dat"))
	cc := exec.Command("git", "checkout", "-q", "--", "x.dat")
	cc.Dir, cc.Env = ctl, []string{"PATH=" + os.Getenv("PATH"), "HOME=" + ctl, "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=" + hostile}
	if out, err := cc.CombinedOutput(); err != nil {
		t.Fatalf("control checkout: %v\n%s", err, out)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("the hostile system config does not take effect in the control, so this test would prove nothing: %v", err)
	}
	_ = os.Remove(marker)

	r := newS2Rig(t)
	r.bundleOf(files, sub)
	r.sys.finish = func(c *exec.Cmd) { c.Env = append(c.Env, "GIT_CONFIG_SYSTEM="+hostile) } // what a hostile /etc/gitconfig is
	if code := r.run(); code != 0 {
		t.Fatalf("exit %d; log:\n%s", code, r.out.String())
	}
	if _, err := os.Stat(marker); err == nil {
		t.Errorf("a filter process ran during the upload checkout")
	}
	if b, _ := os.ReadFile(filepath.Join(r.work, "j1", "x.dat")); string(b) != "pointer-ish\n" {
		t.Errorf("x.dat = %q, want the committed bytes (no smudge)", b)
	}
	for _, p := range []string{filepath.Join(r.work, "j1", "evil", ".git"), filepath.Join(r.work, "j1", ".git", "modules")} {
		if _, err := os.Lstat(p); err == nil {
			t.Errorf("%s exists: a submodule was fetched", p)
		}
	}
}

// bundleOfFanOut bundles a commit whose tree fans out: levels trees deep, each holding n entries
// that all point at the same subtree. It passes fsck --strict and is a few KiB, yet `ls-tree -r`
// of it lists n^levels entries.
func (r *s2Rig) bundleOfFanOut(levels, n int) {
	r.t.Helper()
	repo := r.t.TempDir()
	s2Git(r.t, repo, nil, "init", "-q")
	obj := s2Git(r.t, repo, []byte("x"), "hash-object", "-w", "--stdin")
	kind := "100644 blob"
	for l := 0; l < levels; l++ {
		var in strings.Builder
		for i := 0; i < n; i++ {
			fmt.Fprintf(&in, "%s %s\tf%04d\n", kind, obj, i)
		}
		obj = s2Git(r.t, repo, []byte(in.String()), "mktree")
		kind = "040000 tree"
	}
	r.spec.Sha = s2Git(r.t, repo, nil, "commit-tree", obj, "-m", "fan")
	s2Git(r.t, repo, nil, "update-ref", "refs/bitc/upload/j1", r.spec.Sha)
	s2Git(r.t, repo, nil, "bundle", "create", r.bundlePath(), "refs/bitc/upload/j1")
}

func TestSH37_AFanOutTreeIsRefusedWithoutTheRootAgentBufferingGitsOutput(t *testing.T) {
	r := newS2Rig(t)
	r.bundleOfFanOut(3, 60) // 216000 listed entries from a few KiB of objects
	r.ffree = 100_000 + 1000
	if code := r.run(); code == 0 {
		t.Fatalf("exit 0 for a fan-out tree; log:\n%s", r.out.String())
	}
	r.codeFailed(75, "code", "checkout would exceed inodes")
	var seen []string
	for _, c := range r.sys.cmds {
		verb := ""
		for _, a := range c.Args[1:] {
			switch a {
			case "init", "bundle", "fetch", "rev-parse", "fsck", "ls-tree", "checkout":
				verb = a
			}
			if verb != "" {
				break
			}
		}
		seen = append(seen, verb)
		if _, ok := c.Stdout.(*bytes.Buffer); ok {
			t.Errorf("git %s: the root agent buffers its whole stdout in a bytes.Buffer", verb)
		}
		if _, ok := c.Stderr.(*bytes.Buffer); ok {
			t.Errorf("git %s: the root agent buffers its whole stderr in a bytes.Buffer", verb)
		}
	}
	if strings.Join(seen, " ") != "init bundle fetch rev-parse fsck ls-tree" {
		t.Errorf("git verbs %v, want the run to stop at ls-tree", seen)
	}
}
