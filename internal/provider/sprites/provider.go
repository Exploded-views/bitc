package sprites

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/Exploded-views/bitc/internal/agent"
	"github.com/Exploded-views/bitc/internal/config"
	"github.com/Exploded-views/bitc/internal/provider"
	"github.com/Exploded-views/bitc/internal/state"
)

func init() {
	provider.Register("sprites", provider.Factory{
		Endpoints: func(raw map[string]any) ([]string, error) {
			s, _ := DecodeSettings(raw, "bitc")
			return []string{s.APIBase}, nil
		},
		New: func(core *config.Provider, eff *config.Effective) (provider.Provider, error) {
			return NewProvider(core, eff)
		},
		NewCleaner: func(core *config.Provider, eff *config.Effective) (provider.Cleaner, error) {
			c, err := NewCleaner(core, eff)
			if err != nil {
				return nil, err // never a typed nil in the interface
			}
			return c, nil
		},
		FallbackCap: func(map[string]any) provider.Boundary { return provider.UntilAttach },
		// the Sprite and every checkpoint of it are deleted with the job (decision 10, probe 9.25)
		SendEnv:  func(map[string]any) provider.EnvLevel { return provider.EnvForced },
		Validate: validate,
	})
}

// validate is the 78 check of the provider-specific keys at load (sprites-provider.md 7).
func validate(core *config.Provider) []string {
	s, errs := DecodeSettings(core.Raw, core.NamePrefix)
	if s.Org != "" && core.APIKey != "" {
		slug, has := TokenOrg(core.APIKey)
		if !has || slug != s.Org {
			errs = append(errs, (&OrgTokenError{Slug: slug, Want: s.Org, HasSlug: has}).Error())
		}
	}
	return errs
}

// AgentSpec builds the agent's spec from the job; the orchestration sets it (as RunPod's).
var AgentSpec func(j *provider.JobSpec) agent.Spec

// Provider is the Sprites provider (sprites-provider.md, phase 1: a fresh Sprite per job).
type Provider struct {
	core *config.Provider
	set  Settings
	eff  *config.Effective
	cli  *Client
	// noCreds is set when there is no token: every use is an unavailable-local error.
	noCreds bool

	rt provider.Runtime

	secretMu sync.Mutex
	secret   []byte

	mu   sync.Mutex
	jobs map[string]*jobState // by Sprite name
}

// NewProvider builds the provider from its host section.
func NewProvider(core *config.Provider, eff *config.Effective) (*Provider, error) {
	s, errs := DecodeSettings(core.Raw, core.NamePrefix)
	if len(errs) > 0 {
		return nil, errors.New(errs[0])
	}
	p := &Provider{core: core, set: s, eff: eff, jobs: map[string]*jobState{}}
	if core.APIKey == "" {
		p.noCreds = true
		return p, nil
	}
	scale := eff.Scale
	cli, err := New(Options{APIBase: s.APIBase, Org: s.Org, Token: core.APIKey,
		Timeout: max(scale(30*time.Second), 10*time.Second), HandshakeTimeout: max(scale(30*time.Second), 500*time.Millisecond)})
	if err != nil {
		return nil, err
	}
	p.cli = cli
	return p, nil
}

// Start keeps the host's redactor, so the deploy key joins it (Walk calls Start once).
func (p *Provider) Start(_ context.Context, rt provider.Runtime) error {
	p.rt = rt
	return nil
}

func (p *Provider) Info() provider.Info {
	return provider.Info{Type: "sprites", Caps: provider.Caps{LiveLogs: true, Artifacts: true, Spend: false, SelfDelete: false,
		FallbackUntil: provider.UntilAttach, SendEnv: provider.EnvForced, MultiJob: false},
		Endpoints: []string{p.set.APIBase}, Arch: []string{"amd64"}}
}

