package ssh

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Exploded-views/bitc/internal/agent"
	"github.com/Exploded-views/bitc/internal/config"
	"github.com/Exploded-views/bitc/internal/provider"
	"github.com/Exploded-views/bitc/internal/state"
)

func init() {
	provider.Register("ssh", provider.Factory{
		Fixed: true,
		Endpoints: func(raw map[string]any) ([]string, error) {
			h, _ := raw["host"].(string)
			return []string{h}, nil
		},
		New: func(core *config.Provider, eff *config.Effective) (provider.Provider, error) {
			return NewProvider(core, eff)
		},
		// resources are slots on a host the owner reaches over a pinned session: attach is the
		// latest point a failed job can still move on, and the slot is wiped per job
		FallbackCap:    func(map[string]any) provider.Boundary { return provider.UntilAttach },
		SendEnv:        func(map[string]any) provider.EnvLevel { return provider.EnvForwarded },
		MaxLiveDefault: func(raw map[string]any) int { n, _ := raw["slots"].(int64); return max(int(n), 1) },
		Validate: func(core *config.Provider) []string {
			_, errs := DecodeSettings(core)
			return errs
		},
	})
}

// The orchestration sets these (as RunPod's and Sprites' AgentSpec).
var (
	// AgentSpec builds the agent's spec from the job.
	AgentSpec func(j *provider.JobSpec) agent.Spec
	// ResolveAgentFile returns the sha256 and local path of the agent binary for an architecture:
	// its bytes are what the provider sends over the session (providers delta C.4 item 4).
	ResolveAgentFile func(e *config.Effective, core *config.Provider, arch string) (sha, path string, err error)
)

// Provider is the ssh provider: slots on one host the owner already has (providers delta C).
type Provider struct {
	core *config.Provider
	set  Settings
	eff  *config.Effective
	rt   provider.Runtime

	agentMu   sync.Mutex
	agentSHA  string
	agentPath string
	agentErr  error
	agentDone bool

	mu     sync.Mutex
	jobs   map[string]*jobState // by resource id
	leases map[string]*Lease    // controller-side slot locks, by resource name
	owners map[string]ownerRow  // the last List, for Owner
}

type ownerRow struct {
	tag string
	at  time.Time
}

// NewProvider builds the provider from its host section.
func NewProvider(core *config.Provider, eff *config.Effective) (*Provider, error) {
	s, errs := DecodeSettings(core)
	if len(errs) > 0 {
		return nil, errors.New(errs[0])
	}
	return &Provider{core: core, set: s, eff: eff, jobs: map[string]*jobState{}, leases: map[string]*Lease{}, owners: map[string]ownerRow{}}, nil
}

// Start keeps the host's redactor, so the deploy key joins it (Walk calls Start once).
func (p *Provider) Start(_ context.Context, rt provider.Runtime) error {
	p.rt = rt
	return nil
}

func (p *Provider) Info() provider.Info {
	return provider.Info{Type: "ssh", Caps: provider.Caps{LiveLogs: true, Artifacts: true, Spend: false, SelfDelete: true, Fixed: true,
		FallbackUntil: provider.UntilAttach, SendEnv: provider.EnvForwarded, MultiJob: false},
		Endpoints: []string{p.set.Host}, Arch: []string{p.set.Arch}}
}

// credentials: the identity file must still be a usable key (checked again here: the file may
// have changed since the load).
func (p *Provider) credentials() error {
	if why := checkIdentity(p.set.IdentityFile); why != "" {
		return &provider.ErrUnavailable{Why: "identity_file: " + why, Local: true}
	}
	return nil
}

func (p *Provider) accountDir() (string, error) {
	acct, err := state.AccountDir(p.eff)
	if err != nil {
		return "", &provider.ErrUnavailable{Why: err.Error(), Local: true}
	}
	return acct, nil
}

func (p *Provider) locks() (*SlotLocks, error) {
	acct, err := p.accountDir()
	if err != nil {
		return nil, err
	}
	return &SlotLocks{Dir: acct + "/remote/ssh", N: p.set.Slots}, nil
}

