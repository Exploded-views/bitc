package remote

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Exploded-views/bitc/internal/config"
	"github.com/Exploded-views/bitc/internal/exit"
	"github.com/Exploded-views/bitc/internal/provider"
	"github.com/Exploded-views/bitc/internal/remote/gitref"
	"github.com/Exploded-views/bitc/internal/state"
)

// sweepNFSEnv registers a provider whose resources are res and returns the effective config, the
// accounting dir and the stub that records teardowns. Each resource is owner-tagged.
func sweepNFSEnv(t *testing.T, mk func(install string, now time.Time) []provider.Resource) (*config.Effective, string, *sweepStub) {
	return sweepNFSEnvWith(t, mk, false)
}

// goneStub is a one-resource-per-job provider: an own resource with no reservation older than
// GoneAfter is deleted as "job process gone" on a local home.
type goneStub struct{ *sweepStub }

func (goneStub) SweepRules() provider.SweepPolicy {
	return provider.SweepPolicy{Noun: "sprite", GoneAfter: time.Minute}
}

func sweepNFSEnvWith(t *testing.T, mk func(install string, now time.Time) []provider.Resource, rules bool) (*config.Effective, string, *sweepStub) {
	t.Helper()
	acct := filepath.Join(t.TempDir(), "acct")
	secret, err := state.InstallSecret(acct)
	if err != nil {
		t.Fatal(err)
	}
	install := state.Install16(secret)
	res := mk(install, time.Now())
	owner := map[string]string{}
	for _, r := range res {
		owner[r.ID] = provider.OwnerTag(secret, r.Name)
	}
	sp := &sweepStub{res: res, owner: owner}
	provider.Register("sweepnfs", provider.Factory{
		New: func(*config.Provider, *config.Effective) (provider.Provider, error) {
			if rules {
				return goneStub{sp}, nil
			}
			return sp, nil
		},
	})
	e := &config.Effective{Test: &config.Test{AccountingDir: acct}, Worktree: t.TempDir(),
		Providers: map[string]*config.Provider{"sweepnfs": {APIKey: "k", NamePrefix: "bitc"}}}
	return e, acct, sp
}

func resources(install string, now time.Time) []provider.Resource {
	return []provider.Resource{
		{ID: "expired", Name: "bitc-" + install + "-a", Install: install, Created: now.Add(-3 * time.Hour), Expires: now.Add(-time.Hour), USDPerHour: 500000},
		{ID: "live", Name: "bitc-" + install + "-b", Install: install, Created: now.Add(-10 * time.Minute), Expires: now.Add(time.Hour), USDPerHour: 1500000},
	}
}

// (a) and (b): on a network-filesystem home flock cannot be trusted, so sweep tears down only what
// is past its own expiry, lists the rest with its age and cost, writes nothing, and exits 78.
func TestSweepOnANetworkHomeTearsDownOnlyWhatIsProvablyExpired(t *testing.T) {
	e, acct, sp := sweepNFSEnv(t, resources)
	defer state.StubNetworkFS(acct)()
	var out bytes.Buffer
	err := Sweep(context.Background(), e, false, "", false, &out)
	if exit.Code(err) != exit.Config {
		t.Fatalf("err %v (code %d)", err, exit.Code(err))
	}
	if strings.Join(sp.torn, ",") != "expired" {
		t.Fatalf("torn %v", sp.torn)
	}
	for _, want := range []string{"swept resource expired (", "listed sweepnfs live ", "$1.50/h"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("stdout lacks %q:\n%s", want, out.String())
		}
	}
	for _, want := range []string{"is on NFS", "tore down 1", "left 1"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q: %v", want, err)
		}
	}
	if _, ok := LastSweep(filepath.Join(acct, "remote", "sweepnfs")); ok {
		t.Error("the sweep stamp was written")
	}
}

