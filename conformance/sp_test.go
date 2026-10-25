package conformance

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"path/filepath"

	"github.com/Exploded-views/bitc/conformance/harness"
	"github.com/Exploded-views/bitc/internal/provider"
	"github.com/Exploded-views/bitc/internal/provider/sprites"
	"github.com/Exploded-views/bitc/internal/provider/sprites/spritesfake"
)

// The SP family at the client level. The rows that need the provider and the walk
// (the pause record, the retry, the fallback notes) are completed by S.2.

const spSHA = "1111111111111111111111111111111111111111111111111111111111111111"

func spClient(t *testing.T, f *spritesfake.Fake) *sprites.Client {
	t.Helper()
	c, err := sprites.New(sprites.Options{APIBase: f.URL(), Org: f.Org(), Token: f.Token(), Timeout: 5 * time.Second, HandshakeTimeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func spClass(err error) string {
	var tr *provider.ErrTransient
	var un *provider.ErrUnavailable
	var in *provider.ErrIneligible
	var pa *provider.ErrPreAttach
	switch {
	case err == nil:
		return "ok"
	case errors.As(err, &pa):
		return "preattach"
	case errors.As(err, &un):
		return "unavailable"
	case errors.As(err, &in):
		return "ineligible"
	case errors.As(err, &tr):
		return "transient"
	}
	return "other"
}

// SP2 (client part): a token of another org is refused before any request; a Sprite response
// naming another org is an unavailable error on create, get and list, and the Sprite is named so
// the provider can delete it.
func TestSP2_OrgGuard(t *testing.T) {
	f := spritesfake.New(t, spritesfake.Options{})
	_, err := sprites.New(sprites.Options{APIBase: f.URL(), Org: "sandbox-org", Token: "prod-org/secret-part"})
	var ot *sprites.OrgTokenError
	if !errors.As(err, &ot) || !strings.Contains(err.Error(), `token belongs to "prod-org", not "sandbox-org"`) || strings.Contains(err.Error(), "secret-part") {
		t.Fatalf("token of another org: %v", err)
	}
	if n := len(f.Calls()); n != 0 {
		t.Fatalf("%d requests were made with a token of another org", n)
	}
	c := spClient(t, f)
	ctx := context.Background()
	f.Behave("wrong-org")
	_, err = c.CreateSprite(ctx, "bitc-a", nil)
	var pa *provider.ErrPreAttach
	var om *sprites.OrgMismatchError
	var un *provider.ErrUnavailable
	if !errors.As(err, &pa) || pa.ID != "bitc-a" || !errors.As(err, &om) || !errors.As(err, &un) || om.Got != "other-org" {
		t.Fatalf("create with a wrong org: %v", err)
	}
	if got := f.Live(); len(got) != 1 {
		t.Fatalf("the Sprite must exist for the provider to delete: %v", got)
	}
	if _, err := c.GetSprite(ctx, "bitc-a"); !errors.As(err, &om) || !errors.As(err, &un) || om.Name != "bitc-a" {
		t.Fatalf("get with a wrong org: %v", err)
	}
	if _, err := c.ListSprites(ctx, "bitc-"); !errors.As(err, &om) {
		t.Fatalf("list with a wrong org: %v", err)
	}
	if err := c.DeleteSprite(ctx, "bitc-a"); err != nil {
		t.Fatalf("delete after a mismatch: %v", err)
	}

	// provider half: a Sprite that answers with another organization is deleted at once, one
	// pause record is written and the job goes to the next provider; the next job does not even
	// try to create
	h := harness.New(t)
	e := spRepo(t, h, spOpts{runpod: true})
	e.f.Behave("wrong-org")
	r := h.Run(spRun("echo on-runpod")...)
	if r.Code != 0 || r.Stdout != "on-runpod\n" || !strings.Contains(r.Stderr, "bitc: sprites: heavy: trying runpod: ") || !strings.Contains(r.Stderr, "(org guard)") {
		t.Fatalf("wrong org: %+v", r)
	}
	if len(e.f.Live()) != 0 || e.deletes() != 1 || e.rpCreates() != 1 {
		t.Fatalf("live %v, deletes %d, runpod creates %d", e.f.Live(), e.deletes(), e.rpCreates())
	}
	if n := strings.Count(slurp(filepath.Join(e.acct, "failures.jsonl")), `"kind":"pause"`); n != 1 {
		t.Fatalf("%d pause records, want 1", n)
	}
	before := len(e.created(t))
	if r := h.Run(spRun("echo again")...); r.Code != 0 || len(e.created(t)) != before || !strings.Contains(r.Stderr, "trying runpod: sprites is paused: ") {
		t.Fatalf("while paused: %+v", r)
	}
}

// SP4 (client part): 429, sprite_creation_rate_limited, concurrent_sprite_limit_exceeded (any
// status) and "max sprites per org exceeded" are transient: nothing was created, and the error is
// neither unavailable nor a pre-attach discard (so no pause and no breaker record).
func TestSP4_CapacityIsTransient(t *testing.T) {
	for _, b := range []string{"429-create", "concurrent-limit", "max-sprites-exceeded"} {
		f := spritesfake.New(t, spritesfake.Options{})
		c := spClient(t, f)
		f.Behave(b)
		_, err := c.CreateSprite(context.Background(), "bitc-a", nil)
		if got := spClass(err); got != "transient" {
			t.Errorf("%s: class %s (%v)", b, got, err)
		}
		if len(f.Live()) != 0 {
			t.Errorf("%s: a Sprite was created", b)
		}
		// the retry of the walk (clause 1) finds the API free again
		if _, err := c.CreateSprite(context.Background(), "bitc-a", nil); err != nil {
			t.Errorf("%s: the retry failed: %v", b, err)
		}
	}
}

// SP5 (client part): 401 and 403 (account restricted) are unavailable, on a create and on a get,
// and nothing is created.
func TestSP5_AuthIsUnavailable(t *testing.T) {
	for _, b := range []string{"401", "403-restricted"} {
		f := spritesfake.New(t, spritesfake.Options{})
		c := spClient(t, f)
		f.Behave(b)
		_, err := c.CreateSprite(context.Background(), "bitc-a", nil)
		if got := spClass(err); got != "unavailable" {
			t.Errorf("%s: class %s (%v)", b, got, err)
		}
		if len(f.Live()) != 0 {
			t.Errorf("%s: a Sprite was created", b)
		}
	}
	f := spritesfake.New(t, spritesfake.Options{})
	c := spClient(t, f)
	f.BehaveN("401", -1)
	if _, err := c.GetSprite(context.Background(), "bitc-a"); spClass(err) != "unavailable" {
		t.Errorf("get with 401: %v", err)
	}

	// provider half: one pause record, the next provider, and nothing created; while paused the
	// next job does not call the API at all
	for _, b := range []string{"403-restricted", "401"} {
		h := harness.New(t)
		e := spRepo(t, h, spOpts{runpod: true})
		e.f.BehaveN(b, -1)
		r := h.Run(spRun("echo on-runpod")...)
		if r.Code != 0 || r.Stdout != "on-runpod\n" || !strings.Contains(r.Stderr, "bitc: sprites: heavy: trying runpod: ") || e.rpCreates() != 1 {
			t.Fatalf("%s: %+v", b, r)
		}
		if n := strings.Count(slurp(filepath.Join(e.acct, "failures.jsonl")), `"kind":"pause"`); n != 1 || len(e.f.Live()) != 0 {
			t.Fatalf("%s: %d pause records, live %v", b, n, e.f.Live())
		}
		calls := len(e.f.Calls())
		if r := h.Run(spRun("echo again")...); r.Code != 0 || len(e.f.Calls()) != calls || !strings.Contains(r.Stderr, "sprites is paused: ") {
			t.Fatalf("%s while paused: %+v (%d new calls)", b, r, len(e.f.Calls())-calls)
		}
	}
}

// SP8 (client part): the network and resources policies are set and read back before the first
// exec, and the job policy is set and read back before the attach exec. The call order is the
// fake's record; the client refuses an exec out of order before sending it.
func TestSP8_PoliciesBeforeExec(t *testing.T) {
	f := spritesfake.New(t, spritesfake.Options{Bin: harness.Bin, Exec: func(spritesfake.ExecContext) int { return 0 }})
	c := spClient(t, f)
	ctx := context.Background()
	name := "bitc-a"
	if _, err := c.CreateSprite(ctx, name, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Run(ctx, name, sprites.ExecRequest{Cmd: []string{"true"}}, nil); !errors.Is(err, sprites.ErrNotProvisioned) {
		t.Fatalf("exec before the policies: %v", err)
	}
	build, _ := sprites.AllowDomains("github.com", "archive.ubuntu.com")
	if err := c.ApplyBuildPolicies(ctx, name, build, sprites.Resources{LimitMB: 4096}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Run(ctx, name, sprites.ExecRequest{Cmd: []string{"true"}}, nil); err != nil {
		t.Fatal(err)
	}
	job := spritesfake.NewJob(t, "true", "")
	if _, err := c.Attach(ctx, name, sprites.AttachOptions{AgentSHA: spSHA, Sudo: true, Payload: job.Frame}); !errors.Is(err, sprites.ErrNotProvisioned) {
		t.Fatalf("attach before the job policy: %v", err)
	}
	jobPol, _ := sprites.AllowDomains("github.com")
	if err := c.ApplyJobPolicy(ctx, name, jobPol); err != nil {
		t.Fatal(err)
	}
	s, err := c.Attach(ctx, name, sprites.AttachOptions{AgentSHA: spSHA, Sudo: true, Deadline: time.Now().Add(time.Hour), Payload: job.Frame})
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()

	var order []string
	for _, call := range f.Calls() {
		order = append(order, call.Method+" "+strings.TrimPrefix(call.Path, "/v1/sprites"))
	}
	want := []string{"POST ", "POST /bitc-a/policy/network", "GET /bitc-a/policy/network", "POST /bitc-a/policy/resources", "GET /bitc-a/policy/resources",
		"GET /bitc-a/exec", // the provisioning exec
		"POST /bitc-a/policy/network", "GET /bitc-a/policy/network",
		"GET /bitc-a/exec"} // the attach exec
	if strings.Join(order, "|") != strings.Join(want, "|") {
		t.Fatalf("call order:\n%s\nwant:\n%s", strings.Join(order, "\n"), strings.Join(want, "\n"))
	}
	if n, _ := f.Policies(name); strings.Contains(n, "ubuntu") {
		t.Fatalf("the build policy is still in force at the attach: %s", n)
	}
	// a policy the API did not take blocks every exec
	f2 := spritesfake.New(t, spritesfake.Options{})
	c2 := spClient(t, f2)
	_, _ = c2.CreateSprite(ctx, name, nil)
	f2.Behave("policy-get-mismatch")
	if err := c2.ApplyBuildPolicies(ctx, name, build, sprites.Resources{LimitMB: 4096}); err == nil {
		t.Fatal("a policy that reads back differently was accepted")
	}
	if _, err := c2.Exec(ctx, name, sprites.ExecRequest{Cmd: []string{"true"}}); !errors.Is(err, sprites.ErrNotProvisioned) {
		t.Fatalf("exec after a mismatch: %v", err)
	}
	for _, call := range f2.Calls() {
		if strings.HasSuffix(call.Path, "/exec") {
			t.Fatalf("an exec reached the API: %+v", call)
		}
	}
}