// agentFile resolves the agent binary once: its sha256 names the install on the host.
func (p *Provider) agentFile() (sha, path string, err error) {
	p.agentMu.Lock()
	defer p.agentMu.Unlock()
	if !p.agentDone {
		p.agentDone = true
		if ResolveAgentFile == nil {
			p.agentErr = errors.New("ssh: no agent resolver")
		} else {
			p.agentSHA, p.agentPath, p.agentErr = ResolveAgentFile(p.eff, p.core, p.set.Arch)
		}
		if p.agentErr != nil {
			p.agentDone = false // not cached: a transient failure (a download) may pass next time
		}
	}
	return p.agentSHA, p.agentPath, p.agentErr
}

// needsKey: only a clone over ssh (a private repo) needs the deploy key.
func needsKey(j *provider.JobSpec) bool {
	return strings.HasPrefix(j.CloneURL, "ssh://") || (strings.Contains(j.CloneURL, "@") && strings.Contains(j.CloneURL, ":") && !strings.Contains(j.CloneURL, "://"))
}

// Preflight is the network-free eligibility check (providers delta C.5): a size of this host,
// no services and no root steps (decision 15), a deploy key for a private repo, a payload that fits.
func (p *Provider) Preflight(_ context.Context, j *provider.JobSpec) error {
	if _, ok := p.set.Sizes[j.SizeKey]; !ok {
		return &provider.ErrIneligible{Why: fmt.Sprintf("size %q is not in [providers.ssh] sizes", j.SizeKey)}
	}
	if len(j.Services) > 0 {
		return &provider.ErrIneligible{Why: "services are not run on ssh hosts"}
	}
	if j.RootSteps {
		return &provider.ErrIneligible{Why: "root steps are not run on ssh hosts"}
	}
	if err := p.credentials(); err != nil {
		return err
	}
	if j.AgentBuild != nil {
		return &provider.ErrIneligible{Why: `agent source "build" with a pinned commit is built in the pod; a fixed host takes agent.module_dir`}
	}
	if !p.upload() && needsKey(j) && j.DeployKeyFile == "" {
		return &provider.ErrIneligible{Why: "a private repo needs [[repos]] deploy_key_file for ssh (the key is sent on the session's stdin)"}
	}
	if n := payloadSize(j); n > agent.MaxBootPayload-payloadSlack {
		return &provider.ErrIneligible{Why: fmt.Sprintf("the boot payload is about %d bytes (limit 8 MiB)", n)}
	}
	return nil
}

const payloadSlack = 4 << 10

// payloadSize is the size of the job's boot payload before the transport fills in the ref.
func payloadSize(j *provider.JobSpec) int {
	n := len(j.CloneURL) + len(j.Ref) + len(j.Sha) + len(j.Cwd) + 1024
	for _, a := range j.Argv {
		n += len(a) + 4
	}
	for _, e := range j.Env {
		n += len(e) + 4
	}
	return n * 4 / 3 // base64 of the spec inside the JSON payload
}

// Estimate is usd_per_hour x (E - now), which is $0 by default (providers delta C.3.3).
func (p *Provider) Estimate(_ context.Context, j *provider.JobSpec) (config.Micros, error) {
	return config.Micros(float64(p.set.USDPerHour) * time.Until(j.Expiry).Hours()), nil
}

// Spend: a fixed host has no billing API (Caps.Spend is false).
func (p *Provider) Spend(context.Context) (config.Micros, error) { return 0, nil }

// Parsed are the fields of a resource name (<prefix>-<install16>-<pool>-<size>-<job8>-<E>).
type Parsed struct {
	Install, Pool, Size, Job8 string
	Expires                   time.Time
}

