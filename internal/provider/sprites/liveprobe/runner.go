package liveprobe

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/Exploded-views/bitc/internal/provider"
	"github.com/Exploded-views/bitc/internal/provider/sprites"
)

// Exit codes of the tool.
const (
	ExitOK       = 0
	ExitProbe    = 1 // a probe ended in error, or the run could not start
	ExitUsage    = 64
	ExitLeftover = 3 // a Sprite was left behind: loud failure
)

// RunResult is a finished run.
type RunResult struct {
	Report Report
	Exit   int
}

// Runner runs a plan with the rails of S.0.
type Runner struct {
	cfg    Config
	api    API
	probes []Probe
	marker string
	red    *Redactor
	log    io.Writer

	mu      sync.Mutex
	recs    []*SpriteRec
	live    map[string]*SpriteRec
	n       int
	results []Result

	// refusals is the running probe's guard refusals ("METHOD path"); runProbe resets it before
	// the probe and reads it after. refusalTotal counts them all for the report.
	refusals     []string
	refusalTotal int
}

// NewRunner builds a runner over api (the unguarded client). probes is usually AllProbes().
func NewRunner(cfg Config, api API, probes []Probe) *Runner {
	cfg = cfg.withDefaults()
	var b [32]byte
	_, _ = rand.Read(b[:])
	r := &Runner{cfg: cfg, api: api, probes: probes, marker: hex.EncodeToString(b[:]), live: map[string]*SpriteRec{}}
	r.red = NewRedactor(append(api.Secrets(), r.marker)...)
	r.log = r.red.Writer(cfg.Log)
	return r
}

// Marker is the run's random marker; only ShortMarker's worth may be shown.
func (r *Runner) Marker() string { return r.marker }

func (r *Runner) logf(format string, a ...any) { fmt.Fprintf(r.log, format+"\n", a...) }

func (r *Runner) now() time.Time { return r.cfg.Now() }

func (r *Runner) nextName() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.n++
	return fmt.Sprintf("%s%s-%d", NamePrefix, r.cfg.RunID, r.n)
}

// ---- the Sprite book

func (r *Runner) register(name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !strings.HasPrefix(name, NamePrefix+r.cfg.RunID+"-") {
		return fmt.Errorf("liveprobe: Sprite name %q does not start with %s%s-", name, NamePrefix, r.cfg.RunID)
	}
	if len(r.live) >= MaxLive {
		return ErrSecondSprite
	}
	rec := &SpriteRec{Name: name, Created: r.now()}
	r.live[name] = rec
	r.recs = append(r.recs, rec)
	return nil
}

// unregister drops a Sprite that was definitely not created.
func (r *Runner) unregister(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.live, name)
	for i, rec := range r.recs {
		if rec.Name == name {
			r.recs = append(r.recs[:i], r.recs[i+1:]...)
			return
		}
	}
}

func (r *Runner) confirmDeleted(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if rec := r.live[name]; rec != nil {
		rec.Deleted, rec.DeleteOK = r.now(), true
		delete(r.live, name)
	}
}

func (r *Runner) isLive(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.live[name] != nil
}

// spentSeconds is Sprite time from each create to its confirmed delete (or now).
func (r *Runner) spentSeconds() float64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	var s float64
	for _, rec := range r.recs {
		end := rec.Deleted
		if !rec.DeleteOK {
			end = r.now()
		}
		s += end.Sub(rec.Created).Seconds()
	}
	return s
}

func (r *Runner) spentUSD() float64 { return r.spentSeconds() * r.cfg.PricePerHour / 3600 }

// ---- the guarded API

type guarded struct {
	API
	r *Runner
}

func (g guarded) CreateSprite(ctx context.Context, name string, labels map[string]string) (sprites.Sprite, error) {
	if err := g.r.register(name); err != nil {
		return sprites.Sprite{}, err
	}
	s, err := g.API.CreateSprite(ctx, name, labels)
	if err != nil {
		var pa *provider.ErrPreAttach
		if !errors.As(err, &pa) {
			g.r.unregister(name)
		}
	}
	return s, err
}

func (g guarded) DeleteSprite(ctx context.Context, name string) error {
	err := deleteAny(ctx, g.API, name)
	if err == nil {
		g.r.confirmDeleted(name)
	}
	return err
}

