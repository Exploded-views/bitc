package remote

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Exploded-views/bitc/internal/config"
	"github.com/Exploded-views/bitc/internal/provider"
)

func TestSweepResources(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	install := "0123456789abcdef"
	now := time.Now()
	own := func(id string, expired bool) provider.Resource {
		e := now.Add(time.Hour)
		if expired {
			e = now.Add(-time.Hour)
		}
		return provider.Resource{ID: id, Name: "bitc-" + install + "-heavy-cpu8-0000000" + id[len(id)-1:] + "-1", Install: install, Expires: e}
	}
	foreign := provider.Resource{ID: "f", Name: "bitc-ffffffffffffffff-heavy-cpu8-0000000f-1", Install: "ffffffffffffffff", Expires: now.Add(-time.Hour)}
	res := []provider.Resource{own("a1", true), own("a2", true), own("a3", false), foreign}
	sp := &sweepStub{res: res, owner: map[string]string{"a1": provider.OwnerTag(secret, res[0].Name), "a2": "wrong"}}
	var out, errb bytes.Buffer
	sweepProvider(context.Background(), sweepInput{prov: sp, typ: "stub", prefix: "bitc-", install: install, secret: secret,
		launched: map[string]bool{}, now: now, out: &out, errw: &errb})
	if !strings.Contains(out.String(), "swept resource a1 (") || strings.Contains(out.String(), "a2") || strings.Contains(out.String(), "a3") || strings.Contains(out.String(), " f ") {
		t.Fatalf("%s", out.String())
	}
	if !strings.Contains(errb.String(), "a2 has no verified owner tag; kept") {
		t.Fatal(errb.String())
	}
	out.Reset()
	sp.torn = nil
	sweepProvider(context.Background(), sweepInput{prov: sp, typ: "stub", prefix: "bitc-", install: install, secret: secret,
		launched: map[string]bool{res[1].Name: true}, foreign: true, now: now, out: &out, errw: &errb})
	if !strings.Contains(out.String(), "swept resource a2") || !strings.Contains(out.String(), "swept foreign resource f (") {
		t.Fatalf("%s", out.String())
	}
	out.Reset()
	sp.torn = nil
	sweepProvider(context.Background(), sweepInput{prov: sp, typ: "stub", prefix: "bitc-", install: install, secret: secret,
		launched: map[string]bool{}, dry: true, now: now, out: &out, errw: &errb})
	if len(sp.torn) != 0 || !strings.Contains(out.String(), "swept resource") {
		t.Fatal("dry run tore down", sp.torn)
	}
}

type sweepStub struct {
	stubBase
	res   []provider.Resource
	owner map[string]string
	torn  []string
}

func (s *sweepStub) List(context.Context, []string) ([]provider.Resource, error) { return s.res, nil }
func (s *sweepStub) Owner(_ context.Context, id string) (string, error)          { return s.owner[id], nil }
func (s *sweepStub) Teardown(_ context.Context, id string) error {
	s.torn = append(s.torn, id)
	return nil
}

// stubBase is a no-op provider; test stubs embed it and override what they need.
type stubBase struct{}

func (stubBase) Info() provider.Info                                { return provider.Info{Type: "stub"} }
func (stubBase) Preflight(context.Context, *provider.JobSpec) error { return nil }
func (stubBase) Estimate(context.Context, *provider.JobSpec) (config.Micros, error) {
	return 0, nil
}
func (stubBase) Launch(context.Context, *provider.JobSpec) (provider.Handle, error) {
	return provider.Handle{}, nil
}
func (stubBase) Watch(context.Context, provider.Handle, provider.Sink) (provider.Result, error) {
	return provider.Result{}, nil
}
func (stubBase) Output(context.Context, provider.Handle, provider.Stream, int64, io.Writer) error {
	return nil
}
func (stubBase) Fetch(context.Context, provider.Handle, provider.ArtifactSpec, io.Writer) error {
	return nil
}
func (stubBase) Owner(context.Context, string) (string, error)               { return "", nil }
func (stubBase) Teardown(context.Context, string) error                      { return nil }
func (stubBase) List(context.Context, []string) ([]provider.Resource, error) { return nil, nil }
func (stubBase) Spend(context.Context) (config.Micros, error)                { return 0, nil }

func TestStatusSectionNewErrorYieldsEmptyArrays(t *testing.T) {
	provider.Register("newfail", provider.Factory{
		New: func(*config.Provider, *config.Effective) (provider.Provider, error) { return nil, errors.New("boom") },
	})
	e := &config.Effective{Test: &config.Test{AccountingDir: filepath.Join(t.TempDir(), "acct")},
		Providers: map[string]*config.Provider{"newfail": {APIKey: "k", NamePrefix: "bitc"}}}
	b, err := json.Marshal(StatusSection(e))
	if err != nil {
		t.Fatal(err)
	}
	if s := string(b); !strings.Contains(s, `"own":[]`) || !strings.Contains(s, `"foreign":[]`) || !strings.Contains(s, "boom") {
		t.Fatal(s)
	}
}

