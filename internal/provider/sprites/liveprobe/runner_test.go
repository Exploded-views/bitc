package liveprobe

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Exploded-views/bitc/internal/provider/sprites"
)

func TestRunnerDeletesTheSpriteWhenAProbeFailsOrPanics(t *testing.T) {
	r := newRig(t, nil)
	rep := r.runner(
		quick(1, gRO, time.Minute, func(context.Context, *Env) Outcome { return failed("boom", nil) }),
		quick(2, gRes, time.Minute, func(context.Context, *Env) Outcome { panic("kaboom") }),
	).Run(context.Background())
	if got := r.f.Live(); len(got) != 0 {
		t.Fatalf("Sprites left: %v", got)
	}
	if rep.Exit != ExitProbe {
		t.Fatalf("exit %d, want %d for an error result", rep.Exit, ExitProbe)
	}
	if res := result(t, rep.Report, 2); res.Status != Error || !strings.Contains(res.Finding, "panicked: kaboom") {
		t.Fatalf("panic result: %+v", res)
	}
	if len(rep.Report.Sprites) != 2 || !rep.Report.Sprites[0].DeleteOK || !rep.Report.Sprites[1].DeleteOK {
		t.Fatalf("Sprite book: %+v", rep.Report.Sprites)
	}
}

func TestRunnerDeletesTheSpriteOnCancel(t *testing.T) {
	r := newRig(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	started := quick(1, gRO, time.Minute, func(ctx context.Context, e *Env) Outcome {
		cancel() // the signal
		<-ctx.Done()
		return fine(ctx, e)
	})
	rep := r.runner(started, quick(2, gRes, time.Minute, fine)).Run(ctx)
	if got := r.f.Live(); len(got) != 0 {
		t.Fatalf("Sprites left after a cancel: %v", got)
	}
	if res := result(t, rep.Report, 1); res.Status != Error || !strings.Contains(res.Finding, "interrupted") {
		t.Fatalf("interrupted probe: %+v", res)
	}
	if res := result(t, rep.Report, 2); res.Status != Skipped {
		t.Fatalf("the next group must not start after a cancel: %+v", res)
	}
	if r.creates() != 1 {
		t.Fatalf("%d creates", r.creates())
	}
}

func TestFinalSweepFindsALeftoverAndFailsLoudly(t *testing.T) {
	r := newRig(t, nil)
	stray := NamePrefix + "t1-77"
	rep := r.runner(quick(1, gRO, time.Minute, func(ctx context.Context, e *Env) Outcome {
		// made behind the runner's back, as a bug or a lost response would
		if _, err := r.c.CreateSprite(ctx, stray, nil); err != nil {
			return failed(err.Error(), nil)
		}
		return fine(ctx, e)
	})).Run(context.Background())
	if rep.Exit != ExitLeftover {
		t.Fatalf("exit %d, want %d", rep.Exit, ExitLeftover)
	}
	if len(rep.Report.Leftovers) != 1 || rep.Report.Leftovers[0] != stray {
		t.Fatalf("leftovers: %v", rep.Report.Leftovers)
	}
	if got := r.f.Live(); len(got) != 0 {
		t.Fatalf("the sweep left %v", got)
	}
	if !strings.Contains(Markdown(rep.Report), "LEFTOVER SPRITES") {
		t.Fatal("the report does not shout")
	}
}

func TestAnUnconfirmedDeleteIsALeftover(t *testing.T) {
	r := newRig(t, nil)
	r.f.BehaveN("delete-500", -1)
	rep := r.runner(quick(1, gRO, time.Minute, fine)).Run(context.Background())
	if rep.Exit != ExitLeftover || len(rep.Report.Leftovers) != 1 {
		t.Fatalf("exit %d leftovers %v", rep.Exit, rep.Report.Leftovers)
	}
	if rep.Report.Sprites[0].DeleteOK || !rep.Report.Sprites[0].LeftoverFound {
		t.Fatalf("book: %+v", rep.Report.Sprites[0])
	}
	if !strings.Contains(r.log.String(), "LOUD: could not delete") {
		t.Fatalf("log: %s", r.log.String())
	}
}

func TestRunnerRefusesASecondLiveSprite(t *testing.T) {
	r := newRig(t, nil)
	var second error
	var raw int
	var tryCode int
	rep := r.runner(quick(1, gRO, time.Minute, func(ctx context.Context, e *Env) Outcome {
		_, second = e.API.CreateSprite(ctx, e.NextName(), nil)
		raw, _ = e.API.Raw(ctx, "POST", "/v1/sprites", nil, map[string]string{"name": e.NextName()})
		tryCode, _ = e.TryCreate(ctx, e.NextName(), nil)
		return fine(ctx, e)
	})).Run(context.Background())
	if !errors.Is(second, ErrSecondSprite) {
		t.Fatalf("second create: %v", second)
	}
	if raw != 0 || tryCode != -1 {
		t.Fatalf("raw create %d, try create %d: both must be refused", raw, tryCode)
	}
	if r.creates() != 1 {
		t.Fatalf("%d creates reached the server, want 1", r.creates())
	}
	if rep.Exit != ExitOK {
		t.Fatalf("exit %d", rep.Exit)
	}
}

func TestRunnerRefusesANameOutsideItsPrefix(t *testing.T) {
	r := newRig(t, nil)
	var err1, err2 error
	r.runner(quick(1, gNone, time.Minute, func(ctx context.Context, e *Env) Outcome {
		_, err1 = e.API.CreateSprite(ctx, "other-prod-1", nil)
		_, err2 = e.API.CreateSprite(ctx, NamePrefix+"other-1", nil)
		return fine(ctx, e)
	})).Run(context.Background())
	if err1 == nil || err2 == nil || r.creates() != 0 {
		t.Fatalf("errors %v / %v, creates %d", err1, err2, r.creates())
	}
}

func TestSpritesAreNamedWithThePrefixAndCarryTheOwnerLabel(t *testing.T) {
	r := newRig(t, nil)
	r.runner(quick(1, gRO, time.Minute, fine), quick(2, gRes, time.Minute, fine)).Run(context.Background())
	var names []string
	for _, c := range r.f.Calls() {
		if c.Method == "POST" && c.Path == "/v1/sprites" {
			var b struct {
				Name   string            `json:"name"`
				Labels map[string]string `json:"labels"`
			}
			if err := json.Unmarshal(c.Body, &b); err != nil || b.Labels["bitc"] != "1" {
				t.Fatalf("create body %s: %v", c.Body, err)
			}
			names = append(names, b.Name)
		}
	}
	if len(names) != 2 || names[0] != "bitcproof-s0-t1-1" || names[1] != "bitcproof-s0-t1-2" {
		t.Fatalf("names %v", names)
	}
}

func TestSpendIsSpriteSecondsTimesThePrice(t *testing.T) {
	r := newRig(t, nil)
	r.cfg.PricePerHour, r.cfg.MaxUSD = 3600, 100000 // $1 per Sprite-second
	rep := r.runner(quick(1, gRO, 5*time.Minute, func(ctx context.Context, e *Env) Outcome {
		_ = e.Sleep(ctx, 100*time.Second)
		return fine(ctx, e)
	})).Run(context.Background())
	if len(rep.Report.Sprites) != 1 || rep.Report.Sprites[0].Seconds != 100 {
		t.Fatalf("Sprite book: %+v", rep.Report.Sprites)
	}
	if rep.Report.Spent != 100 {
		t.Fatalf("spent $%v, want $100", rep.Report.Spent)
	}
	if res := result(t, rep.Report, 1); res.SpriteSeconds != 100 {
		t.Fatalf("probe seconds %v", res.SpriteSeconds)
	}
}

func TestTheSpendCapRefusesAGroupThatCouldPassIt(t *testing.T) {
	r := newRig(t, nil)
	r.cfg.PricePerHour, r.cfg.MaxUSD = 3600, 200
	sleeper := func(ctx context.Context, e *Env) Outcome { _ = e.Sleep(ctx, 50*time.Second); return fine(ctx, e) }
	rep := r.runner(quick(1, gRO, 100*time.Second, sleeper), quick(2, gRes, 100*time.Second, sleeper)).Run(context.Background())
	if res := result(t, rep.Report, 1); res.Status != Answered {
		t.Fatalf("the first group fits ($190 of $200): %+v", res)
	}
	res := result(t, rep.Report, 2)
	if res.Status != Skipped || !strings.Contains(res.Finding, "spend cap") {
		t.Fatalf("the second group ($50 spent + $190) must be refused: %+v", res)
	}
	if r.creates() != 1 {
		t.Fatalf("%d creates", r.creates())
	}
}

func TestTheSpendCapRefusesASingleProbeInsideAGroup(t *testing.T) {
	r := newRig(t, nil)
	r.cfg.PricePerHour, r.cfg.MaxUSD = 3600, 300
	a := quick(1, gRO, 100*time.Second, func(ctx context.Context, e *Env) Outcome { _ = e.Sleep(ctx, 250*time.Second); return fine(ctx, e) })
	a.Order = 1
	b := quick(2, gRO, 100*time.Second, fine)
	b.Order = 2
	rep := r.runner(a, b).Run(context.Background())
	if res := result(t, rep.Report, 1); res.Status != Answered {
		t.Fatalf("%+v", res)
	}
	if res := result(t, rep.Report, 2); res.Status != Skipped || !strings.Contains(res.Finding, "spend cap") {
		t.Fatalf("$250 spent + $100 worst passes $300 and must be refused: %+v", res)
	}
}

func TestTheWallClockCapRefusesAGroup(t *testing.T) {
	r := newRig(t, nil)
	r.cfg.MaxMinutes = 1
	rep := r.runner(quick(1, gRO, 100*time.Second, fine)).Run(context.Background())
	if res := result(t, rep.Report, 1); res.Status != Skipped || !strings.Contains(res.Finding, "wall-clock cap") {
		t.Fatalf("%+v", res)
	}
	if r.creates() != 0 {
		t.Fatalf("%d creates", r.creates())
	}
}

func TestAProbeIsCutAtItsDeclaredWorstCase(t *testing.T) {
	r := newRig(t, nil)
	rep := r.runner(quick(1, gRO, 50*time.Millisecond, func(ctx context.Context, e *Env) Outcome {
		<-ctx.Done()
		return failed("stopped", nil)
	})).Run(context.Background())
	if res := result(t, rep.Report, 1); res.Status != Error || !strings.Contains(res.Finding, "cut at the probe's declared worst case") {
		t.Fatalf("%+v", res)
	}
}

func TestAProbeWithASkipReasonMakesNoSprite(t *testing.T) {
	r := newRig(t, nil)
	p := quick(22, gDNS, time.Minute, fine)
	p.Skip = func(Config) string { return "assume-worst: no canary" }
	rep := r.runner(p).Run(context.Background())
	if res := result(t, rep.Report, 22); res.Status != Skipped || res.Finding != "assume-worst: no canary" {
		t.Fatalf("%+v", res)
	}
	if r.creates() != 0 {
		t.Fatalf("%d creates", r.creates())
	}
}

func TestEarlierRunSpritesAreDeletedBeforeTheFirstCreate(t *testing.T) {
	r := newRig(t, nil)
	r.f.AddSprite(NamePrefix+"old-3", r.f.Org(), time.Now())
	rep := r.runner(quick(1, gRO, time.Minute, fine)).Run(context.Background())
	if len(rep.Report.Preexisting) != 1 || rep.Report.Preexisting[0] != NamePrefix+"old-3" || rep.Exit != ExitOK {
		t.Fatalf("preexisting %v exit %d", rep.Report.Preexisting, rep.Exit)
	}
	if got := r.f.Live(); len(got) != 0 {
		t.Fatalf("left %v", got)
	}
}

func TestTheReportAndTheLogNeverHoldTheTokenOrTheWholeMarker(t *testing.T) {
	r := newRig(t, nil)
	var marker string
	rr := r.runner(quick(1, gRO, time.Minute, func(ctx context.Context, e *Env) Outcome {
		marker = e.Marker
		fmt.Fprintf(e.Cfg.Log, "debug: token %s marker %s\n", r.f.Token(), e.Marker)
		return Outcome{Status: Answered, Finding: "echoed " + r.f.Token() + " and " + e.Marker + " short " + e.ShortMarker(),
			Evidence: map[string]any{"t": r.f.Token(), "m": []string{e.Marker}}}
	}))
	rep := rr.Run(context.Background())
	if len(marker) != 64 || rr.Marker() != marker {
		t.Fatalf("marker %q", marker)
	}
	var jl bytes.Buffer
	if err := WriteJSONL(&jl, rep.Report.Results); err != nil {
		t.Fatal(err)
	}
	for name, text := range map[string]string{"jsonl": jl.String(), "markdown": Markdown(rep.Report), "log": r.log.String()} {
		if strings.Contains(text, r.f.Token()) || strings.Contains(text, "fake-token") {
			t.Errorf("%s holds the token:\n%s", name, text)
		}
		if strings.Contains(text, marker) {
			t.Errorf("%s holds the whole marker", name)
		}
	}
	if !strings.Contains(jl.String(), "short "+marker[:8]) {
		t.Fatalf("the short marker is missing: %s", jl.String())
	}
}

func TestADryRunMakesNoRequestAndStaysUnderTheCap(t *testing.T) {
	r := newRig(t, nil)
	var stdout, stderr bytes.Buffer
	code := Main([]string{"--org", r.f.Org(), "--token-file", tokenFile(t, 0o600, r.f.Token()), "--api-base", r.f.URL(), "--dry-run", "--run", "dry1", "--dns-canary-domain", "canary.example.org"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if calls := r.f.Calls(); len(calls) != 0 {
		t.Fatalf("a dry run made %d requests: %+v", len(calls), calls)
	}
	out := stdout.String()
	if !strings.Contains(out, "bitcproof-s0-dry1-<n>") || !strings.Contains(out, "total declared worst case") {
		t.Fatalf("plan: %s", out)
	}
	i := strings.LastIndex(out, "= $")
	var worst float64
	if _, err := fmt.Sscan(out[i+3:], &worst); err != nil || worst <= 0 || worst >= 0.90 {
		t.Fatalf("worst case $%v (%v) in %q", worst, err, out[i:])
	}
}

func TestMainRunsAPlanWithoutSpritesAndWritesTheOutFile(t *testing.T) {
	r := newRig(t, nil)
	out := filepath.Join(t.TempDir(), "out.jsonl")
	var stdout, stderr bytes.Buffer
	code := Main([]string{"--org", r.f.Org(), "--token-file", tokenFile(t, 0o600, r.f.Token()), "--api-base", r.f.URL(), "--probes", "9,10,12", "--out", out, "--run", "m1"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	st, err := os.Stat(out)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("out file: %v %v", st, err)
	}
	b, _ := os.ReadFile(out)
	if lines := strings.Split(strings.TrimSpace(string(b)), "\n"); len(lines) != 3 {
		t.Fatalf("%d lines: %s", len(lines), b)
	}
	if !strings.Contains(stdout.String(), "| 9 | answered | 0 Connectors") || !strings.Contains(stdout.String(), "| 10 | assume-worst |") {
		t.Fatalf("table: %s", stdout.String())
	}
	if r.creates() != 0 {
		t.Fatalf("%d creates for a plan with no Sprite", r.creates())
	}
	if strings.Contains(stdout.String()+stderr.String(), r.f.Token()) {
		t.Fatal("the token was printed")
	}
}

func TestTheOrgGuardRunsBeforeAnyRequest(t *testing.T) {
	r := newRig(t, nil)
	var stdout, stderr bytes.Buffer
	wrong := "other-org/" + strings.Repeat("s3cr3t", 4)
	code := Main([]string{"--org", r.f.Org(), "--token-file", tokenFile(t, 0o600, wrong), "--api-base", r.f.URL(), "--run", "g1"}, &stdout, &stderr)
	if code != ExitUsage {
		t.Fatalf("exit %d", code)
	}
	if len(r.f.Calls()) != 0 {
		t.Fatal("a request was made with the wrong org's token")
	}
	if strings.Contains(stderr.String(), "s3cr3t") || !strings.Contains(stderr.String(), "other-org") {
		t.Fatalf("stderr: %s", stderr.String())
	}
	// a token file others can read is refused, again without a request
	stderr.Reset()
	code = Main([]string{"--org", r.f.Org(), "--token-file", tokenFile(t, 0o644, r.f.Token()), "--api-base", r.f.URL()}, &stdout, &stderr)
	if code != ExitUsage || !strings.Contains(stderr.String(), "mode 600") || len(r.f.Calls()) != 0 {
		t.Fatalf("exit %d, stderr %s", code, stderr.String())
	}
}

func TestARunnerWhoseClientIsForAnotherOrgDoesNothing(t *testing.T) {
	r := newRig(t, nil)
	r.cfg.Org = "someone-else"
	rep := r.runner(quick(1, gRO, time.Minute, fine)).Run(context.Background())
	if rep.Exit != ExitUsage || !strings.Contains(rep.Report.Fatal, "not --org") || len(r.f.Calls()) != 0 {
		t.Fatalf("exit %d fatal %q calls %d", rep.Exit, rep.Report.Fatal, len(r.f.Calls()))
	}
}

var _ = sprites.DefaultAPIBase

func probeByID(t *testing.T, id int) Probe {
	t.Helper()
	for _, p := range AllProbes() {
		if p.ID == id {
			return p
		}
	}
	t.Fatalf("no probe %d", id)
	return Probe{}
}

func TestAnInvalidNameTheServerAcceptedIsDeletedAndLaterGroupsRun(t *testing.T) {
	r := newRig(t, nil)
	r.f.BehaveN("lax-names", -1)
	rep := r.runner(probeByID(t, 13), quick(1, gRO, time.Minute, fine)).Run(context.Background())
	if got := r.f.Live(); len(got) != 0 {
		t.Fatalf("Sprites left: %v", got)
	}
	if rep.Exit != ExitOK {
		t.Fatalf("exit %d, want %d; leftovers %v\n%s", rep.Exit, ExitOK, rep.Report.Leftovers, Markdown(rep.Report))
	}
	if res := result(t, rep.Report, 13); res.Status == Error {
		t.Fatalf("probe 13: %+v", res)
	}
	if res := result(t, rep.Report, 1); res.Status != Answered {
		t.Fatalf("the group after probe 13: %+v", res)
	}
	for _, s := range rep.Report.Sprites {
		if !s.DeleteOK {
			t.Fatalf("book entry not confirmed: %+v", s)
		}
	}
}

func TestAnOwnProbeSpriteIsDeletedWhenTheProbeEnds(t *testing.T) {
	create := func(ctx context.Context, e *Env) {
		if code, _ := e.TryCreate(ctx, e.NextName(), nil); code != 201 && code != 200 {
			panic(fmt.Sprintf("create answered %d", code))
		}
	}
	own := func(run func(context.Context, *Env) Outcome) Probe {
		return Probe{ID: 1, Title: "t", Group: gNone, Own: true, Worst: time.Minute, Run: run}
	}
	cases := []struct {
		name       string
		probe      Probe
		cancel     bool
		wantExit   int
		wantCreate int
	}{
		{"panic after a create", own(func(ctx context.Context, e *Env) Outcome { create(ctx, e); panic("kaboom") }), false, ExitProbe, 2},
		{"early return after a create", own(func(ctx context.Context, e *Env) Outcome { create(ctx, e); return failed("early", nil) }), false, ExitProbe, 2},
		{"signal after a create", own(func(ctx context.Context, e *Env) Outcome { create(ctx, e); return failed("cut", nil) }), true, ExitProbe, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newRig(t, nil)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			p := c.probe
			if c.cancel {
				inner := p.Run
				p.Run = func(pctx context.Context, e *Env) Outcome {
					o := inner(pctx, e)
					cancel() // the signal arrives while the Sprite is live
					return o
				}
			}
			rep := r.runner(p, quick(2, gRO, time.Minute, fine)).Run(ctx)
			if got := r.f.Live(); len(got) != 0 {
				t.Fatalf("Sprites left: %v", got)
			}
			if rep.Exit != c.wantExit || len(rep.Report.Leftovers) != 0 {
				t.Fatalf("exit %d (want %d) leftovers %v", rep.Exit, c.wantExit, rep.Report.Leftovers)
			}
			if r.creates() != c.wantCreate {
				t.Fatalf("%d creates, want %d", r.creates(), c.wantCreate)
			}
			if !c.cancel {
				if res := result(t, rep.Report, 2); res.Status != Answered {
					t.Fatalf("the next group: %+v", res)
				}
			}
		})
	}
}
