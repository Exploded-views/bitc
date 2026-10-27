package plugin

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/Exploded-views/bitc/internal/agent"
	"github.com/Exploded-views/bitc/internal/config"
	"github.com/Exploded-views/bitc/internal/provider"
	"github.com/Exploded-views/bitc/internal/provider/plugin/pin"
	"github.com/Exploded-views/bitc/internal/version"
)

func init() {
	provider.RegisterPlugin(provider.Factory{
		Endpoints: func(map[string]any) ([]string, error) { return nil, nil },
		New: func(core *config.Provider, eff *config.Effective) (provider.Provider, error) {
			return New(core, eff)
		},
		DefaultResource: true,
	})
}

// Method timeouts (A.5); launch has start_timeout and watch has an idle bound.
const (
	helloTimeout   = 5 * time.Second
	preflightBound = 5 * time.Second
	estimateBound  = 10 * time.Second
	ownerBound     = 30 * time.Second
	teardownBound  = 10 * time.Second
	listBound      = 30 * time.Second
	spendBound     = 30 * time.Second
	watchIdle      = 90 * time.Second
	streamIdle     = 60 * time.Second
)

// Provider adapts one plugin type to provider.Provider. Every rail stays in core (A.7): the
// adapter only speaks the protocol, validates what the plugin says, and prices from the host's
// config.
type Provider struct {
	// bounds: test-only per-bound overrides (see bound); nil in production
	bounds map[string]time.Duration
	core   *config.Provider
	eff    *config.Effective
	typ    string
	rt     provider.Runtime

	mu      sync.Mutex
	started bool
	closed  bool
	sess    *session
	deaths  int
	tmpMu   sync.Mutex // guards tmpGen: the spawn that owns the shared TMPDIR path
	tmpGen  int
	hello   helloInfo
	cfg     configInfo
	nonces  map[string]string // resource id -> M (hex), set at Launch
}

// New builds the adapter; it spawns nothing until the first call (A.3.1).
func New(core *config.Provider, eff *config.Effective) (*Provider, error) {
	if core == nil || core.Plugin == nil {
		return nil, errors.New("not a plugin section")
	}
	return &Provider{core: core, eff: eff, typ: core.Type}, nil
}

func (p *Provider) stderrLine(l string) {
	if !p.rt.Debug || p.rt.Stderr == nil {
		return
	}
	red := l
	if p.rt.Red != nil {
		red = p.rt.Red.String(l)
	}
	fmt.Fprintf(p.rt.Stderr, "bitc: %s: plugin: %s\n", p.typ, red)
}

func (p *Provider) config(why string) *provider.ErrConfig {
	return &provider.ErrConfig{Why: "providers." + p.typ + ": " + why}
}

// Start spawns the plugin and runs hello and configure (A.4). It is idempotent; a config-class
// failure is *provider.ErrConfig (78).
func (p *Provider) Start(_ context.Context, rt provider.Runtime) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.started {
		return nil
	}
	p.rt = rt
	s, err := p.spawnAndHandshake()
	if err != nil {
		return err
	}
	p.sess, p.started = s, true
	return nil
}