// RawDeleteSprite is refused: only deleteAny (which is handed the unguarded API) may raw-delete,
// so a probe cannot skip confirmDeleted. The refusal fails the probe that asked.
func (g guarded) RawDeleteSprite(ctx context.Context, name string) (int, []byte) {
	g.r.refuse("DELETE /v1/sprites/" + name + " (RawDeleteSprite)")
	return 0, nil
}

// refuse records a guard refusal for the running probe and logs it.
func (r *Runner) refuse(what string) {
	r.mu.Lock()
	r.refusals = append(r.refusals, what)
	r.refusalTotal++
	r.mu.Unlock()
	r.logf("guard refused %s", what)
}

// takeRefusals returns the refusals recorded since resetRefusals.
func (r *Runner) takeRefusals() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.refusals...)
}

func (r *Runner) resetRefusals() {
	r.mu.Lock()
	r.refusals = nil
	r.mu.Unlock()
}

// deleteAny deletes name through the client; when the client refuses the name as invalid (the
// server may still have accepted it, which is what probe 13 looks for) it sends a raw DELETE
// that escapes the name once. A 2xx means the Sprite is gone. A 404 is confirmed by listing
// the kit's Sprites (the server's prefix filter only ever sees the plain NamePrefix, never the odd
// name) and matching the name exactly, because a server whose delete route rejects an odd name it
// accepted at create can answer 404 for a Sprite that still runs: a Sprite still listed, or a list
// that fails, is an error. A name outside NamePrefix cannot be found by that list, so its 404 is an
// error too.
func deleteAny(ctx context.Context, api API, name string) error {
	err := api.DeleteSprite(ctx, name)
	var inel *provider.ErrIneligible
	if err == nil || !errors.As(err, &inel) {
		return err
	}
	code, _ := api.RawDeleteSprite(ctx, name)
	if code >= 200 && code < 300 {
		return nil
	}
	if code != 404 {
		return fmt.Errorf("raw delete of %q answered %d", name, code)
	}
	if !strings.HasPrefix(name, NamePrefix) {
		return fmt.Errorf("raw delete of %q answered 404 and that cannot be confirmed: the name is outside the kit prefix", name)
	}
	listed, lerr := api.ListSprites(ctx, NamePrefix)
	if lerr != nil {
		return fmt.Errorf("raw delete of %q answered 404 and that could not be confirmed: %w", name, lerr)
	}
	for _, s := range listed {
		if s.Name == name {
			return fmt.Errorf("raw delete of %q answered 404 but the Sprite is still listed", name)
		}
	}
	return nil
}

// nameRE is the client's Sprite name rule (the client keeps its own copy unexported).
var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// Raw is default-deny for writes. It refuses a create first (it would dodge the one-live-Sprite
// rule; Env.TryCreate is the way), normalising the method and path (any case, doubled slashes,
// dot segments). Then a read goes through unless it is under a Sprite that is not live in the book
// (or carries a body or a method-override query), and a write goes through only on the fixed
// table of rawWriteAllowed. Every refusal answers 0, sends nothing and fails the probe.
func (g guarded) Raw(ctx context.Context, method, p string, q url.Values, body any) (int, []byte) {
	if strings.EqualFold(method, "POST") && strings.EqualFold(path.Clean(p), "/v1/sprites") {
		g.r.refuse(strings.ToUpper(method) + " " + p)
		return 0, nil
	}
	if !g.r.rawAllowed(method, p, q, body) {
		g.r.refuse(strings.ToUpper(method) + " " + p)
		return 0, nil
	}
	return g.API.Raw(ctx, method, p, q, body)
}

// rawAllowed decides a raw request after the create check. The client below it already refuses
// any path that is not /v1/ plus plain segments, so the path is not cleaned here.
func (r *Runner) rawAllowed(method, p string, q url.Values, body any) bool {
	if strings.EqualFold(method, "GET") || strings.EqualFold(method, "HEAD") {
		return r.rawReadAllowed(p, q, body)
	}
	return r.rawWriteAllowed(method, p, q)
}

// spritesSeg reports whether a segment after /v1 is the Sprites collection in any spelling a
// server might route to it: "sprites" ignoring case, or that followed by "." or "~".
func spritesSeg(s string) bool {
	l := strings.ToLower(s)
	return l == "sprites" || strings.HasPrefix(l, "sprites.") || strings.HasPrefix(l, "sprites~")
}

