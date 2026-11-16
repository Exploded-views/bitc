package container

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/Exploded-views/bitc/conformance/harness"
	"github.com/Exploded-views/bitc/internal/config"
	"github.com/Exploded-views/bitc/internal/lock"
	"github.com/Exploded-views/bitc/internal/proctest"
	"github.com/Exploded-views/bitc/internal/state"
)

func TestMain(m *testing.M) {
	code := m.Run()
	harness.CleanupFakeDocker()
	os.Exit(code)
}

// fake is a fake docker on PATH in a new dir: the env a runtime call gets, and the state dir the
// fake records into.
type fake struct {
	t     *testing.T
	env   []string
	state string
	path  string // the docker binary
}

func newFake(t *testing.T) *fake {
	t.Helper()
	bin, err := harness.BuildFakeDockerBinary(harness.ModuleRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "docker")
	if err := os.WriteFile(path, b, 0o755); err != nil {
		t.Fatal(err)
	}
	f := &fake{t: t, state: filepath.Join(dir, "state"), path: path}
	f.env = []string{"PATH=" + dir + string(os.PathListSeparator) + os.Getenv("PATH"), "FAKE_DOCKER_DIR=" + f.state}
	return f
}

// plant adds a container record the fake knows (nothing runs).
func (f *fake) plant(name string, labels map[string]string) {
	f.t.Helper()
	if err := os.MkdirAll(filepath.Join(f.state, "c"), 0o755); err != nil {
		f.t.Fatal(err)
	}
	b, _ := json.Marshal(map[string]any{"name": name, "labels": labels})
	if err := os.WriteFile(filepath.Join(f.state, "c", name+".json"), b, 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fake) exists(name string) bool {
	_, err := os.Stat(filepath.Join(f.state, "c", name+".json"))
	return err == nil
}

func TestNamesAndHashes(t *testing.T) {
	if got := Name("j1"); got != "bitc-j1" {
		t.Fatal(got)
	}
	h := StateHash("/state")
	if len(h) != 12 || h != StateHash("/state") || h == StateHash("/other") {
		t.Fatal(h)
	}
	if got := TicketID("0-0000000000000000001-deadbeef.json"); got != "0-0000000000000000001-deadbeef" {
		t.Fatal(got)
	}
}

func TestRunArgv(t *testing.T) {
	s := Spec{Runtime: "/bin/docker", Image: "img:1", Job: "j", Pool: "p", State: "abc", Ticket: "tk", CID: "/s/tk.cid",
		Mounts: []config.Mount{{Source: "/w", Target: "/w"}, {Source: "/a", Target: "/b", ReadOnly: true}},
		Dir:    "/w/sub", CPUs: "1.5", Memory: 8 << 20, Env: []string{"A=1", "B"}, TTY: true, Argv: []string{"echo", "-n", "x"}}
	want := []string{"/bin/docker", "run", "--rm", "--init", "-it", "--name", "bitc-tk", // unique per run: from the ticket, not the job
		"--label", "bitc.job=j", "--label", "bitc.pool=p", "--label", "bitc.state=abc", "--label", "bitc.ticket=tk",
		"--cidfile", "/s/tk.cid", "--mount", "type=bind,source=/w,target=/w", "--mount", "type=bind,source=/a,target=/b,readonly",
		"--workdir", "/w/sub", "--cpus", "1.5", "--memory", "8388608", "-e", "A=1", "-e", "B", "img:1", "echo", "-n", "x"}
	if got := RunArgv(s); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q\nwant %q", got, want)
	}
	s.TTY, s.CPUs, s.Memory, s.Env, s.Mounts = false, "", 0, nil, nil
	got := RunArgv(s)
	for _, a := range []string{"-it", "--cpus", "--memory", "-e", "--mount"} {
		for _, g := range got {
			if g == a {
				t.Fatalf("%s present without its setting: %q", a, got)
			}
		}
	}
	if got[4] != "-i" {
		t.Fatalf("%q", got)
	}
	if got := RemoveArgv("d", "n"); !reflect.DeepEqual(got, []string{"d", "rm", "-f", "n"}) {
		t.Fatal(got)
	}
}

