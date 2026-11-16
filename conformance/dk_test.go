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
// process group of its own, and kills that group on `rm -f`. Unlike the real CLI's default (which
// proxies signals to the container unless it has a TTY) it forwards none, the worst case: only
// bitc's own `docker rm -f` removes a container whose CLI was signalled or killed. FAKE_DOCKER_PROXY=1
// makes it proxy like the real one.

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

// dkName is the container name of job's run, from the `docker run` the fake recorded. The name is
// unique per run (bitc-<ticket id>), so two runs of one job name never share a container.
func dkName(t *testing.T, fd *harness.FakeDocker, job string) string {
	t.Helper()
	names := dkNames(fd, job)
	if len(names) == 0 {
		t.Fatalf("no docker run for job %s: %v", job, fd.Calls())
	}
	return names[0]
}

// dkNames is the container names of every run of job, oldest first.
func dkNames(fd *harness.FakeDocker, job string) []string {
	var out []string
	for _, c := range fd.CallsOf("run") {
		if dkLabel(c, "bitc.job") != job {
			continue
		}
		for i, a := range c {
			if a == "--name" && i+1 < len(c) {
				out = append(out, c[i+1])
			}
		}
	}
	return out
}

// dkEnvFilePath is the --env-file of job's first run (in the state dir, next to the cidfile).
func dkEnvFilePath(t *testing.T, fd *harness.FakeDocker, job string) string {
	t.Helper()
	for _, c := range fd.CallsOf("run") {
		if dkLabel(c, "bitc.job") != job {
			continue
		}
		for i, a := range c {
			if a == "--env-file" && i+1 < len(c) {
				return c[i+1]
			}
		}
	}
	t.Fatalf("no --env-file for job %s: %v", job, fd.Calls())
	return ""
}

// dkGone reports whether path does not exist.
func dkGone(path string) bool { _, err := os.Lstat(path); return os.IsNotExist(err) }