// spawnAndHandshake starts a process and runs hello and configure; p.mu is held.
func (p *Provider) spawnAndHandshake() (*session, error) {
	s, err := p.spawn()
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*session, error) {
		s.fail(err, true)
		s.closeGracefully()
		return nil, p.publicErr(s, err)
	}
	ctx := context.Background()
	raw, eo, err := s.do(ctx, "hello", map[string]any{"protocols": []int{1}, "bitc_version": version.Version, "type": p.typ},
		callOpts{timeout: p.bound("hello", helloTimeout)})
	if err != nil || eo != nil {
		return fail(p.callErr(s, "hello", eo, err))
	}
	h, err := parseHello(raw, p.typ)
	var pe *protoErr
	if errors.As(err, &pe) && pe.Unsupported {
		return fail(&provider.ErrConfig{Why: "providers." + p.typ + ".plugin: " + pe.Why})
	} else if err != nil {
		return fail(err)
	}
	for _, name := range h.Secrets {
		if _, ok := p.core.Plugin.SecretFiles[name]; !ok {
			return fail(&provider.ErrConfig{Why: fmt.Sprintf("providers.%s.plugin.secret_files: %q required by the plugin", p.typ, name)})
		}
		if _, inline := p.core.Raw[name]; inline {
			return fail(&provider.ErrConfig{Why: fmt.Sprintf("providers.%s.%s: secret; set it in plugin.secret_files", p.typ, name)})
		}
	}
	if p.core.FallbackUntil == "attach" && h.Caps.FallbackUntil == provider.UntilResource {
		return fail(&provider.ErrConfig{Why: fmt.Sprintf("providers.%s.fallback_until: \"attach\" is wider than %s allows (\"resource\")", p.typ, p.typ)})
	}
	if p.core.SendEnv == "forced" && h.Caps.MultiJob { // S.4: forced names never reach a multi-job resource
		return fail(&provider.ErrConfig{Why: fmt.Sprintf("providers.%s.send_env: \"forced\" is not allowed: %s reuses resources across jobs", p.typ, p.typ)})
	}
	raw, eo, err = s.do(ctx, "configure", map[string]any{"config": p.configMap(), "sizes": p.core.SizesRaw},
		callOpts{timeout: p.bound("hello", helloTimeout)})
	if err != nil || eo != nil {
		return fail(p.callErr(s, "configure", eo, err))
	}
	c, err := parseConfigure(raw, p.typ)
	if err != nil {
		return fail(err)
	}
	if p.eff.Test != nil {
		for _, ep := range c.Endpoints {
			if !config.LoopbackLiteral(ep) {
				return fail(p.config(fmt.Sprintf("[test]: endpoint %q is not a loopback literal", ep)))
			}
		}
	}
	p.hello, p.cfg = h, c
	return s, nil
}

func (p *Provider) configMap() map[string]any {
	m := map[string]any{}
	for k, v := range p.core.Raw {
		m[k] = v
	}
	return m
}

// session returns the live process, respawning a dead one once (A.3.5); p.mu is not held.
func (p *Provider) session(ctx context.Context) (*session, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, &provider.ErrUnavailable{Why: "plugin " + p.typ + " is closed", Local: true}
	}
	if !p.started {
		s, err := p.spawnAndHandshake()
		if err != nil {
			return nil, err
		}
		p.sess, p.started = s, true
		return s, nil
	}
	if p.sess != nil && !p.sess.isDead() {
		return p.sess, nil
	}
	if p.sess != nil {
		p.sess.mu.Lock()
		deliberate := p.sess.deliber
		p.sess.mu.Unlock()
		if !deliberate {
			p.deaths++
		}
		p.sess = nil
	}
	if p.deaths >= 2 {
		return nil, &provider.ErrUnavailable{Why: "plugin " + p.typ + " died twice; not restarted in this bitc process", Local: true}
	}
	s, err := p.spawnAndHandshake()
	if err != nil {
		return nil, err
	}
	p.sess = s
	return s, nil
}

// dropSession kills the current process on purpose (a stuck plugin, an ambiguous launch): the
// next call respawns it and it does not count as a death.
func (p *Provider) dropSession() {
	p.mu.Lock()
	s := p.sess
	p.mu.Unlock()
	if s != nil {
		s.fail(&diedErr{"was stopped by bitc"}, true)
	}
}

// Close stops the plugin: stdin closed, then SIGTERM, then SIGKILL (A.3.5).
func (p *Provider) Close() error {
	p.mu.Lock()
	s := p.sess
	p.closed = true
	p.mu.Unlock()
	if s != nil {
		s.closeGracefully()
	}
	return nil
}

func (p *Provider) Info() provider.Info {
	p.mu.Lock()
	defer p.mu.Unlock()
	return provider.Info{Type: p.typ, Caps: p.hello.Caps, Endpoints: p.cfg.Endpoints, Arch: p.cfg.Arch, MaxName: p.cfg.MaxName}
}

