package run

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/Exploded-views/bitc/conformance/harness"
	"github.com/Exploded-views/bitc/internal/admit"
	"github.com/Exploded-views/bitc/internal/config"
	"github.com/Exploded-views/bitc/internal/container"
	"github.com/Exploded-views/bitc/internal/exit"
	"github.com/Exploded-views/bitc/internal/state"
)

func TestMain(m *testing.M) {
	code := m.Run()
	harness.CleanupFakeDocker()
	os.Exit(code)
}

// fakeDocker installs the fake docker in a new dir and returns its path, the env a call needs
// and the dir its containers are recorded in.
func fakeDocker(t *testing.T) (path string, env []string, stateDir string) {
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
	path = filepath.Join(dir, "docker")
	if err := os.WriteFile(path, b, 0o755); err != nil {
		t.Fatal(err)
	}
	stateDir = filepath.Join(dir, "state")
	return path, []string{"PATH=" + dir + string(os.PathListSeparator) + os.Getenv("PATH"), "FAKE_DOCKER_DIR=" + stateDir}, stateDir
}

// plantContainer adds a container the fake knows and returns its id.
func plantContainer(t *testing.T, stateDir, name string, labels map[string]string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(stateDir, "c"), 0o755); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("planted " + name))
	id := hex.EncodeToString(sum[:]) // 64 hex digits, as docker's
	b, _ := json.Marshal(map[string]any{"id": id, "name": name, "labels": labels})
	if err := os.WriteFile(filepath.Join(stateDir, "c", name+".json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	return id
}

// removedBy is what each `docker rm` that removed a container was given.
func removedBy(stateDir string) []string {
	b, _ := os.ReadFile(filepath.Join(stateDir, "removed.log"))
	var out []string
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var m map[string]string
		if json.Unmarshal([]byte(l), &m) == nil {
			out = append(out, m["arg"])
		}
	}
	return out
}

func containerExists(stateDir, name string) bool {
	_, err := os.Stat(filepath.Join(stateDir, "c", name+".json"))
	return err == nil
}

func TestContainerEnvIsMinimal(t *testing.T) {
	tpl := func(s string) config.Template { return config.Template{Raw: s} }
	in := EnvInput{
		Caller: []string{"HOME=/h", "TERM=xterm", "SECRET=s", "TZ=UTC", "LANG=C", "BITC_HOLD=x", "MODE=slow", "UNSET_ME=1"},
		Eff:    &config.Effective{Host: config.Host{StateDir: "/state"}},
		Pool: &config.Pool{Name: "box", DefaultsEnvUnset: []string{"UNSET_ME"}, EnvDefault: map[string]config.Template{"MODE": tpl("fast"), "TERM": tpl("dumb")},
			Env: map[string]config.Template{"GREETING": tpl("hi {job}")}, EnvUnset: []string{"TZ"}},
		Hold: &admit.Hold{Pool: "box", Slot: 2, Ticket: "t", Token: "tok", ExtraEnv: []string{"LEGACY_HOLD=1"}},
		Job:  "j1", Vars: config.Vars{"job": "j1"},
	}
	got, err := containerEnv(in, []KV{{Name: "FROM_HOOK", Value: "1"}})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"BITC_JOB=j1", "BITC_POOL=box", "BITC_SLOT=box.2", "FROM_HOOK=1", "GREETING=hi j1", "LANG=C", "MODE=fast", "TERM=xterm"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q\nwant %q", got, want)
	}
	in.Pool.Env = map[string]config.Template{"BAD": tpl("{nope}")}
	if _, err := containerEnv(in, nil); exit.Code(err) != exit.Config {
		t.Fatalf("a bad template is a config error: %v", err)
	}
}

