package sprites

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Exploded-views/bitc/internal/provider"
	"github.com/Exploded-views/bitc/internal/provider/sprites/spritesfake"
)

func newFake(t *testing.T, o spritesfake.Options) (*spritesfake.Fake, *Client) {
	t.Helper()
	f := spritesfake.New(t, o)
	c, err := New(Options{APIBase: f.URL(), Org: f.Org(), Token: f.Token(), Timeout: 5 * time.Second, HandshakeTimeout: 5 * time.Second})
	if err != nil || c == nil || c.base == nil {
		t.Fatalf("New returned no usable client: %v", err)
	}
	return f, c
}

func kind(err error) string {
	var tr *provider.ErrTransient
	var un *provider.ErrUnavailable
	var in *provider.ErrIneligible
	var pa *provider.ErrPreAttach
	var at *provider.ErrAttached
	switch {
	case err == nil:
		return "ok"
	case errors.As(err, &at):
		return "attached"
	case errors.As(err, &pa):
		return "preattach"
	case errors.Is(err, ErrNotFound):
		return "notfound"
	case errors.As(err, &un):
		return "unavailable"
	case errors.As(err, &in):
		return "ineligible"
	case errors.As(err, &tr):
		return "transient"
	}
	return fmt.Sprintf("other(%v)", err)
}

func innerKind(err error) string {
	var pa *provider.ErrPreAttach
	if errors.As(err, &pa) {
		return kind(pa.Err)
	}
	return "none"
}

func TestMapHTTP(t *testing.T) {
	cases := []struct {
		code int
		body string
		want string
	}{
		{200, ``, "ok"}, {201, ``, "ok"}, {204, ``, "ok"},
		{0, ``, "transient"},
		{429, `{"error":"sprite_creation_rate_limited"}`, "transient"},
		{429, ``, "transient"},
		{403, `{"error":"concurrent_sprite_limit_exceeded"}`, "transient"},
		{400, `{"message":"max sprites per org exceeded"}`, "transient"},
		{500, `{"error":"concurrent_sprite_limit_exceeded"}`, "transient"},
		{401, ``, "unavailable"}, {402, ``, "unavailable"},
		{403, `{"message":"Your account is restricted"}`, "unavailable"},
		{404, ``, "notfound"},
		{400, `{"error":"bad"}`, "ineligible"}, {413, ``, "ineligible"}, {422, ``, "ineligible"}, {409, ``, "ineligible"},
		{500, ``, "transient"}, {502, ``, "transient"}, {503, ``, "transient"},
	}
	for _, c := range cases {
		if got := kind(mapHTTP(c.code, []byte(c.body), "x")); got != c.want {
			t.Errorf("%d %s: got %s, want %s", c.code, c.body, got, c.want)
		}
	}
}

func TestMapHTTPNeverEchoesTheBody(t *testing.T) {
	err := mapHTTP(400, []byte(`{"error":"bad","detail":"BITC_SPEC=hunter2"}`), "create")
	if err == nil {
		t.Fatal("a 400 is an error")
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("payload in %q", err)
	}
	err = mapHTTP(403, []byte(`{"message":"first line\nBITC_SPEC=hunter2"}`), "create")
	if err == nil {
		t.Fatal("a 403 is an error")
	}
	if strings.Contains(err.Error(), "hunter2") || !strings.Contains(err.Error(), "first line") {
		t.Fatalf("message handling: %q", err)
	}
}

