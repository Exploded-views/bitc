package run

import (
	"context"
	"os"
	"syscall"
	"testing"

	"github.com/Exploded-views/bitc/internal/exit"
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