func TestWorkTreeMount(t *testing.T) {
	for _, c := range []struct{ wt, cwd, want string }{
		{"/w", "/w", "/w"},
		{"/w", "/w/sub/dir", "/w"},
		{"/w", "/w2", "/w2"}, // a sibling with the same prefix is outside the tree
		{"/w", "/elsewhere", "/elsewhere"},
		{"", "/c", "/c"},
		{"/w/", "/w/x", "/w/"},
	} {
		got := workTreeMount(&config.Effective{Worktree: c.wt, Cwd: c.cwd})
		if got.Source != c.want || got.Target != c.want || got.ReadOnly {
			t.Errorf("%+v: %+v", c, got)
		}
	}
}

func TestContainerRuntimeMissingIs69(t *testing.T) {
	pool := &config.Pool{Name: "box", Container: &config.Container{Runtime: "docker", Image: "i"}}
	_, err := containerRuntime(pool, []string{"PATH=" + t.TempDir()})
	if exit.Code(err) != exit.Unavailable || !strings.Contains(exit.As(err).Line, `container runtime "docker" not found on PATH`) {
		t.Fatalf("%v", err)
	}
	path, env, _ := fakeDocker(t)
	if got, err := containerRuntime(pool, env); err != nil || got != path {
		t.Fatalf("%q %v", got, err)
	}
}

// TestGuardHelperProcess is the guard process of the lifecycle test: the test binary re-executed.
func TestGuardHelperProcess(t *testing.T) {
	if os.Getenv("BITC_TEST_GUARD_HELPER") != "1" {
		t.Skip("helper process")
	}
	args := os.Args
	os.Exit(container.GuardMain(args[len(args)-2:], os.NewFile(container.LifelineFD, "lifeline")))
}

func preparedForContainer(t *testing.T, stateRoot string) (*Prepared, *admit.Hold, EnvInput) {
	t.Helper()
	wt := t.TempDir()
	pool := &config.Pool{Name: "box", Container: &config.Container{Runtime: "docker", Image: "img:1", CPUs: "2", Memory: 64 << 20,
		Mounts: []config.Mount{{Source: "/data", Target: "/data", ReadOnly: true}}}}
	eff := &config.Effective{Worktree: wt, Cwd: wt, Host: config.Host{StateDir: stateRoot}}
	p := &Prepared{Args: RunArgs{Argv: []string{"echo", "hi"}}, Eff: eff, Pool: pool, Layout: state.Layout{Root: stateRoot}, Job: "j1"}
	hold := &admit.Hold{Pool: "box", Slot: 1, Ticket: "0-0000000000000000001-aaaaaaaa.json", Token: "tok"}
	return p, hold, EnvInput{Eff: eff, Pool: pool, Hold: hold, Job: "j1", Vars: config.Vars{}}
}