// With nothing live, a network home still exits 78: it skipped its writes.
func TestSweepOnANetworkHomeExits78EvenWhenNothingIsLeft(t *testing.T) {
	e, acct, sp := sweepNFSEnv(t, func(install string, now time.Time) []provider.Resource { return resources(install, now)[:1] })
	defer state.StubNetworkFS(acct)()
	err := Sweep(context.Background(), e, false, "", false, new(bytes.Buffer))
	if exit.Code(err) != exit.Config || strings.Join(sp.torn, ",") != "expired" || !strings.Contains(err.Error(), "left 0") {
		t.Fatalf("%v %v", err, sp.torn)
	}
}

// (c) a local home is unchanged: the same resources, nil error, the stamp written.
func TestSweepOnALocalHomeIsUnchanged(t *testing.T) {
	e, acct, sp := sweepNFSEnv(t, resources)
	var out bytes.Buffer
	if err := Sweep(context.Background(), e, false, "", false, &out); err != nil {
		t.Fatal(err)
	}
	if strings.Join(sp.torn, ",") != "expired" || strings.Contains(out.String(), "listed") {
		t.Fatalf("torn %v out %q", sp.torn, out.String())
	}
	if _, err := os.Stat(filepath.Join(acct, "remote", "sweepnfs", "last-sweep")); err != nil {
		t.Fatal(err)
	}
}

// hookCalls replaces the rails hooks (set by internal/remote/rails, which this package cannot
// import) with recorders and returns the names they were called with.
func hookCalls(t *testing.T) *[]string {
	t.Helper()
	var calls []string
	op, on, dd, lr := PruneReservations, OrphanNames, DropOrphan, LiveReservations
	t.Cleanup(func() { PruneReservations, OrphanNames, DropOrphan, LiveReservations = op, on, dd, lr })
	PruneReservations = func(string, bool) []string { calls = append(calls, "PruneReservations"); return nil }
	OrphanNames = func(string) map[string]bool { calls = append(calls, "OrphanNames"); return nil }
	DropOrphan = func(string, string) { calls = append(calls, "DropOrphan") }
	LiveReservations = func(string) map[string]bool { calls = append(calls, "LiveReservations"); return nil }
	return &calls
}

