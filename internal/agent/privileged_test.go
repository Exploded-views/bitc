//go:build privileged

// The privileged agent suite (spec 11): RealSystem as root, in a throwaway container only.
package agent_test

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Exploded-views/bitc/internal/agent"
)

// fakeSSH notes that the deploy key file exists during the fetch, then runs the remote command
// (git-upload-pack '<path>') locally, so the real SSH code path runs without a network.
const fakeSSH = `#!/bin/sh
key=
while [ $# -gt 1 ]; do
  case $1 in -i) key=$2; shift 2 ;; -o|-p) shift 2 ;; *) shift ;; esac
done
[ -n "$key" ] && [ -s "$key" ] && : > /tmp/bitc-privileged-key-seen
exec sh -c "$1"
`

func inContainer(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 || os.Getenv("BITC_PRIVILEGED_TEST") != "1" {
		t.Skip("runs only as root in a throwaway container: sh scripts/privileged.sh")
	}
	_, e1 := os.Stat("/.dockerenv")
	_, e2 := os.Stat("/run/.containerenv")
	if e1 != nil && e2 != nil {
		t.Fatal("BITC_PRIVILEGED_TEST=1 outside a container: refusing to touch /usr/sbin, /etc/passwd and /run")
	}
}

// bareRepo makes a bare repository under /srv with one commit (run.sh, sub/keep) at the job ref.
func bareRepo(t *testing.T, script string) (cloneURL, sha string) {
	t.Helper()
	src := t.TempDir()
	_ = os.MkdirAll(filepath.Join(src, "sub"), 0o755)
	_ = os.WriteFile(filepath.Join(src, "run.sh"), []byte(script), 0o755)
	_ = os.WriteFile(filepath.Join(src, "sub", "keep"), nil, 0o644)
	git := func(dir string, a ...string) string {
		c := exec.Command("git", a...)
		c.Dir = dir
		out, err := c.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", a, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git(src, "init", "-q")
	git(src, "add", "-A")
	git(src, "-c", "user.name=t", "-c", "user.email=t@example.invalid", "commit", "-q", "-m", "i")
	sha = git(src, "rev-parse", "HEAD")
	_ = os.MkdirAll("/srv/bitc-rig", 0o755)
	bare, err := os.MkdirTemp("/srv/bitc-rig", "r-*.git")
	if err != nil {
		t.Fatal(err)
	}
	git("/", "init", "-q", "--bare", bare)
	git(src, "push", "-q", bare, "HEAD:refs/bitc/jobs/x")
	return "git@localhost:" + bare, sha
}

func TestPrivilegedAgent(t *testing.T) {
	inContainer(t)
	if err := os.WriteFile("/usr/sbin/ssh", []byte(fakeSSH), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove("/tmp/bitc-privileged-key-seen")
	cloneURL, sha := bareRepo(t, "#!/bin/sh\nid -u\ncat /proc/self/environ | tr '\\0' '\\n' | cut -d= -f1 | grep -v '^PWD$' | sort\n")
	root, err := os.MkdirTemp("", "bitc-rig-")
	if err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(root, 0o755) // the job user must reach its work dir
	results := filepath.Join(root, "run")
	keyDir := filepath.Join(results, ".ssh-j1") // Run's key dir for job j1
	spec := agent.Spec{Job: "j1", Pool: "heavy", Argv: []string{"sh", "../run.sh"}, Cwd: "sub", CloneURL: cloneURL,
		Ref: "refs/bitc/jobs/x", Sha: sha, Timeout: agent.Duration(time.Minute),
		Bearer: hex.EncodeToString(bytes.Repeat([]byte{4}, 32)), MacKey: hex.EncodeToString(bytes.Repeat([]byte{5}, 32)),
		Nonce: "n0nce", Env: []string{"A=1"}, RootSteps: true,
		Setup: []agent.Step{{Name: "key-gone", RunAs: "root", Argv: []string{"sh", "-c", `test ! -e "$0" && test ! -e /root/.ssh`, keyDir}}}}
	var log bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	code := agent.Run(ctx, agent.Options{Stdout: &log, ResultsRoot: results, ToolsRoot: filepath.Join(root, "tools"),
		WorkRoot: filepath.Join(root, "work"), System: agent.RealSystem{}, NoServe: true, Deadline: time.Now().Add(time.Hour)},
		spec, agent.Secrets{DeployKey: "test-deploy-key"})

	t.Run("deploy_key_gone_before_first_repo_step", func(t *testing.T) {
		if _, err := os.Stat("/tmp/bitc-privileged-key-seen"); err != nil {
			t.Fatal("the fetch never saw the key file")
		}
		if strings.Contains(log.String(), "__BITC_SETUP_FAILED__") {
			t.Fatalf("a root step saw the key or ~/.ssh:\n%s", log.String())
		}
	})
	t.Run("job_runs_as_uid_10001_with_only_remote_env", func(t *testing.T) {
		out, _ := os.ReadFile(filepath.Join(results, "j1", "out.log"))
		if code != 0 || string(out) != "10001\nA\nHOME\nPATH\n" {
			t.Fatalf("code %d, out %q\n%s", code, out, log.String())
		}
	})
	t.Run("results_dir_root_owned_0700", func(t *testing.T) {
		st, err := os.Stat(filepath.Join(results, "j1"))
		if err != nil {
			t.Fatal(err)
		}
		if uid := st.Sys().(*syscall.Stat_t).Uid; uid != 0 || st.Mode().Perm() != 0o700 {
			t.Fatalf("uid %d mode %v", uid, st.Mode().Perm())
		}
	})

	// Stage 1 and 2 through the real binary: a spec whose fetch fails at deploy-key (no key, no
	// network), so the agent stops after BOOT and stays alive serving results.
	bin := filepath.Join(t.TempDir(), "bitc")
	if out, err := exec.Command("go", "build", "-o", bin, "../../cmd/bitc").CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := probe.Addr().(*net.TCPAddr).Port
	probe.Close()
	s2 := spec
	s2.Job, s2.Port, s2.CloneURL = "j2", port, "git@localhost:/nonexistent.git"
	enc, err := s2.Encode()
	if err != nil {
		t.Fatal(err)
	}
	child := exec.Command(bin, "__agent")
	child.Env = []string{"BITC_SPEC=" + enc, "BITC_PROVIDER_TYPE=none", "PATH=/usr/bin:/bin",
		"BITC_DEADLINE=" + strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10)}
	stdout, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = child.Process.Kill(); _ = child.Wait() }()
	var boots int
	var dialed bool
	var environ, cmdline []byte
	sc := bufio.NewScanner(stdout)
	for sc.Scan() {
		l := sc.Text()
		if strings.HasPrefix(l, "__BITC_BOOT__ n0nce") {
			boots++
			if boots == 1 {
				c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), time.Second)
				dialed = err == nil
				if c != nil {
					c.Close()
				}
				environ, _ = os.ReadFile(fmt.Sprintf("/proc/%d/environ", child.Process.Pid))
				cmdline, _ = os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", child.Process.Pid))
			}
		}
		if strings.HasPrefix(l, "__BITC_SETUP_FAILED__ n0nce") {
			break
		}
	}
	t.Run("stage1_listener_bound_before_setup", func(t *testing.T) {
		if !dialed {
			t.Fatal("the results port did not accept a connection at BOOT")
		}
	})
	t.Run("stage2_env_empty_same_pid", func(t *testing.T) {
		if len(environ) != 0 || !bytes.Contains(cmdline, []byte("--stage2")) {
			t.Fatalf("environ %q cmdline %q", environ, cmdline)
		}
	})
	t.Run("boot_marker_once_per_boot", func(t *testing.T) {
		if boots != 1 {
			t.Fatalf("%d BOOT markers", boots)
		}
	})
}

