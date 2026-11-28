package liveprobe

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Exploded-views/bitc/internal/provider"
	"github.com/Exploded-views/bitc/internal/provider/sprites"
	"github.com/Exploded-views/bitc/internal/provider/sprites/spritesfake"
)

func deleteCalls(f *spritesfake.Fake) int {
	n := 0
	for _, c := range f.Calls() {
		if c.Method == "DELETE" {
			n++
		}
	}
	return n
}

func resourcesCalls(f *spritesfake.Fake) int {
	n := 0
	for _, c := range f.Calls() {
		if strings.HasSuffix(c.Path, "/policy/resources") && c.Method != "GET" {
			n++
		}
	}
	return n
}

func isGuardRefusal(err error, method string) bool {
	var inel *provider.ErrIneligible
	return errors.As(err, &inel) && strings.HasPrefix(inel.Why, "guard: "+method)
}

// ---- DeleteSprite needs a Sprite the run knows

func TestGuardedDeleteSpriteRefusesANameOutsideTheBook(t *testing.T) {
	t.Run("a valid run-prefixed name that was never registered", func(t *testing.T) {
		r := newRig(t, nil)
		var callErr, setupErr error
		var sent int
		var stillThere bool
		rep := r.runner(quick(1, gRO, time.Minute, func(ctx context.Context, e *Env) Outcome {
			// the Sprite exists in the org, so a delete that went through would remove it
			if _, err := r.c.CreateSprite(ctx, notInBook, nil); err != nil {
				setupErr = err
				return fine(ctx, e)
			}
			before := deleteCalls(r.f)
			callErr = e.API.DeleteSprite(ctx, notInBook)
			sent = deleteCalls(r.f) - before
			stillThere = slices.Contains(r.f.Live(), notInBook)
			_ = r.c.DeleteSprite(ctx, notInBook)
			return fine(ctx, e)
		})).Run(context.Background())
		if setupErr != nil {
			t.Fatalf("setup: %v", setupErr)
		}
		if sent != 0 || !stillThere {
			t.Errorf("%d DELETE(s) reached the fake, Sprite still there: %v; want none and true", sent, stillThere)
		}
		if !isGuardRefusal(callErr, "DeleteSprite") {
			t.Errorf("error %v, want a guard ErrIneligible naming DeleteSprite", callErr)
		}
		if res := result(t, rep.Report, 1); res.Status != Error || !strings.Contains(res.Finding, "guard refused DeleteSprite "+notInBook) {
			t.Errorf("probe %q %q, want Error with the refusal in the finding", res.Status, res.Finding)
		}
		if rep.Report.GuardRefusals != 1 || rep.Exit != ExitProbe {
			t.Errorf("%d refusal(s), exit %d, want 1 and %d", rep.Report.GuardRefusals, rep.Exit, ExitProbe)
		}
		if got := r.f.Live(); len(got) != 0 {
			t.Errorf("Sprites left: %v", got)
		}
	})

	t.Run("a registered live name is still deleted", func(t *testing.T) {
		r := newRig(t, nil)
		var callErr error
		var sent int
		rn, rep := trRun(t, r, func(ctx context.Context, e *Env, rn *Runner) {
			before := deleteCalls(r.f)
			callErr = e.API.DeleteSprite(ctx, e.Sprite)
			sent = deleteCalls(r.f) - before
		})
		if callErr != nil || sent != 1 {
			t.Errorf("error %v, %d DELETE(s), want nil and 1", callErr, sent)
		}
		if res := result(t, rep.Report, 1); res.Status != Answered || rep.Report.GuardRefusals != 0 || rep.Exit != ExitOK {
			t.Errorf("probe %+v, %d refusal(s), exit %d", res, rep.Report.GuardRefusals, rep.Exit)
		}
		if recs, live := book(rn); len(recs) != 1 || !recs[0].DeleteOK || len(live) != 0 {
			t.Errorf("book after the delete: %+v live %v", recs, live)
		}
	})

	t.Run("a name the run deleted is allowed through to deleteAny", func(t *testing.T) {
		r := newRig(t, nil)
		var firstErr, secondErr, setupErr error
		var sent int
		rep := r.runner(quick(1, gRO, time.Minute, func(ctx context.Context, e *Env) Outcome {
			name := e.NextName()
			if _, err := freeSlot(ctx, e, name, 200, 300); err != nil {
				setupErr = err
				return fine(ctx, e)
			}
			firstErr = e.API.DeleteSprite(ctx, name)
			before := deleteCalls(r.f)
			secondErr = e.API.DeleteSprite(ctx, name)
			sent = deleteCalls(r.f) - before
			return fine(ctx, e)
		})).Run(context.Background())
		if setupErr != nil {
			t.Fatalf("setup: %v", setupErr)
		}
		if firstErr != nil {
			t.Errorf("the first delete: %v", firstErr)
		}
		// the Sprite is gone, so the fake answers 404; what matters is that the guard let the call
		// through to the fake and did not refuse it
		if sent < 1 || isGuardRefusal(secondErr, "DeleteSprite") {
			t.Errorf("a delete of a deleted known name: %d DELETE(s) sent, error %v; want it sent and not refused", sent, secondErr)
		}
		if res := result(t, rep.Report, 1); res.Status != Answered || rep.Report.GuardRefusals != 0 {
			t.Errorf("probe %+v, %d refusal(s)", res, rep.Report.GuardRefusals)
		}
	})
}