func TestNewChecksTheOrgToken(t *testing.T) {
	_, err := New(Options{Org: "sandbox-org", Token: "prod-org/abc"})
	var ot *OrgTokenError
	if !errors.As(err, &ot) || ot.Slug != "prod-org" || !strings.Contains(err.Error(), `token belongs to "prod-org", not "sandbox-org"`) {
		t.Fatalf("got %v", err)
	}
	_, err = New(Options{Org: "sandbox-org", Token: "toplevelsecretwithoutslash"})
	if !errors.As(err, &ot) || strings.Contains(err.Error(), "toplevelsecret") {
		t.Fatalf("a token without a slug must not be printed: %v", err)
	}
	// A Fly macaroon is base64 and has a "/" too: what precedes it is not a slug and is never printed.
	for _, tok := range []string{"FlyV1 fm2_FAKEbase64PART/more+fake", "Prod-Org/abc", "fake_secret_part/abc", "/abc"} {
		_, err = New(Options{Org: "sandbox-org", Token: tok})
		if !errors.As(err, &ot) || ot.HasSlug || !strings.Contains(err.Error(), "token carries no org slug") || strings.Contains(err.Error(), "FAKE") || strings.Contains(err.Error(), "fake") || strings.Contains(err.Error(), "Prod") {
			t.Fatalf("%q: %v", tok, err)
		}
	}
	if _, err := New(Options{Org: "", Token: "x/y"}); err == nil {
		t.Fatal("empty org accepted")
	}
	if _, err := New(Options{Org: "o"}); err == nil {
		t.Fatal("no credentials accepted")
	}
	if _, err := New(Options{Org: "o", Token: "o/s"}); err != nil {
		t.Fatal(err)
	}
}

func TestNewRefusesInsecureBases(t *testing.T) {
	for _, b := range []string{"http://api.sprites.dev", "http://10.0.0.1:8080", "ftp://127.0.0.1", "https://api.sprites.dev/v1", "api.sprites.dev"} {
		if _, err := New(Options{APIBase: b, Org: "o", Token: "o/s"}); err == nil {
			t.Errorf("base %q accepted", b)
		}
	}
	for _, b := range []string{"", "https://api.sprites.dev", "http://127.0.0.1:9", "http://localhost:9", "http://[::1]:9"} {
		if _, err := New(Options{APIBase: b, Org: "o", Token: "o/s"}); err != nil {
			t.Errorf("base %q: %v", b, err)
		}
	}
}