func (p *Provider) credentials() error {
	if p.noCreds || p.cli == nil {
		return &provider.ErrUnavailable{Why: "no credentials (set api_key or api_key_file)", Local: true}
	}
	return nil
}

// installSecret is the install secret (the owner tag's key), read once.
func (p *Provider) installSecret() ([]byte, error) {
	p.secretMu.Lock()
	defer p.secretMu.Unlock()
	if p.secret != nil {
		return p.secret, nil
	}
	acct, err := state.AccountDir(p.eff)
	if err != nil {
		return nil, err
	}
	b, err := state.InstallSecret(acct)
	if err != nil {
		return nil, err
	}
	p.secret = b
	return b, nil
}

// UseInstallSecret sets the secret directly (tests).
func (p *Provider) UseInstallSecret(b []byte) {
	p.secretMu.Lock()
	p.secret = b
	p.secretMu.Unlock()
}

// ResourceName is the Sprite name of a job (provider.Namer): the owner tag is part of it.
func (p *Provider) ResourceName(secret []byte, prefix, install, _, size, job string, expiry time.Time, _ int) string {
	sum := sha256.Sum256([]byte(job))
	return EncodeName(secret, prefix, install, size, hex.EncodeToString(sum[:])[:8], expiry)
}

var _ provider.Namer = (*Provider)(nil)

// hostOf is the host of an https URL.
func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// domainMatches reports whether host is allowed by an explicit domain list (exact, or *.d).
func domainMatches(list []string, host string) bool {
	for _, d := range list {
		if d == host || (strings.HasPrefix(d, "*.") && strings.HasSuffix(host, d[1:])) {
			return true
		}
	}
	return false
}

// buildDomains is the build policy: egress_allow, egress_build_allow and the agent URL's host.
func (p *Provider) buildDomains(j *provider.JobSpec) []string {
	d := append(append([]string{}, p.set.EgressAllow...), p.set.EgressBuildAllow...)
	if h := hostOf(j.AgentURL); h != "" && domainRE.MatchString(h) {
		d = append(d, h)
	}
	return d
}

// needsKey: only a clone over ssh (a private repo) needs the deploy key; an https or local clone
// is public and runs without one (design 1.2, 4).
func needsKey(j *provider.JobSpec) bool {
	return strings.HasPrefix(j.CloneURL, "ssh://") || (strings.Contains(j.CloneURL, "@") && strings.Contains(j.CloneURL, ":") && !strings.Contains(j.CloneURL, "://"))
}

// maxPayloadEstimate leaves room for the fields the transport sets after the preflight.
const payloadSlack = 4 << 10

func (p *Provider) Preflight(_ context.Context, j *provider.JobSpec) error {
	if _, ok := p.set.Sizes[j.SizeKey]; !ok {
		return &provider.ErrIneligible{Why: fmt.Sprintf("size %q is not in [providers.sprites] sizes", j.SizeKey)}
	}
	if err := p.credentials(); err != nil {
		return err
	}
	if j.AgentBuild != nil {
		return &provider.ErrIneligible{Why: `agent source "build" with a pinned commit is built in the pod; a Sprite fetches the agent by url`}
	}
	if needsKey(j) && j.DeployKeyFile == "" {
		return &provider.ErrIneligible{Why: "a private repo needs [[repos]] deploy_key_file for sprites (the key is sent on exec stdin)"}
	}
	allowed := p.buildDomains(j)
	if h := hostOf(j.AgentURL); h != "" && !domainRE.MatchString(h) && p.eff.Test == nil {
		return &provider.ErrIneligible{Why: fmt.Sprintf("the agent url host %q is not a domain name the egress policy can allow", h)}
	}
	for _, t := range j.Tools {
		if h := hostOf(t.URL); !domainMatches(allowed, h) {
			return &provider.ErrIneligible{Why: fmt.Sprintf("tool %s downloads from %q, which is not in egress_allow or egress_build_allow", t.Name, h)}
		}
	}
	if n := p.payloadSize(j); n > agent.MaxBootPayload-payloadSlack {
		return &provider.ErrIneligible{Why: fmt.Sprintf("the boot payload is about %d bytes (limit 8 MiB)", n)}
	}
	return nil
}

