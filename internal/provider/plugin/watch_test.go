package plugin

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Exploded-views/bitc/internal/provider"
)

func launched(t *testing.T, r *rig) provider.Handle {
	t.Helper()
	r.start()
	h, err := r.p.Launch(context.Background(), r.job())
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	return h
}

func TestWatchTurnsMarkersWithTheJobNonceIntoPhasesAndSetupLines(t *testing.T) {
	r := newRig(t)
	h := launched(t, r)
	s := newSink()
	res, err := r.p.Watch(context.Background(), h, s)
	if err != nil || res.Exit != 0 || !res.Began {
		t.Fatalf("Watch = %+v, %v", res, err)
	}
	if got := strings.Join(s.phases, ","); got != "begun" {
		t.Errorf("phases = %q (BOOT is not a phase; BEGIN is begun)", got)
	}
	if len(s.setup) != 1 || s.setup[0] != "installing packages" {
		t.Errorf("setup lines = %q: only unmarked lines before BEGIN, never markers", s.setup)
	}
	if s.out[provider.Stdout].String() != "hello\n" || s.out[provider.Stderr].String() != "warn\n" {
		t.Errorf("out = %q / %q", s.out[provider.Stdout], s.out[provider.Stderr])
	}
}

func TestWatchIgnoresMarkersWithAnotherNonce(t *testing.T) {
	r := newRig(t, fault("watch-bad-nonce"))
	h := launched(t, r)
	s := newSink()
	res, err := r.p.Watch(context.Background(), h, s)
	if err != nil || res.Exit != 0 || res.Began || len(s.phases) != 0 {
		t.Fatalf("Watch = %+v, %v, phases %v", res, err, s.phases)
	}
}

func TestWatchRefusesAResultThatDisagreesWithTheExitMarker(t *testing.T) {
	r := newRig(t, fault("watch-exit-mismatch"))
	h := launched(t, r)
	_, err := r.p.Watch(context.Background(), h, newSink())
	if err == nil || !strings.Contains(err.Error(), "result failed verification") {
		t.Fatalf("got %v", err)
	}
}

func TestWatchReturnsTheSetupFailureTheMarkerReports(t *testing.T) {
	r := newRig(t, fault("watch-setup-failed"))
	h := launched(t, r)
	_, err := r.p.Watch(context.Background(), h, newSink())
	var se *provider.ErrSetup
	if !errors.As(err, &se) || se.Step != "deps" || !se.Repo() || se.Line != "npm ERR missing script" {
		t.Fatalf("got %T %v", err, err)
	}
}

func TestWatchLostPhaseAndASilentPlugin(t *testing.T) {
	r := newRig(t, fault("watch-phase-lost"))
	h := launched(t, r)
	s := newSink()
	if _, err := r.p.Watch(context.Background(), h, s); err == nil || !strings.Contains(strings.Join(s.phases, ","), "lost") {
		t.Fatalf("lost phase: err %v phases %v", err, s.phases)
	}
	r2 := newRig(t, fault("watch-silent"), fire("watch", 300*time.Millisecond)) // never sends an event
	h2 := launched(t, r2)
	s2 := newSink()
	_, err := r2.p.Watch(context.Background(), h2, s2)
	if err == nil || !strings.Contains(strings.Join(s2.phases, ","), "lost") {
		t.Fatalf("no event within the watch bound is lost: err %v phases %v", err, s2.phases)
	}
}

func TestWatchRefusesAPhaseEventThatClaimsBeginOrEnd(t *testing.T) {
	r := newRig(t, fault("watch-phase-begun"))
	h := launched(t, r)
	_, err := r.p.Watch(context.Background(), h, newSink())
	wantUnavailableLocal(t, err, "plugin protocol error:")
}

func TestWatchStopsWhenTheContextEnds(t *testing.T) {
	r := newRig(t, fault("watch-silent"))
	h := launched(t, r)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err := r.p.Watch(ctx, h, newSink())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v", err)
	}
}
