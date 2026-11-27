package liveprobe

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"
)

// rawReq is one raw request of a probe.
type rawReq struct {
	method, path string
	q            url.Values
	body         any
}

func (q rawReq) send(ctx context.Context, e *Env) int {
	code, _ := e.API.Raw(ctx, q.method, q.path, q.q, q.body)
	return code
}

// refusalCase is one raw request the guard must refuse. setup may free the group's Sprite and
// make another book name first (the name it returns, "" for none); req builds the request from
// the group's Sprite and that name.
type refusalCase struct {
	name  string
	prep  func(r *rig)
	setup func(ctx context.Context, e *Env, r *rig) (string, error)
	req   func(e *Env, extra string) rawReq
}

// notInBook is a well-formed kit name that was never registered.
const notInBook = NamePrefix + "t1-99"

func sprite(e *Env, tail string) string { return "/v1/sprites/" + e.Sprite + tail }

// freeSlot deletes the group's Sprite and makes a new book name with the given outcome.
func freeSlot(ctx context.Context, e *Env, name string, wantLo, wantHi int) (int, error) {
	if err := e.DeleteSelf(ctx); err != nil {
		return 0, fmt.Errorf("DeleteSelf: %w", err)
	}
	if name == "" {
		return 0, nil
	}
	code, _ := e.TryCreate(ctx, name, nil)
	if code < wantLo || code >= wantHi {
		return code, fmt.Errorf("create of %q answered %d, want [%d,%d)", name, code, wantLo, wantHi)
	}
	return code, nil
}

func runRefusal(t *testing.T, c refusalCase) {
	t.Helper()
	r := newRig(t, nil)
	if c.prep != nil {
		c.prep(r)
	}
	var code, sent int
	var q rawReq
	var setupErr error
	rep := r.runner(quick(1, gRO, time.Minute, func(ctx context.Context, e *Env) Outcome {
		extra := ""
		if c.setup != nil {
			var err error
			if extra, err = c.setup(ctx, e, r); err != nil {
				setupErr = err
				return fine(ctx, e)
			}
		}
		q = c.req(e, extra)
		before := len(r.f.Calls())
		code = q.send(ctx, e)
		sent = len(r.f.Calls()) - before
		if extra != "" {
			_ = e.API.DeleteSprite(ctx, extra)
		}
		return fine(ctx, e)
	})).Run(context.Background())
	if setupErr != nil {
		t.Fatalf("setup: %v", setupErr)
	}
	if code != 0 || sent != 0 {
		t.Errorf("%s %s: answered %d and sent %d request(s), want 0 and none", q.method, q.path, code, sent)
	}
	res := result(t, rep.Report, 1)
	want := "guard refused " + strings.ToUpper(q.method) + " " + q.path
	if res.Status != Error || !strings.Contains(res.Finding, want) {
		t.Errorf("probe result %q %q, want Error with %q", res.Status, res.Finding, want)
	}
	if rep.Exit != ExitProbe {
		t.Errorf("exit %d, want %d", rep.Exit, ExitProbe)
	}
	if got := r.f.Live(); len(got) != 0 {
		t.Errorf("Sprites left: %v", got)
	}
}

