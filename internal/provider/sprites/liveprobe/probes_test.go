package liveprobe

import (
	"context"
	"strings"
	"testing"

	"github.com/Exploded-views/bitc/internal/provider/sprites/spritesfake"
)

func scriptedRig(t *testing.T) *rig {
	t.Helper()
	old := throughputBytes
	throughputBytes = 1 << 20
	t.Cleanup(func() { throughputBytes = old })
	var f *spritesfake.Fake
	r := newRig(t, scripted(&f))
	f = r.f
	return r
}

// The whole default plan against the fake: every item answers, every Sprite is deleted, and the
// findings are the ones the scripted Sprite implies.
func TestTheDefaultPlanRunsAgainstTheFake(t *testing.T) {
	r := scriptedRig(t)
	r.cfg.DNSCanary = "canary.example.org"
	rep := r.runner(AllProbes()...).Run(context.Background())
	if rep.Exit != ExitOK {
		t.Fatalf("exit %d; results:\n%s", rep.Exit, Markdown(rep.Report))
	}
	if len(rep.Report.Results) != 25 {
		t.Fatalf("%d results, want 25", len(rep.Report.Results))
	}
	if got := r.f.Live(); len(got) != 0 {
		t.Fatalf("left %v", got)
	}
	for _, res := range rep.Report.Results {
		if res.Status == Error {
			t.Errorf("item %d errored: %s", res.ID, res.Finding)
		}
	}
	for id, want := range map[int]string{
		24: "x86_64 (amd64)",
		15: "passwordless sudo -n works",
		21: "/run is a tmpfs",
		23: "reaches api.sock: GET /v1/services=403, GET /v1/tasks=200",
		20: "can unshare",
		11: "equal on two reads: true",
		14: "fs/read cannot read root-only /etc/shadow",
		3:  "reachable [ssh github.com]",
		6:  "enforced: a 1280 MiB tmpfs allocation was killed",
		16: "WSS exec",
		13: "63 chars (63 chars): HTTP 201",
		1:  "REST DELETE of a checkpoint works",
		2:  "network policy survived: true; resources policy survived: true",
		5:  "NOT in the checkpoint",
		25: "no checkpoint survives",
		9:  "0 Connectors",
		17: "stayed running",
		22: "OWNER CHECK PENDING",
	} {
		if res := result(t, rep.Report, id); !strings.Contains(res.Finding, want) {
			t.Errorf("item %d: finding %q lacks %q", id, res.Finding, want)
		}
	}
	for id, want := range map[int]string{4: AssumeWorst, 7: AssumeWorst, 8: AssumeWorst, 10: AssumeWorst, 12: AssumeWorst, 18: AssumeWorst, 19: AssumeWorst, 22: AssumeWorst, 3: Answered} {
		if res := result(t, rep.Report, id); res.Status != want {
			t.Errorf("item %d: status %q, want %q (%s)", id, res.Status, want, res.Finding)
		}
	}
	if !strings.Contains(r.log.String(), "queried bp-t1-") {
		t.Fatalf("the canary name was not printed for the owner:\n%s", r.log.String())
	}
	if tp := result(t, rep.Report, 16); !strings.Contains(tp.Finding, "fs/read") || strings.Contains(tp.Finding, "failed") {
		t.Fatalf("throughput: %s", tp.Finding)
	}
}

func TestOneSpritePerGroupAndTheDefaultPlanStaysUnderTheCap(t *testing.T) {
	r := scriptedRig(t)
	rep := r.runner(AllProbes()...).Run(context.Background())
	// ro, ckpt, net, res, thru, ret, wake, tasks, and the name tries' one valid Sprite (22 is
	// skipped without a canary); the rejected name tries are never recorded
	if r.creates() < 9 || len(rep.Report.Sprites) < 9 || len(rep.Report.Sprites) > r.creates() {
		t.Fatalf("%d creates, %d recorded", r.creates(), len(rep.Report.Sprites))
	}
	if rep.Report.Spent <= 0 || rep.Report.Spent >= DefaultMaxUSD {
		t.Fatalf("spent $%v", rep.Report.Spent)
	}
	if res := result(t, rep.Report, 22); res.Status != Skipped {
		t.Fatalf("item 22 without a canary domain: %+v", res)
	}
}

func TestRetentionReportsCheckpointsThatSurviveADelete(t *testing.T) {
	r := scriptedRig(t)
	r.f.BehaveN("checkpoints-survive-delete", -1)
	rep := r.runner(AllProbes()...).Run(context.Background())
	res := result(t, rep.Report, 25)
	if res.Status != Answered || !strings.Contains(res.Finding, "SURVIVE a Sprite delete") || !strings.Contains(res.Finding, `"forwarded"`) {
		t.Fatalf("%+v", res)
	}
	if got := r.f.Live(); len(got) != 0 {
		t.Fatalf("left %v", got)
	}
}

func TestWakeProbeReadsAPausedSpritesStatus(t *testing.T) {
	r := scriptedRig(t)
	// the fake pauses only on its own clock, which the virtual sleep does not move
	var p Probe
	for _, q := range AllProbes() {
		if q.ID == 19 {
			p = q
		}
	}
	p.Run = func(ctx context.Context, e *Env) Outcome {
		e.Cfg.Log = nil
		r.f.Pause(e.Sprite)
		return probeWake(ctx, e)
	}
	rep := r.runner(p).Run(context.Background())
	res := result(t, rep.Report, 19)
	if res.Status != Answered || !strings.Contains(res.Finding, "GET does not wake a Sprite; list does not wake it; fs/read does not wake it") {
		t.Fatalf("the fake's REST calls do not wake a paused Sprite, the probe must say so: %+v", res)
	}
	if res.Evidence["status_after_idle"] != "warm" {
		t.Fatalf("evidence %v", res.Evidence)
	}
}
