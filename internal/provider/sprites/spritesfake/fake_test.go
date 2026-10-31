package spritesfake_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Exploded-views/bitc/internal/provider"
	"github.com/Exploded-views/bitc/internal/provider/sprites"
	"github.com/Exploded-views/bitc/internal/provider/sprites/spritesfake"
)

const sha = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

func raw(t *testing.T, f *spritesfake.Fake, method, path, body string, hdr map[string]string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, f.URL()+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+f.Token())
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func client(t *testing.T, f *spritesfake.Fake) *sprites.Client {
	t.Helper()
	c, err := sprites.New(sprites.Options{APIBase: f.URL(), Org: f.Org(), Token: f.Token(), Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// ready creates name and sets every policy so the client may exec and attach.
func ready(t *testing.T, c *sprites.Client, name string) {
	t.Helper()
	ctx := context.Background()
	if _, err := c.CreateSprite(ctx, name, nil); err != nil {
		t.Fatal(err)
	}
	p, _ := sprites.AllowDomains("github.com")
	if err := c.ApplyBuildPolicies(ctx, name, p, sprites.Resources{LimitMB: 1024}); err != nil {
		t.Fatal(err)
	}
	if err := c.ApplyJobPolicy(ctx, name, p); err != nil {
		t.Fatal(err)
	}
}

func waitFor(t *testing.T, what string, d time.Duration, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestFakeRefusesAWrongToken(t *testing.T) {
	f := spritesfake.New(t, spritesfake.Options{})
	req, _ := http.NewRequest("GET", f.URL()+"/v1/sprites", nil)
	req.Header.Set("Authorization", "Bearer sandbox-org/not-the-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("wrong token: %d", resp.StatusCode)
	}
	resp, _ = http.Get(f.URL() + "/v1/sprites")
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("no token: %d", resp.StatusCode)
	}
}

func TestFakeRefusesUnknownFieldsAndParameters(t *testing.T) {
	f := spritesfake.New(t, spritesfake.Options{})
	for _, c := range []struct {
		method, path, body string
	}{
		{"POST", "/v1/sprites", `{"name":"a","bogus":1}`},
		{"POST", "/v1/sprites", `{"name":"a"} {"x":1}`},
		{"POST", "/v1/sprites", `{"name":"UPPER"}`},
		{"GET", "/v1/sprites?nonsense=1", ``},
		{"GET", "/v1/sprites?max_results=zero", ``},
		{"GET", "/v1/sprites?continuation_token=***", ``},
		{"GET", "/v1/nothing", ``},
		{"GET", "/v2/sprites", ``},
	} {
		if code, _ := raw(t, f, c.method, c.path, c.body, nil); code != 400 && code != 404 {
			t.Errorf("%s %s %s: %d", c.method, c.path, c.body, code)
		}
	}
	if code, _ := raw(t, f, "POST", "/v1/sprites", `{"name":"a"}`, nil); code != 201 {
		t.Fatalf("create: %d", code)
	}
	for _, c := range []struct{ path, body string }{
		{"/policy/network", `{"rules":[{"domain":"github.com","action":"allow","extra":1}]}`},
		{"/policy/network", `{"include":"defaults"}`},
		{"/policy/network", `{"rules":[{"domain":"*","action":"allow"}]}`},
		{"/policy/network", `{"rules":[{"domain":"github.com","action":"deny"}]}`},
		{"/policy/network", `{}`},
		{"/policy/resources", `{"memory":{"limit_mb":1024,"autoscale":true}}`},
		{"/policy/resources", `{"memory":{"limit_mb":0,"autoscale":false}}`},
		{"/policy/resources", `{"memory":{"limit_mb":1,"autoscale":false,"x":1}}`},
		{"/policy/other", `{}`},
	} {
		if code, _ := raw(t, f, "POST", "/v1/sprites/a"+c.path, c.body, nil); code != 400 && code != 404 {
			t.Errorf("%s %s: %d", c.path, c.body, code)
		}
	}
	if code, _ := raw(t, f, "GET", "/v1/sprites/a/exec?cmd=true&sneaky=1", ``, nil); code != 400 {
		t.Errorf("unknown exec parameter: %d", code)
	}
	if code, _ := raw(t, f, "GET", "/v1/sprites/a/exec", ``, nil); code != 400 {
		t.Errorf("exec without cmd: %d", code)
	}
	if code, _ := raw(t, f, "GET", "/v1/sprites/a/exec?cmd=true&path=other", ``, nil); code != 400 {
		t.Errorf("exec with a path that is not cmd[0]: %d", code)
	}
}

func TestFakeServesTheOrganizationOnEveryResponse(t *testing.T) {
	f := spritesfake.New(t, spritesfake.Options{})
	code, body := raw(t, f, "POST", "/v1/sprites", `{"name":"a"}`, nil)
	if code != 201 || !strings.Contains(body, `"organization":"sandbox-org"`) {
		t.Fatalf("create: %d %s", code, body)
	}
	_, one := raw(t, f, "GET", "/v1/sprites/a", ``, nil)
	_, all := raw(t, f, "GET", "/v1/sprites", ``, nil)
	for what, b := range map[string]string{"get": one, "list": all} {
		if !strings.Contains(b, `"organization":"sandbox-org"`) {
			t.Errorf("%s lacks the organization: %s", what, b)
		}
	}
	f.Behave("wrong-org")
	_, one = raw(t, f, "GET", "/v1/sprites/a", ``, nil)
	if !strings.Contains(one, `"organization":"other-org"`) {
		t.Errorf("wrong-org: %s", one)
	}
}

func TestFakeListPaginatesByToken(t *testing.T) {
	f := spritesfake.New(t, spritesfake.Options{PageSize: 2})
	for _, n := range []string{"a", "b", "c", "d", "e"} {
		raw(t, f, "POST", "/v1/sprites", `{"name":"`+n+`"}`, nil)
	}
	var seen []string
	tok := ""
	for i := 0; i < 10; i++ {
		_, b := raw(t, f, "GET", "/v1/sprites?continuation_token="+tok, ``, nil)
		var page struct {
			Sprites []struct{ Name string }
			HasMore bool   `json:"has_more"`
			Next    string `json:"next_continuation_token"`
		}
		if err := json.Unmarshal([]byte(b), &page); err != nil {
			t.Fatal(err, b)
		}
		if len(page.Sprites) > 2 {
			t.Fatalf("page of %d", len(page.Sprites))
		}
		for _, s := range page.Sprites {
			seen = append(seen, s.Name)
		}
		if !page.HasMore {
			break
		}
		tok = page.Next
	}
	if strings.Join(seen, "") != "abcde" {
		t.Fatalf("saw %v", seen)
	}
}

func TestFakeStoresAndServesPolicies(t *testing.T) {
	f := spritesfake.New(t, spritesfake.Options{})
	raw(t, f, "POST", "/v1/sprites", `{"name":"a"}`, nil)
	if code, _ := raw(t, f, "POST", "/v1/sprites/a/policy/network", `{"rules":[{"domain":"github.com","action":"allow"}]}`, nil); code != 200 {
		t.Fatalf("set: %d", code)
	}
	_, got := raw(t, f, "GET", "/v1/sprites/a/policy/network", ``, nil)
	if !strings.Contains(got, "github.com") {
		t.Fatalf("get: %s", got)
	}
	f.Behave("policy-get-mismatch")
	_, got = raw(t, f, "GET", "/v1/sprites/a/policy/network", ``, nil)
	if strings.Contains(got, "github.com") {
		t.Fatalf("mismatch behaviour served the stored policy: %s", got)
	}
	_, got = raw(t, f, "GET", "/v1/sprites/a/policy/network", ``, nil)
	if !strings.Contains(got, "github.com") {
		t.Fatalf("mismatch is once: %s", got)
	}
}

func TestFakeBehavioursAnswerAsDocumented(t *testing.T) {
	for _, c := range []struct {
		behave string
		code   int
		text   string
	}{
		{"429-create", 429, "sprite_creation_rate_limited"},
		{"concurrent-limit", 403, "concurrent_sprite_limit_exceeded"},
		{"max-sprites-exceeded", 400, "max sprites per org exceeded"},
		{"403-restricted", 403, "Your account is restricted"},
		{"create-500-nothing", 500, ""},
		{"create-then-500", 500, ""},
	} {
		f := spritesfake.New(t, spritesfake.Options{})
		f.Behave(c.behave)
		code, body := raw(t, f, "POST", "/v1/sprites", `{"name":"a"}`, nil)
		if code != c.code || !strings.Contains(body, c.text) {
			t.Errorf("%s: %d %s", c.behave, code, body)
		}
		if code, _ := raw(t, f, "POST", "/v1/sprites", `{"name":"b"}`, nil); code != 201 {
			t.Errorf("%s fires once, then the next create answers %d", c.behave, code)
		}
	}
	f := spritesfake.New(t, spritesfake.Options{})
	f.Behave("create-then-500")
	raw(t, f, "POST", "/v1/sprites", `{"name":"a"}`, nil)
	if got := f.Live(); len(got) != 1 {
		t.Errorf("create-then-500 must leave the Sprite: %v", got)
	}
	f = spritesfake.New(t, spritesfake.Options{})
	f.Behave("create-500-nothing")
	raw(t, f, "POST", "/v1/sprites", `{"name":"a"}`, nil)
	if got := f.Live(); len(got) != 0 {
		t.Errorf("create-500-nothing made a Sprite: %v", got)
	}
}

func TestFakeCreateThenLookupFailsAnswersTheNextLookupWith500(t *testing.T) {
	f := spritesfake.New(t, spritesfake.Options{})
	f.Behave("create-then-500-lookup-fails")
	raw(t, f, "POST", "/v1/sprites", `{"name":"a"}`, nil)
	if code, _ := raw(t, f, "GET", "/v1/sprites/a", ``, nil); code != 500 {
		t.Errorf("lookup: %d", code)
	}
	if code, _ := raw(t, f, "GET", "/v1/sprites", ``, nil); code != 500 {
		t.Errorf("list: %d", code)
	}
	if code, _ := raw(t, f, "GET", "/v1/sprites/a", ``, nil); code != 200 {
		t.Errorf("the second lookup answers %d", code)
	}
}

// BehaveN fails the test on a name it does not know, so a typo cannot arm nothing silently.
func TestFakeKnowsItsBehaviourNames(t *testing.T) {
	f := spritesfake.New(t, spritesfake.Options{})
	if !f.KnownBehaviour("429-create") || f.KnownBehaviour("429-craete") {
		t.Fatal("KnownBehaviour does not tell the documented names from typos")
	}
}

func TestFakeRecordsEveryCallAndTheExecStdin(t *testing.T) {
	f := spritesfake.New(t, spritesfake.Options{Exec: func(x spritesfake.ExecContext) int { _, _ = io.Copy(io.Discard, x.Stdin); return 0 }})
	c := client(t, f)
	ready(t, c, "a")
	if _, err := c.Run(context.Background(), "a", sprites.ExecRequest{Cmd: []string{"true"}}, []byte("stdin-bytes")); err != nil {
		t.Fatal(err)
	}
	var methods []string
	for _, call := range f.Calls() {
		methods = append(methods, call.Method+" "+call.Path)
	}
	want := "POST /v1/sprites,POST /v1/sprites/a/policy/network,GET /v1/sprites/a/policy/network,POST /v1/sprites/a/policy/resources,GET /v1/sprites/a/policy/resources,POST /v1/sprites/a/policy/network,GET /v1/sprites/a/policy/network,GET /v1/sprites/a/exec"
	if strings.Join(methods, ",") != want {
		t.Fatalf("calls:\n%s\nwant:\n%s", strings.Join(methods, "\n"), strings.ReplaceAll(want, ",", "\n"))
	}
	ex := f.Execs()
	if len(ex) != 1 || string(ex[0].Stdin) != "stdin-bytes" || ex[0].Query.Get("cmd") != "true" {
		t.Fatalf("%+v", ex)
	}
}

func TestFakeAutoCheckpointsExcludeRunAndDieWithTheSprite(t *testing.T) {
	f := spritesfake.New(t, spritesfake.Options{})
	c := client(t, f)
	ready(t, c, "a")
	ov := f.Overlay("a")
	for p, content := range map[string]string{"work/code.txt": "code", "run/j1/key": "SECRET-KEY-CANARY", "run/.ssh-j1/id": "SECRET-KEY-CANARY", "tools/t/bin": "tool"} {
		_ = os.MkdirAll(filepath.Dir(filepath.Join(ov, p)), 0o755)
		if err := os.WriteFile(filepath.Join(ov, p), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	dir := f.AutoCheckpoint("a")
	if dir == "" || filepath.Base(dir) != "auto-1" {
		t.Fatalf("checkpoint dir %q", dir)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "work/code.txt")); err != nil || string(b) != "code" {
		t.Fatalf("the checkpoint lacks overlay content: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "run")); err == nil {
		t.Fatal("the tmpfs /run was checkpointed")
	}
	if _, err := os.Stat(filepath.Join(dir, ".sprite", "api.sock")); err == nil {
		t.Fatal("a socket was checkpointed")
	}
	cps, err := c.ListCheckpoints(context.Background(), "a")
	if err != nil || len(cps) != 1 || !cps[0].Auto {
		t.Fatalf("%+v %v", cps, err)
	}
	if err := c.DeleteSprite(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); err == nil {
		t.Fatal("deleting the Sprite left its checkpoints")
	}
	if _, err := os.Stat(ov); err == nil {
		t.Fatal("deleting the Sprite left its overlay")
	}
}

func TestFakeTakesAutoCheckpointsOnATimer(t *testing.T) {
	f := spritesfake.New(t, spritesfake.Options{AutoCheckpointEvery: 100 * time.Millisecond, IdleAfter: time.Second})
	c := client(t, f)
	ready(t, c, "a")
	waitFor(t, "two automatic checkpoints", 10*time.Second, func() bool { return len(f.Checkpoints("a")) >= 2 })
}

func TestFakeDeleteBehaviourLeavesTheSprite(t *testing.T) {
	f := spritesfake.New(t, spritesfake.Options{})
	raw(t, f, "POST", "/v1/sprites", `{"name":"a"}`, nil)
	f.Behave("delete-500")
	if code, _ := raw(t, f, "DELETE", "/v1/sprites/a", ``, nil); code != 500 || len(f.Live()) != 1 {
		t.Fatalf("delete-500: %d %v", code, f.Live())
	}
}

func TestFakeServesLoopbackOnly(t *testing.T) {
	f := spritesfake.New(t, spritesfake.Options{})
	if !strings.HasPrefix(f.URL(), "http://127.0.0.1:") {
		t.Fatalf("the fake listens on %s", f.URL())
	}
}

func drain(s *sprites.Session) { _, _ = io.Copy(io.Discard, s.Stdout()) }

func TestFakePolicy500FiresOnceOnAPolicyCall(t *testing.T) {
	f := spritesfake.New(t, spritesfake.Options{})
	c := client(t, f)
	if _, err := c.CreateSprite(context.Background(), "a", nil); err != nil {
		t.Fatal(err)
	}
	f.Behave("policy-500")
	p, _ := sprites.AllowDomains("github.com")
	err := c.ApplyBuildPolicies(context.Background(), "a", p, sprites.Resources{LimitMB: 1024})
	var tr *provider.ErrTransient
	if !errors.As(err, &tr) {
		t.Fatalf("the first policy call: %v", err)
	}
	if err := c.ApplyBuildPolicies(context.Background(), "a", p, sprites.Resources{LimitMB: 1024}); err != nil {
		t.Fatalf("the second: %v", err)
	}
}

func TestFakeLaxNamesAcceptsWhatTheClientCallsInvalid(t *testing.T) {
	f := spritesfake.New(t, spritesfake.Options{})
	if code, _ := raw(t, f, "POST", "/v1/sprites", `{"name":"Bad_a.b"}`, nil); code != 400 {
		t.Fatalf("unarmed create of an invalid name = %d, want 400", code)
	}
	f.BehaveN("lax-names", -1)
	if code, _ := raw(t, f, "POST", "/v1/sprites", `{"name":"Bad_a.b"}`, nil); code != 201 && code != 200 {
		t.Fatalf("lax create of an invalid name = %d, want 2xx", code)
	}
	if code, _ := raw(t, f, "DELETE", "/v1/sprites/Bad_a.b", ``, nil); code != 204 {
		t.Fatalf("delete of that name = %d, want 204", code)
	}
}

// A hook's output written after its session was cut fails: the write reports the dropped
// connection to the command instead of vanishing.
func TestFakeHookOutputAfterACutSessionFails(t *testing.T) {
	started, release, wrote := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	f := spritesfake.New(t, spritesfake.Options{Exec: func(e spritesfake.ExecContext) int {
		close(started)
		<-release
		_, err := e.Stdout.Write([]byte("late\n"))
		wrote <- err
		return 0
	}})
	c := client(t, f)
	ready(t, c, "a")
	s, err := c.Exec(context.Background(), "a", sprites.ExecRequest{Cmd: []string{"true"}, Stdin: true})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	<-started
	f.CutSessions("a")
	close(release)
	if err := <-wrote; err == nil {
		t.Fatal("a write after the session was cut succeeded")
	}
}
