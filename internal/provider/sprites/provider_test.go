package sprites

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Exploded-views/bitc/internal/config"
	"github.com/Exploded-views/bitc/internal/provider"
	"github.com/Exploded-views/bitc/internal/provider/sprites/spritesfake"
)

// newProv builds the provider against a fake; edit changes the raw provider keys.
func newProv(t *testing.T, f *spritesfake.Fake, edit func(raw map[string]any)) *Provider {
	t.Helper()
	raw := map[string]any{"org": f.Org(), "api_base": f.URL(), "sizes": map[string]any{"cpu8": map[string]any{"memory": "16GiB"}, "big": map[string]any{"memory": "16GiB", "usd_per_hour": 3.0}}}
	if edit != nil {
		edit(raw)
	}
	core := &config.Provider{Type: "sprites", NamePrefix: "bitc", APIKey: f.Token(), Raw: raw}
	p, err := NewProvider(core, &config.Effective{Test: &config.Test{TimeScale: 0.05, AccountingDir: t.TempDir()}})
	if err != nil {
		t.Fatal(err)
	}
	p.UseInstallSecret(testSecret)
	return p
}

func TestInfoDeclaresPhaseOne(t *testing.T) {
	f := spritesfake.New(t, spritesfake.Options{})
	i := newProv(t, f, nil).Info()
	c := i.Caps
	if i.Type != "sprites" || !c.LiveLogs || !c.Artifacts || c.Spend || c.SelfDelete || c.FallbackUntil != provider.UntilAttach || c.SendEnv != provider.EnvForced || c.MultiJob {
		t.Fatalf("%+v", i)
	}
	if len(i.Arch) != 1 || i.Arch[0] != "amd64" || len(i.Endpoints) != 1 || i.Endpoints[0] != f.URL() {
		t.Fatalf("arch %v endpoints %v", i.Arch, i.Endpoints)
	}
}

func TestFactoryDeclaresBoundaryEnvLevelAndValidation(t *testing.T) {
	fac, ok := provider.Get("sprites")
	if !ok {
		t.Fatal("sprites is not registered")
	}
	if fac.FallbackCap == nil || fac.FallbackCap(nil) != provider.UntilAttach || fac.SendEnv == nil || fac.SendEnv(nil) != provider.EnvForced || fac.Validate == nil {
		t.Fatalf("%+v", fac)
	}
	core := &config.Provider{Type: "sprites", NamePrefix: "bitc", APIKey: "prod-org/abc", Raw: map[string]any{"org": "sandbox-org", "sizes": map[string]any{"cpu8": map[string]any{"memory": "8GiB"}}}}
	if got := fac.Validate(core); len(got) != 1 || got[0] != `org: token belongs to "prod-org", not "sandbox-org"` {
		t.Fatalf("%q", got)
	}
	core.APIKey = "sandbox-org/abc"
	if got := fac.Validate(core); len(got) != 0 {
		t.Fatalf("%q", got)
	}
	core.APIKey = "no-slash-token"
	if got := fac.Validate(core); len(got) != 1 || !strings.Contains(got[0], "token carries no org slug") || strings.Contains(got[0], "no-slash-token") {
		t.Fatalf("%q", got)
	}
	core.APIKey = "FlyV1 fm2_FAKEbase64PART/more+fake"
	if got := fac.Validate(core); len(got) != 1 || !strings.Contains(got[0], "token carries no org slug") || strings.Contains(got[0], "FAKE") {
		t.Fatalf("a macaroon is not a slug and is never printed: %q", got)
	}
}

func goodJob() *provider.JobSpec {
	return &provider.JobSpec{Job: "j1", SizeKey: "cpu8", CloneURL: "git@github.com:example-org/app.git", DeployKeyFile: "/keys/app",
		AgentURL: "https://downloads.example.com/bitc-amd64", Argv: []string{"echo", "x"}, Expiry: time.Now().Add(time.Hour)}
}