func TestRouteAllowedRequestsReachTheFake(t *testing.T) {
	cfg := map[string]any{"config": map[string]any{"ram_mb": 2048, "cpus": 2}}
	pol := map[string]any{"memory": map[string]any{"limit_mb": 1024, "autoscale": false}}
	cases := []struct {
		name string
		req  func(e *Env) rawReq
	}{
		{"PUT config", func(e *Env) rawReq { return rawReq{"PUT", sprite(e, ""), nil, cfg} }},
		{"PATCH labels", func(e *Env) rawReq {
			return rawReq{"PATCH", sprite(e, ""), nil, map[string]any{"labels": map[string]string{"bitc": "2"}}}
		}},
		{"POST privileges", func(e *Env) rawReq {
			return rawReq{"POST", sprite(e, "/policy/privileges"), nil, map[string]string{"profile": "minimal"}}
		}},
		{"POST resources", func(e *Env) rawReq { return rawReq{"POST", sprite(e, "/policy/resources"), nil, pol} }},
		{"lower-case post resources", func(e *Env) rawReq { return rawReq{"post", sprite(e, "/policy/resources"), nil, pol} }},
		{"lower-case put", func(e *Env) rawReq { return rawReq{"put", sprite(e, ""), nil, cfg} }},
		{"head", func(e *Env) rawReq { return rawReq{"head", sprite(e, ""), nil, nil} }},
		{"Get", func(e *Env) rawReq { return rawReq{"Get", sprite(e, ""), nil, nil} }},
		{"GET the Sprite", func(e *Env) rawReq { return rawReq{"GET", sprite(e, ""), nil, nil} }},
		{"GET list with prefix", func(e *Env) rawReq {
			return rawReq{"GET", "/v1/sprites", url.Values{"prefix": {NamePrefix}}, nil}
		}},
		{"GET list", func(e *Env) rawReq { return rawReq{"GET", "/v1/sprites", nil, nil} }},
		{"GET regions", func(e *Env) rawReq { return rawReq{"GET", "/v1/regions", nil, nil} }},
		{"GET policy/network", func(e *Env) rawReq { return rawReq{"GET", sprite(e, "/policy/network"), nil, nil} }},
		{"GET policy/resources", func(e *Env) rawReq { return rawReq{"GET", sprite(e, "/policy/resources"), nil, nil} }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newRig(t, nil)
			var sent int
			var q rawReq
			rep := r.runner(quick(1, gRO, time.Minute, func(ctx context.Context, e *Env) Outcome {
				q = c.req(e)
				before := len(r.f.Calls())
				q.send(ctx, e)
				sent = len(r.f.Calls()) - before
				return fine(ctx, e)
			})).Run(context.Background())
			if sent != 1 {
				t.Errorf("%s %s: %d request(s) reached the fake, want 1", q.method, q.path, sent)
			}
			if res := result(t, rep.Report, 1); res.Status != Answered {
				t.Errorf("probe result: %+v", res)
			}
			if rep.Exit != ExitOK {
				t.Errorf("exit %d, want %d", rep.Exit, ExitOK)
			}
		})
	}
}

