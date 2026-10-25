package spritesfake_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Exploded-views/bitc/internal/agent"
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

func procState(pid int) string {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return "gone"
	}
	i := bytes.LastIndexByte(b, ')')
	return strings.Fields(string(b[i+1:]))[0]
}

func agentPID(t *testing.T, f *spritesfake.Fake, name, job string) int {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(filepath.Join(f.Overlay(name), "run", job, "agent.pid"))
		if n, perr := strconv.Atoi(strings.Fields(string(b) + " x")[0]); err == nil && perr == nil {
			return n
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("the agent never wrote agent.pid")
	return 0
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

// attachJob starts script as a job on a ready Sprite and returns the live session and the job.
func attachJob(t *testing.T, f *spritesfake.Fake, c *sprites.Client, name, script string) (*sprites.Session, spritesfake.Job) {
	t.Helper()
	ready(t, c, name)
	job := spritesfake.NewJob(t, script, "")
	s, err := c.Attach(context.Background(), name, sprites.AttachOptions{AgentSHA: sha, Sudo: true, Deadline: time.Now().Add(time.Hour), Payload: job.Frame})
	if err != nil {
		t.Fatal(err)
	}
	return s, job
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

func TestFakeRunsAgentCommandsOnlyThroughTheShaCheck(t *testing.T) {
	f := spritesfake.New(t, spritesfake.Options{Bin: spritesfake.BuildBitc(t), AgentSHA: sha})
	c := client(t, f)
	ready(t, c, "a")
	ctx := context.Background()
	// not an agent command and no hook: exit 127
	res, err := c.Run(ctx, "a", sprites.ExecRequest{Cmd: []string{"rm", "-rf", "/"}}, nil)
	if err != nil || res.Exit != 127 {
		t.Fatalf("plain command: %+v %v", res, err)
	}
	// an agent command without the sha check: refused the same way
	res, err = c.Run(ctx, "a", sprites.ExecRequest{Cmd: []string{"sudo", "-n", "/opt/bitc/bin/bitc", "__agent", "list"}}, nil)
	if err != nil || res.Exit != 127 {
		t.Fatalf("agent command without the sha check: %+v %v", res, err)
	}
	// through the check with the wrong sha: the check fails, the agent never runs
	cmd, _ := sprites.AgentCmd(strings.Repeat("c", 64), true, "list")
	res, err = c.Run(ctx, "a", sprites.ExecRequest{Cmd: cmd}, nil)
	if err != nil || res.Exit != 1 || !strings.Contains(string(res.Stderr), "FAILED") {
		t.Fatalf("wrong sha: %+v %v", res, err)
	}
	// with the right sha it runs (list prints the jobs it knows: none)
	cmd, _ = sprites.AgentCmd(sha, true, "list")
	res, err = c.Run(ctx, "a", sprites.ExecRequest{Cmd: cmd}, nil)
	if err != nil || res.Exit != 0 {
		t.Fatalf("right sha: %+v %v", res, err)
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

func TestFakeRunsAJobInTheOverlayAndServesItsResults(t *testing.T) {
	f := spritesfake.New(t, spritesfake.Options{Bin: spritesfake.BuildBitc(t)})
	c := client(t, f)
	s, job := attachJob(t, f, c, "a", "echo hello-from-job")
	go drain(s)
	if code, err := s.Wait(); err != nil || code != 0 {
		t.Fatalf("agent: %d %v %s", code, err, s.Stderr())
	}
	var buf bytes.Buffer
	code, err := c.Read(context.Background(), "a", sha, job.ID, "out.log", 0, &buf)
	if err != nil || code != 0 || !strings.Contains(buf.String(), "hello-from-job") {
		t.Fatalf("read out.log: %d %v %q", code, err, buf.String())
	}
	if _, err := os.Stat(filepath.Join(f.Overlay("a"), "run", job.ID, "result.json")); err != nil {
		t.Fatal(err)
	}
}

// An idle Sprite pauses (warm, processes stopped); an exec wakes it and the job goes on.
func TestFakePausesAnIdleSpriteAndWakesItOnExec(t *testing.T) {
	f := spritesfake.New(t, spritesfake.Options{Bin: spritesfake.BuildBitc(t), IdleAfter: 300 * time.Millisecond})
	c := client(t, f)
	s, job := attachJob(t, f, c, "a", "sleep 60")
	pid := agentPID(t, f, "a", job.ID)
	_ = s.Close() // the exec connection ends; the agent lives on (max_run_after_disconnect)
	waitFor(t, "the Sprite to go warm", 10*time.Second, func() bool { return f.Status("a") == "warm" })
	waitFor(t, "the agent to be stopped", 5*time.Second, func() bool { return procState(pid) == "T" })
	// any exec wakes it
	if _, err := c.Run(context.Background(), "a", sprites.ExecRequest{Cmd: []string{"true"}}, nil); err != nil {
		t.Fatal(err)
	}
	if st := f.Status("a"); st != "running" {
		t.Fatalf("status after the wake: %s", st)
	}
	waitFor(t, "the agent to run again", 5*time.Second, func() bool { return procState(pid) != "T" })
}

// A live Task is activity: the Sprite stays running and the agent is not stopped.
func TestFakeTaskKeepsASpriteRunning(t *testing.T) {
	f := spritesfake.New(t, spritesfake.Options{Bin: spritesfake.BuildBitc(t), IdleAfter: 300 * time.Millisecond})
	c := client(t, f)
	s, job := attachJob(t, f, c, "a", "sleep 60")
	pid := agentPID(t, f, "a", job.ID)
	_ = s.Close()
	hc := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", f.SockPath("a"))
	}}}
	post := func(path, body string) (int, string) {
		resp, err := hc.Post("http://sprite"+path, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, body := post("/v1/tasks", `{"expire":"1s","bogus":1}`); code != 400 {
		t.Fatalf("a Task body with an unknown field: %d %s", code, body)
	}
	code, body := post("/v1/tasks", `{"expire":"1s"}`)
	var task struct{ ID string }
	if code != 200 || json.Unmarshal([]byte(body), &task) != nil || task.ID == "" {
		t.Fatalf("create task: %d %s", code, body)
	}
	time.Sleep(700 * time.Millisecond) // more than twice IdleAfter
	if st := f.Status("a"); st != "running" || procState(pid) == "T" {
		t.Fatalf("with a live Task: status %s, agent state %s", st, procState(pid))
	}
	// renewing keeps it alive; letting it lapse pauses the Sprite
	if code, _ := post("/v1/tasks/"+task.ID, `{"expire":"1s"}`); code != 200 {
		t.Fatalf("renew: %d", code)
	}
	waitFor(t, "the lapsed Task to let the Sprite go warm", 10*time.Second, func() bool { return f.Status("a") == "warm" })
}

func TestFakeColdDropKillsTheJobAndCutsTheSessions(t *testing.T) {
	f := spritesfake.New(t, spritesfake.Options{Bin: spritesfake.BuildBitc(t)})
	c := client(t, f)
	s, job := attachJob(t, f, c, "a", "sleep 60")
	pid := agentPID(t, f, "a", job.ID)
	go drain(s)
	f.ColdDrop("a")
	if _, err := s.Wait(); err == nil {
		t.Fatal("the session survived a cold drop with a clean exit")
	}
	waitFor(t, "the agent to die", 5*time.Second, func() bool { s := procState(pid); return s == "gone" || s == "Z" })
	if st := f.Status("a"); st != "cold" {
		t.Fatalf("status %s", st)
	}
	// the disk stays, and an exec wakes the Sprite (to a fresh, empty process table)
	if _, err := os.Stat(filepath.Join(f.Overlay("a"), "run", job.ID)); err != nil {
		t.Fatalf("the overlay lost the job dir: %v", err)
	}
	if _, err := c.Run(context.Background(), "a", sprites.ExecRequest{Cmd: []string{"true"}}, nil); err != nil {
		t.Fatal(err)
	}
	if f.Status("a") != "running" {
		t.Fatalf("a cold Sprite did not wake on exec: %s", f.Status("a"))
	}
}

func TestFakeColdDropMidJobBehaviour(t *testing.T) {
	f := spritesfake.New(t, spritesfake.Options{Bin: spritesfake.BuildBitc(t)})
	c := client(t, f)
	ready(t, c, "a")
	f.Behave("cold-drop-mid-job")
	job := spritesfake.NewJob(t, "sleep 60", "")
	s, err := c.Attach(context.Background(), "a", sprites.AttachOptions{AgentSHA: sha, Sudo: true, Deadline: time.Now().Add(time.Hour), Payload: job.Frame})
	if err != nil {
		t.Fatal(err)
	}
	go drain(s)
	if _, err := s.Wait(); err == nil {
		t.Fatal("the session ended cleanly")
	}
	waitFor(t, "the Sprite to go cold", 5*time.Second, func() bool { return f.Status("a") == "cold" })
}

func TestFakeColdAfterWarmDropsTheSprite(t *testing.T) {
	f := spritesfake.New(t, spritesfake.Options{Bin: spritesfake.BuildBitc(t), IdleAfter: 200 * time.Millisecond, ColdAfter: 400 * time.Millisecond})
	c := client(t, f)
	s, job := attachJob(t, f, c, "a", "sleep 60")
	pid := agentPID(t, f, "a", job.ID)
	_ = s.Close()
	waitFor(t, "warm", 10*time.Second, func() bool { return f.Status("a") == "warm" })
	waitFor(t, "cold", 10*time.Second, func() bool { return f.Status("a") == "cold" })
	waitFor(t, "the agent to die", 5*time.Second, func() bool { s := procState(pid); return s == "gone" || s == "Z" })
}

// Exec outlives its connection for max_run_after_disconnect only; without it the process dies
// with the connection (assumed worst case).
func TestFakeExecOutlivesItsConnectionOnlyForMaxRun(t *testing.T) {
	f := spritesfake.New(t, spritesfake.Options{Bin: spritesfake.BuildBitc(t)})
	c := client(t, f)
	s, job := attachJob(t, f, c, "a", "sleep 60")
	pid := agentPID(t, f, "a", job.ID)
	_ = s.Close()
	time.Sleep(300 * time.Millisecond)
	if st := procState(pid); st == "gone" || st == "Z" {
		t.Fatalf("the attach agent died with its connection (state %s) despite max_run_after_disconnect", st)
	}
	// a second job started without the parameter dies at disconnect
	job2 := spritesfake.NewJobNamed(t, "j2", "sleep 60", "")
	cmd, _ := sprites.AgentCmd(sha, true, "--boot-stdin")
	s2, err := c.Exec(context.Background(), "a", sprites.ExecRequest{Cmd: cmd, Stdin: true})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = s2.Write(job2.Frame)
	_ = s2.CloseStdin()
	go drain(s2)
	pid2 := agentPID(t, f, "a", job2.ID)
	_ = s2.Close()
	waitFor(t, "the second agent to die with its connection", 5*time.Second, func() bool { s := procState(pid2); return s == "gone" || s == "Z" })
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

func TestFakeDeleteKillsTheProcessesAndAnswers404Twice(t *testing.T) {
	f := spritesfake.New(t, spritesfake.Options{Bin: spritesfake.BuildBitc(t)})
	c := client(t, f)
	s, job := attachJob(t, f, c, "a", "sleep 60")
	pid := agentPID(t, f, "a", job.ID)
	go drain(s)
	if code, _ := raw(t, f, "DELETE", "/v1/sprites/a", ``, nil); code != 204 {
		t.Fatalf("delete: %d", code)
	}
	waitFor(t, "the agent to die", 5*time.Second, func() bool { s := procState(pid); return s == "gone" || s == "Z" })
	if code, _ := raw(t, f, "DELETE", "/v1/sprites/a", ``, nil); code != 404 {
		t.Fatalf("second delete: %d", code)
	}
	if code, _ := raw(t, f, "GET", "/v1/sprites/a/exec?cmd=true", ``, nil); code != 404 {
		t.Fatalf("exec on a deleted Sprite: %d", code)
	}
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

// The provisioning command is `bitc __agent provision` with a ProvisionSpec frame on stdin. The
// hooks answer like the real agent's failure classes (75 infra, 71 repo, last line `setup step
// <name> failed: ...`); with no hook armed the real agent provisions under the overlay.
func TestFakeProvisionHooksAnswerLikeTheAgent(t *testing.T) {
	f := spritesfake.New(t, spritesfake.Options{Bin: spritesfake.BuildBitc(t), AgentSHA: sha})
	c := client(t, f)
	ready(t, c, "a")
	frame, err := agent.ProvisionSpec{}.Frame()
	if err != nil {
		t.Fatal(err)
	}
	cmd, _ := sprites.AgentCmd(sha, true, "provision")
	run := func() (int, string) {
		res, err := c.Run(context.Background(), "a", sprites.ExecRequest{Cmd: cmd, Stdin: true}, frame)
		if err != nil {
			t.Fatal(err)
		}
		return res.Exit, strings.TrimSpace(string(res.Stdout))
	}
	for _, h := range []struct {
		behave string
		exit   int
		line   string
	}{{"provision-infra-fails", 75, "setup step packages failed: "}, {"provision-repo-fails", 71, "setup step tl failed: "}} {
		f.Behave(h.behave)
		code, out := run()
		lines := strings.Split(out, "\n")
		if code != h.exit || !strings.HasPrefix(lines[len(lines)-1], h.line) {
			t.Errorf("%s: exit %d, output %q", h.behave, code, out)
		}
	}
	if code, out := run(); code != 0 || !strings.Contains(out, "provision: done") {
		t.Fatalf("no hook armed: exit %d, output %q", code, out)
	}
	if _, err := os.Stat(filepath.Join(f.Overlay("a"), "var", "lib", "bitc", "provisioned.json")); err != nil {
		t.Fatalf("the real agent left no marker under the overlay: %v", err)
	}
}

// attach-4xx and attach-timeout hit only the exec that starts `__agent --boot-stdin`, so the
// provisioning execs before it are untouched.
func TestFakeAttachBehavioursSkipOtherExecs(t *testing.T) {
	f := spritesfake.New(t, spritesfake.Options{Bin: spritesfake.BuildBitc(t), Exec: func(spritesfake.ExecContext) int { return 0 }})
	c, err := sprites.New(sprites.Options{APIBase: f.URL(), Org: f.Org(), Token: f.Token(), Timeout: 5 * time.Second, HandshakeTimeout: 400 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ready(t, c, "a")
	ctx := context.Background()
	job := spritesfake.NewJob(t, "true", "")
	f.Behave("attach-4xx")
	if res, err := c.Run(ctx, "a", sprites.ExecRequest{Cmd: []string{"true"}}, nil); err != nil || res.Exit != 0 {
		t.Fatalf("a plain exec was hit by attach-4xx: %+v %v", res, err)
	}
	_, err = c.Attach(ctx, "a", sprites.AttachOptions{AgentSHA: sha, Sudo: true, Payload: job.Frame})
	var pa *provider.ErrPreAttach
	if !errors.As(err, &pa) {
		t.Fatalf("attach with attach-4xx: %v", err)
	}
	f.Behave("attach-timeout")
	if res, err := c.Run(ctx, "a", sprites.ExecRequest{Cmd: []string{"true"}}, nil); err != nil || res.Exit != 0 {
		t.Fatalf("a plain exec was hit by attach-timeout: %+v %v", res, err)
	}
	_, err = c.Attach(ctx, "a", sprites.AttachOptions{AgentSHA: sha, Sudo: true, Payload: job.Frame})
	var at *provider.ErrAttached
	if !errors.As(err, &at) {
		t.Fatalf("attach with attach-timeout: %v", err)
	}
}

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

// SessionsDontHold models the worst case of probe 9.17: an open exec session does not keep a
// Sprite running, only a Task does. PauseCount and TaskLog outlive the Sprite.
func TestFakeSessionsDontHoldASprite(t *testing.T) {
	f := spritesfake.New(t, spritesfake.Options{Bin: spritesfake.BuildBitc(t), IdleAfter: 250 * time.Millisecond, SessionsDontHold: true})
	c := client(t, f)
	s, _ := attachJob(t, f, c, "a", "sleep 60")
	defer s.Close() // the session stays open the whole time
	waitFor(t, "an open session alone to let the Sprite go warm", 10*time.Second, func() bool { return f.Status("a") == "warm" })
	if n := f.PauseCount("a"); n < 1 {
		t.Fatalf("PauseCount %d", n)
	}
	// a Task holds it, and the Task's log outlives the Sprite
	f.Wake("a")
	hc := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", f.SockPath("a"))
	}}}
	resp, err := hc.Post("http://sprite/v1/tasks", "application/json", strings.NewReader(`{"expire":"5s"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	before := f.PauseCount("a")
	time.Sleep(800 * time.Millisecond)
	if st := f.Status("a"); st != "running" || f.PauseCount("a") != before {
		t.Fatalf("with a Task: status %s, pauses %d -> %d", st, before, f.PauseCount("a"))
	}
	if log := f.TaskLog("a"); len(log) != 1 || log[0] != "create 5s" {
		t.Fatalf("task log %v", log)
	}
	raw(t, f, "DELETE", "/v1/sprites/a", ``, nil)
	if log := f.TaskLog("a"); len(log) != 1 {
		t.Fatalf("the task log is gone with the Sprite: %v", log)
	}
}

func TestFakeDropTailCutsOnlyTailExecs(t *testing.T) {
	f := spritesfake.New(t, spritesfake.Options{Bin: spritesfake.BuildBitc(t), AgentSHA: sha})
	c := client(t, f)
	ready(t, c, "a")
	f.Behave("drop-tail")
	ctx := context.Background()
	cmd, _ := sprites.AgentCmd(sha, true, "list")
	if res, err := c.Run(ctx, "a", sprites.ExecRequest{Cmd: cmd}, nil); err != nil || res.Exit != 0 {
		t.Fatalf("a list exec was cut by drop-tail: %+v %v", res, err)
	}
	err := c.Tail(ctx, "a", sha, "j1", nil, func(agent.Frame) error { return nil })
	if !errors.Is(err, sprites.ErrStreamEnded) {
		t.Fatalf("the first tail: %v", err)
	}
	// it fires once: the next tail reaches the agent (which has no such job: exit 66)
	err = c.Tail(ctx, "a", sha, "j1", nil, func(agent.Frame) error { return nil })
	var tr *provider.ErrTransient
	if err == nil || errors.Is(err, sprites.ErrStreamEnded) || !errors.As(err, &tr) {
		t.Fatalf("the second tail: %v", err)
	}
}

func TestFakeAttachTruncateMakesTheAgentRefuseItsPayload(t *testing.T) {
	f := spritesfake.New(t, spritesfake.Options{Bin: spritesfake.BuildBitc(t), AgentSHA: sha})
	c := client(t, f)
	ready(t, c, "a")
	f.Behave("attach-truncate")
	job := spritesfake.NewJob(t, "true", "")
	s, err := c.Attach(context.Background(), "a", sprites.AttachOptions{AgentSHA: sha, Sudo: true, Deadline: time.Now().Add(time.Hour), Payload: job.Frame})
	if err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(s.Stdout())
	code, _ := s.Wait()
	if code != 65 || !strings.Contains(string(out), "refused: truncated payload") {
		t.Fatalf("exit %d, output %q", code, out)
	}
	if _, err := os.Stat(filepath.Join(f.Overlay("a"), "run", job.ID)); err == nil {
		t.Fatal("the refused payload started a job")
	}
}

func TestFakeCutSessionsDropsTheConnectionsButNotTheJob(t *testing.T) {
	f := spritesfake.New(t, spritesfake.Options{Bin: spritesfake.BuildBitc(t)})
	c := client(t, f)
	s, job := attachJob(t, f, c, "a", "sleep 60")
	pid := agentPID(t, f, "a", job.ID)
	f.CutSessions("a")
	done := make(chan struct{})
	go func() { _, _ = io.Copy(io.Discard, s.Stdout()); _, _ = s.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the session was not cut")
	}
	if st := procState(pid); st == "gone" || st == "Z" {
		t.Fatalf("the agent died with the connection (state %s) although it has max_run_after_disconnect", st)
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
