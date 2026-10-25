package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"
)

// pluginType is the provider type this plugin implements.
const pluginType = "fakex"

// config is what `configure` supplied: the section's non-core keys (api_base, max_name, arch,
// fault) and the host's prices per size (core key `sizes`, also passed here).
type config struct {
	APIBase string
	MaxName int
	Arch    []string
	Price   map[string]float64 // size key -> usd per hour
}

func (s *server) handle(ctx context.Context, c *call) (any, *rpcError) {
	switch c.method {
	case "hello":
		return s.hello(c)
	case "configure":
		return s.configure(c)
	}
	// Every other method needs configure first.
	s.mu.Lock()
	cfg, api := s.cfg, s.api
	s.mu.Unlock()
	if cfg == nil {
		return nil, transient("%s before configure", c.method)
	}
	switch c.method {
	case "preflight":
		return s.preflight(c, cfg)
	case "estimate":
		return s.estimate(c, cfg)
	case "launch":
		return s.launch(ctx, c, api)
	case "watch":
		return s.watch(ctx, c, api)
	case "output":
		return s.output(ctx, c, api)
	case "fetch":
		return s.fetch(ctx, c, api)
	case "owner":
		return s.owner(ctx, c, api)
	case "teardown":
		return s.teardown(ctx, c, api)
	case "list":
		return s.list(ctx, c, api)
	case "spend":
		return s.spend(ctx, api)
	}
	return nil, transient("unknown method %s", c.method)
}

// hello is the handshake (A.4.4): the protocol, the plugin's own type, the plugin's version and
// its seven caps. `secrets` names the plugin.secret_files entries this plugin needs; the reference
// plugin needs none, FAKEX_DECLARE lets a test ask for some.
func (s *server) hello(c *call) (any, *rpcError) {
	// The type is this plugin's own, not an echo of the one bitc asked for: bitc compares the two
	// (A.4.4) and a differing type is a protocol error, which is how a binary pinned under the
	// wrong provider type is caught.
	caps := map[string]any{
		"live_logs":      true,
		"artifacts":      true,
		"spend":          true,
		"self_delete":    false,
		"fallback_until": envOr("FAKEX_FALLBACK", "attach"), // job input first reaches an instance at the `job` call
		"fixed":          false,
		"multi_job":      os.Getenv("FAKEX_MULTI_JOB") != "",
	}
	h := map[string]any{"protocol": 1, "type": pluginType, "plugin_version": pluginVersion, "caps": caps}
	if d := os.Getenv("FAKEX_DECLARE"); d != "" {
		h["secrets"] = strings.Split(d, ",")
	}
	if s.fault("hello-no-caps") {
		delete(h, "caps")
	}
	return h, nil
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

// configure applies the configuration. It must not touch the network (A.4.5). A configuration the
// plugin cannot use is class `config`, which bitc reports as exit 78 with the text.
func (s *server) configure(c *call) (any, *rpcError) {
	var p struct {
		Config map[string]any `json:"config"`
		Sizes  map[string]struct {
			USDPerHour float64 `json:"usd_per_hour"`
		} `json:"sizes"`
	}
	if json.Unmarshal(c.params, &p) != nil {
		return nil, &rpcError{Class: "config", Why: "configure: bad request"}
	}
	base, _ := p.Config["api_base"].(string)
	u, err := url.Parse(base)
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, &rpcError{Class: "config", Why: fmt.Sprintf("api_base %q is not an http(s) URL", base)}
	}
	cfg := &config{APIBase: base, MaxName: 63, Arch: []string{"amd64"}, Price: map[string]float64{}}
	if n, ok := p.Config["max_name"].(float64); ok {
		cfg.MaxName = int(n)
	}
	if l, ok := p.Config["arch"].([]any); ok {
		cfg.Arch = nil
		for _, a := range l {
			cfg.Arch = append(cfg.Arch, fmt.Sprint(a))
		}
	}
	for k, v := range p.Sizes {
		cfg.Price[k] = v.USDPerHour
	}
	s.mu.Lock()
	if f, ok := p.Config["fault"].(string); ok {
		for k := range parseFaults(f) {
			s.faults[k] = true
		}
	}
	s.cfg = cfg
	s.api = newAPIClient(base, s.secrets["api_key"])
	s.mu.Unlock()
	s.diag()
	res := map[string]any{"endpoints": []string{u.Hostname()}, "arch": cfg.Arch, "max_name": cfg.MaxName}
	if s.fault("configure-no-endpoints") {
		delete(res, "endpoints")
	}
	return res, nil
}

