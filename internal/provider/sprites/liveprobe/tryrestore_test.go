package liveprobe

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Exploded-views/bitc/internal/provider"
	"github.com/Exploded-views/bitc/internal/provider/sprites"
)

// This file needs the new API (Env.TryRestore, the cap constants), so it does not compile at the
// base commit.

func TestTheBodyCapsAreSharedConstants(t *testing.T) {
	if MaxRAMMB != 2048 || MaxCPUs != 2 || MaxLimitMB != 16384 {
		t.Fatalf("caps %d %d %d", MaxRAMMB, MaxCPUs, MaxLimitMB)
	}
}

// book is a copy of the runner's records and its live names.
func book(rn *Runner) (recs []SpriteRec, live []string) {
	rn.mu.Lock()
	defer rn.mu.Unlock()
	for _, rec := range rn.recs {
		recs = append(recs, *rec)
	}
	for n := range rn.live {
		live = append(live, n)
	}
	return recs, live
}

// trRun runs fn as one probe of the group Sprite; fn gets the runner to look at its book.
func trRun(t *testing.T, r *rig, fn func(ctx context.Context, e *Env, rn *Runner)) (*Runner, RunResult) {
	t.Helper()
	var rn *Runner
	rn = r.runner(quick(1, gRO, time.Minute, func(ctx context.Context, e *Env) Outcome {
		fn(ctx, e, rn)
		return fine(ctx, e)
	}))
	return rn, rn.Run(context.Background())
}

// afterDelete takes a checkpoint of the group Sprite, deletes the Sprite and returns the
// checkpoint's id.
func afterDelete(ctx context.Context, t *testing.T, e *Env) string {
	t.Helper()
	cp, err := e.API.CreateCheckpoint(ctx, e.Sprite, "c")
	if err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	if err := e.DeleteSelf(ctx); err != nil {
		t.Fatalf("DeleteSelf: %v", err)
	}
	return cp.ID
}

func TestTryRestoreRecreatesTheSpriteInTheBook(t *testing.T) {
	r := newRig(t, nil)
	r.f.BehaveN("checkpoints-survive-delete", 1)
	r.f.BehaveN("restore-recreates", 1)
	rn, rep := trRun(t, r, func(ctx context.Context, e *Env, rn *Runner) {
		name := e.Sprite
		id := afterDelete(ctx, t, e)
		orig, _ := book(rn)
		if err := e.TryRestore(ctx, name, id); err != nil {
			t.Errorf("TryRestore: %v", err)
			return
		}
		recs, live := book(rn)
		if len(recs) != 2 || len(live) != 1 || live[0] != name {
			t.Fatalf("book after the restore: %d records, live %v", len(recs), live)
		}
		if recs[0] != orig[0] || !recs[0].DeleteOK || recs[0].Deleted.IsZero() {
			t.Errorf("the original record changed: %+v, was %+v", recs[0], orig[0])
		}
		if recs[1].Name != name || recs[1].DeleteOK {
			t.Errorf("the new record: %+v", recs[1])
		}
		before := rn.spentSeconds()
		_ = e.Sleep(ctx, 10*time.Second)
		if got := rn.spentSeconds() - before; got != 10 {
			t.Errorf("spend grew by %v s in 10 s, want 10", got)
		}
		if err := rn.register(NamePrefix + "t1-50"); !errors.Is(err, ErrSecondSprite) {
			t.Errorf("a second Sprite while the recreated one is live: %v, want ErrSecondSprite", err)
		}
	})
	if rep.Exit != ExitOK || len(rep.Report.Leftovers) != 0 || len(r.f.Live()) != 0 {
		t.Fatalf("exit %d leftovers %v live %v", rep.Exit, rep.Report.Leftovers, r.f.Live())
	}
	recs, live := book(rn)
	if len(recs) != 2 || len(live) != 0 || !recs[0].DeleteOK || !recs[1].DeleteOK {
		t.Errorf("the group's clean-up should close the recreated record: %+v live %v", recs, live)
	}
}

