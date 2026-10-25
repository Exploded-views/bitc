// Package harness runs the built bitc binary in throwaway homes (spec 11).
package harness

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// Bin is the bitc binary under test, set by the conformance TestMain.
var Bin string

// StubPlugin is the stub provider plugin (internal/provider/plugin/stubplugin) the conformance
// TestMain built; Plugin installs it for a test.
var StubPlugin string

// Plugin copies the stub plugin to Root/plugins/bitc-provider-<typ> (a private directory, 0700,
// so the pin checks of providers delta A.2 pass) and returns its path and sha256.
func (h *H) Plugin(typ string) (path, sum string) {
	h.T.Helper()
	return h.PluginFrom(typ, StubPlugin)
}

// PluginFrom installs the plugin binary src as Root/plugins/bitc-provider-<typ> (0755, in a 0700
// directory) and returns its path and sha256: the path + sha256 pin a real host writes.
func (h *H) PluginFrom(typ, src string) (path, sum string) {
	h.T.Helper()
	b, err := os.ReadFile(src)
	if err != nil {
		h.T.Fatal(err)
	}
	dir := filepath.Join(h.Root, "plugins")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		h.T.Fatal(err)
	}
	path = filepath.Join(dir, "bitc-provider-"+typ)
	if err := os.WriteFile(path, b, 0o755); err != nil {
		h.T.Fatal(err)
	}
	h2 := sha256.Sum256(b)
	return path, hex.EncodeToString(h2[:])
}

// Result is one finished bitc process.
type Result struct {
	Code   int
	Stdout string
	Stderr string
}

// H is one test's sandbox: HOME, state dir and host config under a temp dir.
type H struct {
	T      *testing.T
	Root   string
	State  string
	Config string
	Cwd    string
	Env    []string

	body  string
	seams []string
	seq   int
	mu    sync.Mutex
	procs []*Proc
	pids  []int
}

// New makes a sandbox with a host file holding only "version = 1" and the [test] table.
func New(t *testing.T) *H {
	t.Helper()
	root := t.TempDir()
	h := &H{T: t, Root: root, State: filepath.Join(root, "state"),
		Config: filepath.Join(root, "etc", "bitc.toml"), Cwd: root}
	if err := os.MkdirAll(filepath.Dir(h.Config), 0o700); err != nil {
		t.Fatal(err)
	}
	h.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + root,
		"XDG_CONFIG_HOME=" + filepath.Join(root, "xdg-config"),
		"XDG_STATE_HOME=" + filepath.Join(root, "xdg-state"),
		"BITC_DIR=" + h.State,
		"BITC_CONFIG=" + h.Config,
		"BITC_POLL=100ms",
		"LC_ALL=C",
	}
	h.HostConfig("")
	t.Cleanup(h.cleanup)
	return h
}

// Q quotes s as a TOML basic string.
func Q(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\t", `\t`)
	return `"` + r.Replace(s) + `"`
}

// HostConfig writes the host file: "version = 1", body, then [test] with accounting_dir under
// Root and every seam added with Seam.
func (h *H) HostConfig(body string) {
	h.body = body
	var b strings.Builder
	b.WriteString("version = 1\n")
	b.WriteString(body)
	b.WriteString("\n\n[test]\naccounting_dir = " + Q(filepath.Join(h.Root, "account")) + "\n")
	for _, s := range h.seams {
		b.WriteString(s + "\n")
	}
	h.Write(h.Config, b.String(), 0o600)
}

// Seam adds one `key = value` line to the [test] table and rewrites the host file.
func (h *H) Seam(line string) {
	h.seams = append(h.seams, line)
	h.HostConfig(h.body)
}

// SetSeam sets one [test] key, replacing an earlier line for it, and rewrites the host file.
func (h *H) SetSeam(key, value string) {
	kept := h.seams[:0:0]
	for _, s := range h.seams {
		if !strings.HasPrefix(s, key+" ") {
			kept = append(kept, s)
		}
	}
	h.seams = append(kept, key+" = "+value)
	h.HostConfig(h.body)
}

// Write writes a file, creating parent dirs (0700).
func (h *H) Write(path, body string, mode os.FileMode) {
	h.T.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		h.T.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		h.T.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		h.T.Fatal(err)
	}
}