func TestPreflightRules(t *testing.T) {
	f := spritesfake.New(t, spritesfake.Options{})
	p := newProv(t, f, nil)
	ctx := context.Background()
	if err := p.Preflight(ctx, goodJob()); err != nil {
		t.Fatalf("a good job: %v", err)
	}
	local := goodJob()
	local.CloneURL, local.Unprivileged, local.DeployKeyFile = "/srv/origin.git", true, ""
	if err := p.Preflight(ctx, local); err != nil {
		t.Fatalf("a local clone in a test run needs no key: %v", err)
	}
	public := goodJob()
	public.CloneURL, public.DeployKeyFile = "https://github.com/example-org/app.git", ""
	if err := p.Preflight(ctx, public); err != nil {
		t.Fatalf("a public https repo needs no deploy key: %v", err)
	}
	sshURL := goodJob()
	sshURL.CloneURL, sshURL.DeployKeyFile = "ssh://git@example.com/app.git", ""
	if err := p.Preflight(ctx, sshURL); err == nil {
		t.Fatal("an ssh:// clone without a key must be ineligible")
	}
	for _, c := range []struct {
		name string
		edit func(j *provider.JobSpec)
		want string
	}{
		{"size", func(j *provider.JobSpec) { j.SizeKey = "huge" }, `size "huge" is not in [providers.sprites] sizes`},
		{"private repo without a key", func(j *provider.JobSpec) { j.DeployKeyFile = "" }, "needs [[repos]] deploy_key_file"},
		{"agent built in the pod", func(j *provider.JobSpec) { j.AgentBuild = &provider.AgentBuild{Commit: strings.Repeat("a", 40)} }, `agent source "build"`},
		{"tool host outside the build policy", func(j *provider.JobSpec) {
			j.Tools = []config.Tool{{Name: "node", URL: "https://evil.example.org/node.tgz", SHA256: strings.Repeat("a", 64)}}
		}, `tool node downloads from "evil.example.org"`},
		{"boot payload over 8 MiB", func(j *provider.JobSpec) { j.Env = []string{"BIG=" + strings.Repeat("x", 7<<20)} }, "the boot payload is about"},
	} {
		j := goodJob()
		c.edit(j)
		err := p.Preflight(ctx, j)
		var in *provider.ErrIneligible
		if !errors.As(err, &in) || !strings.Contains(in.Why, c.want) {
			t.Errorf("%s: %v, want ineligible with %q", c.name, err, c.want)
		}
	}
	// an agent url whose host the egress policy cannot name is ineligible, except in a [test] run
	// (its agent url is a loopback literal)
	ip := goodJob()
	ip.AgentURL = "https://10.1.2.3/bitc"
	if err := p.Preflight(ctx, ip); err != nil {
		t.Errorf("[test] run: %v", err)
	}
	prod := newProv(t, f, nil)
	prod.eff = &config.Effective{}
	var in *provider.ErrIneligible
	if err := prod.Preflight(ctx, ip); !errors.As(err, &in) || !strings.Contains(in.Why, `the agent url host "10.1.2.3"`) {
		t.Errorf("agent host that is not a domain: %v", err)
	}
	// a tool from an allowed host passes; the build mirrors count
	j := goodJob()
	j.Tools = []config.Tool{{Name: "node", URL: "https://codeload.github.com/o/r/node.tgz", SHA256: strings.Repeat("a", 64)}, {Name: "x", URL: "https://archive.ubuntu.com/x.tgz", SHA256: strings.Repeat("b", 64)}}
	if err := p.Preflight(ctx, j); err != nil {
		t.Errorf("tools on allowed hosts: %v", err)
	}
	// no credentials: unavailable, a cause on this host
	core := &config.Provider{Type: "sprites", NamePrefix: "bitc", Raw: p.core.Raw}
	np, err := NewProvider(core, p.eff)
	if err != nil {
		t.Fatal(err)
	}
	var un *provider.ErrUnavailable
	if err := np.Preflight(ctx, goodJob()); !errors.As(err, &un) || !un.Local {
		t.Errorf("no credentials: %v", err)
	}
}

func TestEstimateIsThePriceOverTheTimeToExpiry(t *testing.T) {
	f := spritesfake.New(t, spritesfake.Options{})
	p := newProv(t, f, nil)
	j := goodJob()
	j.Expiry = time.Now().Add(55 * time.Minute)
	got, err := p.Estimate(context.Background(), j)
	if err != nil || got < 694000 || got > 694100 {
		t.Fatalf("%d %v, want about 694057 (55 min at $0.757154/h)", got, err)
	}
	j.SizeKey = "nosuch" // the highest size ($3.00/h)
	if got, _ := p.Estimate(context.Background(), j); got < 2_749_000 || got > 2_751_000 {
		t.Fatalf("unknown size: %d", got)
	}
}