// TestPrivilegedAgentUmask077: the build stub once left the agent under umask 077, so /work was
// created 0700 and the job user could not chdir into its tree (the job ended 127, no output).
func TestPrivilegedAgentUmask077(t *testing.T) {
	inContainer(t)
	if err := os.WriteFile("/usr/sbin/ssh", []byte(fakeSSH), 0o755); err != nil {
		t.Fatal(err)
	}
	cloneURL, sha := bareRepo(t, "#!/bin/sh\nid -u\necho hello\nexit 3\n")
	root, err := os.MkdirTemp("", "bitc-rig-umask-")
	if err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(root, 0o755) // only the roots under test below are the agent's to create
	old := syscall.Umask(0o077)
	defer syscall.Umask(old)
	results := filepath.Join(root, "run")
	spec := agent.Spec{Job: "j1", Pool: "heavy", Argv: []string{"sh", "../run.sh"}, Cwd: "sub", CloneURL: cloneURL,
		Ref: "refs/bitc/jobs/x", Sha: sha, Timeout: agent.Duration(time.Minute),
		Bearer: hex.EncodeToString(bytes.Repeat([]byte{4}, 32)), MacKey: hex.EncodeToString(bytes.Repeat([]byte{5}, 32)),
		Nonce: "n0nce"}
	var log bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	code := agent.Run(ctx, agent.Options{Stdout: &log, ResultsRoot: results, ToolsRoot: filepath.Join(root, "tools"),
		WorkRoot: filepath.Join(root, "work"), System: agent.RealSystem{}, NoServe: true, Deadline: time.Now().Add(time.Hour)},
		spec, agent.Secrets{DeployKey: "test-deploy-key"})
	out, _ := os.ReadFile(filepath.Join(results, "j1", "out.log"))
	errB, _ := os.ReadFile(filepath.Join(results, "j1", "err.log"))
	if code != 3 || string(out) != "10001\nhello\n" {
		t.Fatalf("code %d, out %q, err %q\n%s", code, out, errB, log.String())
	}
}

