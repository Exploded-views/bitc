package conformance

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Exploded-views/bitc/conformance/harness"
	"github.com/Exploded-views/bitc/internal/proctest"
)

// The container driver (docs/containers.md): a local pool with pools.<p>.container runs the
// admitted command in `docker run`. Every test but DK10 runs against a fake docker CLI on PATH
// (internal/container/testdata/fakedocker): it records argv, starts the command on the host in a
// process group of its own, and kills that group on `rm -f`. Like the real CLI it does not forward
// signals, so only bitc's own `docker rm -f` removes a container whose CLI was signalled or killed.

const dkImage = "bitc-test/img:1"

// dkPool is a host file with one container pool.
const dkPool = "[pools.box]\ncontainer = { image = \"" + dkImage + "\" }\n"

var dkTicketRE = regexp.MustCompile(`^[01]-[0-9]{19}-[0-9a-f]{8}$`)

// dkHash is the bitc.state label value: the first 12 hex digits of the state dir path's sha256.
func dkHash(h *harness.H) string {
	sum := sha256.Sum256([]byte(h.State))
	return hex.EncodeToString(sum[:])[:12]
}

// dkWorktree is the work tree bitc mounts: the canonical form of the sandbox's directory.
func dkWorktree(t *testing.T, h *harness.H) string {
	t.Helper()
	wt, err := filepath.EvalSymlinks(h.Cwd)
	if err != nil {
		t.Fatal(err)
	}
	return wt
}

// dkLabel is the value of the --label key=... in a docker run argv.
func dkLabel(argv []string, key string) string {
	for i, a := range argv {
		if a == "--label" && i+1 < len(argv) && strings.HasPrefix(argv[i+1], key+"=") {
			return strings.TrimPrefix(argv[i+1], key+"=")
		}
	}
	return ""
}

// dkPidFile is a command that records its pid in file and then sleeps; it is the container's
// process, so the pid says whether `rm -f` killed it.
func dkPidFile(file string) []string {
	return []string{"sh", "-c", `echo $$ > "$1.tmp"; mv "$1.tmp" "$1"; exec sleep 60`, "_", file}
}

// dkWaitPid waits for the command to have written its pid.
func dkWaitPid(h *harness.H, file string) int {
	h.T.Helper()
	h.Until(func() bool { b, err := os.ReadFile(file); return err == nil && len(b) > 0 }, 20*time.Second)
	return h.Atoi(string(must(os.ReadFile(file))))
}

func TestDK1_RunArgvShape(t *testing.T) {
	h := harness.New(t)
	fd := h.FakeDocker()
	h.HostConfig(dkPool + "[pools.big]\ncontainer = { image = \"big:2\", cpus = 1.5, memory = \"512MiB\", mounts = [\"/data:/data:ro\", \"/cache:/cache\"] }\n")
	wt := dkWorktree(t, h)
	r := h.Run("run", "--name", "j1", "box", "--", "echo", "hi", "--flag")
	if r.Code != 0 || r.Stdout != "hi --flag\n" {
		t.Fatalf("%+v", r)
	}
	runs := fd.CallsOf("run")
	if len(runs) != 1 {
		t.Fatalf("docker run calls: %v", fd.Calls())
	}
	ticket := dkLabel(runs[0], "bitc.ticket")
	if !dkTicketRE.MatchString(ticket) {
		t.Fatalf("bitc.ticket label %q", ticket)
	}
	want := []string{"run", "--rm", "--init", "-i", "--name", "bitc-j1",
		"--label", "bitc.job=j1", "--label", "bitc.pool=box", "--label", "bitc.state=" + dkHash(h), "--label", "bitc.ticket=" + ticket,
		"--cidfile", filepath.Join(h.State, "containers", ticket+".cid"),
		"--mount", "type=bind,source=" + wt + ",target=" + wt, "--workdir", wt,
		"-e", "BITC_JOB=j1", "-e", "BITC_POOL=box", "-e", "BITC_SLOT=box.1", "-e", "LC_ALL=C",
		dkImage, "echo", "hi", "--flag"}
	if !reflect.DeepEqual(runs[0], want) {
		t.Fatalf("docker run argv:\n got %q\nwant %q", runs[0], want)
	}
	// cpus, memory and extra mounts, each only when set; the work tree mount comes first
	if r := h.Run("run", "--name", "j2", "big", "--", "true"); r.Code != 0 {
		t.Fatalf("%+v", r)
	}
	runs = fd.CallsOf("run")
	if len(runs) != 2 {
		t.Fatalf("docker run calls: %v", fd.Calls())
	}
	ticket = dkLabel(runs[1], "bitc.ticket")
	want = []string{"run", "--rm", "--init", "-i", "--name", "bitc-j2",
		"--label", "bitc.job=j2", "--label", "bitc.pool=big", "--label", "bitc.state=" + dkHash(h), "--label", "bitc.ticket=" + ticket,
		"--cidfile", filepath.Join(h.State, "containers", ticket+".cid"),
		"--mount", "type=bind,source=" + wt + ",target=" + wt,
		"--mount", "type=bind,source=/data,target=/data,readonly",
		"--mount", "type=bind,source=/cache,target=/cache",
		"--workdir", wt, "--cpus", "1.5", "--memory", "536870912",
		"-e", "BITC_JOB=j2", "-e", "BITC_POOL=big", "-e", "BITC_SLOT=big.1", "-e", "LC_ALL=C",
		"big:2", "true"}
	if !reflect.DeepEqual(runs[1], want) {
		t.Fatalf("docker run argv with limits:\n got %q\nwant %q", runs[1], want)
	}
	if fd.Exists("bitc-j1") || fd.Exists("bitc-j2") { // --rm: nothing is left behind
		t.Fatal("a finished run left its container")
	}
}