// dkRemovedByID fails unless every removal was by container id: a removal by name could remove a
// container another run owns.
func dkRemovedByID(t *testing.T, fd *harness.FakeDocker) {
	t.Helper()
	for _, a := range fd.RemovedBy() {
		if strings.HasPrefix(a, "bitc-") || !strings.HasPrefix(a, "cid") {
			t.Fatalf("a container was removed by %q, not by id: %v", a, fd.Calls())
		}
	}
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
	name1 := "bitc-" + ticket // unique per run: the job name is only a label
	want := []string{"run", "--rm", "--init", "-i", "--name", name1,
		"--label", "bitc.job=j1", "--label", "bitc.pool=box", "--label", "bitc.state=" + dkHash(h), "--label", "bitc.ticket=" + ticket,
		"--cidfile", filepath.Join(h.State, "containers", ticket+".cid"),
		"--env-file", filepath.Join(h.State, "containers", ticket+".env"), // the values are in this 0600 file, never on argv
		"--mount", "type=bind,source=" + wt + ",target=" + wt, "--workdir", wt,
		dkImage, "echo", "hi", "--flag"}
	if !reflect.DeepEqual(runs[0], want) {
		t.Fatalf("docker run argv:\n got %q\nwant %q", runs[0], want)
	}
	// the env file: 0600, one NAME=VALUE per line sorted by name, and gone once the run ended
	path, mode, body, ok := fd.EnvFile(name1)
	if !ok || mode != "0600" || path != filepath.Join(h.State, "containers", ticket+".env") ||
		body != "BITC_JOB=j1\nBITC_POOL=box\nBITC_SLOT=box.1\nLC_ALL=C\n" {
		t.Fatalf("env file: ok %v mode %q path %q body %q", ok, mode, path, body)
	}
	if !dkGone(path) {
		t.Fatal("a finished run left its env file")
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
	name2 := "bitc-" + ticket
	want = []string{"run", "--rm", "--init", "-i", "--name", name2,
		"--label", "bitc.job=j2", "--label", "bitc.pool=big", "--label", "bitc.state=" + dkHash(h), "--label", "bitc.ticket=" + ticket,
		"--cidfile", filepath.Join(h.State, "containers", ticket+".cid"),
		"--env-file", filepath.Join(h.State, "containers", ticket+".env"),
		"--mount", "type=bind,source=" + wt + ",target=" + wt,
		"--mount", "type=bind,source=/data,target=/data,readonly",
		"--mount", "type=bind,source=/cache,target=/cache",
		"--workdir", wt, "--cpus", "1.5", "--memory", "536870912",
		"big:2", "true"}
	if !reflect.DeepEqual(runs[1], want) {
		t.Fatalf("docker run argv with limits:\n got %q\nwant %q", runs[1], want)
	}
	if fd.Exists(name1) || fd.Exists(name2) { // --rm: nothing is left behind
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
	// a limit this short can fire before the CLI created anything: then there is no container and
	// nothing to remove; one that was created is removed, by its id from the cidfile
	if name := dkName(t, fd, "tl"); fd.Exists(name) || (fd.Created(name) && !fd.Removed(name)) {
		t.Fatalf("the time limit left the container: calls %v", fd.Calls())
	}
	dkRemovedByID(t, fd)
	if env := dkEnvFilePath(t, fd, "tl"); !dkGone(env) {
		t.Fatalf("the time limit left the env file %s", env)
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
	if name := dkName(t, fd, "t1"); !fd.Removed(name) || fd.Exists(name) || !dkGone(dkEnvFilePath(t, fd, "t1")) {
		t.Fatalf("SIGTERM left the container or its env file: calls %v", fd.Calls())
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
		if cn := dkName(t, fd, name); !fd.Removed(cn) || fd.Exists(cn) {
			t.Fatalf("%v left the container: calls %v", sig, fd.Calls())
		}
	}
	// a docker CLI that does proxy signals (the real one without -t) hands the container the TERM:
	// bitc's run still ends 143 and nothing is left
	f := filepath.Join(h.Root, "pid-proxy")
	p = h.StartWith([]string{"FAKE_DOCKER_PROXY=1"}, append([]string{"run", "--name", "px", "box", "--"}, dkPidFile(f)...)...)
	pid = dkWaitPid(h, f)
	p.Signal(syscall.SIGTERM)
	if r := p.Wait(); r.Code != 143 {
		t.Fatalf("proxy: %+v", r)
	}
	h.Until(func() bool { return proctest.Gone(pid) }, 20*time.Second)
	if cn := dkName(t, fd, "px"); fd.Exists(cn) {
		t.Fatalf("a proxying CLI left the container: calls %v", fd.Calls())
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
		cn := dkName(t, fd, c.name)
		envf := dkEnvFilePath(t, fd, c.name)
		h.Until(func() bool { return fd.Removed(cn) && !fd.Exists(cn) && proctest.Gone(pid) && dkGone(envf) }, 20*time.Second)
	}
	dkRemovedByID(t, fd)
}

func TestDK5_SigkilledBitcLeavesNoContainer(t *testing.T) {
	h := harness.New(t)
	fd := h.FakeDocker()
	h.HostConfig(dkPool)
	pidFile := filepath.Join(h.Root, "pid")
	p := h.Start(append([]string{"run", "--name", "k1", "box", "--"}, dkPidFile(pidFile)...)...)
	pid := dkWaitPid(h, pidFile)
	name := dkName(t, fd, "k1")
	if !fd.Running(name) {
		t.Fatal("the container is not running")
	}
	envf := dkEnvFilePath(t, fd, "k1")
	if dkGone(envf) {
		t.Fatal("the env file is gone while the container runs")
	}
	p.Signal(syscall.SIGKILL) // no handler runs: the guard process that outlives bitc removes it
	if r := p.Wait(); r.Code != 137 {
		t.Fatalf("%+v", r)
	}
	// the guard removes the container (by the id in the cidfile) and the env file bitc could not
	h.Until(func() bool { return fd.Removed(name) && !fd.Exists(name) && proctest.Gone(pid) && dkGone(envf) }, 20*time.Second)
	dkRemovedByID(t, fd)
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
	// the live run goes first: the admission of a container pool sweeps, so orphans planted before it
	// would be gone before the sweep command ran
	holder := h.Hold(nil, "--name", "live", "box") // its ticket is locked
	live := dkName(t, fd, "live")
	if !fd.Running(live) {
		t.Fatal("the held run's container is not running")
	}
	deadTicket := "0-0000000000000000001-deadbeef"
	h.Write(filepath.Join(h.State, "pools", "box", "tickets", deadTicket+".json"), `{"state":"running"}`, 0o600)
	labels := func(job, ticket string) map[string]string {
		return map[string]string{"bitc.job": job, "bitc.pool": "box", "bitc.state": state, "bitc.ticket": ticket}
	}
	fd.Plant("bitc-noticket", labels("noticket", "0-0000000000000000002-cafef00d"))                                                      // no ticket file at all
	fd.Plant("bitc-deadticket", labels("deadticket", deadTicket))                                                                        // a ticket nobody holds
	fd.Plant("bitc-elsewhere", map[string]string{"bitc.job": "x", "bitc.pool": "box", "bitc.state": "0123456789ab", "bitc.ticket": "t"}) // another state dir's
	fd.Plant("unrelated", map[string]string{"app": "db"})
	r := h.Run("sweep", "--dry-run")
	for _, name := range []string{"bitc-noticket", "bitc-deadticket"} {
		if r.Code != 0 || !strings.Contains(r.Stdout, "swept container "+name+" (its run is gone; dry run)\n") || !fd.Exists(name) {
			t.Fatalf("dry run %s: %+v", name, r)
		}
	}
	if strings.Contains(r.Stdout, live) || strings.Contains(r.Stdout, "bitc-elsewhere") || strings.Contains(r.Stdout, "unrelated") {
		t.Fatalf("dry run names a container that is not an orphan: %+v", r)
	}
	r = h.Run("sweep")
	for _, name := range []string{"bitc-noticket", "bitc-deadticket"} {
		if r.Code != 0 || !strings.Contains(r.Stdout, "swept container "+name+" (its run is gone)\n") || !fd.Removed(name) || fd.Exists(name) {
			t.Fatalf("sweep %s: %+v calls %v", name, r, fd.Calls())
		}
	}
	for _, name := range []string{live, "bitc-elsewhere", "unrelated"} {
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
	dkRemovedByID(t, fd) // the sweep removes by the id it listed, never by a name
}

// A sweep that cannot tell which tickets are held (the pools dir does not read) stops and removes
// nothing, however many orphans there look to be, and says so; the run that swept at admission
// goes on regardless.
func TestDK12_ASweepErrorRemovesNothing(t *testing.T) {
	h := harness.New(t)
	fd := h.FakeDocker()
	h.HostConfig(dkPool)
	labels := map[string]string{"bitc.job": "x", "bitc.pool": "box", "bitc.state": dkHash(h), "bitc.ticket": "0-0000000000000000002-cafef00d"}
	if r := h.Run("run", "box", "--", "true"); r.Code != 0 { // makes the state dir
		t.Fatalf("%+v", r)
	}
	fd.Plant("bitc-orphan", labels)
	if err := os.RemoveAll(filepath.Join(h.State, "pools")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.State, "pools"), []byte("not a dir"), 0o600); err != nil { // reading pools/ fails
		t.Fatal(err)
	}
	cid := filepath.Join(h.State, "containers", "0-0000000000000000002-cafef00d.cid")
	h.Write(cid, "cidplantedbitc-orphan", 0o600)
	rms := len(fd.CallsOf("rm")) // the priming run's own removal
	r := h.Run("sweep")
	if !strings.Contains(r.Stdout, "container sweep aborted") || strings.Contains(r.Stdout, "swept container") {
		t.Fatalf("%+v", r)
	}
	if !fd.Exists("bitc-orphan") || len(fd.CallsOf("rm")) != rms || dkGone(cid) {
		t.Fatalf("an aborted sweep deleted something: calls %v", fd.Calls())
	}
	// the daemon failing in the middle of the removals stops it there too
	if err := os.Remove(filepath.Join(h.State, "pools")); err != nil {
		t.Fatal(err)
	}
	fd.Plant("bitc-orphan2", labels)
	r = h.RunWith([]string{"FAKE_DOCKER_FAIL=rm"}, "sweep")
	if !strings.Contains(r.Stdout, "container sweep aborted") || !fd.Exists("bitc-orphan") || !fd.Exists("bitc-orphan2") {
		t.Fatalf("%+v", r)
	}
	// and a run's admission with a failing sweep still runs the command
	if r := h.RunWith([]string{"FAKE_DOCKER_FAIL=ps"}, "run", "box", "--", "true"); r.Code != 0 {
		t.Fatalf("%+v", r)
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
		{`container = { image = "x", mounts = ["/a\"b:/c"] }`, "pools.box.container.mounts"},
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
	if len(fd.CallsOf("run")) != 1 || len(fd.CallsOf("rm")) != 0 { // no container was created: nothing to remove
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
	// a refused run removes nothing it did not create: the container of the job name's old scheme,
	// planted here, survives the failed run, the early return and the guard
	fd.Plant("bitc-dup", map[string]string{"app": "x"})
	if r := h.RunWith([]string{"FAKE_DOCKER_FAIL=run"}, "run", "--name", "dup", "box", "--", "true"); r.Code != 69 {
		t.Fatalf("%+v", r)
	}
	if !fd.Exists("bitc-dup") || len(fd.CallsOf("rm")) != 0 { // no cidfile, so no container of its own: no rm at all
		t.Fatalf("a failed run removed a container it did not create: %v", fd.Calls())
	}
}

// Two runs of one job name (--name is the caller's, and a foreground run has no uniqueness) each own
// their container: neither one's finish, kill or guard removes the other's.
func TestDK11_SameJobNameRunsHaveTheirOwnContainers(t *testing.T) {
	h := harness.New(t)
	fd := h.FakeDocker()
	h.HostConfig("[pools.box]\nslots = 3\ncontainer = { image = \"" + dkImage + "\" }\n")
	fd.Plant("bitc-same", map[string]string{"app": "x"}) // some other container with the job's old name
	a := h.Hold(nil, "--name", "same", "box")
	nameA := dkName(t, fd, "same")
	if !fd.Running(nameA) {
		t.Fatal("run A's container is not running")
	}
	// run B, the same --name, ends with its own exit code and leaves A's container alone
	if r := h.Run("run", "--name", "same", "box", "--", "sh", "-c", "exit 3"); r.Code != 3 {
		t.Fatalf("B: %+v", r)
	}
	// run C, the same --name, is SIGKILLed: its guard removes C's container and only that
	f := filepath.Join(h.Root, "pid-c")
	c := h.Start(append([]string{"run", "--name", "same", "box", "--"}, dkPidFile(f)...)...)
	pid := dkWaitPid(h, f)
	names := dkNames(fd, "same")
	if len(names) != 3 || names[0] == names[1] || names[1] == names[2] || names[0] == names[2] {
		t.Fatalf("runs of one job name share a container name: %v", names)
	}
	c.Signal(syscall.SIGKILL)
	if r := c.Wait(); r.Code != 137 {
		t.Fatalf("C: %+v", r)
	}
	envC := dkEnvFilePath(t, fd, "same") // A's: the first run of the job; C's is the last --env-file in the log
	for _, c := range fd.CallsOf("run") {
		for i, a := range c {
			if a == "--env-file" {
				envC = c[i+1]
			}
		}
	}
	h.Until(func() bool { return fd.Removed(names[2]) && !fd.Exists(names[2]) && proctest.Gone(pid) && dkGone(envC) }, 20*time.Second)
	dkRemovedByID(t, fd)
	if !fd.Running(nameA) || fd.Removed(nameA) {
		t.Fatalf("another run's end removed A's container: %v", fd.Calls())
	}
	if r := a.Release(); r.Code != 0 {
		t.Fatalf("A: %+v", r)
	}
	if !fd.Exists("bitc-same") || fd.Removed("bitc-same") {
		t.Fatalf("a container this run never created was removed: %v", fd.Calls())
	}
}

func TestDK9_ContainerEnvIsMinimal(t *testing.T) {
	h := harness.New(t)
	fd := h.FakeDocker()
	h.HostConfig("[pools.box]\ncontainer = { image = \"" + dkImage + "\" }\nenv = { GREETING = \"s3cret-greeting\" }\nenv_default = { MODE = \"fast\", TERM = \"dumb\" }\nenv_unset = [\"TZ\"]\n")
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
	want := map[string]string{"BITC_JOB": "e1", "BITC_POOL": "box", "BITC_SLOT": "box.1", "GREETING": "s3cret-greeting",
		"MODE": "fast", "TERM": "xterm", "LANG": "C.UTF-8", "LC_ALL": "C"}
	// MODE is env_default only: the host's MODE is not passed, so the default applies; TERM and
	// LANG are the host's; TZ is env_unset; nothing else of the host's environment crosses
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("container env:\n got %v\nwant %v", got, want)
	}
	// the values reach docker in a 0600 --env-file, never on its argv (ps and /proc/<pid>/cmdline are
	// readable by every local user); bitc removes the file when the run ends
	run := fd.CallsOf("run")[0]
	argv := strings.Join(run, "\x00")
	for _, secret := range []string{"s3cret-greeting", "fast", "xterm", "C.UTF-8", "GREETING"} {
		if strings.Contains(argv, secret) {
			t.Fatalf("%q is on docker's argv: %q", secret, argv)
		}
	}
	for _, a := range run {
		if a == "-e" || a == "--env" {
			t.Fatalf("an -e on docker's argv: %q", run)
		}
	}
	path, mode, body, ok := fd.EnvFile(dkName(t, fd, "e1"))
	if !ok || mode != "0600" || !strings.Contains(body, "GREETING=s3cret-greeting\n") || !strings.Contains(body, "MODE=fast\n") ||
		strings.Contains(body, "SECRET_TOKEN") || strings.Contains(body, "BITC_HOLD") || strings.Contains(body, "BITC_DIR") {
		t.Fatalf("env file: ok %v mode %q body %q", ok, mode, body)
	}
	if !dkGone(path) {
		t.Fatal("the env file is still there after the run")
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