// ParseName reads a name with this prefix; the pool is whatever lies between install16 and size.
func ParseName(prefix, name string) (Parsed, bool) {
	f := strings.Split(name, "-")
	if len(f) < 6 || f[0] != prefix || len(f[1]) != 16 {
		return Parsed{}, false
	}
	e, err := strconv.ParseInt(f[len(f)-1], 10, 64)
	if err != nil {
		return Parsed{}, false
	}
	return Parsed{Install: f[1], Pool: strings.Join(f[2:len(f)-3], "-"), Size: f[len(f)-3], Job8: f[len(f)-2], Expires: time.Unix(e, 0)}, true
}

// hostRow is one line of the host's `list` (the agent's slot-list).
type hostRow struct {
	Slot       int    `json:"slot"`
	Name       string `json:"name"`
	Tag        string `json:"tag"`
	Job        string `json:"job"`
	Created    int64  `json:"created"`
	LeaseUntil int64  `json:"lease_until"`
}

// listHost asks the host which slots are owned. A host without the agent owns nothing.
func (p *Provider) listHost(ctx context.Context) ([]hostRow, error) {
	sha, _, err := p.agentFile()
	if err != nil {
		return nil, err
	}
	res, out, err := p.output(ctx, "agent "+sha+" list", nil)
	if err != nil {
		return nil, err
	}
	switch {
	case res.Code == 66: // the agent is not installed: no slot was ever leased
		return nil, nil
	case res.Code != 0:
		return nil, p.failure("list", res, out)
	}
	var rows []hostRow
	for _, l := range strings.Split(out, "\n") {
		if strings.TrimSpace(l) == "" {
			continue
		}
		var r hostRow
		if err := json.Unmarshal([]byte(l), &r); err != nil {
			return nil, &provider.ErrTransient{Why: fmt.Sprintf("ssh %s: list failed: unreadable answer", p.set.Host)}
		}
		rows = append(rows, r)
	}
	return rows, nil
}

