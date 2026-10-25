package runpod

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Exploded-views/bitc/internal/agent"
	"github.com/Exploded-views/bitc/internal/config"
	"github.com/Exploded-views/bitc/internal/provider"
)

func init() {
	provider.Register("runpod", provider.Factory{
		Endpoints: func(raw map[string]any) ([]string, error) {
			s, err := DecodeSettings(raw)
			if err != nil {
				return nil, err
			}
			return []string{s.APIBase, s.ProxyDomain}, nil
		},
		New: func(core *config.Provider, eff *config.Effective) (provider.Provider, error) {
			return New(core, eff)
		},
		SelfDelete: selfDelete,
		// one pod per job, deleted at the end: today's behaviour (providers delta S.2)
		SendEnv: func(map[string]any) provider.EnvLevel { return provider.EnvForced },
	})
}

type Provider struct {
	core  *config.Provider
	set   Settings
	api   *client
	http  *http.Client
	test  bool                              // [test] http_results
	scale func(time.Duration) time.Duration // [test] time_scale for the fixed 15s/60s intervals
}

func New(core *config.Provider, eff *config.Effective) (*Provider, error) {
	s, err := DecodeSettings(core.Raw)
	if err != nil {
		return nil, err
	}
	hc := &http.Client{}
	return &Provider{core: core, set: s, api: &client{base: strings.TrimRight(s.APIBase, "/"), key: core.APIKey, http: hc, timeout: requestBound(eff.Scale)},
		http: hc, test: eff.Test != nil && eff.Test.HTTPResults, scale: eff.Scale}, nil
}

func (p *Provider) Info() provider.Info {
	return provider.Info{Type: "runpod", Caps: provider.Caps{LiveLogs: true, Artifacts: true, Spend: true, SelfDelete: true, FallbackUntil: provider.UntilResource,
		SendEnv: provider.EnvForced},
		Endpoints: []string{p.set.APIBase, p.set.ProxyDomain}, Arch: []string{"amd64"}}
}

func (p *Provider) price(size string) config.Micros {
	if z, ok := p.set.Sizes[size]; ok {
		return z.USDPerHour
	}
	return p.set.highest()
}

func (p *Provider) Preflight(_ context.Context, j *provider.JobSpec) error {
	if _, ok := p.set.Sizes[j.SizeKey]; !ok {
		return &provider.ErrIneligible{Why: fmt.Sprintf("size %q is not in [providers.runpod] sizes", j.SizeKey)}
	}
	return nil
}

// Estimate is price x (E - now) (spec 8.3).
func (p *Provider) Estimate(_ context.Context, j *provider.JobSpec) (config.Micros, error) {
	return config.Micros(float64(p.price(j.SizeKey)) * time.Until(j.Expiry).Hours()), nil
}

// createBody is the POST /v2/pods body (spec 8.7); under 100 KiB, checked before sending.
func (p *Provider) createBody(j *provider.JobSpec, as agent.Spec) (map[string]any, error) {
	enc, err := as.Encode()
	if err != nil {
		return nil, err
	}
	z := p.set.Sizes[j.SizeKey]
	env := map[string]string{
		"BITC_SPEC": enc, "BITC_AGENT_SHA256": j.AgentSHA256,
		"BITC_DEADLINE": strconv.FormatInt(j.Deadline.Unix(), 10), "BITC_OWNER": j.Owner, "BITC_PROVIDER_TYPE": "runpod",
		"BITC_API_BASE": strings.TrimRight(p.set.APIBase, "/"),
	}
	image, start := p.set.Image, agent.Stub(j.AgentURL, j.AgentSHA256)
	if b := j.AgentBuild; b != nil { // the pod builds the agent in the pinned image (spec 8.4 step 1)
		image, start = b.Image, agent.StubBuild(as.CloneURL, b.Ref, b.Commit, b.SHA256)
		env["BITC_KNOWN_HOSTS"] = as.KnownHosts
	} else {
		env["BITC_AGENT_URL"] = j.AgentURL
	}
	if j.DeploySecret != "" {
		env["BITC_DEPLOY_KEY"] = "{{ RUNPOD_SECRET_" + j.DeploySecret + " }}"
	}
	body := map[string]any{ // the REST v2 shape (spec 8.7); the v1 keys are refused with a 422
		"name": j.Name, "image": image,
		"cpu":   map[string]any{"id": z.CPUType, "vcpuCount": z.VCPU},
		"cloud": p.set.Cloud, "disk": int(p.set.Disk >> 30), "ports": []string{fmt.Sprintf("%d/http", p.set.Port)},
		"entrypoint": []string{"/bin/sh", "-c"}, "cmd": []string{start},
		"env": env,
	}
	if p.set.DataCenter != "" {
		body["dataCenterIds"] = []string{p.set.DataCenter}
	}
	b, _ := json.Marshal(body)
	if len(b) >= 100<<10 {
		return nil, &provider.ErrIneligible{Why: fmt.Sprintf("the create request is %d bytes (limit 100 KiB)", len(b))}
	}
	return body, nil
}