// tail renders the plugin's last stderr lines, redacted.
func (p *Provider) tail(s *session) string {
	if !p.rt.Debug {
		return ""
	}
	var b strings.Builder
	for _, l := range s.stderr.tail(tailLines) {
		if p.rt.Red != nil {
			l = p.rt.Red.String(l)
		}
		b.WriteString("\n  plugin stderr: " + l)
	}
	return b.String()
}

// publicErr turns an internal failure into the 8.1 type a caller sees (A.6): protocol errors and
// a dead plugin are ErrUnavailable{Local}; a timeout is transient.
func (p *Provider) publicErr(s *session, err error) error {
	var pe *protoErr
	var de *diedErr
	var to *timeoutErr
	switch {
	case errors.As(err, &pe):
		return &provider.ErrUnavailable{Why: pe.Error() + p.tail(s), Local: true}
	case errors.As(err, &de):
		return &provider.ErrUnavailable{Why: de.Error() + p.tail(s), Local: true}
	case errors.As(err, &to):
		return &provider.ErrTransient{Why: to.Error()}
	case errors.Is(err, errTooLarge):
		return &provider.ErrIneligible{Why: "the request is larger than 1 MiB"}
	}
	return err
}

// callErr maps a failed call (a classified error object, or a transport failure) for a method
// other than launch.
func (p *Provider) callErr(s *session, method string, eo *errObj, err error) error {
	if eo != nil {
		e := mapError(method, eo)
		var ec *provider.ErrConfig
		var pe *protoErr
		switch {
		case errors.As(e, &ec):
			return p.config(ec.Why)
		case errors.As(e, &pe):
			return &provider.ErrUnavailable{Why: pe.Error(), Local: true}
		}
		return e
	}
	var de *diedErr
	if errors.As(err, &de) {
		if method == "hello" || method == "configure" { // no session yet: nothing can be retried
			return &provider.ErrUnavailable{Why: de.Error() + p.tail(s), Local: true}
		}
		return &provider.ErrTransient{Why: fmt.Sprintf("%s: %s", method, de.Error()) + p.tail(s)}
	}
	return p.publicErr(s, err)
}

// bound is a method's scaled bound. bounds (nil in production) lets a test shorten exactly the
// bound it means to fire, by name (a method, "launch", "reap", "namecheck"), so every other bound
// keeps its real value and no verdict depends on how fast a loaded machine schedules a timer.
func (p *Provider) bound(name string, d time.Duration) time.Duration {
	if v, ok := p.bounds[name]; ok {
		return v
	}
	return p.eff.Scale(d)
}

// call runs one non-launch request.
func (p *Provider) call(ctx context.Context, method string, params any, bound time.Duration, o callOpts) (json.RawMessage, error) {
	raw, _, err := p.callX(ctx, method, params, bound, o)
	return raw, err
}

// callX is call that also reports whether the call ended by its bound.
func (p *Provider) callX(ctx context.Context, method string, params any, bound time.Duration, o callOpts) (raw json.RawMessage, timedOut bool, err error) {
	s, err := p.session(ctx)
	if err != nil {
		return nil, false, err
	}
	o.timeout = p.bound(method, bound)
	raw, eo, err := s.do(ctx, method, params, o)
	if err != nil || eo != nil {
		if err != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, errStopped)) {
			return nil, false, err
		}
		var to *timeoutErr
		return nil, errors.As(err, &to), p.callErr(s, method, eo, err)
	}
	return raw, false, nil
}

func (p *Provider) jobParams(j *provider.JobSpec) (map[string]any, error) {
	b, err := marshalJob(j)
	if err != nil {
		return nil, err
	}
	return map[string]any{"job": json.RawMessage(b)}, nil
}

func (p *Provider) Preflight(ctx context.Context, j *provider.JobSpec) error {
	if _, ok := p.core.Sizes[j.SizeKey]; !ok {
		return &provider.ErrIneligible{Why: fmt.Sprintf("size %q is not in [providers.%s] sizes", j.SizeKey, p.typ)}
	}
	params, err := p.jobParams(j)
	if err != nil {
		return err
	}
	_, err = p.call(ctx, "preflight", params, preflightBound, callOpts{})
	return err
}