// failure is the error for a nonzero gate call before any resource of the job exists: a session
// that never came up is a host that cannot be reached right now (ErrUnavailable, local to this
// call: no pause, no breaker record, the walk moves on); a wrong host key or a refused identity
// is a pause; anything else is transient with the last line it printed.
func (p *Provider) failure(step string, res Result, stdout string) error {
	if c, why := ClassifyLoose(res.Code, res.Stderr); c != ClassOther {
		return p.sessionError(c, why)
	}
	return &provider.ErrTransient{Why: fmt.Sprintf("ssh %s: %s failed: %s", p.set.Host, step, firstNonEmpty(lastLine(res.Stderr, stdout), fmt.Sprintf("exit %d", res.Code)))}
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

// sessionError maps a pre-session class to its error: no connection is a local, unpaused
// unavailability; a mismatched host key or a refused identity pauses the provider.
func (p *Provider) sessionError(c Class, why string) error {
	if why == "connect timed out" {
		why += " after " + p.set.ConnectTimeout.Round(time.Second).String()
	}
	return &provider.ErrUnavailable{Why: fmt.Sprintf("ssh %s: %s", p.set.Host, why), Local: c == ClassConnect}
}

// List lists the slot groups that carry a name with one of the prefixes: one Resource per group.
// Its ID is <host>/s<i>[+s<j>]/<name>; a failure is a refusal of the launch (providers delta C.3.3).
func (p *Provider) List(ctx context.Context, prefixes []string) ([]provider.Resource, error) {
	if err := p.credentials(); err != nil {
		return nil, err
	}
	rows, err := p.listHost(ctx)
	if err != nil {
		return nil, err
	}
	byName := map[string][]hostRow{}
	var order []string
	for _, r := range rows {
		if r.Name == "" || !hasPrefix(r.Name, prefixes) {
			continue
		}
		if _, ok := byName[r.Name]; !ok {
			order = append(order, r.Name)
		}
		byName[r.Name] = append(byName[r.Name], r)
	}
	var out []provider.Resource
	p.mu.Lock()
	p.owners = map[string]ownerRow{}
	for _, name := range order {
		g := byName[name]
		var slots []int
		for _, r := range g {
			slots = append(slots, r.Slot)
		}
		id := ResourceID(p.set.Host, slots, name)
		res := provider.Resource{ID: id, Name: name, Created: time.Unix(g[0].Created, 0), State: "running", USDPerHour: p.set.USDPerHour}
		if pn, ok := ParseName(strings.SplitN(name, "-", 2)[0], name); ok {
			res.Install, res.Pool, res.SizeKey, res.Job, res.Expires = pn.Install, pn.Pool, pn.Size, pn.Job8, pn.Expires
		}
		out = append(out, res)
		p.owners[id] = ownerRow{tag: g[0].Tag, at: time.Now()}
	}
	p.mu.Unlock()
	return out, nil
}

func hasPrefix(name string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// Owner returns the owner tag the slot's owner file carries, "" for a slot group that is free or
// whose tag is another install's: the proof is the tag itself (HMAC of the name).
func (p *Provider) Owner(ctx context.Context, id string) (string, error) {
	p.mu.Lock()
	row, ok := p.owners[id]
	p.mu.Unlock()
	if ok && time.Since(row.at) < time.Minute {
		return row.tag, nil
	}
	_, _, name, valid := ParseResourceID(id)
	if !valid {
		return "", nil
	}
	rows, err := p.listHost(ctx)
	if err != nil {
		return "", err
	}
	for _, r := range rows {
		if r.Name == name {
			return r.Tag, nil
		}
	}
	return "", nil
}

// Teardown frees the slots of a group by name (providers delta C.3.6): the host compares the
// owner file's name with name under the slots' mutex, so a stale cleanup never touches a later job
// that leased the same slot. The controller's slot locks are released whatever the answer.
func (p *Provider) Teardown(ctx context.Context, id string) error {
	_, slots, name, ok := ParseResourceID(id)
	if !ok {
		return fmt.Errorf("ssh: %q is not a resource id", id)
	}
	defer p.release(id, name)
	sha, _, err := p.agentFile()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	res, out, err := p.output(ctx, "agent "+sha+" teardown "+SlotsArg(slots)+" "+name, nil)
	if err != nil {
		return err
	}
	switch got := strings.TrimSpace(out); {
	case res.Code == 66: // no agent on the host: nothing was ever leased
		return nil
	case res.Code == 0 && (got == "torn" || got == "gone"):
		return nil
	default:
		return fmt.Errorf("ssh %s: teardown of %s failed: %s", p.set.Host, id, firstNonEmpty(lastLine(res.Stderr, out), fmt.Sprintf("exit %d", res.Code)))
	}
}

// release drops the job state and the slot locks of a resource.
func (p *Provider) release(id, name string) {
	p.mu.Lock()
	delete(p.jobs, id)
	l := p.leases[name]
	delete(p.leases, name)
	p.mu.Unlock()
	l.Release()
}

// SlotStatus lists the slot groups held now, by any controller (the lease is ahead), for `status`.
func (p *Provider) SlotStatus(ctx context.Context) (provider.SlotStatus, error) {
	st := provider.SlotStatus{Total: p.set.Slots}
	if err := p.credentials(); err != nil {
		return st, err
	}
	rows, err := p.listHost(ctx)
	if err != nil {
		return st, err
	}
	now := time.Now().Unix()
	idx := map[string]int{}
	for _, r := range rows {
		if r.Name == "" || r.LeaseUntil <= now {
			continue
		}
		i, ok := idx[r.Name]
		if !ok {
			i = len(st.Held)
			idx[r.Name] = i
			h := provider.SlotHold{Name: r.Name, Until: time.Unix(r.LeaseUntil, 0)}
			if pn, ok := ParseName(strings.SplitN(r.Name, "-", 2)[0], r.Name); ok {
				h.Install = pn.Install
			}
			st.Held = append(st.Held, h)
		}
		st.Held[i].Slots = append(st.Held[i].Slots, r.Slot)
	}
	return st, nil
}