func TestLookup(t *testing.T) {
	dir, other := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(other, "docker"), []byte("x"), 0o644); err != nil { // not executable
		t.Fatal(err)
	}
	if _, err := Lookup("docker", []string{"PATH=" + other + ":" + dir}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a non-executable file was found: %v", err)
	}
	if _, err := Lookup("docker", []string{"HOME=/x"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("no PATH: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := Lookup("docker", []string{"PATH=/nonexistent::" + other + ":" + dir, "PATH=" + other + ":" + dir})
	if err != nil || got != filepath.Join(dir, "docker") {
		t.Fatalf("%q %v", got, err)
	}
}

func TestRemoveToleratesAnAbsentContainerButNotAFailure(t *testing.T) {
	f := newFake(t)
	f.plant("here", nil)
	if err := Remove(context.Background(), f.path, "here", f.env); err != nil || f.exists("here") {
		t.Fatalf("%v", err)
	}
	if err := Remove(context.Background(), f.path, "here", f.env); err != nil { // already gone
		t.Fatalf("a container that is gone is not an error: %v", err)
	}
	err := Remove(context.Background(), f.path, "x", append(f.env, "FAKE_DOCKER_FAIL=rm")) // a daemon failure is an error
	if err == nil || !strings.Contains(err.Error(), "Cannot connect") {
		t.Fatalf("%v", err)
	}
	if err := Remove(context.Background(), "/nonexistent/docker", "x", f.env); err == nil {
		t.Fatal("a runtime that cannot start is an error")
	}
}

func TestSweepRemovesOrphansOfThisStateDirOnly(t *testing.T) {
	f := newFake(t)
	l, err := state.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	hash := StateHash(l.Root)
	lab := func(ticket string) map[string]string {
		return map[string]string{LabelPool: "p", LabelState: hash, LabelTicket: ticket}
	}
	const live, dead, none = "0-0000000000000000001-aaaaaaaa", "0-0000000000000000002-bbbbbbbb", "0-0000000000000000003-cccccccc"
	if err := os.MkdirAll(l.Tickets("p"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{live, dead} {
		if err := os.WriteFile(filepath.Join(l.Tickets("p"), id+".json"), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	held, err := lock.Open(filepath.Join(l.Tickets("p"), live+".json"))
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	if ok, err := held.TryLock(); !ok || err != nil {
		t.Fatalf("cannot hold the live ticket: %v", err)
	}
	f.plant("bitc-live", lab(live))
	f.plant("bitc-dead", lab(dead))
	f.plant("bitc-none", lab(none))
	f.plant("bitc-junk", lab("not-a-ticket")) // not written by bitc: never judged
	f.plant("bitc-other", map[string]string{LabelState: "000000000000", LabelTicket: none})
	if err := os.MkdirAll(l.ContainersDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{live, dead} {
		if err := os.WriteFile(filepath.Join(l.ContainersDir(), id+".cid"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var removed, kept []string
	rec := func(into *[]string) func(Found) { return func(x Found) { *into = append(*into, x.Name) } }

	if err := Sweep(context.Background(), f.path, l, f.env, true, rec(&removed), rec(&kept)); err != nil {
		t.Fatal(err)
	}
	if len(removed) != 2 || !f.exists("bitc-dead") || !f.exists("bitc-none") {
		t.Fatalf("dry run removed %v (or removed something)", removed)
	}
	if _, err := os.Stat(filepath.Join(l.ContainersDir(), dead+".cid")); err != nil {
		t.Fatal("a dry run removed a cidfile")
	}
	removed = nil
	if err := Sweep(context.Background(), f.path, l, f.env, false, rec(&removed), rec(&kept)); err != nil {
		t.Fatal(err)
	}
	if len(removed) != 2 || f.exists("bitc-dead") || f.exists("bitc-none") || len(kept) != 0 {
		t.Fatalf("removed %v kept %v", removed, kept)
	}
	for _, n := range []string{"bitc-live", "bitc-junk", "bitc-other"} {
		if !f.exists(n) {
			t.Fatalf("%s was removed", n)
		}
	}
	if _, err := os.Stat(filepath.Join(l.ContainersDir(), dead+".cid")); err == nil {
		t.Fatal("the dead run's cidfile is still there")
	}
	if _, err := os.Stat(filepath.Join(l.ContainersDir(), live+".cid")); err != nil {
		t.Fatal("the live run's cidfile was removed")
	}
	// the runtime failing (daemon down) is the caller's to report; nothing is claimed removed
	removed = nil
	err = Sweep(context.Background(), f.path, l, append(f.env, "FAKE_DOCKER_FAIL=ps"), false, rec(&removed), rec(&kept))
	if err == nil || !strings.Contains(err.Error(), "Cannot connect") || len(removed) != 0 {
		t.Fatalf("%v %v", err, removed)
	}
	if err := Sweep(context.Background(), "/nonexistent/docker", l, f.env, false, rec(&removed), rec(&kept)); err == nil {
		t.Fatal("a runtime that cannot start is an error")
	}
}

// An unreadable pools/ dir says nothing about which tickets are held: every container is kept
// (the sweep never removes on a guess, #24), whatever the error is but "it is not there".
func TestSweepKeepsEverythingWhenTheTicketsCannotBeRead(t *testing.T) {
	f := newFake(t)
	l, err := state.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(l.Root, "pools"), []byte("not a dir"), 0o600); err != nil { // ReadDir: ENOTDIR
		t.Fatal(err)
	}
	const id = "0-0000000000000000001-aaaaaaaa"
	f.plant("bitc-"+id, map[string]string{LabelPool: "p", LabelState: StateHash(l.Root), LabelTicket: id, LabelJob: "j"})
	var removed, kept []string
	err = Sweep(context.Background(), f.path, l, f.env, false, func(x Found) { removed = append(removed, x.Name) }, func(x Found) { kept = append(kept, x.Name) })
	if err != nil || len(removed) != 0 || len(kept) != 1 || !f.exists("bitc-"+id) {
		t.Fatalf("err %v removed %v kept %v", err, removed, kept)
	}
	if got := TicketState(l, id); got != lock.StateUnknown {
		t.Fatalf("TicketState = %v", got)
	}
	// no pools dir at all is just "no tickets": the container is an orphan
	if err := os.Remove(filepath.Join(l.Root, "pools")); err != nil {
		t.Fatal(err)
	}
	if got := TicketState(l, id); got != lock.StateFree {
		t.Fatalf("TicketState = %v", got)
	}
}

func TestGuardMainRemovesOnEOFAndNotAfterDone(t *testing.T) {
	f := newFake(t)
	t.Setenv("PATH", f.env[0][len("PATH="):])
	t.Setenv("FAKE_DOCKER_DIR", f.state)
	f.plant("bitc-g", nil)
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.WriteString("noise\n" + doneLine); err != nil {
		t.Fatal(err)
	}
	if code := GuardMain([]string{f.path, "bitc-g"}, r); code != 0 || !f.exists("bitc-g") {
		t.Fatalf("a done guard touched the container (code %d)", code)
	}
	w.Close()
	r.Close()
	r, w, err = os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.WriteString("partial"); err != nil { // a torn line is not "done"
		t.Fatal(err)
	}
	w.Close()
	if code := GuardMain([]string{f.path, "bitc-g"}, r); code != 0 || f.exists("bitc-g") {
		t.Fatalf("EOF without done left the container (code %d)", code)
	}
	r.Close()
	if code := GuardMain(nil, nil); code != 64 {
		t.Fatalf("usage: %d", code)
	}
}

// TestGuardHelperProcess is the guard process of TestStartGuard: the test binary re-executed.
func TestGuardHelperProcess(t *testing.T) {
	if os.Getenv("BITC_TEST_GUARD_HELPER") != "1" {
		t.Skip("helper process")
	}
	args := os.Args
	os.Exit(GuardMain(args[len(args)-2:], os.NewFile(LifelineFD, "lifeline")))
}

func TestStartGuard(t *testing.T) {
	f := newFake(t)
	env := append(append([]string{}, f.env...), "BITC_TEST_GUARD_HELPER=1")
	exe, args := os.Args[0], []string{"-test.run=^TestGuardHelperProcess$"}

	f.plant("bitc-done", nil)
	g, err := StartGuard(exe, args, f.path, "bitc-done", env)
	if err != nil {
		t.Fatal(err)
	}
	pid := g.cmd.Process.Pid
	g.Done() // the run ended cleanly: the guard goes without removing anything
	for !proctest.Gone(pid) {
		runtime.Gosched() // Done reaps the guard in the background; the test timeout bounds this
	}
	if !f.exists("bitc-done") {
		t.Fatal("a guard that was told done removed the container")
	}
	// a bitc that dies closes the pipe without saying done: the guard removes the container
	f.plant("bitc-died", nil)
	g, err = StartGuard(exe, args, f.path, "bitc-died", env)
	if err != nil {
		t.Fatal(err)
	}
	g.w.Close() // what the kernel does when bitc is SIGKILLed
	if err := g.cmd.Wait(); err != nil {
		t.Fatalf("guard: %v", err)
	}
	if f.exists("bitc-died") {
		t.Fatal("the guard did not remove the container of a bitc that died")
	}
	if _, err := StartGuard("/nonexistent/bitc", args, f.path, "x", env); err == nil {
		t.Fatal("a guard that cannot start is an error")
	}
}