func (r *Runner) rawReadAllowed(p string, q url.Values, body any) bool {
	if body != nil {
		return false
	}
	for k := range q {
		switch strings.ToLower(k) {
		case "_method", "method", "x-http-method-override":
			return false
		}
	}
	if strings.HasPrefix(p, "/v1/") {
		segs := strings.Split(p[1:], "/")
		if len(segs) >= 3 && spritesSeg(segs[1]) {
			return r.isLive(segs[2])
		}
	}
	return true
}

// rawWriteAllowed is the write table: PUT and PATCH /v1/sprites/{s}, POST
// /v1/sprites/{s}/policy/privileges and /v1/sprites/{s}/policy/resources, with {s} a live book
// name that is a valid Sprite name, and no query. Literal segments are byte-exact; the method
// compare ignores case.
func (r *Runner) rawWriteAllowed(method, p string, q url.Values) bool {
	if len(q) > 0 {
		return false
	}
	segs := strings.Split(p, "/")
	if len(segs) < 4 || segs[0] != "" || segs[1] != "v1" || segs[2] != "sprites" {
		return false
	}
	if !nameRE.MatchString(segs[3]) || !r.isLive(segs[3]) {
		return false
	}
	switch strings.ToUpper(method) {
	case "PUT", "PATCH":
		return len(segs) == 4
	case "POST":
		return len(segs) == 6 && segs[4] == "policy" && (segs[5] == "privileges" || segs[5] == "resources")
	}
	return false
}

func (r *Runner) tryCreate(ctx context.Context, name string, labels map[string]string) (int, []byte) {
	if err := r.register(name); err != nil {
		return -1, []byte(err.Error())
	}
	body := map[string]any{"name": name}
	if len(labels) > 0 {
		body["labels"] = labels
	}
	code, b := r.api.Raw(ctx, "POST", "/v1/sprites", nil, body)
	if code >= 400 && code < 500 {
		r.unregister(name)
	}
	return code, b
}

// ---- running

func (r *Runner) add(res Result) {
	r.mu.Lock()
	r.results = append(r.results, r.red.Clean(res))
	r.mu.Unlock()
}

func (r *Runner) skipAll(ps []Probe, why string) {
	for _, p := range ps {
		r.add(Result{ID: p.ID, Status: Skipped, Finding: why})
	}
}

func (r *Runner) newEnv(name string) *Env {
	cfg := r.cfg
	cfg.Log = r.log
	return &Env{API: guarded{r.api, r}, Cfg: cfg, Sprite: name, Marker: r.marker, State: map[string]any{},
		now: r.now, sleep: r.cfg.Sleep, tryCreate: r.tryCreate, nextName: r.nextName,
		deleteSelf: func(ctx context.Context) error { return guarded{r.api, r}.DeleteSprite(ctx, name) }}
}

// Run executes the plan: a pre-run sweep, the groups in order, a final sweep.
func (r *Runner) Run(ctx context.Context) RunResult {
	rep := Report{Run: r.cfg.RunID}
	finish := func(exit int) RunResult {
		r.finalize(&rep)
		for _, res := range rep.Results {
			if res.Status == Error && exit == ExitOK {
				exit = ExitProbe
			}
		}
		if len(rep.Leftovers) > 0 {
			exit = ExitLeftover
		}
		return RunResult{Report: rep, Exit: exit}
	}
	if r.api.Org() != r.cfg.Org {
		rep.Fatal = fmt.Sprintf("the client's org %q is not --org %q", r.api.Org(), r.cfg.Org)
		return RunResult{Report: rep, Exit: ExitUsage}
	}
	plan, err := BuildPlan(r.probes, r.cfg.Probes)
	if err != nil {
		rep.Fatal = err.Error()
		return RunResult{Report: rep, Exit: ExitUsage}
	}
	deadline := r.now().Add(time.Duration(r.cfg.MaxMinutes) * time.Minute)
	ctx, cancel := context.WithTimeout(ctx, time.Duration(r.cfg.MaxMinutes)*time.Minute)
	defer cancel()

	// an earlier run's Sprites would break the one-live-Sprite rule and the spend cap
	left, err := r.api.ListSprites(ctx, NamePrefix)
	if err != nil {
		rep.Fatal = "pre-run sweep: could not list Sprites: " + errText(err)
		return RunResult{Report: rep, Exit: ExitProbe}
	}
	for _, s := range left {
		rep.Preexisting = append(rep.Preexisting, s.Name)
		r.logf("deleting %s, left by an earlier run", s.Name)
		if err := r.deleteWithRetry(s.Name); err != nil {
			rep.Fatal = "pre-run sweep: could not delete " + s.Name + ": " + errText(err)
			rep.Leftovers = append(rep.Leftovers, s.Name)
			return finish(ExitProbe)
		}
	}
	for _, g := range plan.Groups {
		r.runGroup(ctx, g, deadline)
	}
	return finish(ExitOK)
}