func TestRouteRefusedWrites(t *testing.T) {
	post := func(path string) func(e *Env, x string) rawReq {
		return func(e *Env, x string) rawReq { return rawReq{"POST", path, nil, map[string]string{"name": notInBook}} }
	}
	onSprite := func(method, tail string) func(e *Env, x string) rawReq {
		return func(e *Env, x string) rawReq {
			return rawReq{method, sprite(e, tail), nil, map[string]string{"k": "v"}}
		}
	}
	cases := []refusalCase{
		{name: "create", req: post("/v1/sprites")},
		{name: "create .json", req: post("/v1/sprites.json")},
		{name: "create dot", req: post("/v1/sprites.")},
		{name: "create tilde", req: post("/v1/sprites~")},
		{name: "fork", req: func(e *Env, x string) rawReq { return rawReq{"POST", sprite(e, "/fork"), nil, nil} }},
		{name: "restore", req: func(e *Env, x string) rawReq {
			return rawReq{"POST", sprite(e, "/checkpoints/x/restore"), nil, nil}
		}},
		{name: "checkpoint write", req: onSprite("POST", "/checkpoint")},
		{name: "PUT unregistered", req: func(e *Env, x string) rawReq {
			return rawReq{"PUT", "/v1/sprites/" + notInBook, nil, map[string]string{"k": "v"}}
		}},
		{name: "PUT the live name plus a suffix", req: func(e *Env, x string) rawReq {
			return rawReq{"PUT", "/v1/sprites/" + e.Sprite + "-x", nil, map[string]string{"k": "v"}}
		}},
		{name: "PUT a prefix of the live name", req: func(e *Env, x string) rawReq {
			return rawReq{"PUT", "/v1/sprites/" + strings.TrimSuffix(e.Sprite, "1"), nil, map[string]string{"k": "v"}}
		}},
		{name: "PUT after DeleteSelf", setup: func(ctx context.Context, e *Env, r *rig) (string, error) {
			_, err := freeSlot(ctx, e, "", 0, 0)
			return "", err
		}, req: onSprite("PUT", "")},
		{name: "PUT a deleted book name", setup: func(ctx context.Context, e *Env, r *rig) (string, error) {
			name := e.NextName()
			if _, err := freeSlot(ctx, e, name, 200, 300); err != nil {
				return "", err
			}
			if err := e.API.DeleteSprite(ctx, name); err != nil {
				return "", err
			}
			return name, nil
		}, req: func(e *Env, x string) rawReq {
			return rawReq{"PUT", "/v1/sprites/" + x, nil, map[string]string{"k": "v"}}
		}},
		{name: "PUT a name unregistered after a 4xx create", setup: func(ctx context.Context, e *Env, r *rig) (string, error) {
			name := NamePrefix + "t1-BAD"
			_, err := freeSlot(ctx, e, name, 400, 500)
			return name, err
		}, req: func(e *Env, x string) rawReq {
			return rawReq{"PUT", "/v1/sprites/" + x, nil, map[string]string{"k": "v"}}
		}},
		{name: "PUT a book name with a dot", prep: func(r *rig) { r.f.BehaveN("lax-names", -1) },
			setup: func(ctx context.Context, e *Env, r *rig) (string, error) {
				name := e.NextName() + "-a.b"
				_, err := freeSlot(ctx, e, name, 200, 300)
				return name, err
			}, req: func(e *Env, x string) rawReq {
				return rawReq{"PUT", "/v1/sprites/" + x, nil, map[string]string{"k": "v"}}
			}},
		{name: "DELETE", req: onSprite("DELETE", "")},
		{name: "PATCH trailing dot", req: onSprite("PATCH", ".")},
		{name: "POST on the Sprite", req: onSprite("POST", "")},
		{name: "PUT on policy/resources", req: onSprite("PUT", "/policy/resources")},
		{name: "DELETE on policy/privileges", req: onSprite("DELETE", "/policy/privileges")},
		{name: "upper-case Sprites", req: func(e *Env, x string) rawReq {
			return rawReq{"PUT", "/v1/Sprites/" + e.Sprite, nil, map[string]string{"k": "v"}}
		}},
		{name: "upper-case Resources", req: onSprite("POST", "/policy/Resources")},
		{name: "upper-case Policy", req: onSprite("POST", "/Policy/privileges")},
		{name: "sub-path", req: onSprite("POST", "/policy/resources/x")},
		{name: "trailing slash", req: onSprite("PUT", "/")},
		{name: "policy/network", req: onSprite("POST", "/policy/network")},
		{name: "OPTIONS", req: onSprite("OPTIONS", "")},
		{name: "custom method", req: onSprite("PROPFIND", "")},
		{name: "OPTIONS without body or query", req: func(e *Env, x string) rawReq { return rawReq{"OPTIONS", sprite(e, ""), nil, nil} }},
		{name: "custom method without body or query", req: func(e *Env, x string) rawReq { return rawReq{"PROPFIND", sprite(e, ""), nil, nil} }},
		{name: "PUT with a query", req: func(e *Env, x string) rawReq {
			return rawReq{"PUT", sprite(e, ""), url.Values{"upsert": {"true"}}, map[string]string{"k": "v"}}
		}},
		{name: "POST resources with a query", req: func(e *Env, x string) rawReq {
			return rawReq{"POST", sprite(e, "/policy/resources"), url.Values{"x": {"1"}}, map[string]string{"k": "v"}}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { runRefusal(t, c) })
	}
}

func TestRouteRefusedReads(t *testing.T) {
	get := func(path string) func(e *Env, x string) rawReq {
		return func(e *Env, x string) rawReq { return rawReq{"GET", path, nil, nil} }
	}
	cases := []refusalCase{
		{name: "not in the book", req: get("/v1/sprites/" + notInBook)},
		{name: "not in the book, Sprites", req: get("/v1/Sprites/" + notInBook)},
		{name: "not in the book, SPRITES fs/read", req: get("/v1/SPRITES/" + notInBook + "/fs/read")},
		{name: "not in the book, sprites.json", req: get("/v1/sprites.json/" + notInBook)},
		{name: "not in the book, SPRITES.json", req: get("/v1/SPRITES.json/" + notInBook)},
		{name: "not in the book, sprites~", req: get("/v1/sprites~/" + notInBook)},
		{name: "not in the book, fs/read", req: get("/v1/sprites/" + notInBook + "/fs/read")},
		{name: "HEAD not in the book", req: func(e *Env, x string) rawReq { return rawReq{"head", "/v1/sprites/" + notInBook, nil, nil} }},
		{name: "the live name plus a suffix", req: func(e *Env, x string) rawReq {
			return rawReq{"GET", "/v1/sprites/" + e.Sprite + "-x", nil, nil}
		}},
		{name: "a prefix of the live name", req: func(e *Env, x string) rawReq {
			return rawReq{"GET", "/v1/sprites/" + strings.TrimSuffix(e.Sprite, "1"), nil, nil}
		}},
		{name: "case variant of the live name", req: func(e *Env, x string) rawReq {
			return rawReq{"GET", "/v1/sprites/" + strings.ToUpper(e.Sprite), nil, nil}
		}},
		{name: "a deleted Sprite", setup: func(ctx context.Context, e *Env, r *rig) (string, error) {
			_, err := freeSlot(ctx, e, "", 0, 0)
			return "", err
		}, req: func(e *Env, x string) rawReq { return rawReq{"GET", sprite(e, ""), nil, nil} }},
		{name: "a GET with a body", req: func(e *Env, x string) rawReq {
			return rawReq{"GET", "/v1/regions", nil, map[string]string{"k": "v"}}
		}},
		{name: "a GET under the Sprite with a body", req: func(e *Env, x string) rawReq {
			return rawReq{"GET", sprite(e, ""), nil, map[string]string{"k": "v"}}
		}},
		{name: "a GET with _method", req: func(e *Env, x string) rawReq {
			return rawReq{"GET", sprite(e, ""), url.Values{"_method": {"DELETE"}}, nil}
		}},
		{name: "a GET with Method", req: func(e *Env, x string) rawReq {
			return rawReq{"GET", "/v1/regions", url.Values{"Method": {"DELETE"}}, nil}
		}},
		{name: "a GET with X-HTTP-Method-Override", req: func(e *Env, x string) rawReq {
			return rawReq{"GET", "/v1/sprites", url.Values{"X-Http-Method-Override": {"DELETE"}}, nil}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { runRefusal(t, c) })
	}
}

// ---- probe regression

func TestRouteTheDefaultPlanIsNeverRefused(t *testing.T) {
	r := scriptedRig(t)
	r.cfg.DNSCanary = "canary.example.org"
	rep := r.runner(AllProbes()...).Run(context.Background())
	for _, res := range rep.Report.Results {
		if strings.Contains(res.Finding, "guard refused") {
			t.Errorf("item %d was refused: %s", res.ID, res.Finding)
		}
		if _, has := res.Evidence["guard_refusals"]; has {
			t.Errorf("item %d has refusals: %v", res.ID, res.Evidence["guard_refusals"])
		}
	}
	if rep.Exit != ExitOK {
		t.Fatalf("exit %d\n%s", rep.Exit, Markdown(rep.Report))
	}
	if strings.Contains(Markdown(rep.Report), "Guard refusals") {
		t.Fatalf("the report counts refusals:\n%s", Markdown(rep.Report))
	}
	// without these, zero refusals would also pass if the branches never ran
	var put13, patch13, put6, priv4, res6, res6Big int
	for _, c := range r.f.Calls() {
		body := string(c.Body)
		onSprite := strings.HasPrefix(c.Path, "/v1/sprites/"+NamePrefix) && !strings.Contains(strings.TrimPrefix(c.Path, "/v1/sprites/"), "/")
		switch {
		case c.Method == "PUT" && onSprite && strings.Contains(body, `"labels"`):
			put13++
		case c.Method == "PATCH" && onSprite && strings.Contains(body, `"labels"`):
			patch13++
		case c.Method == "PUT" && onSprite && strings.Contains(body, `"ram_mb"`):
			put6++
		case c.Method == "POST" && strings.HasSuffix(c.Path, "/policy/privileges"):
			priv4++
		case c.Method == "POST" && strings.HasSuffix(c.Path, "/policy/resources") && strings.Contains(body, "16384"):
			res6Big++
		case c.Method == "POST" && strings.HasSuffix(c.Path, "/policy/resources") && strings.Contains(body, `"limit_mb":1024`):
			res6++
		}
	}
	if put13 == 0 || patch13 == 0 || put6 == 0 || priv4 == 0 || res6 == 0 || res6Big == 0 {
		t.Fatalf("the fake did not record the probes' writes: probe 13 PUT %d PATCH %d, probe 6 PUT %d POST %d/%d, probe 4 POST %d", put13, patch13, put6, res6, res6Big, priv4)
	}
	if r.creates() < 9 {
		t.Fatalf("%d creates reached the fake", r.creates())
	}
}

// ---- the refusal record

func TestRouteARefusedProbeFailsAndTheNextOneIsClean(t *testing.T) {
	r := newRig(t, nil)
	p1 := quick(1, gRO, time.Minute, func(ctx context.Context, e *Env) Outcome {
		rawReq{"POST", sprite(e, "/fork"), nil, nil}.send(ctx, e)
		return ok("fine", map[string]any{"keep": "me"})
	})
	p2 := quick(2, gRO, time.Minute, func(ctx context.Context, e *Env) Outcome {
		rawReq{"GET", sprite(e, ""), nil, nil}.send(ctx, e)
		return ok("clean", nil)
	})
	rep := r.runner(p1, p2).Run(context.Background())
	res1 := result(t, rep.Report, 1)
	if res1.Status != Error || !strings.HasPrefix(res1.Finding, "guard refused POST /v1/sprites/") || !strings.HasSuffix(res1.Finding, "/fork") {
		t.Fatalf("first probe: %+v", res1)
	}
	if res1.Evidence["keep"] != "me" {
		t.Fatalf("the probe's own evidence was dropped: %v", res1.Evidence)
	}
	if got, _ := res1.Evidence["guard_refusals"].([]any); len(got) != 1 {
		t.Fatalf("guard_refusals: %v", res1.Evidence["guard_refusals"])
	}
	res2 := result(t, rep.Report, 2)
	if res2.Status != Answered || res2.Finding != "clean" {
		t.Fatalf("a clean probe after a refused one: %+v", res2)
	}
	if _, has := res2.Evidence["guard_refusals"]; has {
		t.Fatalf("the refusal leaked into the next probe: %v", res2.Evidence)
	}
	if rep.Exit != ExitProbe {
		t.Fatalf("exit %d, want %d", rep.Exit, ExitProbe)
	}
	if !strings.Contains(Markdown(rep.Report), "Guard refusals: 1") {
		t.Fatalf("the report does not count the refusal:\n%s", Markdown(rep.Report))
	}
	if !strings.Contains(r.log.String(), "guard refused POST /v1/sprites/") {
		t.Fatalf("the log lacks the refusal:\n%s", r.log.String())
	}
}

func TestRouteInterruptedAndRefused(t *testing.T) {
	r := newRig(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	p := quick(1, gRO, time.Minute, func(pctx context.Context, e *Env) Outcome {
		rawReq{"POST", sprite(e, "/fork"), nil, nil}.send(pctx, e)
		cancel()
		<-pctx.Done()
		return ok("fine", nil)
	})
	rep := r.runner(p).Run(ctx)
	res := result(t, rep.Report, 1)
	i, j := strings.Index(res.Finding, "interrupted"), strings.Index(res.Finding, "; guard refused POST /v1/sprites/")
	if res.Status != Error || i != 0 || j < 0 {
		t.Fatalf("finding %q status %q: want the interruption first, then the refusal", res.Finding, res.Status)
	}
	if got, _ := res.Evidence["guard_refusals"].([]any); len(got) != 1 {
		t.Fatalf("guard_refusals: %v", res.Evidence["guard_refusals"])
	}
}

func TestRouteAPanicAfterARefusalKeepsBothFindings(t *testing.T) {
	r := newRig(t, nil)
	p := quick(1, gRO, time.Minute, func(ctx context.Context, e *Env) Outcome {
		rawReq{"POST", sprite(e, "/fork"), nil, nil}.send(ctx, e)
		panic("kaboom")
	})
	rep := r.runner(p).Run(context.Background())
	res := result(t, rep.Report, 1)
	if res.Status != Error || !strings.HasPrefix(res.Finding, "probe panicked: kaboom") || !strings.Contains(res.Finding, "; guard refused POST /v1/sprites/") {
		t.Fatalf("%+v", res)
	}
}

func TestRouteSeveralRefusalsAreCountedInOneFinding(t *testing.T) {
	r := newRig(t, nil)
	p := quick(1, gRO, time.Minute, func(ctx context.Context, e *Env) Outcome {
		rawReq{"POST", sprite(e, "/fork"), nil, nil}.send(ctx, e)
		rawReq{"DELETE", sprite(e, ""), nil, nil}.send(ctx, e)
		rawReq{"GET", "/v1/sprites/" + notInBook, nil, nil}.send(ctx, e)
		return ok("fine", nil)
	})
	rep := r.runner(p).Run(context.Background())
	res := result(t, rep.Report, 1)
	if res.Status != Error || !strings.HasPrefix(res.Finding, "guard refused POST /v1/sprites/") || !strings.HasSuffix(res.Finding, " (+2 more)") {
		t.Fatalf("finding %q", res.Finding)
	}
	got, _ := res.Evidence["guard_refusals"].([]any)
	if len(got) != 3 || !strings.HasPrefix(fmt.Sprint(got[1]), "DELETE /v1/sprites/") || fmt.Sprint(got[2]) != "GET /v1/sprites/"+notInBook {
		t.Fatalf("guard_refusals: %v", res.Evidence["guard_refusals"])
	}
	if !strings.Contains(Markdown(rep.Report), "Guard refusals: 3") {
		t.Fatalf("the report does not count the refusals:\n%s", Markdown(rep.Report))
	}
}

func TestRouteALeftoverBeatsTheProbeExit(t *testing.T) {
	r := newRig(t, nil)
	stray := NamePrefix + "t1-77"
	rep := r.runner(quick(1, gRO, time.Minute, func(ctx context.Context, e *Env) Outcome {
		rawReq{"POST", sprite(e, "/fork"), nil, nil}.send(ctx, e)
		if _, err := r.c.CreateSprite(ctx, stray, nil); err != nil {
			return failed(err.Error(), nil)
		}
		return ok("fine", nil)
	})).Run(context.Background())
	if rep.Exit != ExitLeftover {
		t.Fatalf("exit %d, want %d", rep.Exit, ExitLeftover)
	}
}

func TestRouteARawDeleteSpriteRefusalFailsTheProbe(t *testing.T) {
	r := newRig(t, nil)
	var name string
	rep := r.runner(quick(1, gRO, time.Minute, func(ctx context.Context, e *Env) Outcome {
		name = e.Sprite
		e.API.RawDeleteSprite(ctx, e.Sprite)
		return ok("fine", nil)
	})).Run(context.Background())
	res := result(t, rep.Report, 1)
	if want := "guard refused DELETE /v1/sprites/" + name + " (RawDeleteSprite)"; res.Status != Error || !strings.Contains(res.Finding, want) {
		t.Fatalf("%+v, want %q", res, want)
	}
	if rep.Exit != ExitProbe {
		t.Fatalf("exit %d", rep.Exit)
	}
}

// The one-live-Sprite rule's own refusals are the rule working, not a guard refusal.
func TestRouteTheOneLiveRuleIsNotAGuardRefusal(t *testing.T) {
	r := newRig(t, nil)
	var second error
	var tryCode int
	rep := r.runner(quick(1, gRO, time.Minute, func(ctx context.Context, e *Env) Outcome {
		_, second = e.API.CreateSprite(ctx, e.NextName(), nil)
		tryCode, _ = e.TryCreate(ctx, e.NextName(), nil)
		_, _ = e.API.CreateSprite(ctx, "other-prod-1", nil)
		return ok("fine", nil)
	})).Run(context.Background())
	if second == nil || tryCode != -1 {
		t.Fatalf("second %v try %d", second, tryCode)
	}
	if res := result(t, rep.Report, 1); res.Status != Answered {
		t.Fatalf("%+v", res)
	}
	if rep.Exit != ExitOK {
		t.Fatalf("exit %d", rep.Exit)
	}
}