// AgentSpec is set by the orchestration before Launch.
var AgentSpec func(j *provider.JobSpec) agent.Spec

func (p *Provider) Launch(ctx context.Context, j *provider.JobSpec) (provider.Handle, error) {
	if AgentSpec == nil {
		return provider.Handle{}, errors.New("runpod: no agent spec builder")
	}
	as := AgentSpec(j)
	if !as.Unprivileged || as.TestRoot == "" { // a fake pod binds any free loopback port
		as.Port = p.set.Port
	}
	body, err := p.createBody(j, as)
	if err != nil {
		return provider.Handle{}, err
	}
	code, resp, err := p.api.do(ctx, "POST", "/v2/pods", body)
	if err != nil {
		return provider.Handle{}, &provider.ErrTransient{Why: "create: " + err.Error()}
	}
	if err := mapHTTP(code, resp, "create"); err != nil {
		return provider.Handle{}, err
	}
	var pod struct{ ID string }
	if json.Unmarshal(resp, &pod) != nil || pod.ID == "" {
		return provider.Handle{}, &provider.ErrTransient{Why: "create: no pod id in the response"}
	}
	jobsMu.Lock()
	jobs[pod.ID] = j
	jobsMu.Unlock()
	return provider.Handle{ID: pod.ID, Name: j.Name, ResultsURL: p.resultsURL(pod.ID)}, nil
}

// resultsURL: https://<pod>-<port>.<proxy_domain>/v1/; with [test] http_results the loopback
// form http://<proxy_domain>/<pod>-<port>/v1/ (proxy_domain is then 127.0.0.1:<port>).
func (p *Provider) resultsURL(id string) string {
	if p.test {
		return fmt.Sprintf("http://%s/%s-%d/v1/", p.set.ProxyDomain, id, p.set.Port)
	}
	return fmt.Sprintf("https://%s-%d.%s/v1/", id, p.set.Port, p.set.ProxyDomain)
}

type pod struct {
	ID        string            `json:"id"`
	Name      string            `json:"name"`
	Status    string            `json:"status"`
	Desired   string            `json:"desiredStatus"` // v1 name; read only when `status` is absent
	CreatedAt string            `json:"createdAt"`
	StartedAt string            `json:"startedAt"` // v2: the time of the pod's latest start; a restart changes it
	Cost      float64           `json:"cost"`      // v2: current USD per hour
	CostPerHr float64           `json:"costPerHr"` // v1 name; read only when `cost` is absent
	Env       map[string]string `json:"env"`
}

func (pd pod) state() string {
	if pd.Status != "" {
		return pd.Status
	}
	return pd.Desired
}

func (pd pod) apiCost() float64 {
	if pd.Cost != 0 {
		return pd.Cost
	}
	return pd.CostPerHr
}

