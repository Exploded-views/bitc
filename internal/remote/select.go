package remote

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Exploded-views/bitc/internal/admit"
	"github.com/Exploded-views/bitc/internal/config"
	"github.com/Exploded-views/bitc/internal/exit"
	"github.com/Exploded-views/bitc/internal/provider"
	"github.com/Exploded-views/bitc/internal/run"
	"github.com/Exploded-views/bitc/internal/state"
)

func init() {
	run.RemoteWalk = Walk
	run.NoProviderLeft = NoProviderLeft
}

// NoProviderLeft: 69 if the last failure was ErrUnavailable or ErrIneligible, else 75 (8.2).
func NoProviderLeft(pool string, last error) error {
	var ee *exit.Error
	if errors.As(last, &ee) { // already a bitc outcome (for example "not available in this build")
		return last
	}
	var ine *provider.ErrIneligible
	var una *provider.ErrUnavailable
	if errors.As(last, &ine) || errors.As(last, &una) {
		return exit.Unavailablef("%s: no provider left: %v", pool, last)
	}
	return exit.Starved(pool, "no provider left: "+last.Error())
}

// silentlyIneligible: remote was never asked for (spec 8.2): no size, no profile, tree
// unmatched or nested. "" means eligible so far.
func silentlyIneligible(p *run.Prepared, typ string, inv run.Invocation) string {
	if p.Pool.RemoteSize[typ] == "" {
		return "no size for this pool"
	}
	if p.Pool.Remote == nil {
		return "no [pools." + p.Pool.Name + ".remote] profile"
	}
	matched := false
	for _, g := range p.Eff.RemoteLimits.Worktrees {
		if ok, _ := config.Match(g, p.Eff.Worktree); ok {
			matched = true
		}
	}
	if !matched {
		return "work tree not in remote_limits.worktrees"
	}
	for _, kv := range inv.Env {
		if v, ok := strings.CutPrefix(kv, "BITC_HOLD="); ok {
			if _, nested := admit.VerifyHold(p.Layout, v); nested {
				return "nested inside a hold"
			}
		}
	}
	return ""
}