// Proc is a running bitc.
type Proc struct {
	Cmd  *exec.Cmd
	done chan struct{}
	res  Result
	out  *lockedBuf
}

// Stdout is what the process has printed so far.
func (p *Proc) Stdout() string { return p.out.String() }

// Exited reports whether the process has ended.
func (p *Proc) Exited() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

// Wait waits up to 60 s, then kills bitc's group and waits at most 10 s more; it never hangs.
func (p *Proc) Wait() Result {
	select {
	case <-p.done:
		return p.res
	case <-time.After(60 * time.Second):
	}
	_ = syscall.Kill(-p.Cmd.Process.Pid, syscall.SIGKILL)
	select {
	case <-p.done:
		p.res.Stderr += "\n[harness: killed after 60s]"
		return p.res
	case <-time.After(10 * time.Second):
		return Result{Code: -1, Stderr: "[harness: killed after 60s; bitc did not finish]"}
	}
}

// Signal sends sig to the process (not its group).
func (p *Proc) Signal(sig syscall.Signal) { _ = p.Cmd.Process.Signal(sig) }

// Run runs bitc with args and waits.
func (h *H) Run(args ...string) Result { return h.RunWith(nil, args...) }

// RunWith runs bitc with extra env entries ("K=V" sets, "!K" removes) and waits.
func (h *H) RunWith(env []string, args ...string) Result { return h.StartWith(env, args...).Wait() }

// Start starts bitc in its own process group.
func (h *H) Start(args ...string) *Proc { return h.StartWith(nil, args...) }

type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}
func (l *lockedBuf) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

// StartWith starts bitc with extra env entries in its own process group.
func (h *H) StartWith(env []string, args ...string) *Proc {
	h.T.Helper()
	cmd := exec.Command(Bin, args...)
	cmd.Dir = h.Cwd
	cmd.Env = MergeEnv(h.Env, env)
	var out, errb lockedBuf
	cmd.Stdout, cmd.Stderr = &out, &errb
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// The command bitc runs has its own process group; if it (or a straggler) still holds the
	// pipes after bitc exits, Wait returns 5 s later instead of never.
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Start(); err != nil {
		h.T.Fatal(err)
	}
	p := &Proc{Cmd: cmd, done: make(chan struct{}), out: &out}
	go func() {
		err := cmd.Wait()
		p.res = Result{Code: ExitCode(err, cmd.ProcessState), Stdout: out.String(), Stderr: errb.String()}
		close(p.done)
	}()
	h.mu.Lock()
	h.procs = append(h.procs, p)
	h.mu.Unlock()
	return p
}

// ExitCode is the shell-style code of a finished process (128+N for signal N).
func ExitCode(err error, ps *os.ProcessState) int {
	if ps == nil {
		return -1
	}
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	var ee *exec.ExitError
	if err != nil && !errors.As(err, &ee) && !errors.Is(err, exec.ErrWaitDelay) {
		return -1
	}
	return ps.ExitCode()
}

// MergeEnv overlays extra on base: "K=V" replaces K, "!K" removes it.
func MergeEnv(base, extra []string) []string {
	out := append([]string(nil), base...)
	for _, e := range extra {
		key := strings.TrimPrefix(e, "!")
		if k, _, ok := strings.Cut(key, "="); ok {
			key = k
		}
		kept := out[:0]
		for _, kv := range out {
			if !strings.HasPrefix(kv, key+"=") {
				kept = append(kept, kv)
			}
		}
		out = kept
		if !strings.HasPrefix(e, "!") {
			out = append(out, e)
		}
	}
	return out
}

// Holder is a bitc run holding a slot until Release.
type Holder struct {
	Env  map[string]string
	Proc *Proc
	gate string
}

// HoldScript is the held command: dump env to $1.env, touch $1, wait for $2.
const HoldScript = `env > "$1.env"; : > "$1"; while [ ! -e "$2" ]; do sleep 0.05; done`