func TestDK2_ExitStatusStreamsAndDirectoryAreTheCommands(t *testing.T) {
	h := harness.New(t)
	h.FakeDocker()
	h.HostConfig(dkPool)
	wt := dkWorktree(t, h)
	res := filepath.Join(h.Root, "r.json")
	r := h.Run("run", "--result", res, "box", "--", "sh", "-c", "pwd; echo err >&2; exit 3")
	if r.Code != 3 || r.Stdout != wt+"\n" || !strings.Contains(r.Stderr, "err\n") {
		t.Fatalf("the command's status, streams and directory did not come through: %+v (want cwd %s)", r, wt)
	}
	var got struct{ Origin, Outcome, Provider string }
	if err := json.Unmarshal(must(os.ReadFile(res)), &got); err != nil || got.Origin != "command" || got.Outcome != "command-failed" || got.Provider != "local" {
		t.Fatalf("result.json: %s %v", must(os.ReadFile(res)), err)
	}
	// 125, 126 and 127 from the command are the command's once the container exists
	for _, code := range []string{"125", "126", "127"} {
		if r := h.Run("run", "box", "--", "sh", "-c", "exit "+code); strconv.Itoa(r.Code) != code {
			t.Fatalf("exit %s: %+v", code, r)
		}
	}
}

func TestDK3_TimeLimitRemovesTheContainer(t *testing.T) {
	h := harness.New(t)
	fd := h.FakeDocker()
	scaledHost(h, "[host]\ngrace = \"20s\"\n[pools.box]\ntimeout = \"10s\"\ncontainer = { image = \""+dkImage+"\" }\n")
	pidFile := filepath.Join(h.Root, "pid")
	r := h.Run(append([]string{"run", "--name", "tl", "box", "--"}, dkPidFile(pidFile)...)...)
	if r.Code != 124 || !strings.Contains(r.Stderr, "bitc: box: time limit 10s reached; terminated\n") {
		t.Fatalf("%+v", r)
	}
	if !fd.Removed("bitc-tl") || fd.Exists("bitc-tl") {
		t.Fatalf("the time limit left the container: calls %v", fd.Calls())
	}
	if b, err := os.ReadFile(pidFile); err == nil && len(b) > 0 {
		pid := h.Atoi(string(b))
		h.Until(func() bool { return proctest.Gone(pid) }, 5*time.Second)
	}
	// a container that ends within the limit is untouched by it, and its own 124 is not a timeout
	if r := h.Run("run", "box", "--", "sh", "-c", "exit 124"); r.Code != 124 || strings.Contains(r.Stderr, "time limit") {
		t.Fatalf("%+v", r)
	}
}