func TestSweepOneResourcePerJobRules(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	install := "0123456789abcdef"
	now := time.Now()
	mk := func(id string, exp, created time.Duration, inst string) provider.Resource {
		return provider.Resource{ID: id, Name: id, Install: inst, Expires: now.Add(exp), Created: now.Add(-created)}
	}
	res := []provider.Resource{
		mk("expired", -time.Hour, 3*time.Hour, install),
		mk("gone", time.Hour, 10*time.Minute, install),
		mk("reserved", time.Hour, 10*time.Minute, install),
		mk("fresh", time.Hour, time.Minute, install),
		mk("other", time.Hour, 10*time.Minute, "ffffffffffffffff"),
		mk("otherexp", -time.Hour, 10*time.Minute, "ffffffffffffffff"),
		mk("untagged", time.Hour, 10*time.Minute, install),
	}
	owner := map[string]string{}
	for _, r := range res {
		if r.ID != "untagged" {
			owner[r.ID] = provider.OwnerTag(secret, r.Name)
		}
	}
	sp := &sweepStub{res: res, owner: owner}
	in := sweepInput{prov: sp, typ: "sprites", prefix: "bitc-", install: install, secret: secret, now: now,
		rules: provider.SweepPolicy{Noun: "sprite"}, goneAfter: 2 * time.Minute, reserved: map[string]bool{"reserved": true}}
	var out, errb bytes.Buffer
	in.out, in.errw = &out, &errb
	sweepProvider(context.Background(), in)
	want := "swept sprite expired (past its expiry)\nswept sprite gone (job process gone)\n"
	if out.String() != want {
		t.Fatalf("got %q, want %q", out.String(), want)
	}
	if strings.Join(sp.torn, ",") != "expired,gone" {
		t.Fatalf("torn %v", sp.torn)
	}
	out.Reset()
	sp.torn = nil
	in.foreign = true
	sweepProvider(context.Background(), in)
	if !strings.Contains(out.String(), "swept sprite otherexp (past its expiry; --foreign)") || strings.Contains(out.String(), "swept sprite other (") {
		t.Fatalf("foreign: %q", out.String())
	}
	out.Reset()
	in.foreign, in.dry, sp.torn = false, true, nil
	sweepProvider(context.Background(), in)
	if len(sp.torn) != 0 || !strings.Contains(out.String(), "swept sprite gone") {
		t.Fatalf("dry: %q %v", out.String(), sp.torn)
	}
}

type foreignOrgErr struct{}

func (foreignOrgErr) Error() string {
	return "sprite bitc-x belongs to \"other\", not \"mine\" (org guard)"
}
func (foreignOrgErr) ForeignOrg() (string, string) { return "bitc-x", "other" }

func TestSweepNamesTheForeignOrgSprite(t *testing.T) {
	sp := &listErrStub{err: foreignOrgErr{}}
	var out, errb bytes.Buffer
	sweepProvider(context.Background(), sweepInput{prov: sp, typ: "sprites", prefix: "bitc-", out: &out, errw: &errb})
	if !strings.Contains(errb.String(), "bitc-x") || !strings.Contains(errb.String(), "never touched") || !strings.Contains(errb.String(), "delete it by hand") || out.Len() != 0 {
		t.Fatalf("out %q err %q", out.String(), errb.String())
	}
}

type listErrStub struct {
	stubBase
	err error
}

func (s *listErrStub) List(context.Context, []string) ([]provider.Resource, error) { return nil, s.err }

func TestSweepStampsTheLastListing(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sprites")
	now := time.Now()
	run := func(p provider.Provider, dry bool) {
		var out, errb bytes.Buffer
		sweepProvider(context.Background(), sweepInput{prov: p, typ: "sprites", dir: dir, prefix: "bitc-", now: now, dry: dry, out: &out, errw: &errb})
	}
	run(&listErrStub{err: errors.New("boom")}, false)
	if _, ok := LastSweep(dir); ok {
		t.Fatal("a failed listing is not a sweep")
	}
	run(&sweepStub{}, true)
	if _, ok := LastSweep(dir); ok {
		t.Fatal("a dry run is not a sweep")
	}
	run(&sweepStub{}, false)
	got, ok := LastSweep(dir)
	if !ok || got.Unix() != now.Unix() {
		t.Fatalf("got %v %v, want %v", got, ok, now)
	}
}

type statusStub struct {
	stubBase
	err error
}

func (s statusStub) List(context.Context, []string) ([]provider.Resource, error) { return nil, s.err }

func TestStatusSectionNamesTheForeignOrgSpriteAndTheLastSweep(t *testing.T) {
	provider.Register("statusstub", provider.Factory{
		New: func(*config.Provider, *config.Effective) (provider.Provider, error) {
			return statusStub{err: foreignOrgErr{}}, nil
		},
	})
	acct := filepath.Join(t.TempDir(), "acct")
	e := &config.Effective{Test: &config.Test{AccountingDir: acct}, Providers: map[string]*config.Provider{"statusstub": {APIKey: "k", NamePrefix: "bitc"}}}
	dir := filepath.Join(acct, "remote", "statusstub")
	stampSweep(dir, time.Unix(1_700_000_000, 0))
	b, err := json.Marshal(StatusSection(e))
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if !strings.Contains(s, `"foreign_org":"bitc-x"`) || !strings.Contains(s, `"last_sweep":"2023-11-14T22:13:20Z"`) || !strings.Contains(s, `"est_usd_today"`) {
		t.Fatal(s)
	}
}
