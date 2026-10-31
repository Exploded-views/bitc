// Linux-only: runs a real job on spritesfake.NewJob, which needs a Linux process tree and does not build elsewhere. Also holds the tests that run the real agent (BuildBitc), which is Linux-only by design.

package sprites

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Exploded-views/bitc/internal/agent"
	"github.com/Exploded-views/bitc/internal/provider"
	"github.com/Exploded-views/bitc/internal/provider/sprites/spritesfake"
)

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