func TestContainerRunLifecycle(t *testing.T) {
	path, env, fstate := fakeDocker(t)
	p, hold, in := preparedForContainer(t, t.TempDir())
	genv := append(append([]string{}, env...), "BITC_TEST_GUARD_HELPER=1")
	orig := guardCommand
	t.Cleanup(func() { guardCommand = orig })
	guardCommand = func() (string, []string, error) {
		return os.Args[0], []string{"-test.run=^TestGuardHelperProcess$"}, nil
	}
	var stderr bytes.Buffer
	cr, argv, err := newContainerRun(Invocation{}, p, hold, path, in, nil, genv, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if cr.guard == nil || stderr.Len() != 0 {
		t.Fatalf("guard %v stderr %q", cr.guard, stderr.String())
	}
	wt := p.Eff.Worktree
	const own = "bitc-0-0000000000000000001-aaaaaaaa" // unique per run: the ticket, not the job
	want := []string{path, "run", "--rm", "--init", "-i", "--name", own,
		"--label", "bitc.job=j1", "--label", "bitc.pool=box", "--label", "bitc.state=" + container.StateHash(p.Layout.Root), "--label", "bitc.ticket=0-0000000000000000001-aaaaaaaa",
		"--cidfile", filepath.Join(p.Layout.ContainersDir(), "0-0000000000000000001-aaaaaaaa.cid"),
		"--env-file", filepath.Join(p.Layout.ContainersDir(), "0-0000000000000000001-aaaaaaaa.env"),
		"--mount", "type=bind,source=" + wt + ",target=" + wt, "--mount", "type=bind,source=/data,target=/data,readonly",
		"--workdir", wt, "--cpus", "2", "--memory", "67108864",
		"img:1", "echo", "hi"}
	if !reflect.DeepEqual(argv, want) {
		t.Fatalf("got %q\nwant %q", argv, want)
	}
	// the values are in a 0600 file next to the cidfile, not on argv
	envPath := cr.spec.EnvFile
	st, serr := os.Stat(envPath)
	b, _ := os.ReadFile(envPath)
	if serr != nil || st.Mode().Perm() != 0o600 || string(b) != "BITC_JOB=j1\nBITC_POOL=box\nBITC_SLOT=box.1\n" {
		t.Fatalf("env file: %v %v %q", serr, st, b)
	}
	// docker has not written the cidfile: no container of this run exists, so nothing is removed,
	// whatever another run (the same job name) or an old-scheme name owns; the env file still goes
	other := plantContainer(t, fstate, "bitc-j1", map[string]string{"bitc.job": "j1"})
	cr.kill() // Exec's SIGKILL step
	if created := cr.finish(); created || !containerExists(fstate, "bitc-j1") || len(removedBy(fstate)) != 0 {
		t.Fatalf("created %v, removals %v", created, removedBy(fstate))
	}
	if _, err := os.Stat(envPath); err == nil {
		t.Fatal("finish left the env file")
	}
	cr.finish() // idempotent

	// docker wrote it: the container is removed by that id, and by it only
	cr, _, err = newContainerRun(Invocation{}, p, hold, path, in, nil, genv, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	mine := plantContainer(t, fstate, own, nil)
	if err := os.WriteFile(cr.spec.CID, []byte(mine), 0o600); err != nil {
		t.Fatal(err)
	}
	cr.kill() // by the id in the cidfile, without waiting for the CLI
	if !cr.finish() || containerExists(fstate, own) || !containerExists(fstate, "bitc-j1") {
		t.Fatalf("own container left %v, another's left %v", containerExists(fstate, own), containerExists(fstate, "bitc-j1"))
	}
	if by := removedBy(fstate); len(by) == 0 || slices.Contains(by, other) || slices.Contains(by, own) || by[len(by)-1] != mine {
		t.Fatalf("removed by %v, want only %s", by, mine)
	}
	if _, err := os.Stat(cr.spec.CID); err == nil {
		t.Fatal("finish left the cidfile")
	}
	if _, err := os.Stat(cr.spec.EnvFile); err == nil {
		t.Fatal("finish left the env file")
	}

	// no guard can start: a warning, and the run goes on
	guardCommand = func() (string, []string, error) { return "", nil, errors.New("no self") }
	cr, _, err = newContainerRun(Invocation{}, p, hold, path, in, nil, genv, &stderr)
	if err != nil || cr.guard != nil || !strings.Contains(stderr.String(), "cannot start the container guard (no self)") {
		t.Fatalf("%v %v %q", err, cr.guard, stderr.String())
	}
	cr.finish()

	// refusals: a work tree path docker's --mount cannot take; a bad env template
	p.Eff.Worktree, p.Eff.Cwd = "/a,b", "/a,b"
	if _, _, err := newContainerRun(Invocation{}, p, hold, path, in, nil, genv, &stderr); exit.Code(err) != exit.Config {
		t.Fatalf("%v", err)
	}
	p.Eff.Worktree, p.Eff.Cwd = t.TempDir(), p.Eff.Worktree
	in.Pool = &config.Pool{Name: "box", Env: map[string]config.Template{"X": {Raw: "{nope}"}}}
	if _, _, err := newContainerRun(Invocation{}, p, hold, path, in, nil, genv, &stderr); exit.Code(err) != exit.Config {
		t.Fatalf("%v", err)
	}
}

// Every container variable, a variable docker itself reads included (PATH, DOCKER_HOST), goes in
// the env file and none on argv; a value with a newline cannot be one line of it, so it is refused.
func TestContainerEnvFileHoldsEveryValue(t *testing.T) {
	path, env, _ := fakeDocker(t)
	p, hold, in := preparedForContainer(t, t.TempDir())
	orig := guardCommand
	t.Cleanup(func() { guardCommand = orig })
	guardCommand = func() (string, []string, error) { return "", nil, errors.New("no guard in this test") }
	in.Pool = &config.Pool{Name: "box", Env: map[string]config.Template{"MODE": {Raw: "fast"}, "PATH": {Raw: "/image/bin"}, "DOCKER_HOST": {Raw: "tcp://x"}}}
	host := append(append([]string{}, env...), "MODE=slow", "HOME=/h")
	var stderr bytes.Buffer
	cr, argv, err := newContainerRun(Invocation{}, p, hold, path, in, nil, host, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(argv, "\x00")
	for _, v := range []string{"fast", "/image/bin", "tcp://x", "MODE", "DOCKER_HOST", "slow"} {
		if strings.Contains(joined, v) {
			t.Errorf("%q is on argv: %q", v, argv)
		}
	}
	b, _ := os.ReadFile(cr.spec.EnvFile)
	for _, want := range []string{"MODE=fast\n", "PATH=/image/bin\n", "DOCKER_HOST=tcp://x\n", "BITC_JOB=j1\n"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("env file lacks %q: %q", want, b)
		}
	}
	cr.finish()
	// values docker's env-file parser would refuse or the file cannot hold: a clean 78 naming the
	// variable, never the value, before docker sees anything
	for name, v := range map[string]string{"MULTI": "a\nb", "BAD_UTF8": "ok\xffok", "HUGE": strings.Repeat("x", 64*1024)} {
		in.Pool = &config.Pool{Name: "box", Env: map[string]config.Template{name: {Raw: v}}}
		_, _, err := newContainerRun(Invocation{}, p, hold, path, in, nil, host, &stderr)
		if exit.Code(err) != exit.Config || !strings.Contains(exit.As(err).Line, name) || strings.Contains(exit.As(err).Line, "xxxx") {
			t.Fatalf("%s: %v", name, err)
		}
	}
	// a value just under the limit is fine
	in.Pool = &config.Pool{Name: "box", Env: map[string]config.Template{"BIG": {Raw: strings.Repeat("x", 60*1024)}}}
	cr, _, err = newContainerRun(Invocation{}, p, hold, path, in, nil, host, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	cr.finish()
}

func TestReapOrphans(t *testing.T) {
	path, env, fstate := fakeDocker(t)
	p, _, _ := preparedForContainer(t, t.TempDir())
	hash := container.StateHash(p.Layout.Root)
	plantContainer(t, fstate, "bitc-old", map[string]string{"bitc.pool": "box", "bitc.job": "oldjob", "bitc.state": hash, "bitc.ticket": "0-0000000000000000009-bbbbbbbb"})
	var stderr bytes.Buffer
	var events []state.Event
	reapOrphans(context.Background(), p, path, env, &stderr, func(e state.Event) { events = append(events, e) })
	// the history event is its own kind (not "reaped", which counts dead tickets) and the job is the label's
	if containerExists(fstate, "bitc-old") || stderr.String() != "bitc: removed orphaned container bitc-old (its run is gone)\n" ||
		len(events) != 1 || events[0].Event != "container-reaped" || events[0].Pool != "box" || events[0].Job != "oldjob" {
		t.Fatalf("%q %+v", stderr.String(), events)
	}
	// a daemon that cannot list is not the run's failure: one debug line
	p.Eff.LogLevel = "debug"
	stderr.Reset()
	reapOrphans(context.Background(), p, path, append(env, "FAKE_DOCKER_FAIL=ps"), &stderr, func(state.Event) { t.Fatal("no event expected") })
	if !strings.Contains(stderr.String(), "bitc: debug: box: container sweep aborted:") {
		t.Fatalf("%q", stderr.String())
	}
}