// Walk tries one remote provider (spec 8.2). handled=false means "try the next provider";
// err is this provider's failure for the no-provider-left rule.
func Walk(ctx context.Context, inv run.Invocation, p *run.Prepared, typ string, stderr io.Writer) (run.Outcome, bool, error) {
	core := p.Eff.Providers[typ]
	if core == nil {
		return run.Outcome{}, false, &provider.ErrIneligible{Why: typ + " is not configured"}
	}
	if core.CleanupOnly { // defense in depth: config refuses these jobs before a walk starts
		return run.Outcome{}, false, &provider.ErrIneligible{Why: typ + " jobs are refused in this release"}
	}
	f, ok := provider.Lookup(typ, core)
	if !ok {
		return run.Outcome{}, false, &provider.ErrIneligible{Why: typ + " is not configured"}
	}
	if why := silentlyIneligible(p, typ, inv); why != "" {
		return run.Outcome{}, false, &provider.ErrIneligible{Why: why}
	}
	prov, err := f.New(core, p.Eff)
	if err != nil {
		return run.Outcome{}, false, &provider.ErrIneligible{Why: err.Error()}
	}
	defer CloseProvider(prov) // first deferred, so last to run: a plugin process outlives every cleanup step
	j := &Job{Inv: inv, P: p, Type: typ, Core: core, Prov: prov, Stdout: inv.Stdout, Stderr: stderr, Red: p.Redact}
	j.KeepSignals = true
	defer func() { // last: signals stay absorbed until every cleanup step has finished
		if j.StopSignals != nil {
			j.StopSignals()
		}
	}()
	defer func() {
		for i := len(j.Cleanup) - 1; i >= 0; i-- {
			j.Cleanup[i]()
		}
	}()
	if st, ok := prov.(provider.Starter); ok { // a plugin: spawn, hello, configure (A.4)
		rt := provider.Runtime{Red: p.Redact, Stderr: stderr, Debug: p.Eff.LogLevel == "debug"}
		if err := st.Start(ctx, rt); err != nil {
			var ec *provider.ErrConfig
			if errors.As(err, &ec) { // 78: the host file is wrong, whichever provider comes next
				e := exit.Configf("%s: %s", p.Eff.HostFile, ec.Why)
				fmt.Fprintln(stderr, e.Line)
				return run.Outcome{Code: e.Code, Result: run.Result{Origin: "bitc", Outcome: e.Outcome, ExitCode: e.Code,
					Reason: e.Reason, Pool: p.Pool.Name, Provider: typ, Job: p.Job}}, true, e
			}
			return j.fallback(err)
		}
	}
	if j.Spec, err = buildSpec(p, typ, core, prov); err != nil {
		return j.fallback(err)
	}
	for _, s := range [][32]byte{j.Spec.Bearer, j.Spec.MacKey, j.Spec.Nonce} {
		p.Redact.Add(fmt.Sprintf("%x", s))
	}
	if err := Preflight(ctx, j); err != nil {
		return j.fallback(err)
	}
	release, err := Rails(ctx, j)
	if err != nil {
		return j.fallback(err)
	}
	j.Release = release
	j.Cleanup = append(j.Cleanup, release)
	cleanup, err := Transport(ctx, j)
	if err != nil {
		return j.fallback(err)
	}
	j.DeleteRef = sync.OnceFunc(cleanup) // at BEGIN and, at the latest, at the end; only once
	j.Cleanup = append(j.Cleanup, j.DeleteRef)
	var code int
	var began bool
	var n int
	var suffix string // the last pre-attach discard's "<id> torn down before attach" / "left for sweep"
	for attempt := 1; ; attempt++ {
		if OnLaunch != nil {
			OnLaunch(j)
		}
		n = len(j.Cleanup) // what RunJob appends (teardown, reap, staging) runs before the ledger end
		code, began, err = RunJob(ctx, j)
		// Spec 8.3's cleanup order: tear down (and reap, remove staging) first, then the ledger end;
		// the deferred loop runs the rest afterwards (the reservation and launch lock, the ref).
		for i := len(j.Cleanup) - 1; i >= n; i-- {
			j.Cleanup[i]()
		}
		j.Cleanup = j.Cleanup[:n]
		var pd *PreAttachDiscard
		if !errors.As(err, &pd) {
			break
		}
		// Spec 8.2, B.3.3: a resource was made and discarded before job input reached it. It is a
		// ledger end per attempt (the minutes count), one breaker failure, and the reservation is
		// dropped when the resource is confirmed gone, else kept as an orphan for the sweep.
		if OnEnd != nil {
			OnEnd(j, "fallback")
		}
		if OnFailure != nil {
			OnFailure(j)
		}
		suffix = pd.ID + " torn down before attach"
		if pd.Confirmed {
			if j.Release != nil {
				j.Release()
			}
		} else {
			suffix = pd.ID + " left for sweep"
			if j.Orphan != nil {
				j.Orphan()
			}
		}
		var tr *provider.ErrTransient
		if j.Retried || !pd.Confirmed || !errors.As(pd.Inner, &tr) {
			// The job has at most one retry in total (spec 8.3): none if a create retry or an
			// earlier pre-attach retry spent it; none after an unconfirmed teardown.
			return j.fallback(&discarded{inner: pd.Inner, suffix: suffix})
		}
		j.Retried = true
		if err = j.newAttempt(); err != nil {
			return j.fallback(&discarded{inner: err, suffix: suffix})
		}
		// A signal in this window (cleanup, the retry's rails) is not lost: the one registration
		// stays live, no second resource is created, and the call ends 128+N (spec 8.3).
		wctx, stopWatch := j.signalWindow(ctx)
		var release func()
		release, err = j.retryRails(wctx, ctx)
		stopWatch()
		if release != nil {
			j.Release = release
			j.Cleanup = append(j.Cleanup, release)
		}
		var ie Interrupted
		if errors.As(err, &ie) {
			if OnLaunch != nil { // the attempt's ledger line, so its end pairs with it
				OnLaunch(j)
			}
			code, began = 0, false
			break
		}
		if err != nil { // the retry's own rails (cap, max_live, breaker) say no
			return j.fallback(&discarded{inner: err, suffix: suffix})
		}
	}
	// A resource exists once Launch returned it (RunJob set j.Created). From then on the job
	// never falls back (spec 8.2, 15.8): the loop above tore the resource down and the call is 75,
	// or 69 for a setup failure the agent classed as the repo's own (spec 15.9).
	created := !j.Created.IsZero()
	setup, repoSetup := repoSetupFailure(err)
	// A plugin launch with no definite outcome (spec 8.2, providers delta A.5.4): the adapter
	// reaped by name; a reap that did not confirm the name gone leaves the reservation as an
	// orphan, and the launch counts as one breaker failure although BEGIN never came.
	var amb *provider.ErrAttached
	ambiguous := errors.As(err, &amb) && amb.Ambiguous
	if ambiguous && amb.Unconfirmed && j.Orphan != nil {
		j.Orphan()
	}
	// An interrupt (RunJob's own signal handler, or `bitc cancel`) is never a provider failure:
	// it is checked first, recorded as "interrupted" and skips the breaker (spec 3.3, 8.3).
	var intr Interrupted
	interrupted := errors.As(err, &intr)
	if interrupted && j.reapUnconfirmed && j.Orphan != nil {
		j.Orphan()
	}
	if OnEnd != nil {
		OnEnd(j, endOutcome(code, began, created, interrupted, err))
	}
	if interrupted {
		c := 128 + int(intr.Sig)
		return run.Outcome{Code: c, Result: run.Result{Origin: "bitc", Outcome: "interrupted", ExitCode: c,
			Pool: p.Pool.Name, Provider: typ, Job: p.Job}}, true, nil
	}
	var pa interface{ PausesAfterAttach() bool }
	if err != nil && (began || created) && errors.As(err, &pa) && pa.PausesAfterAttach() && OnUnavailable != nil {
		if why, ok := pauseWorthy(err); ok { // a guard hit with a resource: 75, and the pause record (design 6.1)
			OnUnavailable(j, why)
		}
	}
	if err != nil && ((began && !repoSetup) || ambiguous) && OnFailure != nil { // the breaker counts 75s after BEGIN and ambiguous launches
		OnFailure(j)
	}
	switch {
	case err == nil:
		origin, outcome := "command", "ok"
		if code != 0 {
			outcome = "command-failed"
		}
		return run.Outcome{Code: code, Result: run.Result{Origin: origin, Outcome: outcome, ExitCode: code,
			Pool: p.Pool.Name, Provider: typ, Job: p.Job}}, true, nil
	case created && repoSetup: // the repo's own setup step failed by itself: 69, do not retry, no breaker record (spec 15.9)
		e := exit.Unavailablef("%s: %s: setup step %s failed: %s", typ, p.Pool.Name, setup.Step, j.Red.String(setup.Line))
		fmt.Fprintln(stderr, e.Line)
		return run.Outcome{Code: e.Code, Result: run.Result{Origin: "bitc", Outcome: e.Outcome, ExitCode: e.Code,
			Reason: e.Reason, Pool: p.Pool.Name, Provider: typ, Job: p.Job}}, true, err
	case began || created: // a resource exists (infra setup failure, restart, loss, start timeout): 75, never fallback
		e := exit.Starved(p.Pool.Name, typ+": "+err.Error())
		fmt.Fprintln(stderr, e.Line)
		return run.Outcome{Code: e.Code, Result: run.Result{Origin: "bitc", Outcome: "starved", ExitCode: e.Code,
			Reason: e.Reason, Pool: p.Pool.Name, Provider: typ, Job: p.Job}}, true, err
	default:
		if suffix != "" { // the first attempt's resource was discarded: say so beside the retry's own failure
			err = &discarded{inner: err, suffix: suffix}
		}
		return j.fallback(err)
	}
}

