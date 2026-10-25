package rails

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Exploded-views/bitc/internal/config"
	"github.com/Exploded-views/bitc/internal/lock"
	"github.com/Exploded-views/bitc/internal/proc"
	"github.com/Exploded-views/bitc/internal/proctest"
	"github.com/Exploded-views/bitc/internal/provider"
	"github.com/Exploded-views/bitc/internal/remote"
	"github.com/Exploded-views/bitc/internal/run"
)

func TestCapNote(t *testing.T) {
	got := CapNote(25_000_000, 24_800_000, 0, 380_000)
	if got != "daily cap $25.00: spent $24.80 + committed $0.00 + this job $0.38" {
		t.Fatal(got)
	}
}

func TestEstimateFor95Minutes(t *testing.T) {
	if got := estimate(240_000, 95*60); got != config.Micros(380_000) {
		t.Fatal(got)
	}
}

type stubProvider struct {
	res      []provider.Resource
	spend    config.Micros
	listErr  error
	spendErr error
	owner    map[string]string
	torn     []string
}

func (s *stubProvider) Info() provider.Info {
	return provider.Info{Type: "stub", Caps: provider.Caps{Spend: true}}
}
func (s *stubProvider) List(context.Context, []string) ([]provider.Resource, error) {
	return s.res, s.listErr
}
func (s *stubProvider) Spend(context.Context) (config.Micros, error)       { return s.spend, s.spendErr }
func (s *stubProvider) Owner(_ context.Context, id string) (string, error) { return s.owner[id], nil }
func (s *stubProvider) Teardown(_ context.Context, id string) error {
	s.torn = append(s.torn, id)
	return nil
}
func (s *stubProvider) Preflight(context.Context, *provider.JobSpec) error { return nil }
func (s *stubProvider) Estimate(context.Context, *provider.JobSpec) (config.Micros, error) {
	return 380_000, nil
}
func (s *stubProvider) Launch(context.Context, *provider.JobSpec) (provider.Handle, error) {
	return provider.Handle{}, nil
}
func (s *stubProvider) Watch(context.Context, provider.Handle, provider.Sink) (provider.Result, error) {
	return provider.Result{}, nil
}
func (s *stubProvider) Output(context.Context, provider.Handle, provider.Stream, int64, io.Writer) error {
	return nil
}
func (s *stubProvider) Fetch(context.Context, provider.Handle, provider.ArtifactSpec, io.Writer) error {
	return nil
}

func ownPods(n int, now time.Time) []provider.Resource {
	var out []provider.Resource
	for i := 0; i < n; i++ {
		out = append(out, provider.Resource{ID: fmt.Sprint("p", i), Name: fmt.Sprintf("bitc-%016d-heavy-cpu8-0000000%d-1", 0, i%10),
			USDPerHour: 240_000, Created: now, Expires: now.Add(95 * time.Minute)})
	}
	return out
}