// jobFields are the few fields of the wire job (A.5.1) this plugin reads itself.
type jobFields struct {
	Name    string   `json:"name"`
	Owner   string   `json:"owner"`
	SizeKey string   `json:"size_key"`
	Expiry  string   `json:"expiry"`
	Argv    []string `json:"argv"`
}

func jobOf(raw json.RawMessage) (jobFields, json.RawMessage) {
	var p struct {
		Job json.RawMessage `json:"job"`
	}
	_ = json.Unmarshal(raw, &p)
	var j jobFields
	_ = json.Unmarshal(p.Job, &j)
	return j, p.Job
}

// preflight is the network-free eligibility check: could this job ever run here?
func (s *server) preflight(c *call, cfg *config) (any, *rpcError) {
	j, _ := jobOf(c.params)
	if _, ok := cfg.Price[j.SizeKey]; !ok {
		return nil, &rpcError{Class: "ineligible", Why: fmt.Sprintf("size %q is not offered", j.SizeKey)}
	}
	if len(j.Argv) == 0 {
		return nil, &rpcError{Class: "ineligible", Why: "the job has no command"}
	}
	if s.fault("preflight-ineligible") {
		return nil, &rpcError{Class: "ineligible", Why: "request too large"}
	}
	return map[string]any{}, nil
}

// estimate is the plugin's own figure for the job in micro-dollars; bitc takes the larger of it
// and the host's price times the job's span, so a low figure cannot slip a job past the daily cap.
func (s *server) estimate(c *call, cfg *config) (any, *rpcError) {
	j, _ := jobOf(c.params)
	exp, err := time.Parse(time.RFC3339, j.Expiry)
	if err != nil {
		return nil, &rpcError{Class: "ineligible", Why: "the job has no expiry"}
	}
	micros := int64(cfg.Price[j.SizeKey] * 1e6 * time.Until(exp).Hours())
	switch {
	case s.fault("estimate-zero"):
		micros = 0
	case s.fault("estimate-negative"):
		micros = -5
	case s.fault("estimate-float"):
		return rawResult(`{"usd_micros":1.5}`), nil
	}
	return map[string]any{"usd_micros": micros}, nil
}

// launch creates the instance with the name and owner bitc assigned, waits until it is ready and
// then hands it the job. Job input (argv, env, B, K, M, the deploy key) goes to the API only in
// that last call; everything before it is the pre-attach part, where bitc may still fall back to
// another provider (the `fallback_until = "attach"` caps declaration, conformance row FB15).
func (s *server) launch(ctx context.Context, c *call, api *apiClient) (any, *rpcError) {
	var p struct {
		Secrets struct {
			DeployKey string `json:"deploy_key"`
		} `json:"secrets"`
	}
	_ = json.Unmarshal(c.params, &p)
	j, rawJob := jobOf(c.params)
	if s.fault("echo-launch") { // a fault: a plugin that copies its request to stderr
		fmt.Fprintln(os.Stderr, strings.TrimSpace(string(c.raw)))
	}
	if s.fault("launch-transient-nothing") {
		return nil, transient("no capacity")
	}
	// 1. create: no job input. An error here means nothing was created, so no id: bitc retries once
	// and may move on to the next provider. (Answering an error without an id when something WAS
	// created is a plugin bug; fault create-then-transient-no-id plays it.)
	in, e := api.create(ctx, j.Name, j.SizeKey, j.Owner, j.Expiry)
	if e != nil {
		return nil, e
	}
	s.created.Store(true)
	if s.fault("create-then-transient-no-id") {
		return nil, transient("capacity")
	}
	c.emit("setup", map[string]any{"line": "created " + in.ID})
	// 2. wait for the instance. If it fails now, no job input has reached it: pre_attach, with the
	// assigned name as id, lets bitc discard it and try again or fall back.
	last := ""
	for in.State != "ready" {
		if in.State == "failed" {
			return nil, &rpcError{Class: "pre_attach", Why: in.Why, ID: j.Name, Inner: &inner{Class: "transient", Why: in.Why}}
		}
		if in.State != last {
			last = in.State
			c.emit("setup", map[string]any{"line": "instance " + in.ID + " is " + in.State})
		}
		select {
		case <-ctx.Done():
			// The instance exists: answer with its name, never a bare transient (that is the
			// create-then-transient-no-id bug), so bitc can tear it down.
			return nil, &rpcError{Class: "pre_attach", Why: "launch cancelled", ID: j.Name, Inner: &inner{Class: "transient", Why: "launch cancelled"}}
		case <-time.After(100 * time.Millisecond):
		}
		var found bool
		if in, found, e = api.get(ctx, in.ID); e != nil {
			return nil, &rpcError{Class: "pre_attach", Why: e.Why, ID: j.Name, Inner: &inner{Class: "transient", Why: e.Why}}
		} else if !found {
			return nil, &rpcError{Class: "pre_attach", Why: "the instance vanished", ID: j.Name, Inner: &inner{Class: "transient", Why: "the instance vanished"}}
		}
	}
	// 3. attach: the job object travels exactly as bitc sent it. From here on a failure is `attached`.
	status, b, err := api.do(ctx, "POST", "/v1/instances/"+url.PathEscape(in.ID)+"/job", map[string]any{"job": rawJob, "deploy_key": p.Secrets.DeployKey})
	if e := classify("start the job", status, b, err); e != nil {
		return nil, &rpcError{Class: "attached", Why: e.Why, ID: in.ID, Inner: &inner{Class: "transient", Why: e.Why}}
	}
	if s.fault("launch-malformed") { // a fault: a broken frame, then silence
		s.writeRaw("{this is not json")
		select {}
	}
	return map[string]any{"id": in.ID, "name": in.Name}, nil
}