// Hold runs `bitc run <runArgs...> -- sh -c HoldScript` and waits until the command runs.
// runArgs are run flags followed by the pool.
func (h *H) Hold(env []string, runArgs ...string) *Holder {
	h.T.Helper()
	h.seq++
	ready := filepath.Join(h.Root, fmt.Sprintf("ready-%d", h.seq))
	gate := filepath.Join(h.Root, fmt.Sprintf("release-%d", h.seq))
	args := append([]string{"run"}, runArgs...)
	args = append(args, "--", "sh", "-c", HoldScript, "_", ready, gate)
	p := h.StartWith(env, args...)
	h.Until(func() bool { _, err := os.Stat(ready); return err == nil || p.Exited() }, 20*time.Second)
	if p.Exited() {
		r := p.Wait()
		h.T.Fatalf("holder ended before it ran: code %d stderr %q", r.Code, r.Stderr)
	}
	return &Holder{Env: ReadEnvFile(h.T, ready+".env"), Proc: p, gate: gate}
}

// Release lets the held command finish and returns bitc's result.
func (hd *Holder) Release() Result {
	_ = os.WriteFile(hd.gate, nil, 0o600)
	return hd.Proc.Wait()
}

// ReadEnvFile parses `env` output.
func ReadEnvFile(t *testing.T, path string) map[string]string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	m := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if k, v, ok := strings.Cut(sc.Text(), "="); ok {
			m[k] = v
		}
	}
	return m
}

// Until polls cond every 20 ms until it holds or d passes.
func (h *H) Until(cond func() bool, d time.Duration) {
	h.T.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			h.T.Fatalf("condition not met within %s", d)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Track registers a pid whose process group the cleanup kills (detached jobs).
func (h *H) Track(pid int) { h.mu.Lock(); h.pids = append(h.pids, pid); h.mu.Unlock() }

// Repo makes a git work tree Root/<name> with files committed, and returns its path.
func (h *H) Repo(name string, files map[string]string) string {
	h.T.Helper()
	dir := filepath.Join(h.Root, name)
	for p, body := range files {
		h.Write(filepath.Join(dir, p), body, 0o644)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		h.T.Fatal(err)
	}
	git := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-c", "init.defaultBranch=main", "-c", "user.name=t",
			"-c", "user.email=t@example.invalid", "-c", "commit.gpgsign=false"}, args...)...)
		cmd.Dir = dir
		cmd.Env = append(MergeEnv(h.Env, nil), "GIT_CONFIG_NOSYSTEM=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			h.T.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q")
	git("add", "-A")
	git("commit", "-q", "--allow-empty", "-m", "init")
	// Every test repo has a local bare origin, so remote tests can push without a network and
	// a later transport task never meets a repo without one.
	origin := h.Origin(name)
	git("init", "-q", "--bare", origin)
	git("remote", "add", "origin", origin)
	git("push", "-q", "origin", "HEAD:refs/heads/main")
	return dir
}

// Origin is the path of the bare origin that Repo(name) made.
func (h *H) Origin(name string) string {
	return filepath.Join(h.Root, strings.ReplaceAll(name, "/", "_")+".origin.git")
}

func (h *H) cleanup() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, pid := range h.pids {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
	}
	for _, p := range h.procs {
		if !p.Exited() {
			_ = syscall.Kill(-p.Cmd.Process.Pid, syscall.SIGKILL)
			select {
			case <-p.done:
			case <-time.After(10 * time.Second):
				h.T.Errorf("harness: bitc %d did not finish 10s after SIGKILL", p.Cmd.Process.Pid)
			}
		}
	}
	h.waitDetachedPushes()
}

// waitDetachedPushes waits for the detached `git push` that deletes a remote job's ref (its own
// session, so no pid is tracked) to finish before the temp dir holding the origin is removed.
func (h *H) waitDetachedPushes() {
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		found := false
		ents, _ := filepath.Glob("/proc/[0-9]*/cmdline")
		for _, f := range ents {
			b, err := os.ReadFile(f)
			if err != nil {
				continue
			}
			args := strings.Split(string(b), "\x00")
			if len(args) > 3 && filepath.Base(args[0]) == "git" && args[1] == "-C" &&
				strings.HasPrefix(args[2], h.Root+"/") && args[3] == "push" {
				found = true
				break
			}
		}
		if !found {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Atoi is strconv.Atoi that fails the test.
func (h *H) Atoi(s string) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		h.T.Fatal(err)
	}
	return n
}
