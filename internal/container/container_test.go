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

// plant adds a container record the fake knows (nothing runs) and returns its id.
func (f *fake) plant(name string, labels map[string]string) string {
	f.t.Helper()
	if err := os.MkdirAll(filepath.Join(f.state, "c"), 0o755); err != nil {
		f.t.Fatal(err)
	}
	id := "cid" + name
	b, _ := json.Marshal(map[string]any{"id": id, "name": name, "labels": labels})
	if err := os.WriteFile(filepath.Join(f.state, "c", name+".json"), b, 0o644); err != nil {
		f.t.Fatal(err)
	}
	return id
}

// removedBy is what each `docker rm` that removed a container was given.
func (f *fake) removedBy() []string {
	b, _ := os.ReadFile(filepath.Join(f.state, "removed.log"))
	var out []string
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var m map[string]string
		if json.Unmarshal([]byte(l), &m) == nil {
			out = append(out, m["arg"])
		}
	}
	return out
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
		Dir:    "/w/sub", CPUs: "1.5", Memory: 8 << 20, EnvFile: "/s/tk.env", TTY: true, Argv: []string{"echo", "-n", "x"}}
	want := []string{"/bin/docker", "run", "--rm", "--init", "-it", "--name", "bitc-tk", // unique per run: from the ticket, not the job
		"--label", "bitc.job=j", "--label", "bitc.pool=p", "--label", "bitc.state=abc", "--label", "bitc.ticket=tk",
		"--cidfile", "/s/tk.cid", "--env-file", "/s/tk.env", "--mount", "type=bind,source=/w,target=/w", "--mount", "type=bind,source=/a,target=/b,readonly",
		"--workdir", "/w/sub", "--cpus", "1.5", "--memory", "8388608", "img:1", "echo", "-n", "x"}
	if got := RunArgv(s); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q\nwant %q", got, want)
	}
	s.TTY, s.CPUs, s.Memory, s.EnvFile, s.Mounts = false, "", 0, "", nil
	got := RunArgv(s)
	for _, a := range []string{"-it", "--cpus", "--memory", "--env-file", "-e", "--mount"} {
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
	id := f.plant("here", nil)
	if err := Remove(context.Background(), f.path, id, f.env); err != nil || f.exists("here") {
		t.Fatalf("%v", err)
	}
	if err := Remove(context.Background(), f.path, id, f.env); err != nil { // already gone
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
	if by := f.removedBy(); len(by) != 2 || by[0] == "bitc-dead" || by[1] == "bitc-none" || !strings.HasPrefix(by[0], "cid") || !strings.HasPrefix(by[1], "cid") {
		t.Fatalf("a container was removed by name, not by the id the listing gave: %v", by)
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

// A sweep error aborts the sweep and deletes nothing: neither an unreadable pools/ dir (nothing
// says which tickets are held, #24) nor a docker call that fails halfway lets it go on removing.
// No pools dir at all is not an error: there are simply no tickets.
func TestSweepAbortsOnAnErrorAndDeletesNothing(t *testing.T) {
	f := newFake(t)
	l, err := state.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(l.Root, "pools"), []byte("not a dir"), 0o600); err != nil { // ReadDir: ENOTDIR
		t.Fatal(err)
	}
	const id = "0-0000000000000000001-aaaaaaaa"
	if err := os.MkdirAll(l.ContainersDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	cid, env := filepath.Join(l.ContainersDir(), id+".cid"), filepath.Join(l.ContainersDir(), id+".env")
	for _, p := range []string{cid, env} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	f.plant("bitc-a", map[string]string{LabelPool: "p", LabelState: StateHash(l.Root), LabelTicket: id, LabelJob: "j"})
	var removed, kept []string
	rec := func(into *[]string) func(Found) { return func(x Found) { *into = append(*into, x.Name) } }
	err = Sweep(context.Background(), f.path, l, f.env, false, rec(&removed), rec(&kept))
	if err == nil || len(removed) != 0 || len(kept) != 0 || !f.exists("bitc-a") || len(f.removedBy()) != 0 {
		t.Fatalf("err %v removed %v kept %v", err, removed, kept)
	}
	for _, p := range []string{cid, env} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("an aborted sweep deleted %s", p)
		}
	}
	// no pools dir: no tickets, so the container is an orphan and goes, with the dead run's files
	if err := os.Remove(filepath.Join(l.Root, "pools")); err != nil {
		t.Fatal(err)
	}
	// ... unless the docker call fails: that aborts too, after nothing was removed
	f.plant("bitc-b", map[string]string{LabelPool: "p", LabelState: StateHash(l.Root), LabelTicket: "0-0000000000000000002-bbbbbbbb", LabelJob: "j"})
	err = Sweep(context.Background(), f.path, l, append(f.env, "FAKE_DOCKER_FAIL=rm"), false, rec(&removed), rec(&kept))
	if err == nil || len(removed) != 0 || !f.exists("bitc-a") || !f.exists("bitc-b") {
		t.Fatalf("err %v removed %v", err, removed)
	}
	for _, p := range []string{cid, env} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("a sweep that aborted deleted %s", p)
		}
	}
	if err := Sweep(context.Background(), f.path, l, f.env, false, rec(&removed), rec(&kept)); err != nil || len(removed) != 2 || f.exists("bitc-a") {
		t.Fatalf("err %v removed %v", err, removed)
	}
	for _, p := range []string{cid, env} {
		if _, err := os.Stat(p); err == nil {
			t.Fatalf("the dead run's %s is still there", p)
		}
	}
}

func TestWriteEnvFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "t.env")
	if err := os.WriteFile(path, []byte("stale"), 0o644); err != nil { // a leftover of the same name is replaced, and ends 0600
		t.Fatal(err)
	}
	if err := WriteEnvFile(path, []string{"A=1", "B=two words", "C="}); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	b, _ := os.ReadFile(path)
	if err != nil || st.Mode().Perm() != 0o600 || string(b) != "A=1\nB=two words\nC=\n" {
		t.Fatalf("%v %v %q", err, st.Mode(), b)
	}
	for _, bad := range []string{"A=x\ny", "A=x\x00y", "NOEQUALS", "=v"} {
		if err := WriteEnvFile(filepath.Join(dir, "bad.env"), []string{bad}); err == nil {
			t.Errorf("%q was accepted", bad)
		}
		if _, err := os.Stat(filepath.Join(dir, "bad.env")); err == nil {
			t.Errorf("%q left a file", bad)
		}
	}
	if err := WriteEnvFile(filepath.Join(dir, "no", "dir.env"), []string{"A=1"}); err == nil {
		t.Error("an unwritable path was accepted")
	}
	if got := ReadID(filepath.Join(dir, "none.cid")); got != "" {
		t.Fatalf("%q", got)
	}
	if err := os.WriteFile(filepath.Join(dir, "x.cid"), []byte("abc123\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := ReadID(filepath.Join(dir, "x.cid")); got != "abc123" {
		t.Fatalf("%q", got)
	}
}

func TestGuardMainRemovesByTheCidfileOnEOFAndNotAfterDone(t *testing.T) {
	f := newFake(t)
	t.Setenv("PATH", f.env[0][len("PATH="):])
	t.Setenv("FAKE_DOCKER_DIR", f.state)
	dir := t.TempDir()
	cid, env := filepath.Join(dir, "t.cid"), filepath.Join(dir, "t.env")
	id := f.plant("bitc-g", nil)
	f.plant("bitc-other", nil) // another run's: never touched
	for p, body := range map[string]string{cid: id, env: "A=1\n"} {
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.WriteString("noise\n" + doneLine); err != nil {
		t.Fatal(err)
	}
	if code := GuardMain([]string{f.path, cid, env}, r); code != 0 || !f.exists("bitc-g") {
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
	if code := GuardMain([]string{f.path, cid, env}, r); code != 0 || f.exists("bitc-g") || !f.exists("bitc-other") {
		t.Fatalf("EOF without done left the container, or removed another (code %d)", code)
	}
	if by := f.removedBy(); len(by) != 1 || by[0] != id {
		t.Fatalf("removed by %v, want the id %s from the cidfile", by, id)
	}
	for _, p := range []string{cid, env} {
		if _, err := os.Stat(p); err == nil {
			t.Fatalf("the guard left %s", p)
		}
	}
	r.Close()
	// no cidfile (docker never created the container): nothing of docker's is removed, the env file is
	if err := os.WriteFile(env, []byte("A=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r, w, _ = os.Pipe()
	w.Close()
	if code := GuardMain([]string{f.path, cid, env}, r); code != 0 || !f.exists("bitc-other") || len(f.removedBy()) != 1 {
		t.Fatalf("a guard with no cidfile removed a container (code %d)", code)
	}
	if _, err := os.Stat(env); err == nil {
		t.Fatal("the guard left the env file")
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
	os.Exit(GuardMain(args[len(args)-3:], os.NewFile(LifelineFD, "lifeline")))
}

func TestStartGuard(t *testing.T) {
	f := newFake(t)
	env := append(append([]string{}, f.env...), "BITC_TEST_GUARD_HELPER=1")
	exe, args := os.Args[0], []string{"-test.run=^TestGuardHelperProcess$"}
	dir := t.TempDir()
	cidOf := func(name, id string) (cid, envf string) {
		cid, envf = filepath.Join(dir, name+".cid"), filepath.Join(dir, name+".env")
		if err := os.WriteFile(cid, []byte(id), 0o600); err != nil {
			t.Fatal(err)
		}
		return cid, envf
	}

	cid, envf := cidOf("done", f.plant("bitc-done", nil))
	g, err := StartGuard(exe, args, f.path, cid, envf, env)
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
	cid, envf = cidOf("died", f.plant("bitc-died", nil))
	g, err = StartGuard(exe, args, f.path, cid, envf, env)
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
	if _, err := StartGuard("/nonexistent/bitc", args, f.path, cid, envf, env); err == nil {
		t.Fatal("a guard that cannot start is an error")
	}
}