// TestPrivilegedAgentUmask077Tools: under an inherited umask 077 (Main's Umask(022) not in effect) a
// tool installed below a not-yet-existing parent, from an archive with a nested file and no dir
// entries, must still be traversable and executable by the job user.
func TestPrivilegedAgentUmask077Tools(t *testing.T) {
	inContainer(t)
	if err := os.WriteFile("/usr/sbin/ssh", []byte(fakeSSH), 0o755); err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp("", "bitc-rig-umask-tools-")
	if err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(root, 0o755)
	toolsRoot := filepath.Join(root, "opt", "bitc", "tools")
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	tw := tar.NewWriter(zw)
	body := "#!/bin/sh\necho toolout\n"
	_ = tw.WriteHeader(&tar.Header{Name: "t/lib/x/y/hello.sh", Typeflag: tar.TypeReg, Mode: 0o755, Size: int64(len(body))})
	_, _ = tw.Write([]byte(body))
	_ = tw.Close()
	_ = zw.Close()
	sum := sha256.Sum256(gz.Bytes())
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(gz.Bytes()) }))
	defer srv.Close()
	cloneURL, sha := bareRepo(t, "#!/bin/sh\n"+toolsRoot+"/t/lib/x/y/hello.sh\nid -u\nexit 3\n")
	old := syscall.Umask(0o077)
	defer syscall.Umask(old)
	results := filepath.Join(root, "run")
	spec := agent.Spec{Job: "j1", Pool: "heavy", Argv: []string{"sh", "../run.sh"}, Cwd: "sub", CloneURL: cloneURL,
		Ref: "refs/bitc/jobs/x", Sha: sha, Timeout: agent.Duration(time.Minute),
		Tools:  []agent.Tool{{Name: "t", URL: srv.URL + "/t.tgz", SHA256: hex.EncodeToString(sum[:])}},
		Bearer: hex.EncodeToString(bytes.Repeat([]byte{4}, 32)), MacKey: hex.EncodeToString(bytes.Repeat([]byte{5}, 32)),
		Nonce: "n0nce"}
	var log bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	code := agent.Run(ctx, agent.Options{Stdout: &log, ResultsRoot: results, ToolsRoot: toolsRoot, HTTP: srv.Client(),
		WorkRoot: filepath.Join(root, "work"), System: agent.RealSystem{}, NoServe: true, Deadline: time.Now().Add(time.Hour)},
		spec, agent.Secrets{DeployKey: "test-deploy-key"})
	out, _ := os.ReadFile(filepath.Join(results, "j1", "out.log"))
	errB, _ := os.ReadFile(filepath.Join(results, "j1", "err.log"))
	if code != 3 || string(out) != "toolout\n10001\n" {
		t.Fatalf("code %d, out %q, err %q\n%s", code, out, errB, log.String())
	}
}