// handleID reads params.handle.id.
func handleID(raw json.RawMessage) string {
	var p struct {
		Handle struct {
			ID string `json:"id"`
		} `json:"handle"`
	}
	_ = json.Unmarshal(raw, &p)
	return p.Handle.ID
}

// chunkSize is the decoded bytes per data or out frame (at most 512 KiB, A.4.1).
const chunkSize = 256 << 10

func chunked(b []byte, emit func([]byte)) {
	for len(b) > 0 {
		n := min(len(b), chunkSize)
		emit(b[:n])
		b = b[n:]
	}
}

// watch relays the instance's log lines and live output until the job has ended. The plugin does
// not interpret the agent's markers: it forwards every raw line as a `log` event and bitc core
// parses them (A.5.2). A phase event can only say booting or lost; begun and ended come from
// the markers alone. The result is the instance's exit code and whether the job began.
func (s *server) watch(ctx context.Context, c *call, api *apiClient) (any, *rpcError) {
	id := handleID(c.params)
	next := 0
	off := map[string]int64{"out.log": 0, "err.log": 0}
	streams := map[string]string{"out.log": "stdout", "err.log": "stderr"}
	lastEvent := time.Now()
	pump := func() bool { // one pass over the log and both output files; reports whether anything arrived
		got := false
		lines, n, e := api.logs(ctx, id, next)
		if e == nil {
			for _, l := range lines {
				c.emit("log", map[string]any{"line": l})
				got = true
			}
			next = n
		}
		for _, name := range []string{"out.log", "err.log"} {
			data, ok, e := api.file(ctx, id, name, off[name])
			if e != nil || !ok || len(data) == 0 {
				continue
			}
			chunked(data, func(b []byte) {
				c.emit("out", map[string]any{"stream": streams[name], "data": base64.StdEncoding.EncodeToString(b)})
			})
			off[name] += int64(len(data))
			got = true
		}
		if got {
			lastEvent = time.Now()
		}
		return got
	}
	for {
		in, found, e := api.get(ctx, id)
		if ctx.Err() != nil {
			return nil, transient("watch cancelled")
		}
		if e == nil && !found {
			c.emit("phase", map[string]any{"phase": "lost", "why": "the instance is gone"})
			return map[string]any{"exit": 0, "began": false}, nil
		}
		if e == nil && in.State == "exited" {
			pump()
			pump() // the instance has stopped: one more pass reads what the first one left
			res := map[string]any{"exit": in.Exit, "began": in.Began}
			if s.fault("watch-no-exit") {
				delete(res, "exit")
			}
			return res, nil
		}
		pump()
		if time.Since(lastEvent) >= time.Second { // bitc treats 90 s without any event as lost
			c.emit("heartbeat", nil)
			lastEvent = time.Now()
		}
		select {
		case <-ctx.Done():
			return nil, transient("watch cancelled")
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// streamFile sends a result file from an offset as data events.
func (s *server) streamFile(ctx context.Context, c *call, api *apiClient, id, name string, offset, limit int64) (any, *rpcError) {
	data, ok, e := api.file(ctx, id, name, offset)
	if e != nil {
		return nil, e
	}
	if !ok {
		return nil, transient("%s is not available", name)
	}
	if limit > 0 && offset+int64(len(data)) > limit {
		return nil, transient("%s is larger than the allowed %d bytes", name, limit)
	}
	chunked(data, func(b []byte) { c.emit("data", map[string]any{"data": base64.StdEncoding.EncodeToString(b)}) })
	return map[string]any{"bytes": len(data)}, nil
}

var fileOf = map[string]string{"stdout": "out.log", "stderr": "err.log", "result": "result.json", "state": "state.json"}

// output serves one of the job's result streams from an offset (stdout, stderr, result, state).
func (s *server) output(ctx context.Context, c *call, api *apiClient) (any, *rpcError) {
	var p struct {
		Stream string `json:"stream"`
		Offset int64  `json:"offset"`
	}
	_ = json.Unmarshal(c.params, &p)
	name, ok := fileOf[p.Stream]
	if !ok {
		return nil, &rpcError{Class: "ineligible", Why: "unknown stream " + p.Stream}
	}
	return s.streamFile(ctx, c, api, handleID(c.params), name, p.Offset, 0)
}

// fetch serves the artifacts archive from an offset, refusing one over max_compressed.
func (s *server) fetch(ctx context.Context, c *call, api *apiClient) (any, *rpcError) {
	var p struct {
		Max    int64 `json:"max_compressed"`
		Offset int64 `json:"offset"`
	}
	_ = json.Unmarshal(c.params, &p)
	return s.streamFile(ctx, c, api, handleID(c.params), "artifacts.tar.gz", p.Offset, p.Max)
}

// owner returns the owner tag stored at create; "" for an instance that is not there.
func (s *server) owner(ctx context.Context, c *call, api *apiClient) (any, *rpcError) {
	var p struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(c.params, &p)
	in, found, e := api.get(ctx, p.ID)
	if e != nil {
		return nil, e
	}
	if !found {
		return map[string]any{"owner": ""}, nil
	}
	return map[string]any{"owner": in.Owner}, nil
}

// teardown deletes the instance by id or by name; one already gone is success.
func (s *server) teardown(ctx context.Context, c *call, api *apiClient) (any, *rpcError) {
	var p struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(c.params, &p)
	if s.fault("teardown-fails") {
		return nil, transient("teardown refused")
	}
	if e := api.delete(ctx, p.ID); e != nil {
		return nil, e
	}
	return map[string]any{}, nil
}

// list returns the instances whose names start with one of the prefixes. It follows every page of
// the API, and reports every instance it holds (conformance row PS1).
func (s *server) list(ctx context.Context, c *call, api *apiClient) (any, *rpcError) {
	var p struct {
		Prefixes []string `json:"prefixes"`
	}
	_ = json.Unmarshal(c.params, &p)
	if s.fault("list-fails") || (s.fault("list-fails-after-create") && s.created.Load()) {
		return nil, transient("list failed")
	}
	if s.fault("list-not-array") {
		return map[string]any{"resources": "none"}, nil
	}
	all, e := api.list(ctx)
	if e != nil {
		return nil, e
	}
	out := []map[string]any{}
	for _, in := range all {
		for _, pre := range p.Prefixes {
			if strings.HasPrefix(in.Name, pre) {
				out = append(out, map[string]any{"id": in.ID, "name": in.Name, "size_key": in.Size, "usd_per_hour": in.USDPerHour,
					"created": in.Created, "expires": in.Expires, "state": in.State})
				break
			}
		}
	}
	switch {
	case s.fault("list-omits"):
		out = []map[string]any{}
	case s.fault("list-foreign-name"):
		out = append(out, map[string]any{"id": "foreign-1", "name": "elsewhere-1", "usd_per_hour": 0.1})
	case s.fault("list-negative-price") && len(p.Prefixes) > 0:
		out = append(out, map[string]any{"id": "neg-1", "name": p.Prefixes[0] + "neg", "usd_per_hour": -1})
	}
	return map[string]any{"resources": out}, nil
}

// spend is today's account spend in micro-dollars.
func (s *server) spend(ctx context.Context, api *apiClient) (any, *rpcError) {
	status, b, err := api.do(ctx, "GET", "/v1/billing/today", nil)
	if e := classify("billing", status, b, err); e != nil {
		return nil, e
	}
	var r struct {
		USD float64 `json:"usd"`
	}
	if json.Unmarshal(b, &r) != nil {
		return nil, transient("billing: bad answer")
	}
	return map[string]any{"usd_micros": int64(r.USD * 1e6)}, nil
}