// pauseWorthy: a provider-side ErrUnavailable (401/402/403, no balance) pauses the provider for
// the breaker window (spec 8.3); a local cause (missing credentials) does not.
func pauseWorthy(err error) (string, bool) {
	var una *provider.ErrUnavailable
	if errors.As(err, &una) && !una.Local {
		return una.Why, true
	}
	return "", false
}

// repoSetupFailure finds a setup failure the agent classed as the repo's own (spec 8.4 step 5).
// Any other class, a missing one or an unparsable one is infrastructure (75).
func repoSetupFailure(err error) (*provider.ErrSetup, bool) {
	var se *provider.ErrSetup
	if err != nil && errors.As(err, &se) && se.Repo() {
		return se, true
	}
	return nil, false
}

// endOutcome is the ledger end's exit field (spec 8.3): "fallback" only when nothing was created,
// "69" for a repo-class setup failure after create (spec 15.9).
func endOutcome(code int, began, created, interrupted bool, err error) string {
	_, repoSetup := repoSetupFailure(err)
	switch {
	case interrupted:
		return "interrupted"
	case err != nil && !began && !created:
		return "fallback"
	case repoSetup:
		return "69"
	case err != nil:
		return "75"
	}
	return fmt.Sprint(code)
}

// fallback notes a failure before the fallback boundary and passes to the next provider, or
// stops with on_failure = "stop". Walk calls it with j.Created set only for a pre-attach discard
// (spec 8.2). "running locally" is noted only when local is the next provider and "trying <next>"
// only when the next one is remote. An ineligible job whose next provider is not local gets the
// not-eligible line instead, and the call ends with 69 at the last provider or with "stop" (D3).
func (j *Job) fallback(err error) (run.Outcome, bool, error) {
	if why, ok := pauseWorthy(err); ok && OnUnavailable != nil {
		OnUnavailable(j, why)
	}
	pool, why, list := j.P.Pool.Name, j.Red.String(err.Error()), j.P.Pool.Providers
	var ine *provider.ErrIneligible
	next, stop := nextProvider(list, j.Type), j.P.Eff.RemoteLimits.OnFailure == "stop"
	switch {
	case next == "local" && !stop:
		j.Note("running %s locally: %s", pool, why)
	case errors.As(err, &ine):
		e := exit.Unavailablef("%s: %s: not eligible for remote: %s", j.Type, pool, why)
		fmt.Fprintln(j.Stderr, e.Line)
		if lastProvider(list, j.Type) || stop {
			return run.Outcome{Code: e.Code, Result: run.Result{Origin: "bitc", Outcome: e.Outcome, ExitCode: e.Code,
				Reason: e.Reason, Pool: pool, Provider: j.Type, Job: j.P.Job}}, true, err
		}
		return run.Outcome{}, false, err
	case next != "" && !stop:
		j.Note("%s: trying %s: %s", pool, next, why)
	}
	if stop {
		e := exit.As(NoProviderLeft(j.P.Pool.Name, err))
		fmt.Fprintln(j.Stderr, e.Line)
		return run.Outcome{Code: e.Code, Result: run.Result{Origin: "bitc", Outcome: e.Outcome, ExitCode: e.Code,
			Reason: e.Reason, Pool: j.P.Pool.Name, Provider: j.Type, Job: j.P.Job}}, true, err
	}
	return run.Outcome{}, false, err
}