func TestTokenFileRulesAndNeverFormatted(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "tok")
	if err := os.WriteFile(p, []byte("o/the-secret-value\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(p, 0o644)
	if _, err := New(Options{Org: "o", TokenFile: p}); err == nil || !strings.Contains(err.Error(), "mode 600") {
		t.Fatalf("a 0644 token file was accepted: %v", err)
	}
	_ = os.Chmod(p, 0o600)
	c, err := New(Options{Org: "o", TokenFile: p})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{fmt.Sprint(c), fmt.Sprintf("%v %+v %#v %s", c, c, c, c), fmt.Sprintf("%v", c.token), fmt.Sprintf("%#v", c.token)} {
		if strings.Contains(s, "the-secret-value") {
			t.Fatalf("token formatted: %s", s)
		}
	}
	if got := c.Secrets(); len(got) != 1 || got[0] != "o/the-secret-value" {
		t.Fatalf("Secrets() = %v", got)
	}
	if _, err := New(Options{Org: "o", TokenFile: filepath.Join(dir, "missing")}); err == nil {
		t.Fatal("missing token file accepted")
	}
	if _, err := New(Options{Org: "o", TokenFile: p, Token: "o/x"}); err == nil {
		t.Fatal("both Token and TokenFile accepted")
	}
}

func TestTokenIsSentOnlyInTheAuthorizationHeader(t *testing.T) {
	f, c := newFake(t, spritesfake.Options{})
	ctx := context.Background()
	if _, err := c.CreateSprite(ctx, "bitc-a", map[string]string{"bitc": "1"}); err != nil {
		t.Fatal(err)
	}
	_, _ = c.GetSprite(ctx, "bitc-a")
	_, _ = c.ListSprites(ctx, "bitc-")
	if n := len(f.Calls()); n != 3 {
		t.Fatalf("%d requests reached the API, want 3", n)
	}
	for _, call := range f.Calls() {
		if call.Auth != "Bearer "+f.Token() {
			t.Errorf("%s %s: auth %q", call.Method, call.Path, call.Auth)
		}
		if strings.Contains(call.Path+call.RawQuery+string(call.Body), f.Token()) {
			t.Errorf("token in the URL or body of %s %s", call.Method, call.Path)
		}
	}
}

func TestCreateGetListDelete(t *testing.T) {
	f, c := newFake(t, spritesfake.Options{PageSize: 2})
	ctx := context.Background()
	for _, n := range []string{"bitc-a", "bitc-b", "bitc-c", "other-x"} {
		s, err := c.CreateSprite(ctx, n, map[string]string{"bitc": "1"})
		if err != nil || s.Name != n || s.Organization != f.Org() || s.ID == "" {
			t.Fatalf("create %s: %+v %v", n, s, err)
		}
	}
	g, err := c.GetSprite(ctx, "bitc-b")
	if err != nil || g.Name != "bitc-b" || g.Labels["bitc"] != "1" {
		t.Fatalf("get: %+v %v", g, err)
	}
	if _, err := c.GetSprite(ctx, "bitc-zzz"); kind(err) != "notfound" {
		t.Fatalf("get missing: %v", err)
	}
	list, err := c.ListSprites(ctx, "bitc-")
	var names []string
	for _, s := range list {
		names = append(names, s.Name)
	}
	if err != nil || strings.Join(names, ",") != "bitc-a,bitc-b,bitc-c" {
		t.Fatalf("list across pages: %v %v", names, err)
	}
	if err := c.DeleteSprite(ctx, "bitc-b"); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteSprite(ctx, "bitc-b"); err != nil {
		t.Fatalf("a delete of a missing Sprite means done: %v", err)
	}
	if got := strings.Join(f.Live(), ","); got != "bitc-a,bitc-c,other-x" {
		t.Fatalf("live: %s", got)
	}
}

func TestCreateRefusesABadName(t *testing.T) {
	_, c := newFake(t, spritesfake.Options{})
	for _, n := range []string{"", "Bitc", "a/b", "a b", strings.Repeat("a", 64), "-a"} {
		if _, err := c.CreateSprite(context.Background(), n, nil); kind(err) != "ineligible" {
			t.Errorf("%q: %v", n, err)
		}
	}
}

func TestCreateErrorClasses(t *testing.T) {
	cases := []struct {
		behave string
		kind   string // of the returned error
		inner  string // of ErrPreAttach.Err, when it is one
		made   bool   // a Sprite exists afterwards
	}{
		{"429-create", "transient", "", false},
		{"concurrent-limit", "transient", "", false},
		{"max-sprites-exceeded", "transient", "", false},
		{"403-restricted", "unavailable", "", false},
		{"401", "unavailable", "", false},
		{"create-500-nothing", "preattach", "transient", false},
		{"create-then-500", "preattach", "transient", true},
		{"create-then-500-lookup-fails", "preattach", "transient", true},
		{"wrong-org", "preattach", "unavailable", true},
		{"no-org", "preattach", "unavailable", true},
	}
	for _, tc := range cases {
		t.Run(tc.behave, func(t *testing.T) {
			f, c := newFake(t, spritesfake.Options{})
			f.Behave(tc.behave)
			s, err := c.CreateSprite(context.Background(), "bitc-x", nil)
			if got := kind(err); got != tc.kind || innerKind(err) != orNone(tc.inner) {
				t.Fatalf("got %s/%s (%v), want %s/%s", got, innerKind(err), err, tc.kind, tc.inner)
			}
			var pa *provider.ErrPreAttach
			if errors.As(err, &pa) && pa.ID != "bitc-x" {
				t.Fatalf("ErrPreAttach.ID = %q, want the Sprite name", pa.ID)
			}
			if tc.kind != "preattach" && s.Name != "" {
				t.Fatalf("a refused create returned a Sprite: %+v", s)
			}
			if made := len(f.Live()) == 1; made != tc.made {
				t.Fatalf("sprite exists = %v, want %v", made, tc.made)
			}
		})
	}
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

func TestCreateOfAnExistingNameIsAmbiguous(t *testing.T) {
	f, c := newFake(t, spritesfake.Options{})
	if _, err := c.CreateSprite(context.Background(), "bitc-x", nil); err != nil {
		t.Fatal(err)
	}
	_, err := c.CreateSprite(context.Background(), "bitc-x", nil)
	if kind(err) != "preattach" || len(f.Live()) != 1 {
		t.Fatalf("got %v", err)
	}
}

func TestCreateRetriesWithoutLabelsWhenUnsupported(t *testing.T) {
	f, c := newFake(t, spritesfake.Options{})
	f.Behave("labels-unsupported")
	s, err := c.CreateSprite(context.Background(), "bitc-x", map[string]string{"bitc": "1"})
	if err != nil || s.Name != "bitc-x" || len(s.Labels) != 0 {
		t.Fatalf("%+v %v", s, err)
	}
	posts := 0
	for _, call := range f.Calls() {
		if call.Method == "POST" && call.Path == "/v1/sprites" {
			posts++
		}
	}
	if posts != 2 {
		t.Fatalf("%d POSTs, want 2", posts)
	}
}

func TestOrgGuardOnGetAndList(t *testing.T) {
	f, c := newFake(t, spritesfake.Options{})
	ctx := context.Background()
	if _, err := c.CreateSprite(ctx, "bitc-a", nil); err != nil {
		t.Fatal(err)
	}
	f.Behave("wrong-org")
	_, err := c.GetSprite(ctx, "bitc-a")
	var om *OrgMismatchError
	var un *provider.ErrUnavailable
	if !errors.As(err, &om) || !errors.As(err, &un) || om.Name != "bitc-a" || om.Got != "other-org" {
		t.Fatalf("get: %v", err)
	}
	if _, err := c.ListSprites(ctx, "bitc-"); !errors.As(err, &om) {
		t.Fatalf("list: %v", err)
	}
}

func TestListFailsOnAForeignOrgSprite(t *testing.T) {
	f, c := newFake(t, spritesfake.Options{})
	f.AddSprite("bitc-foreign", "somebody-else", time.Now())
	var om *OrgMismatchError
	if _, err := c.ListSprites(context.Background(), "bitc-"); !errors.As(err, &om) {
		t.Fatalf("got %v", err)
	}
}

func TestPoliciesAreSetAndVerified(t *testing.T) {
	f, c := newFake(t, spritesfake.Options{})
	ctx := context.Background()
	if _, err := c.CreateSprite(ctx, "bitc-a", nil); err != nil {
		t.Fatal(err)
	}
	build, err := AllowDomains("github.com", "archive.ubuntu.com", "github.com")
	if err != nil || len(build.Rules) != 2 {
		t.Fatalf("%+v %v", build, err)
	}
	if err := c.ApplyBuildPolicies(ctx, "bitc-a", build, Resources{LimitMB: 16384}); err != nil {
		t.Fatal(err)
	}
	net, res := f.Policies("bitc-a")
	if !strings.Contains(net, "archive.ubuntu.com") || !strings.Contains(res, `"limit_mb":16384`) || !strings.Contains(res, `"autoscale":false`) {
		t.Fatalf("stored: %s %s", net, res)
	}
	job, _ := AllowDomains("github.com")
	if err := c.ApplyJobPolicy(ctx, "bitc-a", job); err != nil {
		t.Fatal(err)
	}
	if net, _ = f.Policies("bitc-a"); strings.Contains(net, "ubuntu") {
		t.Fatalf("job policy did not replace the build policy: %s", net)
	}
}

func TestPolicyMismatchIsTransientAndBlocksExec(t *testing.T) {
	f, c := newFake(t, spritesfake.Options{})
	ctx := context.Background()
	_, _ = c.CreateSprite(ctx, "bitc-a", nil)
	f.Behave("policy-get-mismatch")
	build, _ := AllowDomains("github.com")
	err := c.ApplyBuildPolicies(ctx, "bitc-a", build, Resources{LimitMB: 1024})
	var pm *PolicyMismatchError
	var tr *provider.ErrTransient
	if !errors.As(err, &pm) || !errors.As(err, &tr) {
		t.Fatalf("got %v", err)
	}
	if _, err := c.Exec(ctx, "bitc-a", ExecRequest{Cmd: []string{"true"}}); !errors.Is(err, ErrNotProvisioned) {
		t.Fatalf("exec after a mismatch: %v", err)
	}
}

func TestPoliciesRefuseWildcardsAndBadValues(t *testing.T) {
	_, c := newFake(t, spritesfake.Options{})
	ctx := context.Background()
	_, _ = c.CreateSprite(ctx, "bitc-a", nil)
	for _, d := range []string{"*", "", "1.2.3.4", "include", "a b.com", "github", "*.", "*.*.com"} {
		if _, err := AllowDomains(d); err == nil {
			t.Errorf("AllowDomains accepted %q", d)
		}
	}
	if _, err := AllowDomains("*.githubusercontent.com"); err != nil {
		t.Errorf("a wildcard subdomain is explicit enough: %v", err)
	}
	bad := NetworkPolicy{Rules: []NetRule{{Domain: "*", Action: "allow"}}}
	if err := c.ApplyBuildPolicies(ctx, "bitc-a", bad, Resources{LimitMB: 1}); kind(err) != "ineligible" {
		t.Errorf("wildcard policy: %v", err)
	}
	ok, _ := AllowDomains("github.com")
	if err := c.ApplyBuildPolicies(ctx, "bitc-a", ok, Resources{LimitMB: 0}); kind(err) != "ineligible" {
		t.Errorf("zero memory: %v", err)
	}
}

func TestListCheckpointsIncludesAutoOnes(t *testing.T) {
	f, c := newFake(t, spritesfake.Options{})
	ctx := context.Background()
	_, _ = c.CreateSprite(ctx, "bitc-a", nil)
	f.AutoCheckpoint("bitc-a")
	cps, err := c.ListCheckpoints(ctx, "bitc-a")
	if err != nil || len(cps) != 1 || !cps[0].Auto || cps[0].ID != "auto-1" {
		t.Fatalf("%+v %v", cps, err)
	}
	if _, err := c.ListCheckpoints(ctx, "bitc-zzz"); kind(err) != "notfound" {
		t.Fatalf("missing sprite: %v", err)
	}
}

func TestNoAnswerIsTransientAndASlowServerTimesOut(t *testing.T) {
	f := spritesfake.New(t, spritesfake.Options{})
	c, _ := New(Options{APIBase: "http://127.0.0.1:1", Org: f.Org(), Token: f.Token(), Timeout: time.Second})
	if _, err := c.GetSprite(context.Background(), "bitc-a"); kind(err) != "transient" {
		t.Fatalf("refused connection: %v", err)
	}
	if _, err := c.CreateSprite(context.Background(), "bitc-a", nil); kind(err) != "preattach" {
		t.Fatalf("a create with no answer is ambiguous: %v", err)
	}
	var buf bytes.Buffer
	_ = buf
}

func TestRedirectsAreNotFollowedAndTheTokenStaysOnTheFirstRequest(t *testing.T) {
	var hits atomic.Int32
	var leaked atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Header.Get("Authorization") != "" {
			leaked.Store(true)
		}
	}))
	defer target.Close()
	var origAuth atomic.Bool
	orig := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origAuth.Store(r.Header.Get("Authorization") != "")
		http.Redirect(w, r, target.URL+"/v1/sprites", http.StatusTemporaryRedirect)
	}))
	defer orig.Close()
	c, err := New(Options{APIBase: orig.URL, Org: "acme", Token: "acme/secret-token", Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.ListSprites(context.Background(), "bitc-")
	if err == nil {
		t.Fatal("a redirect answer was accepted")
	}
	if hits.Load() != 0 || leaked.Load() {
		t.Fatalf("the redirect was followed (%d hits, token leaked %v)", hits.Load(), leaked.Load())
	}
	if !origAuth.Load() {
		t.Fatal("the original request carried no token")
	}
}

func TestListConnections(t *testing.T) {
	f, c := newFake(t, spritesfake.Options{})
	ctx := context.Background()
	got, err := c.ListConnections(ctx)
	if err != nil || len(got) != 0 {
		t.Fatalf("none: %v %v", got, err)
	}
	f.AddConnection("github-app")
	f.AddConnection("another")
	got, err = c.ListConnections(ctx)
	if err != nil || len(got) != 2 || got[0] != "another" || got[1] != "github-app" {
		t.Fatalf("two: %v %v", got, err)
	}
	f.Behave("401")
	var un *provider.ErrUnavailable
	if _, err := c.ListConnections(ctx); !errors.As(err, &un) {
		t.Fatalf("401: %v", err)
	}
}

func TestOrgMismatchNamesTheSprite(t *testing.T) {
	var fo provider.ForeignOrg
	err := error(&OrgMismatchError{Name: "bitc-x", Got: "other", Want: "mine"})
	if !errors.As(err, &fo) {
		t.Fatal("not a ForeignOrg")
	}
	if n, o := fo.ForeignOrg(); n != "bitc-x" || o != "other" {
		t.Fatalf("%q %q", n, o)
	}
}