// deleteWithRetry deletes name on its own (uncancelable) context, three tries.
func (r *Runner) deleteWithRetry(name string) error {
	var err error
	for i := 0; i < 3; i++ {
		dctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		err = deleteAny(dctx, r.api, name)
		cancel()
		if err == nil {
			r.confirmDeleted(name)
			return nil
		}
		_ = r.cfg.Sleep(context.Background(), 2*time.Second)
	}
	return err
}

func (r *Runner) runGroup(ctx context.Context, g PlanGroup, deadline time.Time) {
	var run []Probe
	for _, p := range g.Probes {
		if p.Skip != nil {
			if why := p.Skip(r.cfg); why != "" {
				r.add(Result{ID: p.ID, Status: Skipped, Finding: why})
				continue
			}
		}
		run = append(run, p)
	}
	if len(run) == 0 {
		return
	}
	if ctx.Err() != nil {
		r.skipAll(run, "skipped: the run was interrupted or hit the wall-clock cap")
		return
	}
	if g.Name == gNone {
		env := r.newEnv("")
		for _, p := range run {
			r.runProbe(ctx, p, env, p.Own)
			r.cleanupAll() // an Own probe may end (error, panic, signal) with a Sprite still live
		}
		return
	}
	worstD := g.Worst(r.cfg)
	if need := r.spentUSD() + USD(worstD, r.cfg.PricePerHour); need > r.cfg.MaxUSD {
		r.skipAll(run, fmt.Sprintf("skipped: spend cap (spent $%.4f + worst case $%.4f would pass $%.2f)", r.spentUSD(), USD(worstD, r.cfg.PricePerHour), r.cfg.MaxUSD))
		return
	}
	if r.now().Add(worstD).After(deadline) {
		r.skipAll(run, "skipped: wall-clock cap (--max-minutes)")
		return
	}
	name := r.nextName()
	first := len(r.snapshot())
	setupStart := r.now()
	defer r.cleanup(name)
	spec := Groups[g.Name]
	_, err := guarded{r.api, r}.CreateSprite(ctx, name, map[string]string{"bitc": "1"})
	if err == nil {
		var pol sprites.NetworkPolicy
		pol, err = sprites.AllowDomains(spec.Domains...)
		if err == nil {
			err = r.api.ApplyBuildPolicies(ctx, name, pol, sprites.Resources{LimitMB: max(spec.MemMB, 512)})
		}
	}
	if err != nil {
		for _, p := range run {
			r.add(Result{ID: p.ID, Status: Error, Finding: "could not make the Sprite: " + errText(err)})
		}
		return
	}
	env := r.newEnv(name)
	setup := r.now().Sub(setupStart).Seconds()
	for i, p := range run {
		if ctx.Err() != nil {
			r.skipAll(run[i:], "skipped: the run was interrupted or hit the wall-clock cap")
			break
		}
		r.runProbe(ctx, p, env, true)
	}
	// the create and the delete are Sprite time too: charge them to the first and last probe
	r.mu.Lock()
	if len(r.results) > first {
		r.results[first].SpriteSeconds += setup
	}
	r.mu.Unlock()
	endProbes := r.now()
	r.cleanup(name)
	if tail := r.now().Sub(endProbes).Seconds(); tail > 0 {
		r.mu.Lock()
		if len(r.results) > first {
			r.results[len(r.results)-1].SpriteSeconds += tail
		}
		r.mu.Unlock()
	}
}

func (r *Runner) snapshot() []Result {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Result(nil), r.results...)
}

// cleanupAll deletes every Sprite still in the book, on an uncancelable context.
func (r *Runner) cleanupAll() {
	r.mu.Lock()
	names := make([]string, 0, len(r.live))
	for n := range r.live {
		names = append(names, n)
	}
	r.mu.Unlock()
	for _, n := range names {
		r.cleanup(n)
	}
}