func TestDK4_SignalAndCancelRemoveTheContainer(t *testing.T) {
	h := harness.New(t)
	fd := h.FakeDocker()
	h.HostConfig(dkPool)
	// SIGTERM to bitc: 128+15, and the container is removed
	pidFile := filepath.Join(h.Root, "pid-term")
	p := h.Start(append([]string{"run", "--name", "t1", "box", "--"}, dkPidFile(pidFile)...)...)
	pid := dkWaitPid(h, pidFile)
	p.Signal(syscall.SIGTERM)
	if r := p.Wait(); r.Code != 143 {
		t.Fatalf("%+v", r)
	}
	h.Until(func() bool { return proctest.Gone(pid) }, 20*time.Second)
	if !fd.Removed("bitc-t1") || fd.Exists("bitc-t1") {
		t.Fatalf("SIGTERM left the container: calls %v", fd.Calls())
	}
	// SIGINT and SIGHUP too
	for i, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGHUP} {
		name := "s" + string(rune('1'+i))
		f := filepath.Join(h.Root, "pid-"+name)
		p := h.Start(append([]string{"run", "--name", name, "box", "--"}, dkPidFile(f)...)...)
		dkWaitPid(h, f)
		p.Signal(sig)
		if r := p.Wait(); r.Code != 128+int(sig) {
			t.Fatalf("%v: %+v", sig, r)
		}
		if !fd.Removed("bitc-"+name) || fd.Exists("bitc-"+name) {
			t.Fatalf("%v left the container: calls %v", sig, fd.Calls())
		}
	}
	// bitc cancel and bitc cancel --kill on detached jobs
	for _, c := range []struct {
		name string
		args []string
	}{{"c1", []string{"cancel", "c1"}}, {"c2", []string{"cancel", "c2", "--kill"}}} {
		f := filepath.Join(h.Root, "pid-"+c.name)
		detached(t, h, append([]string{"--name", c.name, "box", "--"}, dkPidFile(f)...)...)
		pid := dkWaitPid(h, f)
		if r := h.Run(c.args...); r.Code != 0 {
			t.Fatalf("%v: %+v", c.args, r)
		}
		h.Until(func() bool { return fd.Removed("bitc-"+c.name) && !fd.Exists("bitc-"+c.name) && proctest.Gone(pid) }, 20*time.Second)
	}
}

func TestDK5_SigkilledBitcLeavesNoContainer(t *testing.T) {
	h := harness.New(t)
	fd := h.FakeDocker()
	h.HostConfig(dkPool)
	pidFile := filepath.Join(h.Root, "pid")
	p := h.Start(append([]string{"run", "--name", "k1", "box", "--"}, dkPidFile(pidFile)...)...)
	pid := dkWaitPid(h, pidFile)
	if !fd.Running("bitc-k1") {
		t.Fatal("the container is not running")
	}
	p.Signal(syscall.SIGKILL) // no handler runs: the guard process that outlives bitc removes it
	if r := p.Wait(); r.Code != 137 {
		t.Fatalf("%+v", r)
	}
	h.Until(func() bool { return fd.Removed("bitc-k1") && !fd.Exists("bitc-k1") && proctest.Gone(pid) }, 20*time.Second)
	// and the slot is free: the next run is admitted and its prune finds nothing left to reap
	if r := h.Run("run", "--wait", "0s", "box", "--", "true"); r.Code != 0 {
		t.Fatalf("%+v", r)
	}
}

func TestDK6_OrphansAreSweptByLabel(t *testing.T) {
	h := harness.New(t)
	fd := h.FakeDocker()
	h.HostConfig(dkPool)
	state := dkHash(h)
	deadTicket := "0-0000000000000000001-deadbeef"
	h.Write(filepath.Join(h.State, "pools", "box", "tickets", deadTicket+".json"), `{"state":"running"}`, 0o600)
	labels := func(job, ticket string) map[string]string {
		return map[string]string{"bitc.job": job, "bitc.pool": "box", "bitc.state": state, "bitc.ticket": ticket}
	}
	fd.Plant("bitc-noticket", labels("noticket", "0-0000000000000000002-cafef00d"))                                                      // no ticket file at all
	fd.Plant("bitc-deadticket", labels("deadticket", deadTicket))                                                                        // a ticket nobody holds
	fd.Plant("bitc-elsewhere", map[string]string{"bitc.job": "x", "bitc.pool": "box", "bitc.state": "0123456789ab", "bitc.ticket": "t"}) // another state dir's
	fd.Plant("unrelated", map[string]string{"app": "db"})
	holder := h.Hold(nil, "--name", "live", "box") // a live run: its ticket is locked
	if !fd.Running("bitc-live") {
		t.Fatal("the held run's container is not running")
	}
	r := h.Run("sweep", "--dry-run")
	for _, name := range []string{"bitc-noticket", "bitc-deadticket"} {
		if r.Code != 0 || !strings.Contains(r.Stdout, "swept container "+name+" (its run is gone; dry run)\n") || !fd.Exists(name) {
			t.Fatalf("dry run %s: %+v", name, r)
		}
	}
	if strings.Contains(r.Stdout, "bitc-live") || strings.Contains(r.Stdout, "bitc-elsewhere") || strings.Contains(r.Stdout, "unrelated") {
		t.Fatalf("dry run names a container that is not an orphan: %+v", r)
	}
	r = h.Run("sweep")
	for _, name := range []string{"bitc-noticket", "bitc-deadticket"} {
		if r.Code != 0 || !strings.Contains(r.Stdout, "swept container "+name+" (its run is gone)\n") || !fd.Removed(name) || fd.Exists(name) {
			t.Fatalf("sweep %s: %+v calls %v", name, r, fd.Calls())
		}
	}
	for _, name := range []string{"bitc-live", "bitc-elsewhere", "unrelated"} {
		if !fd.Exists(name) || fd.Removed(name) {
			t.Fatalf("sweep removed %s, which is not an orphan of this state dir", name)
		}
	}
	if r := holder.Release(); r.Code != 0 {
		t.Fatalf("%+v", r)
	}
	// the next admission's prune reaps one too
	fd.Plant("bitc-late", labels("late", "0-0000000000000000003-0badcafe"))
	r = h.Run("run", "box", "--", "true")
	if r.Code != 0 || !strings.Contains(r.Stderr, "bitc: removed orphaned container bitc-late (its run is gone)\n") || !fd.Removed("bitc-late") || fd.Exists("bitc-late") {
		t.Fatalf("admission: %+v calls %v", r, fd.Calls())
	}
	if !fd.Exists("bitc-elsewhere") || !fd.Exists("unrelated") {
		t.Fatal("the admission prune touched a container that is not this state dir's")
	}
}

