package run

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"

	"github.com/Exploded-views/bitc/internal/config"
	"github.com/Exploded-views/bitc/internal/exit"
	"github.com/Exploded-views/bitc/internal/redact"
)

// A signal the supervisor caught before runLocal's handler existed still interrupts the run.
func TestSignalCtxTakesOverAPendingSignal(t *testing.T) {
	pending := make(chan os.Signal, 1)
	pending <- syscall.SIGTERM
	ctx, stop := signalCtx(context.Background(), cancels(Invocation{Detached: true}), pending)
	if ctx.Err() == nil {
		t.Fatal("a pending signal did not cancel")
	}
	if sig := stop(); sig != syscall.SIGTERM {
		t.Fatal(sig)
	}
	_ = stop() // idempotent: failed() may call it after the gap check did
}

// Spec 7: a detached job survives SIGHUP.
func TestADetachedRunIsNotCancelledByHangup(t *testing.T) {
	for _, s := range cancels(Invocation{Detached: true}) {
		if s == syscall.SIGHUP {
			t.Fatal("SIGHUP cancels a detached run")
		}
	}
	if len(cancels(Invocation{})) != 4 {
		t.Fatal("a foreground run cancels on INT, TERM, HUP and QUIT")
	}
}

func TestSignalOfReadsTheCancelCause(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	if signalOf(ctx) != 0 {
		t.Fatal("not cancelled yet")
	}
	cancel(Signalled(syscall.SIGTERM))
	if signalOf(ctx) != syscall.SIGTERM {
		t.Fatal(signalOf(ctx))
	}
}

func TestOutcomeOfMapsBitcErrors(t *testing.T) {
	for err, want := range map[error]string{
		exit.Starved("p", "queue"): "starved",
		exit.Refused("p", "no"):    "refused",
		exit.Configf("x"):          "config",
		exit.Unavailablef("hook"):  "unavailable",
		exit.Internal("bug"):       "internal",
	} {
		r := resultOf(err, "p", "j")
		if r.Origin != "bitc" || r.Outcome != want || r.ExitCode != exit.Code(err) {
			t.Errorf("%v: %+v", err, r)
		}
	}
}

// A signal that signalCtx's channel took but its goroutine has not yet recorded (a loaded host)
// must still reach Exec: the run ends 128+N, never runs to completion. The pre_run hook raises
// the signal after the handlers exist; the seam holds the record back until Exec has started.
func TestASignalTakenButNotYetRecordedReachesExec(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "bitc.toml")
	hook := fmt.Sprintf(`hooks.pre_run = { argv = ["sh", "-c", "kill -TERM %d"] }`, os.Getpid())
	if err := os.WriteFile(cfg, []byte("version = 1\n[pools.lint]\n"+hook+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	var once sync.Once
	free := func() { once.Do(func() { close(release) }) }
	signalRecorded = func() { <-release }
	beforeRecord = free // Exec has started: the check before it has passed
	t.Cleanup(func() { signalRecorded, beforeRecord = nil, nil; free() })
	devnull, _ := os.Open(os.DevNull)
	t.Cleanup(func() { devnull.Close() })
	inv := Invocation{Flags: config.Flags{ConfigFile: cfg, StateDir: filepath.Join(dir, "state")},
		Args: []string{"lint", "--", "sleep", "2"}, Stdin: devnull, Stdout: devnull, Stderr: devnull,
		Env: []string{"PATH=" + os.Getenv("PATH"), "HOME=" + dir}, Cwd: dir}
	red := redact.New()
	p, err := Prepare(inv, red)
	if err != nil {
		t.Fatal(err)
	}
	out := Execute(context.Background(), inv, p, devnull)
	if out.Code != 143 || out.Result.Outcome != "interrupted" {
		t.Fatalf("the command ran to completion after a cancel: %+v", out)
	}
}