// overCap is a plugin answer outside its range: an error, which counts as over the cap (8.3).
func (p *Provider) overCap(field string) error {
	return &provider.ErrTransient{Why: fmt.Sprintf("daily cap %s: plugin %s reported %s out of range", p.core.DailyUSD.Dollars(), p.typ, field)}
}

// hostPrice is the host's price of a size, the highest for an unknown or empty one (A.7.1).
func (p *Provider) hostPrice(size string) config.Micros {
	if v, ok := p.core.Sizes[size]; ok {
		return v
	}
	var hi config.Micros
	for _, v := range p.core.Sizes {
		hi = max(hi, v)
	}
	return hi
}

// Estimate is max(host price x (E - now), the plugin's figure) (A.7.1).
func (p *Provider) Estimate(ctx context.Context, j *provider.JobSpec) (config.Micros, error) {
	host := config.Micros(float64(p.hostPrice(j.SizeKey)) * time.Until(j.Expiry).Hours())
	if _, known := p.core.Sizes[j.SizeKey]; !known { // the rails' highest-price probe: the host answers
		return host, nil
	}
	params, err := p.jobParams(j)
	if err != nil {
		return 0, err
	}
	raw, err := p.call(ctx, "estimate", params, estimateBound, callOpts{})
	if err != nil {
		return 0, err
	}
	figure, ok := microsOf(raw, "usd_micros")
	if !ok {
		return 0, p.overCap("usd_micros")
	}
	return max(host, figure), nil
}

// microsOf reads {"<key>": <non-negative integer>}.
func microsOf(raw json.RawMessage, key string) (config.Micros, bool) {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return 0, false
	}
	v, ok := m[key]
	if !ok {
		return 0, false
	}
	var n int64
	if json.Unmarshal(v, &n) != nil || n < 0 || bytes.ContainsAny(v, ".eE") {
		return 0, false
	}
	return config.Micros(n), true
}

func (p *Provider) Spend(ctx context.Context) (config.Micros, error) {
	if !p.Info().Caps.Spend {
		return 0, nil
	}
	raw, err := p.call(ctx, "spend", map[string]any{}, spendBound, callOpts{})
	if err != nil {
		return 0, err
	}
	n, ok := microsOf(raw, "usd_micros")
	if !ok {
		return 0, p.overCap("usd_micros")
	}
	return n, nil
}

func (p *Provider) Owner(ctx context.Context, id string) (string, error) {
	raw, err := p.call(ctx, "owner", map[string]any{"id": id}, ownerBound, callOpts{})
	if err != nil {
		return "", err
	}
	var r struct {
		Owner *string `json:"owner"`
	}
	if json.Unmarshal(raw, &r) != nil || r.Owner == nil {
		return "", &provider.ErrUnavailable{Why: "plugin protocol error: owner: owner is required", Local: true}
	}
	return *r.Owner, nil
}

func (p *Provider) Teardown(ctx context.Context, id string) error {
	_, err := p.call(ctx, "teardown", map[string]any{"id": id}, teardownBound, callOpts{})
	return err
}

// wireResource is one entry of a list result (8.1 Resource fields, snake_case).
type wireResource struct {
	ID         string          `json:"id"`
	Name       string          `json:"name"`
	Install    string          `json:"install"`
	Pool       string          `json:"pool"`
	Job        string          `json:"job"`
	SizeKey    string          `json:"size_key"`
	USDPerHour json.RawMessage `json:"usd_per_hour"`
	Created    string          `json:"created"`
	Expires    string          `json:"expires"`
	State      string          `json:"state"`
}