// cleanup deletes name when it is still live.
func (r *Runner) cleanup(name string) {
	if !r.isLive(name) {
		return
	}
	if err := r.deleteWithRetry(name); err != nil {
		r.logf("LOUD: could not delete %s: %s", name, errText(err))
	}
}

func (r *Runner) runProbe(ctx context.Context, p Probe, env *Env, hasSprite bool) {
	if need := r.spentUSD() + USD(p.Worst, r.cfg.PricePerHour); need > r.cfg.MaxUSD {
		r.add(Result{ID: p.ID, Status: Skipped, Finding: fmt.Sprintf("skipped: spend cap (spent $%.4f + worst case $%.4f would pass $%.2f)", r.spentUSD(), USD(p.Worst, r.cfg.PricePerHour), r.cfg.MaxUSD)})
		return
	}
	pctx, cancel := context.WithTimeout(ctx, p.Worst)
	defer cancel()
	t0 := r.now()
	r.resetRefusals()
	panicked := false
	out := func() (o Outcome) {
		defer func() {
			if v := recover(); v != nil {
				panicked = true
				o = Outcome{Status: Error, Finding: fmt.Sprintf("probe panicked: %v", v)}
			}
		}()
		return p.Run(pctx, env)
	}()
	dt := r.now().Sub(t0).Seconds()
	interrupted := ctx.Err() != nil
	switch {
	case interrupted:
		out = Outcome{Status: Error, Finding: "interrupted: the run was cancelled or hit the wall-clock cap", Evidence: out.Evidence}
	case pctx.Err() != nil && out.Status == Error:
		out.Finding += " (cut at the probe's declared worst case " + p.Worst.String() + ")"
	}
	switch out.Status {
	case Answered, AssumeWorst, Skipped, Error:
	default:
		out = Outcome{Status: Error, Finding: "probe returned no status", Evidence: out.Evidence}
	}
	// A guard refusal answered the probe 0 and nothing else, so only this record can fail it. It is
	// applied last: an interruption or a panic finding stays first, and the probe's evidence stays.
	if refs := r.takeRefusals(); len(refs) > 0 {
		f := "guard refused " + refs[0]
		if len(refs) > 1 {
			f += fmt.Sprintf(" (+%d more)", len(refs)-1)
		}
		if interrupted || panicked {
			f = out.Finding + "; " + f
		}
		ev := map[string]any{}
		for k, v := range out.Evidence {
			ev[k] = v
		}
		ev["guard_refusals"] = refs
		out = Outcome{Status: Error, Finding: f, Evidence: ev}
	}
	res := Result{ID: p.ID, Status: out.Status, Finding: out.Finding, Evidence: out.Evidence}
	if hasSprite {
		res.SpriteSeconds = dt
	}
	r.add(res)
	r.logf("item %d %s: %s | running total $%.4f of $%.2f", p.ID, out.Status, oneLine(res.Finding, 100), r.spentUSD(), r.cfg.MaxUSD)
}

// finalize runs the final sweep and fills the report.
func (r *Runner) finalize(rep *Report) {
	sctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	left, err := r.api.ListSprites(sctx, NamePrefix)
	switch {
	case err != nil:
		rep.Leftovers = append(rep.Leftovers, "(final sweep could not list Sprites: "+errText(err)+")")
	default:
		for _, s := range left {
			already := false
			for _, l := range rep.Leftovers {
				already = already || l == s.Name
			}
			if !already {
				rep.Leftovers = append(rep.Leftovers, s.Name)
			}
			if err := r.deleteWithRetry(s.Name); err != nil {
				r.logf("LOUD: leftover %s could not be deleted: %s", s.Name, errText(err))
			}
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	rep.Results = append([]Result(nil), r.results...)
	rep.GuardRefusals = r.refusalTotal
	var total float64
	for _, rec := range r.recs {
		c := *rec
		end := c.Deleted
		if !c.DeleteOK {
			end = r.now()
		}
		c.Seconds = end.Sub(c.Created).Seconds()
		for _, l := range rep.Leftovers {
			c.LeftoverFound = c.LeftoverFound || l == c.Name
		}
		total += c.Seconds
		rep.Sprites = append(rep.Sprites, c)
	}
	rep.Spent = total * r.cfg.PricePerHour / 3600
}