// ---- a failed create of a re-used name

// A name the run created and deleted is created again and the create fails with a 4xx that is not
// an ErrPreAttach: only the record that create added goes, and the earlier record keeps its Deleted
// time and DeleteOK.
func TestFailedCreateOfAReusedNameKeepsTheDeletedRecord(t *testing.T) {
	cases := []struct {
		name   string
		create func(ctx context.Context, e *Env, name string) error
	}{
		{"guarded.CreateSprite", func(ctx context.Context, e *Env, name string) error {
			_, err := e.API.CreateSprite(ctx, name, nil)
			var pa *provider.ErrPreAttach
			if err == nil || errors.As(err, &pa) {
				return fmt.Errorf("want a failed create that is not an ErrPreAttach, got %w", err)
			}
			return nil
		}},
		{"tryCreate", func(ctx context.Context, e *Env, name string) error {
			code, _ := e.TryCreate(ctx, name, nil)
			if code < 400 || code >= 500 {
				return fmt.Errorf("want a 4xx answer, got %d", code)
			}
			return nil
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newRig(t, nil)
			var before, after []SpriteRec
			var liveAfter []string
			var createErr, setupErr error
			_, rep := trRun(t, r, func(ctx context.Context, e *Env, rn *Runner) {
				name := e.NextName()
				if _, err := freeSlot(ctx, e, name, 200, 300); err != nil {
					setupErr = err
					return
				}
				if err := e.API.DeleteSprite(ctx, name); err != nil {
					setupErr = err
					return
				}
				before, _ = book(rn)
				r.f.BehaveN("403-restricted", 1)
				createErr = c.create(ctx, e, name)
				after, liveAfter = book(rn)
			})
			if setupErr != nil {
				t.Fatalf("setup: %v", setupErr)
			}
			if createErr != nil {
				t.Fatalf("the create: %v", createErr)
			}
			if len(before) != 2 || !before[1].DeleteOK || before[1].Deleted.IsZero() {
				t.Fatalf("the book before the create: %+v", before)
			}
			if len(after) != len(before) {
				t.Fatalf("recs after the failed create: %+v, was %+v (no extra record may remain)", after, before)
			}
			for i := range before {
				if after[i] != before[i] {
					t.Errorf("record %d changed: %+v, was %+v", i, after[i], before[i])
				}
			}
			if len(liveAfter) != 0 {
				t.Errorf("live after the failed create: %v", liveAfter)
			}
			if rep.Exit != ExitOK || len(r.f.Live()) != 0 {
				t.Errorf("exit %d, live %v", rep.Exit, r.f.Live())
			}
		})
	}
}

// ---- ApplyBuildPolicies caps LimitMB

func TestApplyBuildPoliciesCapsResources(t *testing.T) {
	cases := []struct {
		limit int
		sent  bool
	}{
		{-1, false},
		{0, false},
		{1, true},
		{MaxLimitMB, true},
		{MaxLimitMB + 1, false},
	}
	for _, c := range cases {
		t.Run("LimitMB "+strconv.Itoa(c.limit), func(t *testing.T) {
			r := newRig(t, nil)
			var callErr error
			var sent, resCalls int
			var resBefore, resAfter string
			rep := r.runner(quick(1, gRO, time.Minute, func(ctx context.Context, e *Env) Outcome {
				p, err := sprites.AllowDomains("registry.npmjs.org")
				if err != nil {
					return failed(err.Error(), nil)
				}
				_, resBefore = r.f.Policies(e.Sprite)
				before, resBeforeCalls := len(r.f.Calls()), resourcesCalls(r.f)
				callErr = e.API.ApplyBuildPolicies(ctx, e.Sprite, p, sprites.Resources{LimitMB: c.limit})
				sent = len(r.f.Calls()) - before
				resCalls = resourcesCalls(r.f) - resBeforeCalls
				_, resAfter = r.f.Policies(e.Sprite)
				return fine(ctx, e)
			})).Run(context.Background())
			res := result(t, rep.Report, 1)
			if c.sent {
				if callErr != nil || sent < 1 || resCalls != 1 {
					t.Errorf("error %v, %d request(s), %d resources write(s); want nil, some and 1", callErr, sent, resCalls)
				}
				if res.Status != Answered || rep.Report.GuardRefusals != 0 {
					t.Errorf("probe %q %q, %d refusal(s)", res.Status, res.Finding, rep.Report.GuardRefusals)
				}
				return
			}
			if sent != 0 || resCalls != 0 || resAfter != resBefore {
				t.Errorf("%d request(s), %d resources write(s), policy %q -> %q; want none and no change", sent, resCalls, resBefore, resAfter)
			}
			if !isGuardRefusal(callErr, "ApplyBuildPolicies") {
				t.Errorf("error %v, want a guard ErrIneligible naming ApplyBuildPolicies", callErr)
			}
			if res.Status != Error || !strings.Contains(res.Finding, "guard refused ApplyBuildPolicies") || rep.Exit != ExitProbe {
				t.Errorf("probe %q %q exit %d, want Error naming the method and exit %d", res.Status, res.Finding, rep.Exit, ExitProbe)
			}
		})
	}
}