func TestDK7_ConfigErrorsAre78(t *testing.T) {
	h := harness.New(t)
	h.FakeDocker()
	for _, c := range []struct{ body, key string }{
		{`container = { image = "x", runtime = "podman" }`, "pools.box.container.runtime"},
		{`container = { runtime = "docker" }`, "pools.box.container.image"},
		{`container = { image = "" }`, "pools.box.container.image"},
		{`container = { image = "-rm" }`, "pools.box.container.image"},
		{`container = { image = "a b" }`, "pools.box.container.image"},
		{`container = { image = "x", cpus = 0 }`, "pools.box.container.cpus"},
		{`container = { image = "x", cpus = "two" }`, "pools.box.container.cpus"},
		{`container = { image = "x", memory = "lots" }`, "pools.box.container.memory"},
		{`container = { image = "x", memory = "1KiB" }`, "pools.box.container.memory"},
		{`container = { image = "x", mounts = ["data:/data"] }`, "pools.box.container.mounts"},
		{`container = { image = "x", mounts = ["/data"] }`, "pools.box.container.mounts"},
		{`container = { image = "x", mounts = ["/a:/b:zz"] }`, "pools.box.container.mounts"},
		{`container = { image = "x", mounts = ["/a,b:/c"] }`, "pools.box.container.mounts"},
		{"container = { image = \"x\" }\n[[pools.box.hooks.wrap]]\nwhen_worktree = \"*\"\nargv = [\"nice\"]", "hooks.wrap"},
		{`container = { image = "x", privileged = true }`, "privileged"},
	} {
		h.HostConfig("[pools.box]\n" + c.body + "\n")
		if r := h.Run("run", "box", "--", "true"); r.Code != 78 || !strings.Contains(r.Stderr, c.key) {
			t.Fatalf("%s: want 78 naming %s: %+v", c.body, c.key, r)
		}
		if r := h.Run("config", "check"); r.Code != 78 || !strings.Contains(r.Stderr, c.key) {
			t.Fatalf("config check %s: want 78 naming %s: %+v", c.body, c.key, r)
		}
	}
	// the key is host-only: a repo file may not choose the image or the mounts
	h.HostConfig("[pools.box]\n")
	h.Cwd = h.Repo("app", map[string]string{".bitc.toml": "version = 1\n[pools.box]\ncontainer = { image = \"x\" }\n"})
	if r := h.Run("run", "box", "--", "true"); r.Code != 78 || !strings.Contains(r.Stderr, "host-only") || !strings.Contains(r.Stderr, "container") {
		t.Fatalf("%+v", r)
	}
	// and a valid one loads
	h.Cwd = h.Root
	h.HostConfig(dkPool)
	if r := h.Run("config", "check"); r.Code != 0 {
		t.Fatalf("%+v", r)
	}
}

