package sprites

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Exploded-views/bitc/internal/agent"
	"github.com/Exploded-views/bitc/internal/provider"
	"github.com/Exploded-views/bitc/internal/provider/sprites/spritesfake"
)

const testSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// provisioned creates name and sets both policies, so it may be exec'd; withJob also sets the job
// policy, so it may be attached.
func provisioned(t *testing.T, c *Client, name string, withJob bool) {
	t.Helper()
	ctx := context.Background()
	if _, err := c.CreateSprite(ctx, name, nil); err != nil {
		t.Fatal(err)
	}
	build, _ := AllowDomains("github.com", "archive.ubuntu.com")
	if err := c.ApplyBuildPolicies(ctx, name, build, Resources{LimitMB: 2048}); err != nil {
		t.Fatal(err)
	}
	if withJob {
		job, _ := AllowDomains("github.com")
		if err := c.ApplyJobPolicy(ctx, name, job); err != nil {
			t.Fatal(err)
		}
	}
}

func TestAgentCmdShape(t *testing.T) {
	cmd, err := AgentCmd(testSHA, true, "--boot-stdin")
	if err != nil || len(cmd) != 3 || cmd[0] != "sh" || cmd[1] != "-c" {
		t.Fatalf("%q %v", cmd, err)
	}
	want := `echo "` + testSHA + `  /opt/bitc/bin/bitc" | sha256sum -c - >/dev/null && exec sudo -n /opt/bitc/bin/bitc __agent --boot-stdin`
	if cmd[2] != want {
		t.Fatalf("got  %s\nwant %s", cmd[2], want)
	}
	cmd, _ = AgentCmd(testSHA, false, "tail", "j1", "0", "0", "0")
	if strings.Contains(cmd[2], "sudo") || !strings.HasSuffix(cmd[2], "exec /opt/bitc/bin/bitc __agent tail j1 0 0 0") {
		t.Fatalf("no-sudo form: %s", cmd[2])
	}
}

func TestAgentCmdRefusesShellSyntax(t *testing.T) {
	for _, a := range []string{"j1; rm -rf /", "$(id)", "a b", "`x`", "a|b", "a&&b", "", "../x", "x\ny", "'q'"} {
		if _, err := AgentCmd(testSHA, true, "read", a); err == nil {
			t.Errorf("argument %q accepted", a)
		}
	}
	for _, sha := range []string{"", "abc", strings.ToUpper(testSHA), testSHA + "0", `x"; id; echo "`} {
		if _, err := AgentCmd(sha, true, "list"); err == nil {
			t.Errorf("sha %q accepted", sha)
		}
	}
}

func TestExecQueryCarriesTheCommandAndNothingElse(t *testing.T) {
	r := ExecRequest{Cmd: []string{"sh", "-c", "true"}, Dir: "/work", Stdin: true, MaxRunAfterDisconnect: 90*time.Minute + 200*time.Millisecond}
	q, err := r.query()
	if err != nil {
		t.Fatal(err)
	}
	want := url.Values{"cmd": {"sh", "-c", "true"}, "path": {"sh"}, "tty": {"false"}, "stdin": {"true"}, "dir": {"/work"}, "max_run_after_disconnect": {"5401s"}}
	if q.Encode() != want.Encode() {
		t.Fatalf("got %s\nwant %s", q.Encode(), want.Encode())
	}
	if _, err := (ExecRequest{}).query(); err == nil {
		t.Fatal("empty command accepted")
	}
	if _, err := (ExecRequest{Cmd: []string{strings.Repeat("a", 9000)}}).query(); err == nil {
		t.Fatal("oversized command accepted")
	}
}