// tree is every path under dir with its mode and content, for a before/after comparison.
func tree(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, _ := d.Info()
		b, _ := os.ReadFile(p)
		out[p] = info.Mode().String() + " " + string(b)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func goneResources(install string, now time.Time) []provider.Resource {
	return []provider.Resource{{ID: "orphan", Name: "bitc-" + install + "-c", Install: install,
		Created: now.Add(-10 * time.Minute), Expires: now.Add(time.Hour), USDPerHour: 1500000}}
}

// The "job process gone" rule rests on reservations, which cannot be trusted on a network home:
// there a live-looking resource older than GoneAfter is listed, never torn down, and no rails hook
// runs and nothing in the accounting dir changes.
func TestSweepOnANetworkHomeNeverAppliesTheJobProcessGoneRule(t *testing.T) {
	e, acct, sp := sweepNFSEnvWith(t, goneResources, true)
	calls := hookCalls(t)
	defer state.StubNetworkFS(acct)()
	before := tree(t, acct)
	var out bytes.Buffer
	err := Sweep(context.Background(), e, false, "", false, &out)
	if exit.Code(err) != exit.Config || len(sp.torn) != 0 || !strings.Contains(out.String(), "listed sweepnfs orphan ") {
		t.Fatalf("err %v torn %v out %q", err, sp.torn, out.String())
	}
	if len(*calls) != 0 {
		t.Errorf("rails hooks ran on a network home: %v", *calls)
	}
	after := tree(t, acct)
	if len(before) != len(after) {
		t.Errorf("the account dir changed: %v -> %v", before, after)
	}
	for p, v := range before {
		if after[p] != v {
			t.Errorf("%s changed", p)
		}
	}
}

// On a local home the same resource is job-process-gone and torn down, and the hooks run.
func TestSweepOnALocalHomeAppliesTheJobProcessGoneRule(t *testing.T) {
	e, _, sp := sweepNFSEnvWith(t, goneResources, true)
	calls := hookCalls(t)
	var out bytes.Buffer
	if err := Sweep(context.Background(), e, false, "", false, &out); err != nil {
		t.Fatal(err)
	}
	if strings.Join(sp.torn, ",") != "orphan" || !strings.Contains(out.String(), "(job process gone)") {
		t.Fatalf("torn %v out %q", sp.torn, out.String())
	}
	if strings.Join(*calls, ",") != "PruneReservations,LiveReservations,OrphanNames" {
		t.Fatalf("hooks %v", *calls)
	}
}

// A network home whose install secret is missing or unreadable has nothing provably its own: sweep
// tears down nothing, writes nothing (no secret, no dir) and still exits 78.
func TestSweepOnANetworkHomeWithoutAUsableSecretExits78AndWritesNothing(t *testing.T) {
	for name, setup := range map[string]func(acct string){
		"no dir":    func(string) {},
		"no secret": func(acct string) { _ = os.MkdirAll(acct, 0o700) },
		"unreadable": func(acct string) {
			_ = os.MkdirAll(acct, 0o700)
			_ = os.WriteFile(filepath.Join(acct, "install-secret"), []byte("short"), 0o600)
		},
	} {
		t.Run(name, func(t *testing.T) {
			e, _, sp := sweepNFSEnv(t, resources)
			root := t.TempDir()
			acct := filepath.Join(root, "home", "acct")
			e.Test.AccountingDir = acct
			setup(acct)
			defer state.StubNetworkFS(root)() // a missing dir is judged by its nearest existing parent
			before := map[string]string{}
			if _, err := os.Stat(acct); err == nil {
				before = tree(t, acct)
			}
			err := Sweep(context.Background(), e, false, "", false, new(bytes.Buffer))
			if exit.Code(err) != exit.Config || len(sp.torn) != 0 {
				t.Fatalf("err %v torn %v", err, sp.torn)
			}
			if _, statErr := os.Stat(acct); statErr != nil && len(before) != 0 {
				t.Fatal("the dir vanished")
			}
			after := map[string]string{}
			if _, statErr := os.Stat(acct); statErr == nil {
				after = tree(t, acct)
			}
			if len(before) != len(after) {
				t.Fatalf("writes: %v -> %v", before, after)
			}
		})
	}
}

// refEnv gives e a worktree whose "origin" holds one of this install's agent refs, older than 3h.
func refEnv(t *testing.T, e *config.Effective, acct string) (ref string, remote string) {
	t.Helper()
	secret, err := state.ReadInstallSecret(acct)
	if err != nil {
		t.Fatal(err)
	}
	install := state.Install16(secret)
	remote = filepath.Join(t.TempDir(), "origin.git")
	git := func(dir string, args ...string) string {
		t.Helper()
		c := exec.Command("git", args...)
		c.Dir = dir
		c.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		b, err := c.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, b)
		}
		return string(b)
	}
	git(t.TempDir(), "init", "-q", "--bare", remote)
	git(e.Worktree, "init", "-q")
	git(e.Worktree, "commit", "-q", "--allow-empty", "-m", "x")
	git(e.Worktree, "remote", "add", "origin", remote)
	ref = gitref.AgentRefPrefix + install + "/job-" + strconv.FormatInt(time.Now().Add(-4*time.Hour).Unix(), 10) + "-x"
	git(e.Worktree, "push", "-q", "origin", "HEAD:"+ref)
	e.Providers["sweepnfs"].Transport.Remote = "origin"
	return ref, remote
}

func remoteHasRef(t *testing.T, remote, ref string) bool {
	t.Helper()
	b, err := exec.Command("git", "-C", remote, "for-each-ref", ref).Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(b)) != ""
}

// The git ref sweep deletes refs on a remote by age; on a network home it is skipped like every
// other write, since the install is shared with hosts whose jobs run longer (Fixes #62).
func TestSweepOnANetworkHomeLeavesOldJobRefsAlone(t *testing.T) {
	e, acct, _ := sweepNFSEnv(t, resources)
	ref, remote := refEnv(t, e, acct)
	defer state.StubNetworkFS(acct)()
	err := Sweep(context.Background(), e, false, "", false, new(bytes.Buffer))
	if exit.Code(err) != exit.Config || !strings.Contains(err.Error(), "git ref sweep") {
		t.Fatalf("%v", err)
	}
	if !remoteHasRef(t, remote, ref) {
		t.Fatal("the old job ref was deleted on a network home")
	}
}

