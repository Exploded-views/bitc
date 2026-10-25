package plugin

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Exploded-views/bitc/internal/config"
	"github.com/Exploded-views/bitc/internal/provider"
)

// jobNonce remembers M per resource id: Watch parses markers with it (A.5.2).
func (p *Provider) rememberNonce(id string, j *provider.JobSpec) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.nonces == nil {
		p.nonces = map[string]string{}
	}
	p.nonces[id] = hex.EncodeToString(j.Nonce[:])
}

func (p *Provider) nonceOf(h provider.Handle) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.nonces[h.ID]
}

// redactJob puts every per-job secret into the redactor before the launch frame is written
// (A.3.3): the deploy key, B, K and M raw and as hex, and each forwarded value whose name is
// forced or looks like a secret.
func (p *Provider) redactJob(j *provider.JobSpec) {
	if p.rt.Red == nil {
		return
	}
	p.redactValue(j.DeployKey)
	for _, b := range [][32]byte{j.Bearer, j.MacKey, j.Nonce} {
		p.rt.Red.Add(string(b[:]))
		p.rt.Red.Add(hex.EncodeToString(b[:]))
	}
	forced := map[string]bool{}
	for _, n := range p.eff.RemoteLimits.ForwardEnvForce {
		forced[n] = true
	}
	for _, kv := range j.Env {
		for i := 0; i < len(kv); i++ {
			if kv[i] == '=' {
				if forced[kv[:i]] || config.SecretLikeName(kv[:i]) {
					p.redactValue(kv[i+1:])
				}
				break
			}
		}
	}
}

// effectiveBoundary is the host's fallback_until, never later than the plugin's declaration.
func (p *Provider) effectiveBoundary() provider.Boundary {
	b, _ := provider.ParseBoundary(p.core.FallbackUntil)
	p.mu.Lock()
	c := p.hello.Caps.FallbackUntil
	p.mu.Unlock()
	return min(b, c)
}

// Launch sends the launch request and classifies the outcome (A.5.4, A.5.5, A.6). Once the
// frame is written, only a well-formed result or a definite classified error is a definite
// outcome; everything else is an ambiguous launch, read as attached, reaped by name and 75.
func (p *Provider) Launch(ctx context.Context, j *provider.JobSpec) (provider.Handle, error) {
	s, err := p.session(ctx)
	if err != nil {
		return provider.Handle{}, err // nothing was written: a pre-resource failure
	}
	params, err := p.jobParams(j)
	if err != nil {
		return provider.Handle{}, err
	}
	if j.DeployKey != "" {
		params["secrets"] = map[string]string{"deploy_key": j.DeployKey}
	}
	p.redactJob(j)
	raw, eo, err := s.do(ctx, "launch", params, callOpts{timeout: p.eff.Scale(p.core.StartTimeout), onEvent: launchEvent})
	switch {
	case errors.Is(err, errTooLarge):
		return provider.Handle{}, &provider.ErrIneligible{Why: "the launch request is larger than 1 MiB"}
	case err != nil:
		return provider.Handle{}, p.ambiguous(j, s, p.ambiguityWhy(s, err))
	case eo != nil:
		return provider.Handle{}, p.launchError(j, s, eo)
	}
	var r struct {
		ID         string `json:"id"`
		Name       string `json:"name"`
		ResultsURL string `json:"results_url"`
	}
	if json.Unmarshal(raw, &r) != nil || r.ID == "" || r.Name != j.Name {
		return provider.Handle{}, p.ambiguous(j, s, "plugin protocol error: launch result needs id and the assigned name")
	}
	p.rememberNonce(r.ID, j)
	return provider.Handle{ID: r.ID, Name: r.Name, ResultsURL: r.ResultsURL}, nil
}

// launchEvent accepts setup lines; a begun or ended phase is a protocol error (A.6).
func launchEvent(raw []byte) (bool, error) {
	var ev struct {
		Event string `json:"event"`
		Phase string `json:"phase"`
	}
	if err := json.Unmarshal(raw, &ev); err != nil {
		return false, protoErrf("event: %v", err)
	}
	if ev.Event == "phase" && ev.Phase != "booting" && ev.Phase != "lost" {
		return false, protoErrf("phase event %q: only booting and lost may come from a plugin", ev.Phase)
	}
	return false, nil
}

// ambiguityWhy is the reason text of an ambiguous launch.
func (p *Provider) ambiguityWhy(s *session, err error) string {
	var pe *protoErr
	var to *timeoutErr
	switch {
	case errors.As(err, &pe):
		return pe.Error() + p.tail(s)
	case errors.As(err, &to):
		return "launch timed out after " + to.after.String()
	case errors.Is(err, context.Canceled):
		return "launch cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		return "launch deadline passed"
	}
	return err.Error() + p.tail(s)
}