func TestOwnerVerifiesTheTagInTheName(t *testing.T) {
	f := spritesfake.New(t, spritesfake.Options{})
	p := newProv(t, f, nil)
	n := EncodeName(testSecret, "bitc", testInstall, "cpu8", "deadbeef", time.Now().Add(time.Hour))
	tag, err := p.Owner(context.Background(), n)
	if err != nil || !provider.OwnerMatches(testSecret, n, tag) {
		t.Fatalf("own name: %q %v", tag, err)
	}
	for _, other := range []string{EncodeName([]byte("another-install-secret-0123456789"), "bitc", testInstall, "cpu8", "deadbeef", time.Now().Add(time.Hour)),
		strings.Replace(n, "cpu8", "cpu9", 1), "bitc-junk", "other-" + n[5:]} {
		if tag, err := p.Owner(context.Background(), other); err != nil || tag != "" || provider.OwnerMatches(testSecret, other, tag) {
			t.Errorf("%q: %q %v", other, tag, err)
		}
	}
}

func TestResourceNameIsTheProvidersOwnRule(t *testing.T) {
	f := spritesfake.New(t, spritesfake.Options{})
	p := newProv(t, f, nil)
	exp := time.Unix(1_800_000_000, 0)
	got := provider.NameFor(p, testSecret, "bitc", testInstall, "heavy", "cpu8", "job-1", exp, 63)
	if !spriteNameRE.MatchString(got) || strings.Contains(got, "heavy") || !VerifyName(testSecret, got) {
		t.Fatalf("%q", got)
	}
	pn, _ := ParseName("bitc", got)
	if pn.Size != "cpu8" || pn.Install != testInstall || !pn.Expires.Equal(exp) {
		t.Fatalf("%+v", pn)
	}
}

func TestListMapsSpritesToResources(t *testing.T) {
	f := spritesfake.New(t, spritesfake.Options{PageSize: 1})
	p := newProv(t, f, nil)
	now := time.Now()
	own := EncodeName(testSecret, "bitc", testInstall, "cpu8", "deadbeef", now.Add(time.Hour))
	f.AddSprite(own, f.Org(), now)
	f.AddSprite("bitc-junk", f.Org(), now) // a name that does not parse
	f.AddSprite("unrelated-sprite", f.Org(), now)
	got, err := p.List(context.Background(), []string{"bitc-"})
	if err != nil || len(got) != 2 {
		t.Fatalf("%v %v", got, err)
	}
	by := map[string]provider.Resource{}
	for _, r := range got {
		by[r.Name] = r
	}
	r := by[own]
	if r.ID != own || r.SizeKey != "cpu8" || r.Install != testInstall || r.USDPerHour != 757154 || r.Expires.IsZero() || r.State != "running" || r.Created.IsZero() {
		t.Fatalf("own: %+v", r)
	}
	if j := by["bitc-junk"]; j.SizeKey != "" || !j.Expires.IsZero() || j.USDPerHour != 3_000_000 {
		t.Fatalf("an unparsable name is priced at the highest size with no expiry: %+v", j)
	}
	// another organization anywhere in the list fails it as unavailable
	f.Behave("wrong-org")
	var un *provider.ErrUnavailable
	if _, err := p.List(context.Background(), []string{"bitc-"}); !errors.As(err, &un) {
		t.Fatalf("wrong org: %v", err)
	}
}

func TestListRetriesATransientFailureOnce(t *testing.T) {
	f := spritesfake.New(t, spritesfake.Options{})
	p := newProv(t, f, nil)
	f.Behave("list-500")
	if _, err := p.List(context.Background(), []string{"bitc-"}); err != nil {
		t.Fatalf("one 5xx: %v", err)
	}
	f.BehaveN("list-500", 2)
	var tr *provider.ErrTransient
	if _, err := p.List(context.Background(), []string{"bitc-"}); !errors.As(err, &tr) {
		t.Fatalf("two 5xx: %v", err)
	}
}

func TestTeardownTreats404AsDone(t *testing.T) {
	f := spritesfake.New(t, spritesfake.Options{})
	p := newProv(t, f, nil)
	if err := p.Teardown(context.Background(), "bitc-nosuch"); err != nil {
		t.Fatalf("404: %v", err)
	}
	f.AddSprite("bitc-a", f.Org(), time.Now())
	f.Behave("delete-500")
	if err := p.Teardown(context.Background(), "bitc-a"); err == nil {
		t.Fatal("a failed delete reported done")
	}
	if err := p.Teardown(context.Background(), "bitc-a"); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if len(f.Live()) != 0 {
		t.Fatalf("live %v", f.Live())
	}
}

