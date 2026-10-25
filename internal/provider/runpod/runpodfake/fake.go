// Package runpodfake is a fake RunPod REST v2 API for tests (spec 11): every created pod is a
// local `bitc __agent` process in its own process group, its stdout is the container log, and a
// second loopback server proxies /<pod>-<port>/v1/<file> to the agent's results server.
package runpodfake

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/Exploded-views/bitc/internal/agent"
)

type Options struct {
	Bin     string            // the bitc binary that runs as each pod's agent
	Secrets map[string]string // RUNPOD_SECRET_<name> values
}

type logLine struct{ TS, Line string }

type pod struct {
	ID, Name, Created, Status string
	Started                   string  // startedAt: changes on every restart
	Cost                      float64 // USD per hour, listed as the v2 `cost` field
	Env                       map[string]string
	testRoot                  string
	cmd                       *exec.Cmd
	log                       []logLine
	gone, began, ended        bool
	lostPending               bool // lost-after-begin: the pod vanishes once a reader resumed after its BEGIN line
	static                    bool
	logServed                 bool // a log line was served to a reader (logs-stall-after-first)
	dropTail                  bool // drop-log-tail: from BEGIN, out.log/err.log stay unreadable until result.json was fetched
	beginServed               bool // the BEGIN line was served to a reader (lost-after-begin)
	resultServed              bool
}

type Fake struct {
	t       testing.TB
	o       Options
	mu      sync.Mutex
	api     *httptest.Server
	proxy   *httptest.Server
	pods    map[string]*pod
	calls   []string
	bodies  []map[string]any
	logQs   []string // raw queries of the log reads
	auth    []string // Authorization values the API saw
	proxied []string // Authorization values the proxy saw
	spend   float64
	once    map[string]int
	seq     int
	holds   map[string]chan struct{} // "POST /v2/pods" -> closed by Release
	held    map[string]bool          // a call reached its hold
	agents  sync.WaitGroup           // one per running agent reader/waiter
}

func New(t testing.TB, o Options) *Fake {
	f := &Fake{t: t, o: o, pods: map[string]*pod{}, once: map[string]int{}, holds: map[string]chan struct{}{}, held: map[string]bool{}}
	f.api = httptest.NewServer(http.HandlerFunc(f.serveAPI))
	f.proxy = httptest.NewServer(http.HandlerFunc(f.serveProxy))
	t.Cleanup(func() {
		f.mu.Lock()
		for call, ch := range f.holds { // a held handler would keep Close waiting
			close(ch)
			delete(f.holds, call)
		}
		for _, p := range f.pods {
			if p.cmd != nil && p.cmd.Process != nil {
				_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
			}
		}
		f.mu.Unlock()
		done := make(chan struct{})
		go func() { f.agents.Wait(); close(done) }()
		select { // every agent is reaped before t.TempDir is removed; bounded
		case <-done:
		case <-time.After(10 * time.Second):
		}
		f.api.Close()
		f.proxy.Close()
	})
	return f
}

func (f *Fake) URL() string       { return f.api.URL }
func (f *Fake) ProxyAddr() string { return strings.TrimPrefix(f.proxy.URL, "http://") }

// Behave arms a behaviour: transient-create, transient-create-twice (the same no-capacity answer on a create and on its one retry), create-500-twice, create-429-twice, create-500-after-create (makes the pod, answers 500), unavailable, slow-create,
// list-500, list-500-after-create (the first list once a pod was made), billing-500, refuse-create (422 with a two-line message), restart-after-begin, restart-after-begin-no-boot (startedAt changes, the agent never boots again: build mode once the agent ref is gone), lost-after-begin, drop-log-tail, forge-result, logs-429-burst (the next 5 log reads answer 429, the first with Retry-After: 1), logs-stall-after-first (once a log line was served, since= queries return nothing, as the live API did 2026-10-23; tail queries still work); or
// "hold <METHOD> <path>" (e.g. "hold POST /v2/pods"): every such call is recorded and does its
// work (a create makes its pod), then its answer waits until Release or until the client gives
// up. Race tests use it instead of fixed delays.
func (f *Fake) Behave(b string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if call, ok := strings.CutPrefix(b, "hold "); ok {
		f.holds[call] = make(chan struct{})
		return
	}
	f.once[b] = map[string]int{"transient-create-twice": 2, "create-500-twice": 2, "create-429-twice": 2, "logs-429-burst": 5}[b]
	if f.once[b] == 0 {
		f.once[b] = 1
	}
}