// nextProvider is the provider after typ in the walk's list, "" if typ is last.
func nextProvider(list []string, typ string) string {
	i := slices.Index(list, typ)
	if i < 0 || i+1 >= len(list) {
		return ""
	}
	return list[i+1]
}

// lastProvider reports whether typ is the walk's last provider.
func lastProvider(list []string, typ string) bool {
	return len(list) > 0 && list[len(list)-1] == typ
}

// jobSecrets returns a job's B, K and M. Under the [test] seam fixed_secrets they are the fixed
// values 0x0b.., 0x0c.. and 0x0d.. (32 bytes each), which the plugin conformance suite seeds as
// canaries (providers delta A.10.2, row FB15); otherwise they are random.
func jobSecrets(e *config.Effective) (b, k, m [32]byte) {
	if e != nil && e.Test != nil && e.Test.FixedSecrets {
		for i := range b {
			b[i], k[i], m[i] = 0x0b, 0x0c, 0x0d
		}
		return b, k, m
	}
	return rand32(), rand32(), rand32()
}

func rand32() (b [32]byte) {
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return b
}

// buildSpec fills the provider-independent part of the job (spec 8.1); the remote env and the
// code ref come from 2.1b and 2.5.
func buildSpec(p *run.Prepared, typ string, core *config.Provider, prov provider.Provider) (*provider.JobSpec, error) {
	maxName := prov.Info().MaxName
	rel, err := filepath.Rel(p.Eff.Worktree, p.Eff.Cwd)
	if err != nil || strings.HasPrefix(rel, "..") {
		return nil, &provider.ErrIneligible{Why: "the cwd is not inside the work tree"}
	}
	acct, err := state.AccountDir(p.Eff)
	if err != nil {
		return nil, &provider.ErrUnavailable{Why: err.Error(), Local: true}
	}
	secret, err := state.InstallSecret(acct)
	if err != nil {
		return nil, &provider.ErrUnavailable{Why: err.Error(), Local: true}
	}
	argv := append([]string{filepath.Base(p.Args.Argv[0])}, p.Args.Argv[1:]...)
	now := time.Now()
	timeout := p.Pool.RemoteTimeout
	bearer, macKey, nonce := jobSecrets(p.Eff)
	j := &provider.JobSpec{Job: p.Job, Pool: p.Pool.Name, Argv: argv, Cwd: rel, SizeKey: p.Pool.RemoteSize[typ],
		Timeout: timeout, Created: now, Deadline: now.Add(core.StartTimeout + timeout),
		Bearer: bearer, MacKey: macKey, Nonce: nonce, Install: state.Install16(secret),
		Unprivileged: p.Eff.Test != nil, MaxName: maxName}
	j.Expiry = j.Deadline.Add(15 * time.Minute)
	j.Name = provider.NameFor(prov, secret, core.NamePrefix, j.Install, p.Pool.Name, j.SizeKey, p.Job, j.Expiry, nameLimit(maxName))
	j.Owner = provider.OwnerTag(secret, j.Name)
	if core.Agent.Source == "build" && (p.Eff.Test == nil || p.Eff.Test.AgentURL == "") {
		if src, err := ResolveAgentSource(p.Eff, core); err == nil {
			j.AgentBuild = src.Build
		}
	}
	if p.Eff.Test != nil {
		j.TestRoot = filepath.Join(os.TempDir(), "bitc-fake-"+j.Install+"-"+p.Job)
	}
	if p.Pool.Remote != nil {
		j.Artifacts = p.Pool.Remote.Artifacts
	}
	if p.Eff.Remote != nil {
		j.Packages, j.Tools, j.Setup, j.Services = p.Eff.Remote.Packages, p.Eff.Remote.Tools, p.Eff.Remote.Setup, p.Eff.Remote.Services
	}
	if p.Eff.Grant != nil {
		j.RootSteps, j.DeploySecret, j.DeployKeyFile = p.Eff.Grant.RootSteps, p.Eff.Grant.DeploySecret, p.Eff.Grant.DeployKeyFile
	}
	return j, nil
}