func TestTryRestoreOn404LeavesTheBookAsItWas(t *testing.T) {
	r := newRig(t, nil)
	_, rep := trRun(t, r, func(ctx context.Context, e *Env, rn *Runner) {
		name := e.Sprite
		id := afterDelete(ctx, t, e)
		before, _ := book(rn)
		err := e.TryRestore(ctx, name, id)
		if !errors.Is(err, sprites.ErrNotFound) {
			t.Errorf("TryRestore: %v, want a not-found error", err)
		}
		recs, live := book(rn)
		if len(recs) != 1 || recs[0] != before[0] || len(live) != 0 {
			t.Errorf("book after a 404: %+v live %v, was %+v", recs, live, before)
		}
	})
	if rep.Exit != ExitOK {
		t.Fatalf("exit %d", rep.Exit)
	}
}

func TestTryRestoreOn5xxKeepsTheNameLive(t *testing.T) {
	r := newRig(t, nil)
	r.f.BehaveN("restore-500", 1)
	rn, rep := trRun(t, r, func(ctx context.Context, e *Env, rn *Runner) {
		name := e.Sprite
		id := afterDelete(ctx, t, e)
		err := e.TryRestore(ctx, name, id)
		var tr *provider.ErrTransient
		if !errors.As(err, &tr) {
			t.Errorf("TryRestore: %v, want a transient error", err)
		}
		recs, live := book(rn)
		if len(recs) != 2 || len(live) != 1 || live[0] != name {
			t.Errorf("book after a 5xx: %+v live %v, want the name kept live", recs, live)
		}
	})
	if rep.Exit != ExitOK || len(rep.Report.Leftovers) != 0 || len(r.f.Live()) != 0 {
		t.Fatalf("exit %d leftovers %v live %v", rep.Exit, rep.Report.Leftovers, r.f.Live())
	}
	if recs, live := book(rn); len(recs) != 2 || len(live) != 0 || !recs[1].DeleteOK {
		t.Errorf("the group's clean-up should close the kept record: %+v live %v", recs, live)
	}
}

func TestTryRestoreOnAnAuthRefusalUnregisters(t *testing.T) {
	r := newRig(t, nil)
	_, rep := trRun(t, r, func(ctx context.Context, e *Env, rn *Runner) {
		name := e.Sprite
		id := afterDelete(ctx, t, e)
		before, _ := book(rn)
		r.f.Behave("401")
		err := e.TryRestore(ctx, name, id)
		var un *provider.ErrUnavailable
		if !errors.As(err, &un) {
			t.Errorf("TryRestore: %v, want an unavailable error", err)
		}
		recs, live := book(rn)
		if len(recs) != 1 || recs[0] != before[0] || len(live) != 0 {
			t.Errorf("book after a 401: %+v live %v, was %+v", recs, live, before)
		}
	})
	if rep.Exit != ExitOK {
		t.Fatalf("exit %d", rep.Exit)
	}
}

func TestTryRestoreOnAnIdTheClientRefusesUnregisters(t *testing.T) {
	r := newRig(t, nil)
	_, rep := trRun(t, r, func(ctx context.Context, e *Env, rn *Runner) {
		name := e.Sprite
		afterDelete(ctx, t, e)
		before, _ := book(rn)
		calls := len(r.f.Calls())
		err := e.TryRestore(ctx, name, "../x")
		var inel *provider.ErrIneligible
		if !errors.As(err, &inel) {
			t.Errorf("TryRestore: %v, want an ineligible error", err)
		}
		recs, live := book(rn)
		if len(recs) != 1 || recs[0] != before[0] || len(live) != 0 || len(r.f.Calls()) != calls {
			t.Errorf("book after a refused id: %+v live %v, was %+v; %d request(s) sent", recs, live, before, len(r.f.Calls())-calls)
		}
	})
	if rep.Exit != ExitOK {
		t.Fatalf("exit %d", rep.Exit)
	}
}

func TestTryRestoreOnALiveNameIsAPlainRestore(t *testing.T) {
	r := newRig(t, nil)
	_, rep := trRun(t, r, func(ctx context.Context, e *Env, rn *Runner) {
		cp, err := e.API.CreateCheckpoint(ctx, e.Sprite, "c")
		if err != nil {
			t.Fatalf("checkpoint: %v", err)
		}
		before, liveBefore := book(rn)
		restores := restoreCalls(r.f)
		if err := e.TryRestore(ctx, e.Sprite, cp.ID); err != nil {
			t.Errorf("TryRestore of a live name: %v", err)
		}
		if got := restoreCalls(r.f) - restores; got != 1 {
			t.Errorf("%d restore(s) reached the fake, want 1", got)
		}
		// an error on this path never unregisters
		if err := e.TryRestore(ctx, e.Sprite, "cp-99"); !errors.Is(err, sprites.ErrNotFound) {
			t.Errorf("TryRestore of an unknown checkpoint: %v, want not found", err)
		}
		after, liveAfter := book(rn)
		if len(after) != 1 || after[0] != before[0] || len(liveAfter) != 1 || liveAfter[0] != liveBefore[0] {
			t.Errorf("book changed: %+v live %v, was %+v live %v", after, liveAfter, before, liveBefore)
		}
	})
	if rep.Exit != ExitOK {
		t.Fatalf("exit %d", rep.Exit)
	}
}

