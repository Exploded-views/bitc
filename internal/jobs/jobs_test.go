package jobs

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Exploded-views/bitc/internal/proc"
	"github.com/Exploded-views/bitc/internal/proctest"
	"github.com/Exploded-views/bitc/internal/state"
)

func TestTerminateAddsNewlineOnlyWhenNeeded(t *testing.T) {
	for in, want := range map[string]string{
		"":        "__EXIT__ 3\n",
		"out\n":   "out\n__EXIT__ 3\n",
		"out":     "out\n__EXIT__ 3\n",
		"out\r":   "out\r\n__EXIT__ 3\n",
		"out\x00": "out\x00\n__EXIT__ 3\n",
	} {
		p := filepath.Join(t.TempDir(), "log")
		_ = os.WriteFile(p, []byte(in), 0o600)
		f, _ := os.OpenFile(p, os.O_WRONLY|os.O_APPEND, 0)
		if err := Terminate(f)(3); err != nil {
			t.Fatal(err)
		}
		f.Close()
		if b, _ := os.ReadFile(p); string(b) != want {
			t.Errorf("%q -> %q, want %q", in, b, want)
		}
	}
}

func TestLastExit(t *testing.T) {
	cases := map[string]int{
		"__EXIT__ 75\nretry\n__EXIT__ 0\n": 0,
		"x __EXIT__ 9\n__EXIT__ 4\n":       4,
		"a\x00b\n__EXIT__ 7\n":             7,
	}
	for in, want := range cases {
		if got, ok := LastExit([]byte(in)); !ok || got != want {
			t.Errorf("%q: %d %v", in, got, ok)
		}
	}
	for _, in := range []string{"", "x __EXIT__ 9\n", "a\x00__EXIT__ 3\n", "__EXIT__\n"} {
		if _, ok := LastExit([]byte(in)); ok {
			t.Errorf("%q: found an exit line", in)
		}
	}
}

func TestTail(t *testing.T) {
	var b strings.Builder
	for i := 1; i <= 100; i++ {
		b.WriteString(strings.Repeat("x", i%3) + "\n")
	}
	if got := Tail([]byte(b.String()), 60); len(got) != 60 {
		t.Fatal(len(got))
	}
	if got := Tail([]byte("a\nb"), 5); strings.Join(got, ",") != "a,b" {
		t.Fatal(got)
	}
}

func TestBumpMovesTheOldGeneration(t *testing.T) {
	l, _ := state.Open(filepath.Join(t.TempDir(), "s"))
	r := Open(l, "abcd1234", "j1")
	g1, err := r.Bump()
	if err != nil || g1 != 1 {
		t.Fatal(g1, err)
	}
	_ = os.WriteFile(filepath.Join(r.Dir, "out.log"), []byte("one\n"), 0o600)
	g2, _ := r.Bump()
	if g2 != 2 {
		t.Fatal(g2)
	}
	if b, _ := os.ReadFile(filepath.Join(r.Dir, "prev-1", "out.log")); string(b) != "one\n" {
		t.Fatalf("%q", b)
	}
	if _, err := os.Stat(filepath.Join(r.Dir, "out.log")); !os.IsNotExist(err) {
		t.Fatal("old log left in place")
	}
}

func TestWaitReportsAnInternalErrorResult(t *testing.T) {
	l, _ := state.Open(filepath.Join(t.TempDir(), "s"))
	r := Open(l, "abcd1234", "j1")
	gen, err := r.Bump()
	if err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(t.TempDir(), "j1.log")
	_ = os.WriteFile(log, []byte("boom\n__EXIT__ 69\n"), 0o600)
	if err := r.WriteSpec(Spec{Gen: gen, Log: log}); err != nil {
		t.Fatal(err)
	}
	if err := r.WriteResult(gen, map[string]any{"origin": "bitc", "outcome": "internal", "exit_code": 69}); err != nil {
		t.Fatal(err)
	}
	out, code := Wait(WaitOptions{Record: &r, Timeout: time.Second, Poll: 10 * time.Millisecond})
	if code != 69 || !strings.HasPrefix(out, "RESULT internal error\n") {
		t.Fatalf("%d %q", code, out)
	}
}

// detach and the supervisor both update status.json; neither order loses the supervisor's
// write, and detach never moves the state back.
func TestUpdateStatusMergesInEitherOrder(t *testing.T) {
	l, _ := state.Open(filepath.Join(t.TempDir(), "s"))
	r := Open(l, "abcd1234", "j1")
	gen, _ := r.Bump()
	_ = r.UpdateStatus(gen, func(s *Status) { s.State, s.Pid, s.Start = "running", 7, 9 }) // supervisor first
	_ = r.UpdateStatus(gen, func(s *Status) {                                              // detach, late
		if s.Pid == 0 {
			s.Pid, s.Start = 7, 9
		}
	})
	if st, _ := r.ReadStatus(gen); st.State != "running" || st.Gen != gen || st.Pid != 7 {
		t.Fatalf("%+v", st)
	}
	_ = r.UpdateStatus(gen-1, func(s *Status) { s.State = "ended" }) // a superseded generation
	if st, _ := r.ReadStatus(gen); st.State != "running" {
		t.Fatalf("%+v", st)
	}
}

func TestSupervisorDeadTreatsAZombieAsDead(t *testing.T) {
	self, err := proc.ReadStat(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if SupervisorDead(Status{Pid: os.Getpid(), Start: self.Start}) || SupervisorDead(Status{}) {
		t.Fatal("a live supervisor, or one with no pid yet, is not dead")
	}
	if !SupervisorDead(Status{Pid: os.Getpid(), Start: self.Start + 1}) {
		t.Fatal("a reused pid is dead")
	}
	z := exec.Command("true")
	if err := z.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = z.Wait() }()
	for i := 0; i < 200 && !proctest.Gone(z.Process.Pid); i++ {
		time.Sleep(10 * time.Millisecond)
	}
	zs, err := proc.ReadStat(z.Process.Pid) // unreaped: state Z
	if err != nil || zs.State != 'Z' {
		t.Fatalf("%+v %v", zs, err)
	}
	if !SupervisorDead(Status{Pid: z.Process.Pid, Start: zs.Start}) {
		t.Fatal("a zombie supervisor is dead")
	}
}