func TestCommittedSum(t *testing.T) {
	now := time.Now()
	core := &config.Provider{DailyUSD: 25_000_000, MaxLive: 20}
	cases := []struct {
		name string
		sp   *stubProvider
		resv []reservation
		want string // "" = launch
	}{
		{"over by spend", &stubProvider{spend: 24_800_000}, nil, "daily cap $25.00: spent $24.80 + committed $0.00 + this job $0.38"},
		{"live pods to expiry", &stubProvider{spend: 21_000_000, res: ownPods(11, now)}, nil, "daily cap $25.00: spent $21.00 + committed $4.18 + this job $0.38"},
		{"reservation counted", &stubProvider{spend: 24_600_000}, []reservation{{Name: "other", Estimate: 380_000}}, "daily cap $25.00: spent $24.60 + committed $0.38 + this job $0.38"},
		{"listed reservation counted once", &stubProvider{spend: 24_000_000, res: ownPods(1, now)}, []reservation{{Name: ownPods(1, now)[0].Name, Estimate: 380_000}}, ""},
		{"list fails", &stubProvider{listErr: errors.New("cannot list live resources (HTTP 503)")}, nil, "cannot list live resources (HTTP 503)"},
		{"spend fails", &stubProvider{spendErr: errors.New("cannot read today's spend (HTTP 500)")}, nil, "cannot read today's spend (HTTP 500)"},
	}
	for _, c := range cases {
		err := checkCap(context.Background(), &capInput{core: core, prov: c.sp, ownPrefix: "bitc-", worstSpan: 125 * time.Minute,
			highest: 480_000, resv: c.resv, thisJob: 380_000, now: now, note: func(string) {}})
		got := ""
		if err != nil {
			got = err.Error()
		}
		if got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
	sp := &stubProvider{res: ownPods(6, now)}
	err := checkCap(context.Background(), &capInput{core: &config.Provider{DailyUSD: 100_000_000, MaxLive: 6}, prov: sp,
		ownPrefix: "bitc-", thisJob: 380_000, now: now, note: func(string) {}})
	if err == nil || err.Error() != "6 live of max_live 6" {
		t.Fatal(err)
	}
}

func TestExpiredOwnResourceTornDownNotCounted(t *testing.T) {
	now := time.Now()
	secret := []byte("0123456789abcdef0123456789abcdef")
	old := provider.Resource{ID: "old", Name: "bitc-0000000000000000-heavy-cpu8-00000000-1", USDPerHour: 240_000,
		Created: now.Add(-3 * time.Hour), Expires: now.Add(-time.Hour)}
	sp := &stubProvider{spend: 24_500_000, res: []provider.Resource{old}, owner: map[string]string{"old": provider.OwnerTag(secret, old.Name)}}
	var notes []string
	err := checkCap(context.Background(), &capInput{core: &config.Provider{DailyUSD: 25_000_000, MaxLive: 6}, prov: sp,
		ownPrefix: "bitc-", secret: secret, thisJob: 380_000, now: now, note: func(s string) { notes = append(notes, s) }})
	if err != nil || len(sp.torn) != 1 || !strings.Contains(notes[0], "tore down old") {
		t.Fatal(err, sp.torn, notes)
	}
}

func TestBreaker(t *testing.T) {
	dir := t.TempDir()
	b := config.Breaker{Failures: 3, Window: time.Hour}
	for i := 0; i < 2; i++ {
		_ = RecordFailure(dir)
	}
	if _, paused := Paused(dir, b, time.Now()); paused {
		t.Fatal("paused after two failures")
	}
	_ = RecordFailure(dir)
	if why, paused := Paused(dir, b, time.Now()); !paused || !strings.Contains(why, "3 failures") {
		t.Fatal(why)
	}
	if _, paused := Paused(dir, b, time.Now().Add(2*time.Hour)); paused {
		t.Fatal("the window did not clear")
	}
	dir2 := t.TempDir()
	_ = Pause(dir2, "HTTP 401", time.Hour)
	if why, paused := Paused(dir2, b, time.Now()); !paused || !strings.Contains(why, "HTTP 401") {
		t.Fatal(why)
	}
}

func TestLedgerToday(t *testing.T) {
	dir := t.TempDir()
	l := Ledger{Dir: dir}
	_ = l.Launch("a", 380_000)
	_ = l.End("a", 3, 380_000, 12_000, "0", true)
	_ = l.Launch("b", 380_000) // still running: listed in L or reserved, so not counted here
	_ = l.Launch("c", 380_000)
	_ = l.End("c", 0, 380_000, 0, "fallback", false) // a capacity fallback created nothing
	if got := LedgerToday(dir, time.Now()); got != 12_000 {
		t.Fatal(got)
	}
}

func TestLedgerLines(t *testing.T) {
	dir := t.TempDir()
	l := Ledger{Dir: dir}
	_ = l.Launch("n", 380_000)
	_ = l.Begin("n")
	_ = l.End("n", 4, 380_000, 16_000, "0", true)
	b, _ := os.ReadFile(filepath.Join(dir, "ledger.jsonl"))
	got := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(got) != 3 {
		t.Fatal(got)
	}
	for i, ev := range []string{`"event":"launch"`, `"event":"begin"`, `"event":"end"`} {
		if !strings.Contains(got[i], ev) || !strings.Contains(got[i], `"name":"n"`) || !strings.Contains(got[i], `"ts":"`) {
			t.Fatal(got[i])
		}
	}
	for _, want := range []string{`"minutes":4`, `"est_usd":0.38`, `"cost_usd":0.016`, `"created":true`, `"exit":"0"`} {
		if !strings.Contains(got[2], want) {
			t.Fatalf("%s lacks %s", got[2], want)
		}
	}
}

func TestPruneReservations(t *testing.T) {
	now := time.Now()
	gone := exec.Command("true")
	_ = gone.Run()
	zombie := exec.Command("true") // started, never reaped until cleanup: state Z
	_ = zombie.Start()
	t.Cleanup(func() { _ = zombie.Wait() })
	for i := 0; i < 200 && !proctest.Gone(zombie.Process.Pid); i++ {
		time.Sleep(10 * time.Millisecond)
	}
	zst, _ := proc.ReadStat(zombie.Process.Pid)
	me, _ := proc.ReadStat(os.Getpid())
	rs := []reservation{
		{Pid: os.Getpid(), Start: me.Start, Name: "live", Expiry: now.Add(time.Hour).Unix()},
		{Pid: os.Getpid(), Start: me.Start, Name: "expired", Expiry: now.Add(-time.Minute).Unix()},
		{Pid: gone.Process.Pid, Name: "dead", Expiry: now.Add(time.Hour).Unix()},
		{Pid: zombie.Process.Pid, Start: zst.Start, Name: "zombie", Expiry: now.Add(time.Hour).Unix()},
		{Pid: os.Getpid(), Start: me.Start + 1, Name: "reused", Expiry: now.Add(time.Hour).Unix()},
	}
	got := pruneReservations(rs, now)
	if len(got) != 1 || got[0].Name != "live" {
		t.Fatal(got)
	}
}

func TestLegacyReservationLineAndLaunchLock(t *testing.T) {
	legacy := t.TempDir()
	_ = os.MkdirAll(filepath.Join(legacy, "locks"), 0o700)
	j := &remote.Job{P: &run.Prepared{Eff: &config.Effective{Compat: &config.Compat{LegacyDir: legacy}}},
		Core: &config.Provider{LaunchWait: time.Second}, Spec: &provider.JobSpec{Name: "bitc-x", Expiry: time.Unix(1_900_000_000, 0)}}
	unlock := legacyLaunchLock(context.Background(), j)
	if !lock.Held(filepath.Join(legacy, "locks", "runpod-launch.lock")) {
		t.Fatal("the legacy launch lock was not taken")
	}
	unlock()
	drop := legacyReservation(j, 380_000)
	b, _ := os.ReadFile(filepath.Join(legacy, "runpod-reservations"))
	if want := fmt.Sprintf("%d bitc-x 0.38 1900000000\n", os.Getpid()); string(b) != want {
		t.Fatalf("%q, want %q", b, want)
	}
	drop()
	if b, _ := os.ReadFile(filepath.Join(legacy, "runpod-reservations")); len(b) != 0 {
		t.Fatalf("line not removed: %q", b)
	}
}

func TestBadInstallSecretIsLocalAndWritesNoPause(t *testing.T) {
	acct := t.TempDir()
	if err := os.Chmod(acct, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(acct, "install-secret"), []byte("short"), 0o600); err != nil {
		t.Fatal(err)
	}
	j := &remote.Job{Type: "stub", P: &run.Prepared{Eff: &config.Effective{Test: &config.Test{AccountingDir: acct}}},
		Core: &config.Provider{LaunchWait: time.Second}, Prov: &stubProvider{}, Spec: &provider.JobSpec{Name: "bitc-x"}}
	_, err := Check(context.Background(), j)
	var una *provider.ErrUnavailable
	if !errors.As(err, &una) {
		t.Fatalf("want ErrUnavailable, got %v", err)
	}
	if !una.Local {
		t.Fatalf("a host-side fault must be Local so it never pauses the provider: %v", err)
	}
	if _, serr := os.Stat(filepath.Join(acct, "remote", "stub", "failures.jsonl")); serr == nil {
		t.Fatal("a pause record was written for a local fault")
	}
}

func TestPruneReservationsInDropsDeadAndHonoursDry(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "reservations.json")
	me, _ := proc.ReadStat(os.Getpid())
	exp := time.Now().Add(time.Hour).Unix()
	rs := []reservation{
		{Pid: os.Getpid(), Start: me.Start, Name: "live", Expiry: exp},
		{Pid: 2147483000, Name: "dead", Expiry: exp},
	}
	if err := writeReservations(path, rs); err != nil {
		t.Fatal(err)
	}
	if got := PruneReservationsIn(dir, true); len(got) != 1 || got[0] != "dead" || len(readReservations(path)) != 2 {
		t.Fatalf("dry: %v", got)
	}
	if got := PruneReservationsIn(dir, false); len(got) != 1 || got[0] != "dead" {
		t.Fatalf("%v", got)
	}
	if left := readReservations(path); len(left) != 1 || left[0].Name != "live" {
		t.Fatal(left)
	}
	if got := PruneReservationsIn(t.TempDir(), false); got != nil {
		t.Fatal(got)
	}
}
