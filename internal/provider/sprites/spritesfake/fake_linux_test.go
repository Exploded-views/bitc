// Linux-only: uses spritesfake.NewJob (Linux-only) and reads /proc. Also holds the tests that run the real agent (BuildBitc), which is Linux-only by design.

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