// payloadSize is the size of the job's boot payload before the transport fills in the ref.
func (p *Provider) payloadSize(j *provider.JobSpec) int {
	n := len(j.CloneURL) + len(j.Ref) + len(j.Sha) + len(j.Cwd) + 1024
	for _, a := range j.Argv {
		n += len(a) + 4
	}
	for _, e := range j.Env {
		n += len(e) + 4
	}
	return n * 4 / 3 // base64 of the spec inside the JSON payload
}

// Estimate is price x (E - now) (spec 8.3).
func (p *Provider) Estimate(_ context.Context, j *provider.JobSpec) (config.Micros, error) {
	return p.set.EstimateFor(j.SizeKey, time.Until(j.Expiry)), nil
}

// Spend: Sprites has no billing API (Caps.Spend is false), so the cap relies on the ledger.
func (p *Provider) Spend(context.Context) (config.Micros, error) { return 0, nil }

// Owner returns the owner tag of a Sprite this install made, "" for any other: the proof is in the
// name (sprites-provider.md 2.1), so no request is made. The value is what provider.OwnerMatches
// compares.
func (p *Provider) Owner(_ context.Context, id string) (string, error) {
	secret, err := p.installSecret()
	if err != nil {
		return "", err
	}
	if _, ok := ParseName(p.core.NamePrefix, id); !ok || !VerifyName(secret, id) {
		return "", nil
	}
	return provider.OwnerTag(secret, id), nil
}

// Teardown deletes the Sprite (404 is done).
func (p *Provider) Teardown(ctx context.Context, id string) error {
	if err := p.credentials(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	err := p.cli.DeleteSprite(ctx, id)
	p.forget(id)
	return err
}

// List lists the Sprites whose names start with a prefix. A transient failure is retried once.
func (p *Provider) List(ctx context.Context, prefixes []string) ([]provider.Resource, error) {
	if err := p.credentials(); err != nil {
		return nil, err
	}
	return listResources(ctx, p.cli, p.set, p.eff, prefixes)
}

// spriteLister is the one read a listing needs (the Client; the cleanup handle keeps only this
// and spriteDeleter).
type spriteLister interface {
	ListSprites(ctx context.Context, prefix string) ([]Sprite, error)
}

// listResources lists the Sprites under the prefixes as provider resources.
func listResources(ctx context.Context, cli spriteLister, set Settings, eff *config.Effective, prefixes []string) ([]provider.Resource, error) {
	seen := map[string]bool{}
	var out []provider.Resource
	for _, pre := range prefixes {
		sp, err := cli.ListSprites(ctx, pre)
		var tr *provider.ErrTransient
		if errors.As(err, &tr) { // a 5xx before any create: one retry, no lookup
			select {
			case <-ctx.Done():
			case <-time.After(eff.Scale(2 * time.Second)):
				sp, err = cli.ListSprites(ctx, pre)
			}
		}
		if err != nil {
			return nil, err
		}
		for _, s := range sp {
			if seen[s.Name] {
				continue
			}
			seen[s.Name] = true
			r := provider.Resource{ID: s.Name, Name: s.Name, Created: s.CreatedAt, State: stateOf(s.Status)}
			if pn, ok := ParseName(strings.TrimSuffix(pre, "-"), s.Name); ok {
				r.Install, r.SizeKey, r.Job, r.Expires = pn.Install, pn.Size, pn.Job8, pn.Expires
			}
			r.USDPerHour = set.Price(r.SizeKey) // an unparsable name is priced at the highest size
			out = append(out, r)
		}
	}
	return out, nil
}

func stateOf(status string) string {
	switch status {
	case "cold", "warm", "running":
		return "running"
	}
	return "starting"
}