func TestDK8_RuntimeFailuresAre69(t *testing.T) {
	h := harness.New(t)
	fd := h.FakeDocker()
	h.HostConfig(dkPool)
	// the daemon refuses before any container exists: 69 with a line that says so, not the 125
	// docker uses for it
	r := h.RunWith([]string{"FAKE_DOCKER_FAIL=run"}, "run", "box", "--", "true")
	if r.Code != 69 || !strings.Contains(r.Stderr, "Cannot connect to the Docker daemon") ||
		!strings.Contains(r.Stderr, "bitc: box: docker could not start the container (it exited 125 before the command ran; its message is above)\n") {
		t.Fatalf("%+v", r)
	}
	if len(fd.CallsOf("run")) != 1 {
		t.Fatalf("calls: %v", fd.Calls())
	}
	// no docker on PATH: 69 before admission, one line, no state left behind
	empty := t.TempDir()
	r = h.RunWith([]string{"PATH=" + empty}, "run", "box", "--", "true")
	if r.Code != 69 || !strings.Contains(r.Stderr, `bitc: box: container runtime "docker" not found on PATH`) {
		t.Fatalf("%+v", r)
	}
	if got := tickets(h, "box"); len(got) != 0 {
		t.Fatalf("a refused run left tickets: %v", got)
	}
	// a container name already in use is docker's refusal too (125 with no container id written)
	fd.Plant("bitc-dup", map[string]string{"app": "x"})
	r = h.Run("run", "--name", "dup", "box", "--", "true")
	if r.Code != 69 || !strings.Contains(r.Stderr, "already in use") {
		t.Fatalf("%+v", r)
	}
}

func TestDK9_ContainerEnvIsMinimal(t *testing.T) {
	h := harness.New(t)
	h.FakeDocker()
	h.HostConfig("[pools.box]\ncontainer = { image = \"" + dkImage + "\" }\nenv = { GREETING = \"hi\" }\nenv_default = { MODE = \"fast\", TERM = \"dumb\" }\nenv_unset = [\"TZ\"]\n")
	r := h.RunWith([]string{"SECRET_TOKEN=s3cr3t", "TERM=xterm", "LANG=C.UTF-8", "TZ=UTC", "AWS_ACCESS_KEY_ID=x", "MODE=slow"}, "run", "--name", "e1", "box", "--", "env")
	if r.Code != 0 {
		t.Fatalf("%+v", r)
	}
	got := map[string]string{}
	for _, l := range strings.Split(strings.TrimSpace(r.Stdout), "\n") {
		k, v, _ := strings.Cut(l, "=")
		got[k] = v
	}
	delete(got, "PATH") // the fake's own PATH so the command resolves; a real container has its image's
	want := map[string]string{"BITC_JOB": "e1", "BITC_POOL": "box", "BITC_SLOT": "box.1", "GREETING": "hi",
		"MODE": "fast", "TERM": "xterm", "LANG": "C.UTF-8", "LC_ALL": "C"}
	// MODE is env_default only: the host's MODE is not passed, so the default applies; TERM and
	// LANG are the host's; TZ is env_unset; nothing else of the host's environment crosses
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("container env:\n got %v\nwant %v", got, want)
	}
}

// DK10 is opt-in: it needs a docker daemon and a small local image, and never pulls.
func TestDK10_RealDockerRunsAndTimesOut(t *testing.T) {
	if os.Getenv("BITC_TEST_DOCKER") != "1" {
		t.Skip("set BITC_TEST_DOCKER=1 to run against a real docker")
	}
	docker, err := exec.LookPath("docker")
	if err != nil {
		t.Skip("docker is not on PATH")
	}
	out, err := exec.Command(docker, "image", "ls", "--format", "{{.Repository}}:{{.Tag}}").Output()
	if err != nil {
		t.Skipf("docker image ls: %v", err)
	}
	image := ""
	for _, pref := range []string{"alpine:", "busybox:"} {
		for _, l := range strings.Fields(string(out)) {
			if strings.HasPrefix(l, pref) && !strings.HasSuffix(l, ":<none>") {
				image = l
				break
			}
		}
		if image != "" {
			break
		}
	}
	if image == "" {
		t.Skip("no local alpine or busybox image (this test never pulls)")
	}
	h := harness.New(t)
	h.HostConfig("[host]\ngrace = \"2s\"\n[pools.box]\ncontainer = { image = \"" + image + "\" }\n")
	if r := h.Run("run", "box", "--", "true"); r.Code != 0 {
		t.Fatalf("true: %+v", r)
	}
	r := h.Run("run", "--timeout", "3s", "box", "--", "sleep", "60")
	if r.Code != 124 || !strings.Contains(r.Stderr, "time limit 3s reached; terminated") {
		t.Fatalf("sleep: %+v", r)
	}
	left, err := exec.Command(docker, "ps", "-a", "-q", "--filter", "label=bitc.state="+dkHash(h)).Output()
	if err != nil || strings.TrimSpace(string(left)) != "" {
		t.Fatalf("containers left behind: %q %v", left, err)
	}
}