func TestTryRestoreOnAnUnknownNameIsAGuardRefusal(t *testing.T) {
	r := newRig(t, nil)
	var err error
	var sent int
	rep := r.runner(quick(1, gRO, time.Minute, func(ctx context.Context, e *Env) Outcome {
		before := len(r.f.Calls())
		err = e.TryRestore(ctx, notInBook, "cp-1")
		sent = len(r.f.Calls()) - before
		return fine(ctx, e)
	})).Run(context.Background())
	var inel *provider.ErrIneligible
	if !errors.As(err, &inel) || sent != 0 {
		t.Errorf("error %v, %d request(s) sent, want ErrIneligible and none", err, sent)
	}
	if res := result(t, rep.Report, 1); res.Status != Error || !strings.Contains(res.Finding, "guard refused") {
		t.Errorf("probe %+v, want Error with a guard refusal", res)
	}
}

// A restore of a deleted name while another Sprite is live is the one-live rule's refusal, not a
// guard refusal, and sends nothing.
func TestTryRestoreWhileAnotherSpriteIsLive(t *testing.T) {
	r := newRig(t, nil)
	var err error
	var sent int
	rep := r.runner(quick(1, gRO, time.Minute, func(ctx context.Context, e *Env) Outcome {
		name := e.Sprite
		id := afterDelete(ctx, t, e)
		other := e.NextName()
		if code, _ := e.TryCreate(ctx, other, nil); code < 200 || code >= 300 {
			return failed("the second Sprite could not be made", nil)
		}
		before := len(r.f.Calls())
		err = e.TryRestore(ctx, name, id)
		sent = len(r.f.Calls()) - before
		_ = e.API.DeleteSprite(ctx, other)
		return fine(ctx, e)
	})).Run(context.Background())
	if !errors.Is(err, ErrSecondSprite) || sent != 0 {
		t.Errorf("error %v, %d request(s) sent, want ErrSecondSprite and none", err, sent)
	}
	if res := result(t, rep.Report, 1); res.Status != Answered {
		t.Errorf("probe %+v", res)
	}
}

// Probe 25 restores twice on one name (the checkpoint, then the made-up id): the second restore
// takes the live path, so no guard refusal and no second register.
func TestTryRestoreSecondRestoreTakesTheLivePath(t *testing.T) {
	for _, behave := range []string{"restore-recreates", "restore-500"} {
		t.Run(behave, func(t *testing.T) {
			r := newRig(t, nil)
			r.f.BehaveN("checkpoints-survive-delete", 1)
			r.f.BehaveN(behave, 1)
			rn, rep := trRun(t, r, func(ctx context.Context, e *Env, rn *Runner) {
				name := e.Sprite
				id := afterDelete(ctx, t, e)
				first := e.TryRestore(ctx, name, id)
				if (behave == "restore-recreates") != (first == nil) {
					t.Errorf("first restore under %s: %v", behave, first)
				}
				calls := len(r.f.Calls())
				_ = e.TryRestore(ctx, name, NamePrefix+"t1-keep")
				if got := len(r.f.Calls()) - calls; got != 1 {
					t.Errorf("the second restore sent %d request(s), want 1", got)
				}
				if recs, live := book(rn); len(recs) != 2 || len(live) != 1 {
					t.Errorf("book after two restores: %+v live %v", recs, live)
				}
			})
			if res := result(t, rep.Report, 1); res.Status != Answered || rep.Report.GuardRefusals != 0 {
				t.Errorf("probe %+v, %d refusals", res, rep.Report.GuardRefusals)
			}
			if rep.Exit != ExitOK || len(r.f.Live()) != 0 {
				t.Errorf("exit %d live %v", rep.Exit, r.f.Live())
			}
			if recs, live := book(rn); len(recs) != 2 || len(live) != 0 {
				t.Errorf("final book %+v live %v", recs, live)
			}
		})
	}
}