func TestExecGuardRefusesAnUnverifiedSprite(t *testing.T) {
	f, c := newFake(t, spritesfake.Options{})
	ctx := context.Background()
	if _, err := c.CreateSprite(ctx, "bitc-a", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Exec(ctx, "bitc-a", ExecRequest{Cmd: []string{"true"}}); !errors.Is(err, ErrNotProvisioned) {
		t.Fatalf("exec before the policies: %v", err)
	}
	build, _ := AllowDomains("github.com")
	if err := c.ApplyBuildPolicies(ctx, "bitc-a", build, Resources{LimitMB: 1024}); err != nil {
		t.Fatal(err)
	}
	// an exec is allowed now; the attach still needs the job policy
	_, err := c.Attach(ctx, "bitc-a", AttachOptions{AgentSHA: testSHA, Sudo: true, Payload: []byte("x")})
	if !errors.Is(err, ErrNotProvisioned) {
		t.Fatalf("attach before the job policy: %v", err)
	}
	for _, call := range f.Calls() {
		if strings.HasSuffix(call.Path, "/exec") {
			t.Fatalf("an exec request reached the API despite the guard: %+v", call)
		}
	}
	// a Sprite found by lookup (a restarted host) is not guarded: tail and teardown must work
	f.AddSprite("bitc-old", f.Org(), time.Now())
	if _, err := c.Exec(ctx, "bitc-old", ExecRequest{Cmd: []string{"true"}}); errors.Is(err, ErrNotProvisioned) {
		t.Fatalf("a Sprite the client did not create was guarded: %v", err)
	}
}

func TestApplyJobPolicyAloneDoesNotUnlockExecOrAttach(t *testing.T) {
	f, c := newFake(t, spritesfake.Options{})
	ctx := context.Background()
	if _, err := c.CreateSprite(ctx, "bitc-a", nil); err != nil {
		t.Fatal(err)
	}
	job, _ := AllowDomains("github.com")
	if err := c.ApplyJobPolicy(ctx, "bitc-a", job); !errors.Is(err, ErrNotProvisioned) {
		t.Fatalf("ApplyJobPolicy on a Sprite without build policies: %v", err)
	}
	if _, err := c.Exec(ctx, "bitc-a", ExecRequest{Cmd: []string{"true"}}); !errors.Is(err, ErrNotProvisioned) {
		t.Fatalf("exec: %v", err)
	}
	if _, err := c.Attach(ctx, "bitc-a", AttachOptions{AgentSHA: testSHA, Sudo: true, Payload: []byte("x")}); !errors.Is(err, ErrNotProvisioned) {
		t.Fatalf("attach: %v", err)
	}
	for _, call := range f.Calls() {
		if strings.HasSuffix(call.Path, "/exec") || strings.Contains(call.Path, "/policy/") {
			t.Fatalf("a request reached the API: %s %s", call.Method, call.Path)
		}
	}
}

func TestRunCollectsExitStdoutAndStderr(t *testing.T) {
	_, c := newFake(t, spritesfake.Options{Exec: func(x spritesfake.ExecContext) int {
		in, _ := io.ReadAll(x.Stdin)
		_, _ = x.Stdout.Write(append([]byte("got:"), in...))
		_, _ = x.Stderr.Write([]byte("warn"))
		return 7
	}})
	provisioned(t, c, "bitc-a", false)
	res, err := c.Run(context.Background(), "bitc-a", ExecRequest{Cmd: []string{"cat"}}, []byte("hello"))
	if err != nil || res.Exit != 7 || string(res.Stdout) != "got:hello" || string(res.Stderr) != "warn" {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestSessionStreamsLargeOutputAndInput(t *testing.T) {
	big := bytes.Repeat([]byte("0123456789abcdef"), 1<<16) // 1 MiB
	_, c := newFake(t, spritesfake.Options{Exec: func(x spritesfake.ExecContext) int {
		in, _ := io.ReadAll(x.Stdin)
		_, _ = x.Stdout.Write(in)
		return 0
	}})
	provisioned(t, c, "bitc-a", false)
	res, err := c.Run(context.Background(), "bitc-a", ExecRequest{Cmd: []string{"cat"}}, big)
	if err != nil || res.Exit != 0 || !bytes.Equal(res.Stdout, big) {
		t.Fatalf("exit %d, %d bytes back of %d, err %v", res.Exit, len(res.Stdout), len(big), err)
	}
}

func TestExecWithAnHTTPErrorIsAClassifiedError(t *testing.T) {
	f, c := newFake(t, spritesfake.Options{})
	provisioned(t, c, "bitc-a", false)
	f.Behave("exec-upgrade-5xx")
	_, err := c.Exec(context.Background(), "bitc-a", ExecRequest{Cmd: []string{"true"}})
	if kind(err) != "transient" {
		t.Fatalf("got %s (%v)", kind(err), err)
	}
	f.Behave("exec-upgrade-4xx")
	_, err = c.Exec(context.Background(), "bitc-a", ExecRequest{Cmd: []string{"true"}})
	if kind(err) != "ineligible" {
		t.Fatalf("a 4xx upgrade: got %s (%v)", kind(err), err)
	}
	_, err = c.Exec(context.Background(), "bitc-zzz", ExecRequest{Cmd: []string{"true"}})
	if kind(err) != "notfound" {
		t.Fatalf("missing Sprite: %s (%v)", kind(err), err)
	}
}

func TestRunOfAStreamThatEndsWithoutAnExitIsTransient(t *testing.T) {
	f, c := newFake(t, spritesfake.Options{})
	provisioned(t, c, "bitc-a", false)
	f.Behave("drop-exec-socket")
	_, err := c.Run(context.Background(), "bitc-a", ExecRequest{Cmd: []string{"true"}}, nil)
	if !errors.Is(err, ErrStreamEnded) && kind(err) != "transient" {
		t.Fatalf("got %v", err)
	}
}

func TestAttachClassification(t *testing.T) {
	job := spritesfake.NewJob(t, "echo hi", "")
	type want struct{ kind, inner string }
	cases := []struct {
		behave string
		want   want
	}{
		// an HTTP error before 101 is definitely refused: pre-attach, the class of the status
		{"exec-upgrade-4xx", want{"preattach", "ineligible"}},
		{"exec-upgrade-5xx", want{"preattach", "transient"}},
		// no answer after the request was written: the agent may have started: attached
		{"exec-upgrade-timeout", want{"attached", ""}},
		// 101, then the socket is cut: attached (the payload may have been delivered)
		{"drop-exec-socket", want{"attached", ""}},
	}
	for _, tc := range cases {
		t.Run(tc.behave, func(t *testing.T) {
			f, c := newFake(t, spritesfake.Options{Bin: spritesfake.BuildBitc(t)})
			c.handshake = 400 * time.Millisecond
			provisioned(t, c, "bitc-a", true)
			f.Behave(tc.behave)
			s, err := c.Attach(context.Background(), "bitc-a", AttachOptions{AgentSHA: testSHA, Sudo: true, Deadline: time.Now().Add(time.Hour), Payload: job.Frame})
			if err == nil {
				// the cut can land after the payload was written: the session then ends without an exit
				_, werr := s.Wait()
				if !errors.Is(werr, ErrStreamEnded) || tc.want.kind != "attached" {
					t.Fatalf("attach succeeded (wait: %v), want %s", werr, tc.want.kind)
				}
				return
			}
			got := kind(err)
			if got != tc.want.kind || innerKind(err) != orNone(tc.want.inner) {
				t.Fatalf("got %s/%s (%v), want %s/%s", got, innerKind(err), err, tc.want.kind, tc.want.inner)
			}
			var at *provider.ErrAttached
			var pa *provider.ErrPreAttach
			if (errors.As(err, &at) && at.ID != "bitc-a") || (errors.As(err, &pa) && pa.ID != "bitc-a") {
				t.Fatalf("the error does not name the Sprite: %+v %+v", at, pa)
			}
		})
	}
}

func TestAttachWithNothingListeningIsPreAttach(t *testing.T) {
	f, c := newFake(t, spritesfake.Options{})
	provisioned(t, c, "bitc-a", true)
	c.base.Host = "127.0.0.1:1" // nothing sent: a refused connection
	_ = f
	_, err := c.Attach(context.Background(), "bitc-a", AttachOptions{AgentSHA: testSHA, Sudo: true, Payload: []byte("x")})
	if kind(err) != "preattach" || innerKind(err) != "transient" {
		t.Fatalf("got %s/%s (%v)", kind(err), innerKind(err), err)
	}
}

func TestAttachRefusesBadInput(t *testing.T) {
	_, c := newFake(t, spritesfake.Options{})
	provisioned(t, c, "bitc-a", true)
	for name, o := range map[string]AttachOptions{
		"no payload": {AgentSHA: testSHA},
		"bad sha":    {AgentSHA: "zz", Payload: []byte("x")},
		"too big":    {AgentSHA: testSHA, Payload: make([]byte, agent.MaxBootPayload+9)},
	} {
		if _, err := c.Attach(context.Background(), "bitc-a", o); kind(err) != "ineligible" {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// The boot payload (spec, keys, deploy key) reaches the agent through exec stdin only. It is in no
// URL, header, REST body or recorded query, and the exec URL holds nothing secret.
func TestAttachDeliversThePayloadOnStdinOnly(t *testing.T) {
	const key = "TEST-DEPLOY-KEY-CANARY-9f3a"
	job := spritesfake.NewJob(t, "echo ran", key)
	f, c := newFake(t, spritesfake.Options{Bin: spritesfake.BuildBitc(t), AgentSHA: testSHA})
	provisioned(t, c, "bitc-a", true)
	s := mustAttach(t, c, "bitc-a", AttachOptions{AgentSHA: testSHA, Sudo: true, Deadline: time.Now().Add(time.Hour), Payload: job.Frame})
	defer s.Close()
	go drain(s)
	if code, err := s.Wait(); err != nil || code != 0 {
		t.Fatalf("agent exit %d, %v\n%s", code, err, s.Stderr())
	}
	execs := f.Execs()
	if len(execs) != 1 || !bytes.Equal(execs[0].Stdin, job.Frame) {
		t.Fatalf("recorded stdin differs from the payload (%d execs)", len(execs))
	}
	q := execs[0].Query
	if q.Get("stdin") != "true" || !strings.HasPrefix(q.Get("max_run_after_disconnect"), "4") && !strings.HasPrefix(q.Get("max_run_after_disconnect"), "3") {
		t.Fatalf("query: %v", q)
	}
	for _, call := range f.Calls() {
		blob := call.Path + "?" + call.RawQuery + string(call.Body)
		if strings.Contains(blob, key) || strings.Contains(blob, job.ID+"-") && strings.Contains(blob, "mac_key") {
			t.Errorf("job input in %s %s", call.Method, call.Path)
		}
	}
	if strings.Contains(execs[0].Query.Encode(), key) {
		t.Fatal("deploy key in the exec query")
	}
	// the agent ran the job in the Sprite's overlay
	if _, err := os.Stat(filepath.Join(f.Overlay("bitc-a"), "run", job.ID, "result.json")); err != nil {
		t.Fatalf("no result.json: %v", err)
	}
}

func TestTailAndReadAgainstARealJob(t *testing.T) {
	job := spritesfake.NewJob(t, "echo to-out; echo to-err >&2; exit 3", "")
	f, c := newFake(t, spritesfake.Options{Bin: spritesfake.BuildBitc(t)})
	provisioned(t, c, "bitc-a", true)
	ctx := context.Background()
	s := mustAttach(t, c, "bitc-a", AttachOptions{AgentSHA: testSHA, Sudo: true, Deadline: time.Now().Add(time.Hour), Payload: job.Frame})
	go drain(s)
	_, _ = s.Wait()
	got := map[string][]byte{}
	var done bool
	err := c.Tail(ctx, "bitc-a", testSHA, job.ID, nil, func(fr agent.Frame) error {
		if fr.File != "" {
			got[fr.File] = append(got[fr.File], fr.Data...)
		}
		done = done || fr.Done
		return nil
	})
	if err != nil || !done {
		t.Fatalf("tail: done=%v err=%v", done, err)
	}
	if !strings.Contains(string(got["out.log"]), "to-out") || !strings.Contains(string(got["err.log"]), "to-err") || !strings.Contains(string(got["agent.log"]), "__BITC_") {
		t.Fatalf("frames: out=%q err=%q agent=%q", got["out.log"], got["err.log"], got["agent.log"])
	}
	// resume: from the end of out.log, nothing more of out.log arrives
	var more int
	_ = c.Tail(ctx, "bitc-a", testSHA, job.ID, map[string]int64{"out.log": int64(len(got["out.log"]))}, func(fr agent.Frame) error {
		if fr.File == "out.log" {
			more += len(fr.Data)
		}
		return nil
	})
	if more != 0 {
		t.Fatalf("resumed tail repeated %d bytes of out.log", more)
	}
	var buf bytes.Buffer
	code, err := c.Read(ctx, "bitc-a", testSHA, job.ID, "result.json", 0, &buf)
	if err != nil || code != 0 {
		t.Fatalf("read: %d %v", code, err)
	}
	var res map[string]any
	if json.Unmarshal(buf.Bytes(), &res) != nil {
		t.Fatalf("result.json: %q", buf.Bytes())
	}
	// agent verdicts come back as exit statuses, not errors
	for file, want := range map[string]int{"nope.txt": 64, "agent.log": 65} {
		off := int64(0)
		if want == 65 {
			off = 1 << 40
		}
		code, err := c.Read(ctx, "bitc-a", testSHA, job.ID, file, off, io.Discard)
		if err != nil || code != want {
			t.Errorf("read %s@%d: code %d err %v, want %d", file, off, code, err, want)
		}
	}
	if code, err := c.Read(ctx, "bitc-a", testSHA, "j-missing", "result.json", 0, io.Discard); err != nil || code != 66 {
		t.Errorf("missing job: %d %v", code, err)
	}
	_ = f
}

func TestTailRefusesBadArguments(t *testing.T) {
	_, c := newFake(t, spritesfake.Options{})
	provisioned(t, c, "bitc-a", false)
	ctx := context.Background()
	if err := c.Tail(ctx, "bitc-a", testSHA, "j1; id", nil, func(agent.Frame) error { return nil }); kind(err) != "ineligible" {
		t.Errorf("bad job id: %v", err)
	}
	if err := c.Tail(ctx, "bitc-a", testSHA, "j1", map[string]int64{"out.log": -1}, func(agent.Frame) error { return nil }); kind(err) != "ineligible" {
		t.Errorf("negative offset: %v", err)
	}
	if _, err := c.Read(ctx, "bitc-a", testSHA, "j1", "result.json", -5, io.Discard); kind(err) != "ineligible" {
		t.Errorf("negative read offset: %v", err)
	}
}

func TestTailThatIsCutIsErrStreamEnded(t *testing.T) {
	job := spritesfake.NewJob(t, "sleep 30", "")
	f, c := newFake(t, spritesfake.Options{Bin: spritesfake.BuildBitc(t)})
	provisioned(t, c, "bitc-a", true)
	ctx := context.Background()
	s := mustAttach(t, c, "bitc-a", AttachOptions{AgentSHA: testSHA, Sudo: true, Deadline: time.Now().Add(time.Hour), Payload: job.Frame})
	go drain(s)
	// wait for the agent to be running its job
	deadline := time.Now().Add(20 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(f.Overlay("bitc-a"), "run", job.ID, "agent.pid")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("agent did not start")
		}
		time.Sleep(50 * time.Millisecond)
	}
	errc := make(chan error, 1)
	go func() {
		errc <- c.Tail(ctx, "bitc-a", testSHA, job.ID, nil, func(agent.Frame) error { return nil })
	}()
	time.Sleep(500 * time.Millisecond)
	f.ColdDrop("bitc-a")
	select {
	case err := <-errc:
		if !errors.Is(err, ErrStreamEnded) {
			t.Fatalf("got %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("tail did not end after the cold drop")
	}
}

func TestReadFramesDecodesNDJSON(t *testing.T) {
	in := `{"file":"out.log","off":5,"data":"aGk="}` + "\n" + `{"hb":true,"alive":true}` + "\n" + `{"done":true}` + "\n"
	var got []agent.Frame
	err := ReadFrames(strings.NewReader(in), func(f agent.Frame) error { got = append(got, f); return nil })
	if err != nil || len(got) != 3 || string(got[0].Data) != "hi" || got[0].Off != 5 || got[1].Alive == nil || !*got[1].Alive || !got[2].Done {
		t.Fatalf("%+v %v", got, err)
	}
	if err := ReadFrames(strings.NewReader("not json\n"), func(agent.Frame) error { return nil }); err == nil {
		t.Fatal("garbage accepted")
	}
	stop := errors.New("stop")
	if err := ReadFrames(strings.NewReader(in), func(agent.Frame) error { return stop }); !errors.Is(err, stop) {
		t.Fatalf("callback error lost: %v", err)
	}
}

func drain(s *Session) { _, _ = io.Copy(io.Discard, s.Stdout()) }

// mustAttach attaches and fails the test unless the session is live.
func mustAttach(t *testing.T, c *Client, name string, o AttachOptions) *Session {
	t.Helper()
	s, err := c.Attach(context.Background(), name, o)
	if err != nil {
		t.Fatal(err)
	}
	if s == nil || s.conn == nil {
		t.Fatal("Attach returned a session without a connection")
	}
	return s
}

func TestNoSudoOptionIsClientWideForTailAndRead(t *testing.T) {
	f := spritesfake.New(t, spritesfake.Options{Bin: spritesfake.BuildBitc(t)})
	ctx := context.Background()
	for _, noSudo := range []bool{false, true} {
		c, err := New(Options{APIBase: f.URL(), Org: f.Org(), Token: f.Token(), Timeout: 5 * time.Second, HandshakeTimeout: 5 * time.Second, NoSudo: noSudo})
		if err != nil {
			t.Fatal(err)
		}
		name := "bitc-sudo-" + strings.ToLower(strconv.FormatBool(noSudo))
		provisioned(t, c, name, false)
		_ = c.Tail(ctx, name, testSHA, "j9", nil, func(agent.Frame) error { return nil })
		_, _ = c.Read(ctx, name, testSHA, "j9", "out.log", 0, io.Discard)
		if c.Sudo() == noSudo {
			t.Fatalf("Sudo() = %v with NoSudo = %v", c.Sudo(), noSudo)
		}
		n := 0
		for _, e := range f.Execs() {
			if e.Sprite != name {
				continue
			}
			n++
			if has := strings.Contains(strings.Join(e.Query["cmd"], " "), "sudo"); has == noSudo {
				t.Errorf("NoSudo=%v: exec %d command %q", noSudo, n, e.Query["cmd"])
			}
		}
		if n != 2 {
			t.Errorf("NoSudo=%v: %d agent execs, want 2", noSudo, n)
		}
	}
}

// A process that exits before it has read all of its stdin (an agent that refuses its input) cuts
// the connection under the writer; the exit status it sent first is the result.
func TestRunKeepsTheExitOfAnExecThatEndsBeforeReadingStdin(t *testing.T) {
	_, c := newFake(t, spritesfake.Options{Exec: func(x spritesfake.ExecContext) int {
		_, _ = x.Stdout.Write([]byte("refused: nothing read\n"))
		return 3
	}})
	provisioned(t, c, "bitc-a", false)
	res, err := c.Run(context.Background(), "bitc-a", ExecRequest{Cmd: []string{"refuse"}}, bytes.Repeat([]byte("x"), 8<<20))
	if err != nil || res.Exit != 3 || string(res.Stdout) != "refused: nothing read\n" {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestTailOfAJobThatIsNotThereYetIsATypedExit66(t *testing.T) {
	_, c := newFake(t, spritesfake.Options{Bin: spritesfake.BuildBitc(t)})
	provisioned(t, c, "bitc-a", false)
	err := c.Tail(context.Background(), "bitc-a", testSHA, "j1", nil, func(agent.Frame) error { return nil })
	var te *TailExitError
	var tr *provider.ErrTransient
	if !errors.As(err, &te) || te.Code != 66 || !errors.As(err, &tr) {
		t.Fatalf("%T %v", err, err)
	}
}

// A frame handler that fails ends the tail at once, even though the agent's tail is still running
// on the Sprite (it would only stop at the job's end).
func TestTailReturnsAtOnceWhenTheFrameHandlerFails(t *testing.T) {
	job := spritesfake.NewJob(t, "sleep 60", "")
	f, c := newFake(t, spritesfake.Options{Bin: spritesfake.BuildBitc(t)})
	provisioned(t, c, "bitc-a", true)
	s := mustAttach(t, c, "bitc-a", AttachOptions{AgentSHA: testSHA, Sudo: true, Deadline: time.Now().Add(time.Hour), Payload: job.Frame})
	go drain(s)
	for i := 0; i < 500; i++ { // the agent makes the job's directory a moment after the attach
		if _, err := os.Stat(filepath.Join(f.Overlay("bitc-a"), "run", job.ID, "agent.log")); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	boom := errors.New("handler failed")
	got := make(chan error, 1)
	go func() {
		got <- c.Tail(context.Background(), "bitc-a", testSHA, job.ID, nil, func(agent.Frame) error { return boom })
	}()
	select {
	case err := <-got:
		if !errors.Is(err, boom) {
			t.Fatalf("%v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Tail did not return after its handler failed")
	}
}