func (p *Provider) List(ctx context.Context, prefixes []string) ([]provider.Resource, error) {
	code, b, err := p.api.do(ctx, "GET", "/v2/pods?limit=1000", nil)
	if err != nil || code != 200 {
		return nil, &provider.ErrTransient{Why: fmt.Sprintf("cannot list live resources (HTTP %d)", code)}
	}
	var pods []pod
	if err := json.Unmarshal(b, &pods); err != nil {
		var wrapped struct{ Pods []pod }
		if json.Unmarshal(b, &wrapped) != nil {
			return nil, &provider.ErrTransient{Why: "cannot parse the pod list"}
		}
		pods = wrapped.Pods
	}
	var out []provider.Resource
	for _, pd := range pods {
		for _, pre := range prefixes {
			if !strings.HasPrefix(pd.Name, pre) {
				continue
			}
			r, ok := ParseName(strings.TrimSuffix(pre, "-"), pd.Name)
			created, cerr := time.Parse(time.RFC3339, pd.CreatedAt)
			if !ok || cerr != nil { // unparsable: counted at the highest price and longest span (RL10)
				r = provider.Resource{Name: pd.Name, SizeKey: "", Expires: time.Time{}}
			}
			r.ID, r.Created, r.State = pd.ID, created, strings.ToLower(pd.state())
			price := p.price(r.SizeKey)
			if cost := config.Micros(pd.apiCost() * 1e6); cost > price {
				price = cost
			}
			r.USDPerHour = price
			out = append(out, r)
			break // one pod, one row, even when two prefixes match
		}
	}
	return out, nil
}

func (p *Provider) Spend(ctx context.Context) (config.Micros, error) {
	code, b, err := p.api.do(ctx, "GET", "/v2/billing/pods?bucketSize=day&lastN=1", nil)
	if err != nil || code != 200 {
		return 0, &provider.ErrTransient{Why: fmt.Sprintf("cannot read today's spend (HTTP %d)", code)}
	}
	var bill struct {
		Records []struct {
			TotalAmount float64 `json:"totalAmount"`
		} `json:"records"`
	}
	if err := json.Unmarshal(b, &bill); err != nil {
		return 0, &provider.ErrTransient{Why: "cannot parse today's spend"}
	}
	var sum float64
	for _, r := range bill.Records {
		sum += r.TotalAmount
	}
	return config.Micros(sum * 1e6), nil
}

func (p *Provider) Owner(ctx context.Context, id string) (string, error) {
	code, b, err := p.api.do(ctx, "GET", "/v2/pods/"+id, nil)
	if err != nil || code != 200 {
		return "", fmt.Errorf("get pod %s: HTTP %d", id, code)
	}
	var pd pod
	if err := json.Unmarshal(b, &pd); err != nil {
		return "", err
	}
	return pd.Env["BITC_OWNER"], nil
}

func (p *Provider) Teardown(ctx context.Context, id string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	code, _, err := p.api.do(ctx, "DELETE", "/v2/pods/"+id, nil)
	if err != nil {
		return err
	}
	if code == 404 || (code >= 200 && code < 300) {
		return nil
	}
	return fmt.Errorf("teardown %s: HTTP %d", id, code)
}

// status is the pod's status, "gone" on 404.
// statusStart is status plus the pod's startedAt ("" when the API omits it).
func (p *Provider) statusStart(ctx context.Context, id string) (string, string, error) {
	code, b, err := p.api.do(ctx, "GET", "/v2/pods/"+id, nil)
	if err != nil {
		return "", "", err
	}
	if code == 404 {
		return "gone", "", nil
	}
	var pd pod
	_ = json.Unmarshal(b, &pd)
	return pd.state(), pd.StartedAt, nil
}

// results GETs a results-server file from off, never with the API key.
func (p *Provider) results(ctx context.Context, h provider.Handle, name string, off int64, bearer string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", h.ResultsURL+name, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	if off > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", off))
	}
	return doBounded(ctx, p.http, req, requestBound(p.scale)) // a stalled read must not block Watch
}

// jobs maps a pod id to its job spec (bearer, nonce, deadline) for Watch, Output and Fetch.
var (
	jobsMu sync.Mutex
	jobs   = map[string]*provider.JobSpec{}
)

func jobOf(id string) *provider.JobSpec {
	jobsMu.Lock()
	defer jobsMu.Unlock()
	return jobs[id]
}