// ambiguous reaps by name on a fresh process and returns the attached, ambiguous error (A.5.4).
func (p *Provider) ambiguous(j *provider.JobSpec, s *session, why string) error {
	p.dropSession()
	_, confirmed := p.reapByName(j)
	reason := "launch ambiguous: " + why
	if !confirmed {
		reason += "; " + j.Name + " left for sweep"
	}
	return &provider.ErrAttached{ID: j.Name, Err: &provider.ErrTransient{Why: reason}, Ambiguous: true, Unconfirmed: !confirmed}
}

// reapByName lists with the job's full name as the only prefix, checks each match's owner against
// the tag core computed, and tears down the matches. confirmed means the lookup worked and nothing
// of this job is left; foreign owners are never touched.
func (p *Provider) reapByName(j *provider.JobSpec) (found []string, confirmed bool) {
	ctx, cancel := context.WithTimeout(context.Background(), p.eff.Scale(listBound+ownerBound+teardownBound))
	defer cancel()
	rs, err := p.listRaw(ctx, []string{j.Name})
	if err != nil {
		return nil, false
	}
	confirmed = true
	for _, r := range rs {
		if r.Name != j.Name {
			continue
		}
		owner, err := p.Owner(ctx, r.ID)
		if err != nil || owner != j.Owner {
			confirmed = false
			continue
		}
		found = append(found, r.ID)
		if p.Teardown(ctx, r.ID) != nil {
			confirmed = false
		}
	}
	return found, confirmed
}

// launchError classifies a final error of launch (A.6 "launch errors, in full").
func (p *Provider) launchError(j *provider.JobSpec, s *session, eo *errObj) error {
	plain := eo.plain(eo.Why)
	switch eo.Class {
	case "ineligible", "transient", "unavailable", "unavailable_local":
		if eo.ID != "" { // a resource exists: attached under both boundaries
			p.reapByName(j)
			return &provider.ErrAttached{ID: eo.ID, Err: plain}
		}
		return p.nameCheck(j, s, eo.Class, plain)
	case "setup":
		p.reapByName(j)
		id := firstNonEmpty(eo.ID, j.Name)
		return &provider.ErrAttached{ID: id, Err: mapError("launch", eo)}
	case "pre_attach":
		inner := eo.innerErr()
		if eo.ID != j.Name || inner == nil {
			return p.ambiguous(j, s, "plugin protocol error: pre_attach needs the assigned name as id and an inner transient, unavailable or ineligible error")
		}
		if p.effectiveBoundary() == provider.UntilAttach {
			return &provider.ErrPreAttach{ID: eo.ID, Err: inner}
		}
		return &provider.ErrAttached{ID: eo.ID, Err: inner}
	case "attached":
		if eo.ID == "" {
			return p.ambiguous(j, s, "plugin protocol error: attached needs an id")
		}
		inner := eo.innerErr()
		if inner == nil {
			inner = &provider.ErrTransient{Why: eo.Why}
		}
		p.reapByName(j)
		return &provider.ErrAttached{ID: eo.ID, Err: inner}
	}
	return p.ambiguous(j, s, fmt.Sprintf("plugin protocol error: launch answered error class %q", eo.Class))
}

// innerErr maps the inner error of pre_attach or attached; nil if it is not one of the three
// allowed classes.
func (e *errObj) innerErr() error {
	if e.Inner == nil {
		return nil
	}
	in := errObj{Class: e.Inner.Class}
	return in.plain(e.Inner.Why)
}

// nameCheck is A.5.5: after an id-less classified launch error, list by the job's full name.
func (p *Provider) nameCheck(j *provider.JobSpec, s *session, class string, plain error) error {
	ctx, cancel := context.WithTimeout(context.Background(), p.eff.Scale(listBound+ownerBound))
	defer cancel()
	rs, err := p.listRaw(ctx, []string{j.Name})
	if err != nil {
		return p.ambiguous(j, s, "launch said "+class+" and the name check failed: "+err.Error())
	}
	for _, r := range rs {
		if r.Name != j.Name {
			continue
		}
		owner, err := p.Owner(ctx, r.ID)
		if err != nil {
			return p.ambiguous(j, s, "launch said "+class+" and the owner check failed: "+err.Error())
		}
		if owner != j.Owner {
			continue
		}
		tctx, c2 := context.WithTimeout(context.Background(), p.eff.Scale(teardownBound))
		torn := p.Teardown(tctx, r.ID) == nil
		c2()
		why := fmt.Sprintf("launch said %s but %s exists", class, j.Name)
		if !torn {
			why += "; " + j.Name + " left for sweep"
		}
		return &provider.ErrAttached{ID: r.ID, Err: &provider.ErrTransient{Why: why}, Ambiguous: true, Unconfirmed: !torn}
	}
	return plain // nothing of this job exists: the 8.2 pre-resource path
}

var _ = time.Second