func (f *Fake) take(b string) bool {
	if f.once[b] > 0 {
		f.once[b]--
		return true
	}
	return false
}

// LogQueries are the raw query strings of every log read so far.
func (f *Fake) LogQueries() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.logQs...)
}

func (f *Fake) SetSpend(usd float64) { f.mu.Lock(); f.spend = usd; f.mu.Unlock() }

// Release lets the held calls of call ("POST /v2/pods") answer and stops holding new ones.
func (f *Fake) Release(call string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if ch, ok := f.holds[call]; ok {
		close(ch)
		delete(f.holds, call)
	}
}

// Held reports whether a call has reached its hold: its work is done, its answer waits.
func (f *Fake) Held(call string) bool { f.mu.Lock(); defer f.mu.Unlock(); return f.held[call] }

// AddPod lists a pod with no process (other installs, expired pods, legacy pods).
func (f *Fake) AddPod(id, name string, created time.Time, env map[string]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pods[id] = &pod{ID: id, Name: name, Created: created.UTC().Format(time.RFC3339), Status: "RUNNING", Env: env, static: true}
}

// SetCost sets the per-hour `cost` the API lists for a pod.
func (f *Fake) SetCost(id string, usdPerHour float64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if p := f.pods[id]; p != nil {
		p.Cost = usdPerHour
	}
}

func (f *Fake) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// Live returns the names of the pods the API still holds (not deleted): the truth a provider's
// list must match (provider suite row PS1).
func (f *Fake) Live() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, p := range f.pods {
		if !p.gone {
			out = append(out, p.Name)
		}
	}
	return out
}

func (f *Fake) Auth() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.auth...)
}
func (f *Fake) Proxied() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.proxied...)
}
func (f *Fake) Bodies() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any(nil), f.bodies...)
}

var podPath = regexp.MustCompile(`^/v2/pods/([^/]+)(/logs|/action)?$`)

func (f *Fake) serveAPI(w http.ResponseWriter, r *http.Request) {
	call := r.Method + " " + r.URL.Path
	f.mu.Lock()
	f.calls = append(f.calls, call)
	f.auth = append(f.auth, r.Header.Get("Authorization"))
	gate := f.holds[call]
	f.mu.Unlock()
	if gate == nil {
		f.route(w, r)
		return
	}
	// A held call does its work, then keeps its answer: a create whose response never arrived.
	rec := httptest.NewRecorder()
	f.route(rec, r)
	f.mu.Lock()
	f.held[call] = true
	f.mu.Unlock()
	select {
	case <-gate:
	case <-r.Context().Done():
		return
	}
	for k, v := range rec.Header() {
		w.Header()[k] = v
	}
	w.WriteHeader(rec.Code)
	_, _ = w.Write(rec.Body.Bytes())
}

func (f *Fake) route(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == "POST" && r.URL.Path == "/v2/pods":
		f.create(w, r)
	case r.Method == "GET" && r.URL.Path == "/v2/pods":
		f.mu.Lock()
		fail := f.take("list-500") || (f.seq > 0 && f.take("list-500-after-create"))
		var out []map[string]any
		for _, p := range f.pods {
			if !p.gone {
				out = append(out, map[string]any{"id": p.ID, "name": p.Name, "status": p.Status, "createdAt": p.Created, "cost": p.Cost, "env": p.Env})
			}
		}
		f.mu.Unlock()
		if fail {
			w.WriteHeader(500)
			return
		}
		_ = json.NewEncoder(w).Encode(out)
	case r.Method == "GET" && r.URL.Path == "/v2/billing/pods":
		f.mu.Lock()
		fail, spend := f.take("billing-500"), f.spend
		f.mu.Unlock()
		if fail {
			w.WriteHeader(500)
			return
		}
		fmt.Fprintf(w, `{"records":[{"totalAmount":%g}]}`, spend)
	default:
		m := podPath.FindStringSubmatch(r.URL.Path)
		if m == nil {
			w.WriteHeader(404)
			return
		}
		f.mu.Lock()
		p := f.pods[m[1]]
		f.mu.Unlock()
		if p == nil || p.gone {
			w.WriteHeader(404)
			return
		}
		switch {
		case m[2] == "/logs":
			f.logs(w, r, p)
		case m[2] == "/action":
			f.mu.Lock()
			p.Status = "EXITED"
			f.mu.Unlock()
			f.kill(p)
		case r.Method == "DELETE":
			f.kill(p)
			f.mu.Lock()
			p.gone = true
			f.mu.Unlock()
			w.WriteHeader(204)
		default:
			f.mu.Lock()
			b, _ := json.Marshal(map[string]any{"id": p.ID, "name": p.Name, "status": p.Status, "createdAt": p.Created, "startedAt": p.Started, "env": p.Env})
			f.mu.Unlock()
			_, _ = w.Write(b)
		}
	}
}