func TestSweepRulesOnePerJob(t *testing.T) {
	f := spritesfake.New(t, spritesfake.Options{})
	p := newProv(t, f, nil)
	p.core.StartTimeout = 7 * time.Minute
	r := p.SweepRules()
	if r.Noun != "sprite" || r.GoneAfter != 7*time.Minute || !r.TimerRequired {
		t.Fatalf("%+v", r)
	}
}

func doctorOf(t *testing.T, p *Provider) map[string]provider.DoctorLine {
	t.Helper()
	out := map[string]provider.DoctorLine{}
	for _, l := range p.DoctorLines(context.Background()) {
		out[l.Check] = l
	}
	return out
}

func TestDoctorLinesCleanOrg(t *testing.T) {
	f := spritesfake.New(t, spritesfake.Options{})
	p := newProv(t, f, nil)
	f.AddSprite(EncodeName(testSecret, "bitc", testInstall, "cpu8", "deadbeef", time.Now().Add(time.Hour)), f.Org(), time.Now())
	got := doctorOf(t, p)
	for _, check := range []string{"sprites org", "sprites connectors", "sprites outside prefix", "sprites installs"} {
		if got[check].Level != "ok" {
			t.Fatalf("%s: %+v (all %+v)", check, got[check], got)
		}
	}
}

func TestDoctorLinesFailOnAConnector(t *testing.T) {
	f := spritesfake.New(t, spritesfake.Options{})
	p := newProv(t, f, nil)
	f.AddConnection("github-app")
	l := doctorOf(t, p)["sprites connectors"]
	if l.Level != "fail" || !strings.Contains(l.Detail, "github-app") || !strings.Contains(l.Detail, "1 Connector") {
		t.Fatalf("%+v", l)
	}
}

func TestDoctorLinesFailOnASpriteOutsideThePrefix(t *testing.T) {
	f := spritesfake.New(t, spritesfake.Options{})
	p := newProv(t, f, nil)
	f.AddSprite("somebody-elses-app", f.Org(), time.Now())
	l := doctorOf(t, p)["sprites outside prefix"]
	if l.Level != "fail" || !strings.Contains(l.Detail, "somebody-elses-app") {
		t.Fatalf("%+v", l)
	}
}

func TestDoctorLinesWarnOnMoreThanOneInstall(t *testing.T) {
	f := spritesfake.New(t, spritesfake.Options{})
	p := newProv(t, f, nil)
	other := EncodeName([]byte("another-install-secret-0123456789"), "bitc", "ffffffffffffffff", "cpu8", "deadbeef", time.Now().Add(time.Hour))
	f.AddSprite(other, f.Org(), time.Now())
	f.AddSprite(EncodeName(testSecret, "bitc", testInstall, "cpu8", "cafebabe", time.Now().Add(time.Hour)), f.Org(), time.Now())
	l := doctorOf(t, p)["sprites installs"]
	if l.Level != "warn" || !strings.Contains(l.Detail, "2 installs") || !strings.Contains(l.Detail, "ffffffffffffffff") || !strings.Contains(l.Detail, "per-host share") {
		t.Fatalf("%+v", l)
	}
}

func TestDoctorLinesNameTheSpriteOfAnotherOrg(t *testing.T) {
	f := spritesfake.New(t, spritesfake.Options{})
	p := newProv(t, f, nil)
	f.AddSprite("bitc-foreign", "somebody-else", time.Now())
	l := doctorOf(t, p)["sprites org"]
	if l.Level != "fail" || !strings.Contains(l.Detail, "bitc-foreign") || !strings.Contains(l.Detail, "somebody-else") {
		t.Fatalf("%+v", l)
	}
}

func TestDoctorLinesUnauthorizedOrgListing(t *testing.T) {
	f := spritesfake.New(t, spritesfake.Options{})
	p := newProv(t, f, nil)
	f.BehaveN("401", 1) // the org listing takes the 401
	got := doctorOf(t, p)
	if got["sprites org"].Level != "fail" {
		t.Fatalf("%+v", got)
	}
}