// TestPrivilegedAgentBootStdin: --boot-stdin through the real binary as root: stage 1 reads the
// length-prefixed payload, moves the deploy key into its own memfd, re-execs with an empty
// environment and no listener; the key reaches the fetch (the fake ssh sees the key file), no
// memfd of the running agent holds it, the job runs as uid 10001, results land root-owned 0700
// with agent.log and agent.pid, and the agent exits by itself after result.json.
func TestPrivilegedAgentBootStdin(t *testing.T) {
	inContainer(t)
	if err := os.WriteFile("/usr/sbin/ssh", []byte(fakeSSH), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove("/tmp/bitc-privileged-key-seen")
	// The fetch is held so the agent can be inspected at a point that does not race: no child has
	// changed its uid yet (Go forks with CLONE_VM, and a setuid in the child marks the agent's
	// shared mm non-dumpable, after which /proc/<agent>/environ and fd are closed even to root
	// without CAP_SYS_PTRACE). The fake ssh runs as part of the fetch, as root.
	_ = os.Remove("/tmp/bitc-hold-at")
	_ = os.Remove("/tmp/bitc-hold-go")
	if err := os.WriteFile("/usr/sbin/ssh", []byte(strings.Replace(fakeSSH, "exec sh", ": > /tmp/bitc-hold-at\ni=0\nwhile [ ! -e /tmp/bitc-hold-go ] && [ $i -lt 300 ]; do sleep 0.1; i=$((i+1)); done\nexec sh", 1)), 0o755); err != nil {
		t.Fatal(err)
	}
	cloneURL, sha := bareRepo(t, "#!/bin/sh\nid -u\nsleep 4\n")
	bin := filepath.Join(t.TempDir(), "bitc")
	if out, err := exec.Command("go", "build", "-o", bin, "../../cmd/bitc").CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	const key = "test-deploy-key-boot-stdin"
	spec := agent.Spec{Job: "jstdin", Pool: "heavy", Argv: []string{"sh", "../run.sh"}, Cwd: "sub", CloneURL: cloneURL,
		Ref: "refs/bitc/jobs/x", Sha: sha, Timeout: agent.Duration(time.Minute), Port: -1,
		Bearer: hex.EncodeToString(bytes.Repeat([]byte{4}, 32)), MacKey: hex.EncodeToString(bytes.Repeat([]byte{5}, 32)), Nonce: "n0nce"}
	enc, err := spec.Encode()
	if err != nil {
		t.Fatal(err)
	}
	frame, err := agent.BootPayload{Spec: enc, DeployKey: key, Deadline: time.Now().Add(time.Hour).Unix()}.Frame()
	if err != nil {
		t.Fatal(err)
	}
	child := exec.Command(bin, "__agent", "--boot-stdin")
	child.Env = []string{"PATH=/usr/bin:/bin"}
	child.Stdin = bytes.NewReader(frame)
	stdout, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = child.Process.Kill(); _ = child.Wait() }()
	var log strings.Builder
	var environ, cmdline []byte
	var memfdWithKey, openFds []string
	sc := bufio.NewScanner(stdout)
	for sc.Scan() {
		l := sc.Text()
		log.WriteString(l + "\n")
		if strings.HasPrefix(l, "__BITC_BOOT__ n0nce") { // stage 2 is up and the fetch is about to be held
			for i := 0; i < 300; i++ {
				if _, err := os.Stat("/tmp/bitc-hold-at"); err == nil {
					break
				}
				time.Sleep(100 * time.Millisecond)
			}
			proc := fmt.Sprintf("/proc/%d", child.Process.Pid)
			var errEnv, errFd error
			environ, errEnv = os.ReadFile(proc + "/environ")
			cmdline, _ = os.ReadFile(proc + "/cmdline")
			ents, errFd := os.ReadDir(proc + "/fd")
			if errEnv != nil || errFd != nil {
				t.Fatalf("cannot inspect the agent: %v %v", errEnv, errFd)
			}
			for _, e := range ents {
				target, err := os.Readlink(proc + "/fd/" + e.Name())
				if err != nil {
					continue // a runtime-internal fd that closed meanwhile
				}
				openFds = append(openFds, e.Name()+" -> "+target)
				if !strings.HasPrefix(target, "/memfd:") {
					continue // never open anything else: reading a pipe's fd would block
				}
				if b, _ := os.ReadFile(proc + "/fd/" + e.Name()); bytes.Contains(b, []byte(key)) {
					memfdWithKey = append(memfdWithKey, e.Name())
				}
			}
			_ = os.WriteFile("/tmp/bitc-hold-go", nil, 0o600)
		}
	}
	waitErr := child.Wait()
	t.Run("boot_stdin_agent_env_empty_and_no_listener", func(t *testing.T) {
		if len(environ) != 0 || !bytes.Contains(cmdline, []byte("--stage2-stdin")) {
			t.Fatalf("environ %q cmdline %q\n%s", environ, cmdline, log.String())
		}
		for _, fd := range openFds {
			if strings.Contains(fd, "socket:") || strings.Contains(fd, "/memfd:") {
				t.Errorf("unexpected open fd %s (a listener or a boot memfd is still open)", fd)
			}
		}
	})
	t.Run("boot_stdin_key_in_no_memfd_and_reached_the_fetch", func(t *testing.T) {
		if len(memfdWithKey) != 0 {
			t.Fatalf("memfds holding the deploy key: %v", memfdWithKey)
		}
		if _, err := os.Stat("/tmp/bitc-privileged-key-seen"); err != nil {
			t.Fatalf("the fetch never saw the key file\n%s", log.String())
		}
	})
	t.Run("boot_stdin_job_uid_results_and_self_exit", func(t *testing.T) {
		dir := "/run/bitc/jstdin"
		out, _ := os.ReadFile(filepath.Join(dir, "out.log"))
		st, err := os.Stat(dir)
		if err != nil || waitErr != nil || string(out) != "10001\n" {
			t.Fatalf("exit %v, stat %v, out %q\n%s", waitErr, err, out, log.String())
		}
		if uid := st.Sys().(*syscall.Stat_t).Uid; uid != 0 || st.Mode().Perm() != 0o700 {
			t.Fatalf("uid %d mode %v", uid, st.Mode().Perm())
		}
		for _, f := range []string{"agent.log", "agent.pid", "result.json"} {
			if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
				t.Errorf("no %s: %v", f, err)
			}
		}
	})
}