func (f *Fake) create(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	switch {
	case f.take("slow-create"):
		f.mu.Unlock()
		time.Sleep(time.Second)
		f.mu.Lock()
	case f.take("transient-create") || f.take("transient-create-twice"):
		f.mu.Unlock()
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"error":"There are no longer any instances available with the requested specifications"}`))
		return
	case f.take("create-429-twice"):
		f.mu.Unlock()
		w.WriteHeader(429)
		return
	case f.take("create-500-twice"):
		f.mu.Unlock()
		w.WriteHeader(500)
		return
	case f.take("refuse-create"): // a 422 whose message has a second line and echoes the env
		f.mu.Unlock()
		w.WriteHeader(422)
		_, _ = w.Write([]byte(`{"title":"Unprocessable Entity","detail":"unknown field zzz\nsecond line env BITC_SPEC=secret"}`))
		return
	case f.take("unavailable"):
		f.mu.Unlock()
		w.WriteHeader(401)
		return
	}
	f.mu.Unlock()
	if len(raw) >= 100<<10 {
		w.WriteHeader(413)
		return
	}
	var body map[string]any
	if json.Unmarshal(raw, &body) != nil {
		w.WriteHeader(400)
		return
	}
	if why := checkV2Body(body); why != "" {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(422)
		b, _ := json.Marshal(map[string]any{"title": "Unprocessable Entity", "status": 422, "detail": why})
		_, _ = w.Write(b)
		return
	}
	env := map[string]string{}
	for k, v := range body["env"].(map[string]any) {
		s := fmt.Sprint(v)
		for name, val := range f.o.Secrets {
			s = strings.ReplaceAll(s, "{{ RUNPOD_SECRET_"+name+" }}", val)
		}
		env[k] = s
	}
	spec, err := agent.DecodeSpec(env["BITC_SPEC"])
	if err != nil || !loopbackOnly(spec) {
		w.WriteHeader(400) // a fake pod never contacts a host that is not loopback
		return
	}
	// Always test mode against this fake: the agent runs as a plain process in a private dir and
	// its self-delete hook calls this API, never the real one, whatever the request carried.
	if !spec.Unprivileged || spec.TestRoot == "" {
		spec.Unprivileged = true
		if spec.TestRoot == "" {
			spec.TestRoot = f.t.TempDir()
		}
		env["BITC_SPEC"], _ = spec.Encode()
	}
	env["BITC_API_BASE"] = f.api.URL
	f.mu.Lock()
	f.seq++
	id := fmt.Sprintf("pod%d", f.seq)
	p := &pod{ID: id, Name: fmt.Sprint(body["name"]), Created: time.Now().UTC().Format(time.RFC3339), Status: "RUNNING", Started: time.Now().UTC().Format(time.RFC3339Nano), Env: env, testRoot: spec.TestRoot}
	f.pods[id] = p
	f.bodies = append(f.bodies, body)
	f.mu.Unlock()
	f.start(p)
	f.mu.Lock()
	lost := f.take("create-500-after-create")
	f.mu.Unlock()
	if lost { // the pod exists, the answer is a 500: a create whose result the client cannot trust
		w.WriteHeader(500)
		return
	}
	w.WriteHeader(201)
	fmt.Fprintf(w, `{"id":%q}`, id)
}

// v2Keys are the top-level keys of the REST v2 create body (spec 8.7); anything else is a 422.
var v2Keys = map[string]bool{"name": true, "image": true, "cpu": true, "cloud": true, "disk": true, "env": true,
	"ports": true, "entrypoint": true, "cmd": true, "dataCenterIds": true, "mounts": true}

// checkV2Body mirrors the real API's strictness: an unknown top-level key, or a missing or mistyped
// required one (name, image, cpu.id, cpu.vcpuCount, env, cmd), is refused. "" means the body is valid.
func checkV2Body(b map[string]any) string {
	for k := range b {
		if !v2Keys[k] {
			return "unknown field " + k
		}
	}
	for _, k := range []string{"name", "image"} {
		if s, ok := b[k].(string); !ok || s == "" {
			return "missing or invalid field " + k
		}
	}
	cpu, ok := b["cpu"].(map[string]any)
	if !ok {
		return "missing or invalid field cpu"
	}
	if s, ok := cpu["id"].(string); !ok || s == "" {
		return "missing or invalid field cpu.id"
	}
	if n, ok := cpu["vcpuCount"].(float64); !ok || n < 1 || n != float64(int(n)) {
		return "missing or invalid field cpu.vcpuCount"
	}
	for k := range cpu {
		if k != "id" && k != "vcpuCount" {
			return "unknown field cpu." + k
		}
	}
	env, ok := b["env"].(map[string]any)
	if !ok {
		return "missing or invalid field env"
	}
	for _, v := range env {
		if _, ok := v.(string); !ok {
			return "env values must be strings"
		}
	}
	if !stringList(b["cmd"], true) {
		return "missing or invalid field cmd"
	}
	for _, k := range []string{"entrypoint", "ports", "dataCenterIds"} {
		if v, present := b[k]; present && !stringList(v, false) {
			return "invalid field " + k
		}
	}
	if v, present := b["cloud"]; present {
		if s, ok := v.(string); !ok || (s != "SECURE" && s != "COMMUNITY") {
			return "invalid field cloud"
		}
	}
	if v, present := b["disk"]; present {
		if n, ok := v.(float64); !ok || n < 1 || n != float64(int(n)) {
			return "invalid field disk"
		}
	}
	return ""
}

func stringList(v any, nonEmpty bool) bool {
	l, ok := v.([]any)
	if !ok || (nonEmpty && len(l) == 0) {
		return false
	}
	for _, e := range l {
		if _, ok := e.(string); !ok {
			return false
		}
	}
	return true
}

// loopbackOnly: every URL the agent would fetch from names a loopback host or a local path.
func loopbackOnly(spec agent.Spec) bool {
	urls := []string{spec.CloneURL}
	for _, t := range spec.Tools {
		urls = append(urls, t.URL)
	}
	for _, raw := range urls {
		if raw == "" || strings.HasPrefix(raw, "/") {
			continue
		}
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" {
			return false // scp-style or opaque: a remote host
		}
		switch u.Hostname() {
		case "127.0.0.1", "localhost", "::1":
		default:
			return false
		}
	}
	return true
}

// start runs the pod's agent; its stdout lines are the container log.
func (f *Fake) start(p *pod) {
	cmd := exec.Command(f.o.Bin, "__agent")
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "RUNPOD_POD_ID=" + p.ID, "RUNPOD_API_KEY=scoped-" + p.ID}
	for k, v := range p.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	out, _ := cmd.StdoutPipe()
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		f.t.Errorf("runpodfake: start agent: %v", err)
		return
	}
	f.mu.Lock()
	p.cmd = cmd
	f.mu.Unlock()
	f.agents.Add(1)
	go func() {
		defer f.agents.Done()
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			line := sc.Text()
			f.mu.Lock()
			restart, silent := false, false
			if strings.HasPrefix(line, agent.MarkBegin+" ") {
				p.began = true
				if f.take("drop-log-tail") {
					p.dropTail = true // withheld from BEGIN on, so the live tail misses the end every time
				}
				if f.take("lost-after-begin") {
					p.lostPending = true // gone once a log reader has the BEGIN line, never before
				}
				restart = f.take("restart-after-begin")
				silent = !restart && f.take("restart-after-begin-no-boot")
			}
			drop := strings.HasPrefix(line, agent.MarkExit+" ") && p.dropTail
			if strings.HasPrefix(line, agent.MarkExit+" ") {
				p.ended = true
				if f.take("forge-result") {
					forge(p)
				}
			}
			if !drop {
				p.log = append(p.log, logLine{TS: time.Now().UTC().Format(time.RFC3339Nano), Line: line})
			}
			if restart { // the BEGIN line is in the log first: the command had started
				f.mu.Unlock()
				f.kill(p)
				_ = cmd.Wait()
				f.start(p) // the same test root: the agent finds "ran" and reports LOST
				return
			}
			if silent { // the container restarted and crash-loops: only startedAt shows it
				f.mu.Unlock()
				f.kill(p)
				_ = cmd.Wait()
				time.Sleep(time.Second) // the host's first status read (its baseline) is long past
				f.mu.Lock()
				p.Started = time.Now().UTC().Format(time.RFC3339Nano)
				f.mu.Unlock()
				return
			}
			f.mu.Unlock()
		}
		_ = cmd.Wait() // reap the agent and close the pipe
	}()
}

// forge rewrites result.json with exit 0 and a MAC made with another key.
func forge(p *pod) {
	matches, _ := filepath.Glob(filepath.Join(p.testRoot, "run", "*", "result.json"))
	for _, m := range matches {
		r := agent.Sign([]byte("a forged key a forged key a forg"), agent.ResultFile{Exit: 0})
		b, _ := json.Marshal(r)
		_ = os.WriteFile(m, b, 0o600)
	}
}

func (f *Fake) kill(p *pod) {
	f.mu.Lock()
	cmd := p.cmd
	f.mu.Unlock()
	if cmd != nil && cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}

// logs serves server-sent events: lines after since, or the last tail; then keeps the
// connection open for up to 2s for new lines.
func (f *Fake) logs(w http.ResponseWriter, r *http.Request, p *pod) {
	f.mu.Lock()
	f.logQs = append(f.logQs, r.URL.RawQuery)
	burst := f.take("logs-429-burst")
	first := burst && f.once["logs-429-burst"] == 4
	f.mu.Unlock()
	if burst {
		if first {
			w.Header().Set("Retry-After", "1")
		}
		w.WriteHeader(429)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	since := r.URL.Query().Get("since")
	tail, _ := strconv.Atoi(r.URL.Query().Get("tail"))
	sent := 0
	f.mu.Lock()
	if p.lostPending && p.beginServed { // a reader already has the BEGIN line (served by an earlier read): now the pod vanishes
		p.gone = true
	}
	start := 0
	if since == "" && tail > 0 && len(p.log) > tail {
		start = len(p.log) - tail
	}
	f.mu.Unlock()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		lines := append([]logLine(nil), p.log...)
		if p.lostPending { // a pod that is about to vanish shows nothing after its BEGIN line
			for i, l := range lines {
				if strings.HasPrefix(l.Line, agent.MarkBegin+" ") {
					lines = lines[:i+1]
					break
				}
			}
		}
		stall := since != "" && p.logServed && f.once["logs-stall-after-first"] > 0
		f.mu.Unlock()
		if stall { // the live stream returned nothing for since= after the first batch
			lines = nil
		}
		for i := start + sent; i < len(lines); i++ {
			if since != "" && lines[i].TS <= since {
				sent++
				continue
			}
			f.mu.Lock()
			p.logServed = true
			if strings.HasPrefix(lines[i].Line, agent.MarkBegin+" ") {
				p.beginServed = true
			}
			f.mu.Unlock()
			b, _ := json.Marshal(map[string]string{"ts": lines[i].TS, "line": lines[i].Line})
			fmt.Fprintf(w, "data: %s\n\n", b)
			sent++
		}
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
		if r.Context().Err() != nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}

var proxyPath = regexp.MustCompile(`^/([^/]+)-([0-9]+)(/v1/.*)$`)

// serveProxy forwards /<pod>-<port>/v1/<file> to the pod's agent (its port is in <TestRoot>/port).
func (f *Fake) serveProxy(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.proxied = append(f.proxied, r.Header.Get("Authorization"))
	f.mu.Unlock()
	m := proxyPath.FindStringSubmatch(r.URL.Path)
	if m == nil {
		w.WriteHeader(404)
		return
	}
	f.mu.Lock()
	p := f.pods[m[1]]
	unreachable := p == nil || p.gone || p.lostPending
	f.mu.Unlock()
	if unreachable {
		w.WriteHeader(502)
		return
	}
	f.mu.Lock()
	withheld := false
	switch m[3] {
	case "/v1/result.json":
		p.resultServed = true
	case "/v1/out.log", "/v1/err.log":
		withheld = p.dropTail && !p.resultServed // the tail the log stream and the live tail both missed
	}
	f.mu.Unlock()
	if withheld {
		w.WriteHeader(503)
		return
	}
	port, err := os.ReadFile(filepath.Join(p.testRoot, "port"))
	if err != nil {
		w.WriteHeader(502)
		return
	}
	target, _ := url.Parse("http://127.0.0.1:" + strings.TrimSpace(string(port)))
	rp := httputil.NewSingleHostReverseProxy(target)
	r.URL.Path = m[3]
	rp.ServeHTTP(w, r)
}
