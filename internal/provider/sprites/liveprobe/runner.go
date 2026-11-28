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
	"strconv"
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

// registerRec adds name to the book and hands back the record it appended, so the caller can undo
// exactly that record (unregisterRec) and no other. It is the book's one entry point.
func (r *Runner) registerRec(name string) (*SpriteRec, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !strings.HasPrefix(name, NamePrefix+r.cfg.RunID+"-") {
		return nil, fmt.Errorf("liveprobe: Sprite name %q does not start with %s%s-", name, NamePrefix, r.cfg.RunID)
	}
	if len(r.live) >= MaxLive {
		return nil, ErrSecondSprite
	}
	rec := &SpriteRec{Name: name, Created: r.now()}
	r.live[name] = rec
	r.recs = append(r.recs, rec)
	return rec, nil
}

// unregisterRec removes one record, matched by pointer and never by name: the live entry only when
// it is that record, and that record from recs. An earlier record of the same name (a Sprite the
// run deleted) keeps its Deleted time and DeleteOK.
func (r *Runner) unregisterRec(rec *SpriteRec) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.live[rec.Name] == rec {
		delete(r.live, rec.Name)
	}
	for i, x := range r.recs {
		if x == rec {
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

// isKnown reports whether the run ever registered name, live or deleted.
func (r *Runner) isKnown(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.knownLocked(name)
}

func (r *Runner) knownLocked(name string) bool {
	for _, rec := range r.recs {
		if rec.Name == name {
			return true
		}
	}
	return false
}

// gate lets a typed call through when name is live in the book (live) or known to it (otherwise).
// A refusal is recorded for the running probe, sends nothing and is an ErrIneligible.
func (r *Runner) gate(method, name string, liveOnly bool) error {
	if liveOnly && r.isLive(name) || !liveOnly && r.isKnown(name) {
		return nil
	}
	what := "live in the run"
	if !liveOnly {
		what = "known to the run"
	}
	r.refuse(method + " " + name + " (not " + what + ")")
	return &provider.ErrIneligible{Why: "guard: " + method + " on a Sprite that is not " + what}
}

// gateRun is gate for Run and Exec: a live Sprite, and MaxRunAfterDisconnect within its cap.
func (r *Runner) gateRun(method, name string, req sprites.ExecRequest) error {
	if err := r.gate(method, name, true); err != nil {
		return err
	}
	if req.MaxRunAfterDisconnect > MaxRunAfterDisconnect {
		r.refuse(method + " " + name + " (MaxRunAfterDisconnect over " + MaxRunAfterDisconnect.String() + ")")
		return &provider.ErrIneligible{Why: "guard: " + method + " with MaxRunAfterDisconnect over " + MaxRunAfterDisconnect.String()}
	}
	return nil
}

// definiteNonCreate reports whether a restore's error means the Sprite was surely not recreated:
// a 404 (ErrNotFound), a 401, 402 or 403 (ErrUnavailable), another 4xx or a refusal before sending
// (ErrIneligible). Any other error (429, quota, 5xx, a network error, a timeout) may have recreated it.
func definiteNonCreate(err error) bool {
	var inel *provider.ErrIneligible
	var un *provider.ErrUnavailable
	return errors.Is(err, sprites.ErrNotFound) || errors.As(err, &inel) || errors.As(err, &un)
}

// tryRestore is Env.TryRestore. Three cases, chosen from the book: an unknown name is a guard
// refusal; a live name is a plain restore (no register, no unregister); a name the run deleted is
// registered first, because a restore may recreate the Sprite, and kept unless the restore surely
// did not.
func (r *Runner) tryRestore(ctx context.Context, name, id string) error {
	r.mu.Lock()
	live, known := r.live[name] != nil, r.knownLocked(name)
	r.mu.Unlock()
	switch {
	case live:
		return r.api.RestoreCheckpoint(ctx, name, id)
	case !known:
		r.refuse("TryRestore " + name + " (not known to the run)")
		return &provider.ErrIneligible{Why: "guard: TryRestore on a Sprite that is not known to the run"}
	}
	rec, err := r.registerRec(name)
	if err != nil {
		return err
	}
	err = r.api.RestoreCheckpoint(ctx, name, id)
	if err != nil && definiteNonCreate(err) {
		r.unregisterRec(rec)
	}
	return err
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
	rec, err := g.r.registerRec(name)
	if err != nil {
		return sprites.Sprite{}, err
	}
	s, err := g.API.CreateSprite(ctx, name, labels)
	if err != nil {
		var pa *provider.ErrPreAttach
		if !errors.As(err, &pa) {
			// only the record this create added: an earlier record of a re-used name stays
			g.r.unregisterRec(rec)
		}
	}
	return s, err
}

// DeleteSprite needs a name the run knows, live or deleted (a delete of a deleted name is a probe
// asking what the API says to it). A name the run never registered is refused: it could be any
// Sprite of the org.
//
// Residual: a known but deleted name is let through to a real DELETE, so a Sprite that another
// actor made under that exact name after the run deleted it could be deleted. The names carry
// NamePrefix, RunID and a counter, which makes that very unlikely, and probe 25 needs the known gate.
func (g guarded) DeleteSprite(ctx context.Context, name string) error {
	if err := g.r.gate("DeleteSprite", name, false); err != nil {
		return err
	}
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

// The typed writes need a live Sprite of the run. A refusal sends nothing, fails the probe that
// asked and answers ErrIneligible.

func (g guarded) ApplyBuildPolicies(ctx context.Context, name string, build sprites.NetworkPolicy, res sprites.Resources) error {
	if err := g.r.gate("ApplyBuildPolicies", name, true); err != nil {
		return err
	}
	// Resources carries LimitMB and nothing else: the same 1..MaxLimitMB bounds as Raw's resources body
	if res.LimitMB < 1 || res.LimitMB > MaxLimitMB {
		g.r.refuse("ApplyBuildPolicies " + name + " (LimitMB outside 1.." + strconv.Itoa(MaxLimitMB) + ")")
		return &provider.ErrIneligible{Why: "guard: ApplyBuildPolicies with LimitMB outside 1.." + strconv.Itoa(MaxLimitMB)}
	}
	return g.API.ApplyBuildPolicies(ctx, name, build, res)
}

func (g guarded) ApplyJobPolicy(ctx context.Context, name string, job sprites.NetworkPolicy) error {
	if err := g.r.gate("ApplyJobPolicy", name, true); err != nil {
		return err
	}
	return g.API.ApplyJobPolicy(ctx, name, job)
}

func (g guarded) Run(ctx context.Context, name string, req sprites.ExecRequest, stdin []byte) (sprites.ExecResult, error) {
	if err := g.r.gateRun("Run", name, req); err != nil {
		return sprites.ExecResult{}, err
	}
	return g.API.Run(ctx, name, req, stdin)
}

func (g guarded) Exec(ctx context.Context, name string, req sprites.ExecRequest) (*sprites.Session, error) {
	if err := g.r.gateRun("Exec", name, req); err != nil {
		return nil, err
	}
	return g.API.Exec(ctx, name, req)
}

func (g guarded) CreateCheckpoint(ctx context.Context, name, comment string) (sprites.Checkpoint, error) {
	if err := g.r.gate("CreateCheckpoint", name, true); err != nil {
		return sprites.Checkpoint{}, err
	}
	return g.API.CreateCheckpoint(ctx, name, comment)
}

// RestoreCheckpoint is live-only like the other writes; a restore of a Sprite the run deleted is
// Env.TryRestore.
func (g guarded) RestoreCheckpoint(ctx context.Context, name, id string) error {
	if err := g.r.gate("RestoreCheckpoint", name, true); err != nil {
		return err
	}
	return g.API.RestoreCheckpoint(ctx, name, id)
}

func (g guarded) DeleteCheckpoint(ctx context.Context, name, id string) error {
	if err := g.r.gate("DeleteCheckpoint", name, true); err != nil {
		return err
	}
	return g.API.DeleteCheckpoint(ctx, name, id)
}

func (g guarded) FSWrite(ctx context.Context, name, path string, data []byte) error {
	if err := g.r.gate("FSWrite", name, true); err != nil {
		return err
	}
	return g.API.FSWrite(ctx, name, path, data)
}

// The typed reads: GetSprite and FSRead can wake a Sprite, so they need a live one; ListCheckpoints
// and GetCheckpoint may also read a Sprite the run deleted (probe 25 asks what survived).

func (g guarded) GetSprite(ctx context.Context, name string) (sprites.Sprite, error) {
	if err := g.r.gate("GetSprite", name, true); err != nil {
		return sprites.Sprite{}, err
	}
	return g.API.GetSprite(ctx, name)
}

func (g guarded) FSRead(ctx context.Context, name, path string, w io.Writer) (int64, error) {
	if err := g.r.gate("FSRead", name, true); err != nil {
		return 0, err
	}
	return g.API.FSRead(ctx, name, path, w)
}

func (g guarded) ListCheckpoints(ctx context.Context, name string) ([]sprites.Checkpoint, error) {
	if err := g.r.gate("ListCheckpoints", name, false); err != nil {
		return nil, err
	}
	return g.API.ListCheckpoints(ctx, name)
}

func (g guarded) GetCheckpoint(ctx context.Context, name, id string) (sprites.Checkpoint, error) {
	if err := g.r.gate("GetCheckpoint", name, false); err != nil {
		return sprites.Checkpoint{}, err
	}
	return g.API.GetCheckpoint(ctx, name, id)
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
// table of rawWriteRoute, with a body that passes checkTableBody. Every refusal answers 0, sends nothing and fails the probe.
func (g guarded) Raw(ctx context.Context, method, p string, q url.Values, body any) (int, []byte) {
	if strings.EqualFold(method, "POST") && strings.EqualFold(path.Clean(p), "/v1/sprites") {
		g.r.refuse(strings.ToUpper(method) + " " + p)
		return 0, nil
	}
	send, ok := g.r.rawAllowed(method, p, q, body)
	if !ok {
		g.r.refuse(strings.ToUpper(method) + " " + p)
		return 0, nil
	}
	return g.API.Raw(ctx, method, p, q, send)
}

// rawAllowed decides a raw request after the create check and returns the body to send. The client
// below it already refuses any path that is not /v1/ plus plain segments, so the path is not cleaned
// here. A write's body is the checked re-marshal (checkTableBody), never the caller's value.
func (r *Runner) rawAllowed(method, p string, q url.Values, body any) (any, bool) {
	if strings.EqualFold(method, "GET") || strings.EqualFold(method, "HEAD") {
		return body, r.rawReadAllowed(p, q, body)
	}
	route := r.rawWriteRoute(method, p, q)
	if route == "" {
		return nil, false
	}
	checked, ok := checkTableBody(route, body)
	if !ok {
		return nil, false
	}
	return checked, true
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

// rawWriteRoute is the write table: PUT and PATCH /v1/sprites/{s} (routeSprite), POST
// /v1/sprites/{s}/policy/privileges (routePrivileges) and /v1/sprites/{s}/policy/resources
// (routeResources), with {s} a live book name that is a valid Sprite name, and no query. Literal
// segments are byte-exact; the method compare ignores case. It answers "" for anything else.
func (r *Runner) rawWriteRoute(method, p string, q url.Values) string {
	if len(q) > 0 {
		return ""
	}
	segs := strings.Split(p, "/")
	if len(segs) < 4 || segs[0] != "" || segs[1] != "v1" || segs[2] != "sprites" {
		return ""
	}
	if !nameRE.MatchString(segs[3]) || !r.isLive(segs[3]) {
		return ""
	}
	switch strings.ToUpper(method) {
	case "PUT", "PATCH":
		if len(segs) == 4 {
			return routeSprite
		}
	case "POST":
		if len(segs) == 6 && segs[4] == "policy" {
			switch segs[5] {
			case "privileges":
				return routePrivileges
			case "resources":
				return routeResources
			}
		}
	}
	return ""
}

func (r *Runner) tryCreate(ctx context.Context, name string, labels map[string]string) (int, []byte) {
	rec, err := r.registerRec(name)
	if err != nil {
		return -1, []byte(err.Error())
	}
	body := map[string]any{"name": name}
	if len(labels) > 0 {
		body["labels"] = labels
	}
	code, b := r.api.Raw(ctx, "POST", "/v1/sprites", nil, body)
	if code >= 400 && code < 500 {
		r.unregisterRec(rec)
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
		now: r.now, sleep: r.cfg.Sleep, tryCreate: r.tryCreate, tryRestore: r.tryRestore, nextName: r.nextName,
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