// TestPrivilegedAgentKeyOnATmpfs: in no-listener mode (exec delivery) the deploy key goes on a
// tmpfs the agent mounts on KeyRoot for the fetch (the fake ssh sees the key file on a tmpfs),
// and the mount and the directory are gone before any repo step runs.
func TestPrivilegedAgentKeyOnATmpfs(t *testing.T) {
	inContainer(t)
	_ = os.Remove("/tmp/bitc-key-on-tmpfs")
	script := strings.Replace(fakeSSH, "exec sh", `d=$(dirname "$key"); [ "$(stat -f -c %T "$d")" = tmpfs ] && : > /tmp/bitc-key-on-tmpfs
exec sh`, 1)
	if err := os.WriteFile("/usr/sbin/ssh", []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	cloneURL, sha := bareRepo(t, "#!/bin/sh\nid -u\n")
	root, err := os.MkdirTemp("", "bitc-rig-key-")
	if err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(root, 0o755)
	results, keyRoot := filepath.Join(root, "run"), filepath.Join(root, "bitc-key")
	spec := agent.Spec{Job: "j1", Pool: "heavy", Argv: []string{"sh", "../run.sh"}, Cwd: "sub", CloneURL: cloneURL,
		Ref: "refs/bitc/jobs/x", Sha: sha, Timeout: agent.Duration(time.Minute), Port: -1, RootSteps: true,
		Bearer: hex.EncodeToString(bytes.Repeat([]byte{4}, 32)), MacKey: hex.EncodeToString(bytes.Repeat([]byte{5}, 32)), Nonce: "n0nce",
		Setup: []agent.Step{{Name: "key-gone", RunAs: "root", Argv: []string{"sh", "-c", `test ! -e "$0" && ! grep -q " $0 " /proc/self/mountinfo`, keyRoot}}}}
	var log bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	code := agent.Run(ctx, agent.Options{Stdout: &log, ResultsRoot: results, ToolsRoot: filepath.Join(root, "tools"), KeyRoot: keyRoot,
		WorkRoot: filepath.Join(root, "work"), System: agent.RealSystem{}, Deadline: time.Now().Add(time.Hour)},
		spec, agent.Secrets{DeployKey: "test-deploy-key"})
	if _, err := os.Stat("/tmp/bitc-key-on-tmpfs"); err != nil {
		t.Fatalf("the fetch did not see the key file on a tmpfs\n%s", log.String())
	}
	if code != 0 || strings.Contains(log.String(), "__BITC_SETUP_FAILED__") {
		t.Fatalf("code %d: the key dir or its mount outlived the fetch\n%s", code, log.String())
	}
}