func TestSweepOnALocalHomeDeletesOldJobRefs(t *testing.T) {
	e, acct, _ := sweepNFSEnv(t, resources)
	ref, remote := refEnv(t, e, acct)
	var out bytes.Buffer
	if err := Sweep(context.Background(), e, false, "", false, &out); err != nil {
		t.Fatal(err)
	}
	if remoteHasRef(t, remote, ref) || !strings.Contains(out.String(), "swept ref "+ref) {
		t.Fatalf("out %q", out.String())
	}
}

// secretStub can verify an owner tag only with the install secret it is handed (as sprites' can,
// where it cannot read the account dir itself on a network home).
type secretStub struct {
	*sweepStub
	secret []byte
}

func (s *secretStub) UseInstallSecret(b []byte) { s.secret = b }
func (s *secretStub) Owner(ctx context.Context, id string) (string, error) {
	if s.secret == nil {
		return "", errors.New("accounting dir is on NFS")
	}
	return s.sweepStub.Owner(ctx, id)
}

// Sweep hands the install secret it read to a provider that wants it, so an expired, verified
// resource is torn down on a network home too.
func TestSweepOnANetworkHomeGivesProvidersTheInstallSecret(t *testing.T) {
	e, acct, sp := sweepNFSEnv(t, resources)
	st := &secretStub{sweepStub: sp}
	provider.Register("sweepnfs", provider.Factory{
		New: func(*config.Provider, *config.Effective) (provider.Provider, error) { return st, nil },
	})
	defer state.StubNetworkFS(acct)()
	err := Sweep(context.Background(), e, false, "", false, new(bytes.Buffer))
	if exit.Code(err) != exit.Config || strings.Join(sp.torn, ",") != "expired" {
		t.Fatalf("err %v torn %v", err, sp.torn)
	}
}

// An Owner error is shown, not dropped.
func TestSweepPrintsAnOwnerError(t *testing.T) {
	now := time.Now()
	sp := &secretStub{sweepStub: &sweepStub{res: []provider.Resource{{ID: "x", Name: "bitc-0123456789abcdef-a", Install: "0123456789abcdef", Expires: now.Add(-time.Hour)}}}}
	var out, errb bytes.Buffer
	sweepProvider(context.Background(), sweepInput{prov: sp, typ: "stub", prefix: "bitc-", install: "0123456789abcdef", now: now, out: &out, errw: &errb})
	if !strings.Contains(errb.String(), "owner check of x: accounting dir is on NFS") {
		t.Fatalf("%q", errb.String())
	}
}

type failTeardown struct{ *sweepStub }

func (failTeardown) Teardown(context.Context, string) error { return errors.New("api down") }

// The 78 message counts each kind of leftover separately, and each gets a line with its age and cost.
func TestSweepOnANetworkHomeCountsListedKeptAndFailedSeparately(t *testing.T) {
	mk := func(install string, now time.Time) []provider.Resource {
		return []provider.Resource{
			{ID: "live", Name: "bitc-" + install + "-a", Install: install, Created: now.Add(-10 * time.Minute), Expires: now.Add(time.Hour), USDPerHour: 1500000},
			{ID: "kept", Name: "bitc-" + install + "-b", Install: install, Created: now.Add(-3 * time.Hour), Expires: now.Add(-time.Hour), USDPerHour: 500000},
			{ID: "fails", Name: "bitc-" + install + "-c", Install: install, Created: now.Add(-3 * time.Hour), Expires: now.Add(-time.Hour), USDPerHour: 250000},
			{ID: "nodate", Name: "bitc-" + install + "-d", Install: install, Expires: now.Add(time.Hour)},
		}
	}
	e, acct, sp := sweepNFSEnv(t, mk)
	sp.owner["kept"] = "wrong"
	provider.Register("sweepnfs", provider.Factory{
		New: func(*config.Provider, *config.Effective) (provider.Provider, error) { return failTeardown{sp}, nil },
	})
	defer state.StubNetworkFS(acct)()
	var out bytes.Buffer
	var err error
	stderr := captureStderr(t, func() { err = Sweep(context.Background(), e, false, "", false, &out) })
	if exit.Code(err) != exit.Config {
		t.Fatalf("%v", err)
	}
	for _, want := range []string{"could not tear down fails: api down (age 3h0m0s cost $0.25/h)", "kept has no verified owner tag; kept (age 3h0m0s cost $0.50/h)"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr lacks %q:\n%s", want, stderr)
		}
	}
	for _, want := range []string{"tore down 0", "2 listed", "1 kept", "1 failed"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message lacks %q: %v", want, err)
		}
	}
	for _, want := range []string{"listed sweepnfs live ", "age 10m0s cost $1.50/h", "listed sweepnfs nodate ", "age unknown cost $0.00/h"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("stdout lacks %q:\n%s", want, out.String())
		}
	}
}