// listRaw asks for the resources whose names start with one of prefixes, validated (A.7.1.5)
// and priced at max(host price of the size, the plugin's figure) (A.7.1.3).
func (p *Provider) listRaw(ctx context.Context, prefixes []string) ([]provider.Resource, error) {
	raw, err := p.call(ctx, "list", map[string]any{"prefixes": prefixes}, listBound, callOpts{})
	if err != nil {
		return nil, err
	}
	var r struct {
		Resources []wireResource `json:"resources"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, &provider.ErrUnavailable{Why: "plugin protocol error: list: " + err.Error(), Local: true}
	}
	out := make([]provider.Resource, 0, len(r.Resources))
	for _, w := range r.Resources {
		switch {
		case w.ID == "":
			return nil, p.overCap("id")
		case w.Name == "" || !hasAnyPrefix(w.Name, prefixes):
			return nil, p.overCap("name")
		}
		price := p.hostPrice(w.SizeKey)
		if len(w.USDPerHour) > 0 && string(w.USDPerHour) != "null" {
			var usd float64
			if json.Unmarshal(w.USDPerHour, &usd) != nil || usd < 0 || math.IsNaN(usd) || math.IsInf(usd, 0) {
				return nil, p.overCap("usd_per_hour")
			}
			price = max(price, config.Micros(math.Round(usd*1e6)))
		}
		res := provider.Resource{ID: w.ID, Name: w.Name, Install: w.Install, Pool: w.Pool, Job: w.Job, SizeKey: w.SizeKey, USDPerHour: price, State: w.State}
		if res.Install == "" {
			if parts := strings.SplitN(w.Name, "-", 3); len(parts) == 3 {
				res.Install = parts[1]
			}
		}
		res.Created, _ = time.Parse(time.RFC3339, w.Created)
		res.Expires, _ = time.Parse(time.RFC3339, w.Expires)
		out = append(out, res)
	}
	return out, nil
}

func hasAnyPrefix(name string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

func (p *Provider) List(ctx context.Context, prefixes []string) ([]provider.Resource, error) {
	return p.listRaw(ctx, prefixes)
}

func handleParams(h provider.Handle) map[string]any {
	return map[string]any{"id": h.ID, "name": h.Name, "results_url": h.ResultsURL}
}

var streamNames = map[provider.Stream]string{provider.Stdout: "stdout", provider.Stderr: "stderr", provider.ResultFile: "result", provider.StateFile: "state"}

// chunks decodes data events into w, at most maxChunk decoded bytes per frame.
func chunks(w io.Writer) func(raw []byte) (bool, error) {
	return func(raw []byte) (bool, error) {
		var ev struct {
			Event string `json:"event"`
			Data  string `json:"data"`
		}
		if err := json.Unmarshal(raw, &ev); err != nil {
			return false, protoErrf("event: %v", err)
		}
		if ev.Event != "data" {
			return false, nil
		}
		b, err := base64.StdEncoding.DecodeString(ev.Data)
		if err != nil || len(b) > maxChunk {
			return false, protoErrf("data event: bad base64 or more than 512 KiB")
		}
		if _, err := w.Write(b); err != nil {
			return false, err
		}
		return false, nil
	}
}

func (p *Provider) Output(ctx context.Context, h provider.Handle, st provider.Stream, off int64, w io.Writer) error {
	_, err := p.call(ctx, "output", map[string]any{"handle": handleParams(h), "stream": streamNames[st], "offset": off}, streamIdle,
		callOpts{idle: true, onEvent: chunks(w)})
	return err
}

func (p *Provider) Fetch(ctx context.Context, h provider.Handle, a provider.ArtifactSpec, w io.Writer) error {
	_, err := p.call(ctx, "fetch", map[string]any{"handle": handleParams(h), "max_compressed": a.MaxCompressed, "offset": a.Offset}, streamIdle,
		callOpts{idle: true, onEvent: chunks(w)})
	return err
}

// Watch follows the plugin's log events, turning the markers that carry the job's nonce into
// phases and setup lines exactly as a built-in does (A.5.2). A plugin's own phase event can only
// report booting or lost.
func (p *Provider) Watch(ctx context.Context, h provider.Handle, out provider.Sink) (provider.Result, error) {
	nonce := p.nonceOf(h)
	var began, exitSeen bool
	var exitCode, boots int
	var stopErr error
	onEvent := func(raw []byte) (bool, error) {
		var ev struct {
			Event  string `json:"event"`
			Phase  string `json:"phase"`
			Why    string `json:"why"`
			Line   string `json:"line"`
			Stream string `json:"stream"`
			Data   string `json:"data"`
		}
		if err := json.Unmarshal(raw, &ev); err != nil {
			return false, protoErrf("event: %v", err)
		}
		switch ev.Event {
		case "phase":
			switch ev.Phase {
			case "booting":
				out.Phase(provider.Booting, ev.Why)
			case "lost":
				out.Phase(provider.Lost, ev.Why)
				stopErr = fmt.Errorf("%s was lost: %s", h.ID, firstNonEmpty(ev.Why, "the plugin reported it lost"))
				return true, nil
			default:
				return false, protoErrf("phase event %q: only booting and lost may come from a plugin", ev.Phase)
			}
		case "log":
			m, ok := agent.ParseMarker(ev.Line, nonce)
			switch {
			case !ok:
				if !began {
					out.Setup(ev.Line)
				}
			case m.Kind == agent.MarkBoot:
				boots++
				if boots > 1 {
					stopErr = fmt.Errorf("%s restarted %s", h.ID, map[bool]string{true: "while the command ran", false: "before the command started"}[began])
					return true, nil
				}
			case m.Kind == agent.MarkSetupFailed:
				stopErr = &provider.ErrSetup{Step: m.Arg, Class: m.Class, Line: m.Line}
				return true, nil
			case m.Kind == agent.MarkLost:
				out.Phase(provider.Lost, "")
				stopErr = fmt.Errorf("%s was lost", h.ID)
				return true, nil
			case m.Kind == agent.MarkBegin:
				if !began {
					began = true
					out.Phase(provider.Begun, "")
				}
			case m.Kind == agent.MarkExit:
				exitSeen = true
				_, _ = fmt.Sscan(m.Arg, &exitCode)
			}
		case "out":
			b, err := base64.StdEncoding.DecodeString(ev.Data)
			if err != nil || len(b) > maxChunk {
				return false, protoErrf("out event: bad base64 or more than 512 KiB")
			}
			switch ev.Stream {
			case "stdout":
				out.Out(provider.Stdout, b)
			case "stderr":
				out.Out(provider.Stderr, b)
			default:
				return false, protoErrf("out event: stream %q", ev.Stream)
			}
		}
		return false, nil
	}
	raw, timedOut, err := p.callX(ctx, "watch", map[string]any{"handle": handleParams(h)}, watchIdle, callOpts{idle: true, onEvent: onEvent})
	switch {
	case errors.Is(err, errStopped):
		return provider.Result{Began: began}, stopErr
	case ctx.Err() != nil:
		return provider.Result{Began: began}, ctx.Err()
	case timedOut:
		out.Phase(provider.Lost, "no event from the plugin")
		return provider.Result{Began: began}, fmt.Errorf("%s was lost: no event from the plugin for %s", h.ID, p.bound("watch", watchIdle))
	case err != nil:
		return provider.Result{Began: began}, err
	}
	var r struct {
		Exit  *int   `json:"exit"`
		Began *bool  `json:"began"`
		Why   string `json:"why"`
	}
	if json.Unmarshal(raw, &r) != nil || r.Exit == nil || r.Began == nil || *r.Exit < 0 || *r.Exit > 255 {
		return provider.Result{Began: began}, &provider.ErrUnavailable{Why: "plugin protocol error: watch result needs exit (0..255) and began", Local: true}
	}
	if exitSeen && exitCode != *r.Exit {
		return provider.Result{Began: began}, fmt.Errorf("%s result failed verification", h.ID)
	}
	if *r.Began && !began {
		began = true
		out.Phase(provider.Begun, "")
	}
	return provider.Result{Exit: *r.Exit, Began: began, Why: r.Why}, nil
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

// DoctorLine is the detail of `ok plugin-<type>:` in doctor (A.8): the protocol, the plugin's own
// version and the first 12 characters of the pinned sha256.
func (p *Provider) DoctorLine() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return fmt.Sprintf("protocol 1, %s, sha256 %s", p.hello.Version, pin.Short(p.core.Plugin.SHA256))
}