// An existing network-home account dir that fails its owner/mode check still ends in the 78, with
// nothing torn down and nothing written.
func TestSweepOnANetworkHomeWithAnUnusableDirExits78(t *testing.T) {
	e, acct, sp := sweepNFSEnv(t, resources)
	if err := os.Chmod(acct, 0o750); err != nil {
		t.Fatal(err)
	}
	defer state.StubNetworkFS(acct)()
	before := tree(t, acct)
	err := Sweep(context.Background(), e, false, "", false, new(bytes.Buffer))
	if exit.Code(err) != exit.Config || len(sp.torn) != 0 {
		t.Fatalf("err %v torn %v", err, sp.torn)
	}
	for p, v := range tree(t, acct) {
		if before[p] != v {
			t.Errorf("%s changed", p)
		}
	}
}

// --dry-run on a network home counts what it would have torn down.
func TestSweepDryRunOnANetworkHomeCountsWouldTearDown(t *testing.T) {
	e, acct, sp := sweepNFSEnv(t, resources)
	defer state.StubNetworkFS(acct)()
	err := Sweep(context.Background(), e, true, "", false, new(bytes.Buffer))
	if exit.Code(err) != exit.Config || len(sp.torn) != 0 || !strings.Contains(err.Error(), "would tear down 1") {
		t.Fatalf("err %v torn %v", err, sp.torn)
	}
}

type countOwner struct {
	*sweepStub
	calls int
}

func (c *countOwner) Owner(ctx context.Context, id string) (string, error) {
	c.calls++
	return "", errors.New("owner api down")
}

// A resource's owner is asked for once, however many branches want it.
func TestSweepAsksForAnOwnerOncePerResource(t *testing.T) {
	now := time.Now()
	install := "0123456789abcdef"
	r := provider.Resource{ID: "x", Name: "bitc-" + install + "-a", Install: install, Expires: now.Add(-time.Hour)}
	c := &countOwner{sweepStub: &sweepStub{res: []provider.Resource{r}}}
	defer func(f func(string) map[string]bool) { OrphanNames = f }(OrphanNames)
	OrphanNames = func(string) map[string]bool { return map[string]bool{r.Name: true} }
	var out, errb bytes.Buffer
	sweepProvider(context.Background(), sweepInput{prov: c, typ: "stub", dir: t.TempDir(), prefix: "bitc-", install: install, now: now, out: &out, errw: &errb})
	if c.calls != 1 || strings.Count(errb.String(), "owner check of x") != 1 {
		t.Fatalf("calls %d: %q", c.calls, errb.String())
	}
}

// captureStderr returns what f wrote to os.Stderr.
func captureStderr(t *testing.T, f func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = w
	// restore stderr even when f fails the test or panics
	t.Cleanup(func() { os.Stderr = old; w.Close() })
	done := make(chan string)
	go func() { b, _ := io.ReadAll(r); done <- string(b) }()
	f()
	os.Stderr = old
	w.Close()
	return <-done
}
