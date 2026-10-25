// Command fakeapi is the loopback fake of the fakex API that the reference plugin talks to
// (providers delta A.10.2). It is what a plugin author's own fake looks like to the conformance
// suite: an API the plugin is a client of, plus the /__bitc/ control contract.
//
// Every instance whose job is started runs bitc's own agent (`bitc __agent`, the binary named by
// -bitc) as a plain local process in a private directory, as the RunPod fake does; the agent's
// stdout is the instance's log and its result files are served as the instance's files.
//
// The /__bitc/ contract the provider suite uses:
//
//	POST /__bitc/behave {"behaviour": "<name>"}  the behaviour of instances created from now on:
//	    ok, create-fails (every create answers 503), preattach-fails (the instance fails before
//	    it gets the job), provisioning-forever (the instance never becomes ready), lost-after-begin, restart-after-begin, drop-log-tail, forge-result;
//	    paginate-2 makes the instance list pages of two
//	GET  /__bitc/calls   every API call in order: {seq, method, path, query, headers, body, attach},
//	                     the call that first carries job input marked "attach": true
//	GET  /__bitc/live    the names of the instances the fake holds
//	GET  /__bitc/torn    the ids of the instances deleted so far
//	POST /__bitc/spend {"usd": n}                 (optional) today's spend the billing endpoint reports
//	POST /__bitc/seed {name, owner, created, expires}  (optional) an instance with no process
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Exploded-views/bitc/internal/agent"
	"github.com/Exploded-views/bitc/internal/config"
)

// logLine and the instance as the API shows it.
type view struct {
	ID         string  `json:"id"`
	Name       string  `json:"name"`
	Owner      string  `json:"owner"`
	Size       string  `json:"size"`
	State      string  `json:"state"`
	Why        string  `json:"why"`
	Created    string  `json:"created"`
	Expires    string  `json:"expires"`
	USDPerHour float64 `json:"usd_per_hour"`
	Exit       int     `json:"exit"`
	Began      bool    `json:"began"`
}

type inst struct {
	view
	seq       int
	behaviour string // the behaviour in force when it was created
	jobID     string // the job's id, the agent's results dir
	nonce     string
	testRoot  string
	spec      agent.Spec
	deadline  string
	cmd       *exec.Cmd
	log       []string
	gone      bool
	// behaviour state
	exitSeen, lostPending, beginServed, dropTail, resultServed bool
	exitCode                                                   int
}

type recorded struct {
	Seq     int               `json:"seq"`
	Method  string            `json:"method"`
	Path    string            `json:"path"`
	Query   string            `json:"query"`
	Headers map[string]string `json:"headers"`
	Body    string            `json:"body"`
	Attach  bool              `json:"attach"`
}

type fake struct {
	bitc, root string
	mu         sync.Mutex
	insts      map[string]*inst
	seq        int
	behaviour  string
	paginate   bool
	calls      []recorded
	torn       []string
	spend      float64
	agents     sync.WaitGroup
}

func main() {
	listen := flag.String("listen", "127.0.0.1:0", "address to listen on")
	bitc := flag.String("bitc", "", "the bitc binary that runs as each instance's agent")
	root := flag.String("root", "", "directory for the agents' private roots")
	flag.Parse()
	if *bitc == "" || *root == "" {
		fmt.Fprintln(os.Stderr, "usage: fakeapi -bitc <bitc> -root <dir> [-listen addr]")
		os.Exit(64)
	}
	f := &fake{bitc: *bitc, root: *root, insts: map[string]*inst{}, behaviour: "ok"}
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-sigs
		f.killAll()
		os.Exit(0)
	}()
	fmt.Printf("listening http://%s\n", ln.Addr())
	_ = os.Stdout.Sync()
	_ = http.Serve(ln, http.HandlerFunc(f.serve))
}

func (f *fake) killAll() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, in := range f.insts {
		killGroup(in.cmd)
	}
}

func killGroup(cmd *exec.Cmd) {
	if cmd != nil && cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}

var (
	jobPath   = regexp.MustCompile(`^/v1/instances/([^/]+)/job$`)
	logsPath  = regexp.MustCompile(`^/v1/instances/([^/]+)/logs$`)
	filesPath = regexp.MustCompile(`^/v1/instances/([^/]+)/files/([a-z.]+)$`)
	instPath  = regexp.MustCompile(`^/v1/instances/([^/]+)$`)
)

func (f *fake) serve(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/__bitc/") {
		f.control(w, r)
		return
	}
	body, _ := io.ReadAll(r.Body)
	r.Body = io.NopCloser(bytes.NewReader(body))
	hdr := map[string]string{}
	for k := range r.Header {
		hdr[k] = r.Header.Get(k)
	}
	f.mu.Lock()
	f.calls = append(f.calls, recorded{Seq: len(f.calls) + 1, Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Headers: hdr,
		Body: string(body), Attach: r.Method == "POST" && jobPath.MatchString(r.URL.Path)})
	f.mu.Unlock()
	switch {
	case r.Method == "POST" && r.URL.Path == "/v1/instances":
		f.create(w, body)
	case r.Method == "GET" && r.URL.Path == "/v1/instances":
		f.page(w, r)
	case r.Method == "GET" && r.URL.Path == "/v1/billing/today":
		f.mu.Lock()
		usd := f.spend
		f.mu.Unlock()
		writeJSON(w, 200, map[string]any{"usd": usd})
	case r.Method == "POST" && jobPath.MatchString(r.URL.Path):
		f.startJob(w, jobPath.FindStringSubmatch(r.URL.Path)[1], body)
	case r.Method == "GET" && logsPath.MatchString(r.URL.Path):
		f.logs(w, r, logsPath.FindStringSubmatch(r.URL.Path)[1])
	case r.Method == "GET" && filesPath.MatchString(r.URL.Path):
		m := filesPath.FindStringSubmatch(r.URL.Path)
		f.file(w, r, m[1], m[2])
	case instPath.MatchString(r.URL.Path) && (r.Method == "GET" || r.Method == "DELETE"):
		id := instPath.FindStringSubmatch(r.URL.Path)[1]
		if r.Method == "GET" {
			f.get(w, id)
		} else {
			f.remove(w, id)
		}
	default:
		w.WriteHeader(404)
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// find returns the live instance by id or name; the lock is held.
func (f *fake) find(idOrName string) *inst {
	id, _ := url.PathUnescape(idOrName)
	if in := f.insts[id]; in != nil && !in.gone {
		return in
	}
	for _, in := range f.insts {
		if in.Name == id && !in.gone {
			return in
		}
	}
	return nil
}

func (f *fake) control(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.URL.Path {
	case "/__bitc/behave":
		var b struct{ Behaviour string }
		_ = json.Unmarshal(body, &b)
		switch b.Behaviour {
		case "paginate-2":
			f.paginate = true
		case "ok", "create-fails", "preattach-fails", "provisioning-forever", "lost-after-begin", "restart-after-begin", "drop-log-tail", "forge-result":
			f.behaviour = b.Behaviour
		default:
			writeJSON(w, 400, map[string]string{"error": "unknown behaviour " + b.Behaviour})
			return
		}
		w.WriteHeader(204)
	case "/__bitc/calls":
		writeJSON(w, 200, append([]recorded{}, f.calls...))
	case "/__bitc/live":
		names := []string{}
		for _, in := range f.insts {
			if !in.gone {
				names = append(names, in.Name)
			}
		}
		sort.Strings(names)
		writeJSON(w, 200, names)
	case "/__bitc/torn":
		writeJSON(w, 200, append([]string{}, f.torn...))
	case "/__bitc/spend":
		var b struct{ USD float64 }
		_ = json.Unmarshal(body, &b)
		f.spend = b.USD
		w.WriteHeader(204)
	case "/__bitc/seed":
		var b struct{ Name, Owner, Created, Expires string }
		_ = json.Unmarshal(body, &b)
		f.seq++
		in := &inst{seq: f.seq, view: view{ID: fmt.Sprintf("seed-%d", f.seq), Name: b.Name, Owner: b.Owner, Size: "cpu8", State: "running", Created: b.Created, Expires: b.Expires}}
		f.insts[in.ID] = in
		writeJSON(w, 200, map[string]string{"id": in.ID})
	default:
		w.WriteHeader(404)
	}
}

// create makes an instance with the name and owner given; it carries no job input.
func (f *fake) create(w http.ResponseWriter, body []byte) {
	var b struct{ Name, Size, Owner, Expires string }
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.behaviour == "create-fails" {
		w.WriteHeader(503)
		return
	}
	if json.Unmarshal(body, &b) != nil || b.Name == "" || b.Size == "" {
		writeJSON(w, 400, map[string]string{"error": "name and size are required"})
		return
	}
	f.seq++
	now := time.Now().UTC()
	exp := b.Expires
	if exp == "" {
		exp = now.Add(time.Hour).Format(time.RFC3339)
	}
	in := &inst{seq: f.seq, behaviour: f.behaviour, view: view{ID: fmt.Sprintf("fx-%d", f.seq), Name: b.Name, Owner: b.Owner, Size: b.Size,
		State: "ready", Created: now.Format(time.RFC3339), Expires: exp}}
	if f.behaviour == "provisioning-forever" {
		in.State = "provisioning" // never becomes ready: a launch waiting for it can only be cancelled
	}
	if f.behaviour == "preattach-fails" {
		in.State, in.Why = "failed", "setup step packages failed: apt timeout"
	}
	f.insts[in.ID] = in
	writeJSON(w, 201, in.view)
}

// page lists the live instances in creation order, a page after the cursor (the last seq seen).
func (f *fake) page(w http.ResponseWriter, r *http.Request) {
	after, _ := strconv.Atoi(r.URL.Query().Get("cursor"))
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	f.mu.Lock()
	defer f.mu.Unlock()
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	if f.paginate {
		limit = 2
	}
	var live []*inst
	for _, in := range f.insts {
		if !in.gone && in.seq > after {
			live = append(live, in)
		}
	}
	sort.Slice(live, func(i, j int) bool { return live[i].seq < live[j].seq })
	out := struct {
		Instances []view `json:"instances"`
		Next      string `json:"next"`
	}{Instances: []view{}}
	for i, in := range live {
		if i == limit {
			out.Next = strconv.Itoa(live[i-1].seq)
			break
		}
		out.Instances = append(out.Instances, in.view)
	}
	writeJSON(w, 200, out)
}

// vanish applies lost-after-begin: once a reader has the BEGIN line, the instance disappears. The lock is held.
func (f *fake) vanish(in *inst) {
	if in.lostPending && in.beginServed && !in.gone {
		in.gone = true
		killGroup(in.cmd)
	}
}

func (f *fake) get(w http.ResponseWriter, id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	in := f.find(id)
	if in != nil {
		f.vanish(in)
	}
	if in == nil || in.gone {
		w.WriteHeader(404)
		return
	}
	writeJSON(w, 200, in.view)
}

func (f *fake) remove(w http.ResponseWriter, id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	in := f.find(id)
	if in == nil {
		w.WriteHeader(404)
		return
	}
	killGroup(in.cmd)
	in.gone = true
	f.torn = append(f.torn, in.ID)
	w.WriteHeader(204)
}

func (f *fake) logs(w http.ResponseWriter, r *http.Request, id string) {
	since, _ := strconv.Atoi(r.URL.Query().Get("since"))
	f.mu.Lock()
	defer f.mu.Unlock()
	in := f.find(id)
	if in != nil {
		f.vanish(in)
	}
	if in == nil || in.gone {
		w.WriteHeader(404)
		return
	}
	lines := in.log
	if in.lostPending { // an instance that is about to vanish shows nothing after its BEGIN line
		for i, l := range lines {
			if strings.HasPrefix(l, agent.MarkBegin+" ") {
				lines = lines[:i+1]
			}
		}
	}
	if since > len(lines) {
		since = len(lines)
	}
	out := append([]string{}, lines[since:]...)
	for _, l := range out {
		if strings.HasPrefix(l, agent.MarkBegin+" ") {
			in.beginServed = true
		}
	}
	writeJSON(w, 200, map[string]any{"lines": out, "next": len(lines)})
}

// file serves a result file of the agent from an offset: out.log and err.log while they grow.
func (f *fake) file(w http.ResponseWriter, r *http.Request, id, name string) {
	f.mu.Lock()
	in := f.find(id)
	var dir string
	withheld := false
	if in != nil {
		dir = filepath.Join(in.testRoot, "run", in.jobID)
		if name == "result.json" {
			in.resultServed = true
		}
		withheld = in.dropTail && !in.resultServed && (name == "out.log" || name == "err.log")
	}
	f.mu.Unlock()
	if in == nil || in.testRoot == "" {
		w.WriteHeader(404)
		return
	}
	if withheld { // drop-log-tail: the tail the log stream missed is not readable until the result was fetched
		w.WriteHeader(503)
		return
	}
	offset, _ := strconv.ParseInt(r.URL.Query().Get("offset"), 10, 64)
	var b []byte
	var err error
	if name == "out.log" || name == "err.log" {
		if b, err = os.ReadFile(filepath.Join(dir, name+".part")); err != nil {
			b, err = os.ReadFile(filepath.Join(dir, name))
		}
	} else {
		b, err = os.ReadFile(filepath.Join(dir, name))
	}
	if err != nil {
		w.WriteHeader(404)
		return
	}
	if offset > int64(len(b)) {
		offset = int64(len(b))
	}
	_, _ = w.Write(b[offset:])
}

// wireJob is the part of the protocol-1 job object (A.5.1) the fake needs to start the agent.
type wireJob struct {
	Job       string   `json:"job"`
	Pool      string   `json:"pool"`
	Argv      []string `json:"argv"`
	Cwd       string   `json:"cwd"`
	Env       []string `json:"env"`
	CloneURL  string   `json:"clone_url"`
	Ref       string   `json:"ref"`
	Sha       string   `json:"sha"`
	TimeoutS  int      `json:"timeout_s"`
	Deadline  string   `json:"deadline"`
	Bearer    string   `json:"bearer"`
	MacKey    string   `json:"mac_key"`
	Nonce     string   `json:"nonce"`
	Packages  []string `json:"packages"`
	RootSteps bool     `json:"root_steps"`
	Artifacts []struct {
		Path string `json:"path"`
	} `json:"artifacts"`
	Tools []struct {
		Name, URL, SHA256 string
	} `json:"tools"`
	Setup []struct {
		Name  string   `json:"name"`
		RunAs string   `json:"run_as"`
		Argv  []string `json:"argv"`
	} `json:"setup"`
	Services []struct {
		Name          string   `json:"name"`
		RunAs         string   `json:"run_as"`
		Argv          []string `json:"argv"`
		Ready         []string `json:"ready"`
		ReadyTimeoutS int      `json:"ready_timeout_s"`
	} `json:"services"`
}

// startJob is the attach call: the instance gets the job and runs the agent.
func (f *fake) startJob(w http.ResponseWriter, id string, body []byte) {
	var req struct {
		Job       wireJob `json:"job"`
		DeployKey string  `json:"deploy_key"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, 400, map[string]string{"error": "bad job"})
		return
	}
	j := req.Job
	f.mu.Lock()
	in := f.find(id)
	if in == nil {
		f.mu.Unlock()
		w.WriteHeader(404)
		return
	}
	if in.State != "ready" {
		f.mu.Unlock()
		w.WriteHeader(409)
		return
	}
	spec := agent.Spec{Job: j.Job, Pool: j.Pool, Argv: j.Argv, Cwd: j.Cwd, Env: j.Env, CloneURL: j.CloneURL, Ref: j.Ref, Sha: j.Sha,
		KnownHosts: config.BuiltinKnownHosts, Timeout: agent.Duration(time.Duration(j.TimeoutS) * time.Second), Packages: j.Packages,
		RootSteps: j.RootSteps, Bearer: j.Bearer, MacKey: j.MacKey, Nonce: j.Nonce, Unprivileged: true,
		TestRoot: filepath.Join(f.root, in.ID)}
	for _, t := range j.Tools {
		spec.Tools = append(spec.Tools, agent.Tool{Name: t.Name, URL: t.URL, SHA256: t.SHA256})
	}
	for _, s := range j.Setup {
		spec.Setup = append(spec.Setup, agent.Step{Name: s.Name, RunAs: s.RunAs, Argv: s.Argv})
	}
	for _, s := range j.Services {
		spec.Services = append(spec.Services, agent.Service{Name: s.Name, RunAs: s.RunAs, Argv: s.Argv, Ready: s.Ready,
			ReadyTimeout: agent.Duration(time.Duration(s.ReadyTimeoutS) * time.Second)})
	}
	for _, a := range j.Artifacts {
		spec.Artifacts = append(spec.Artifacts, a.Path)
	}
	if !loopbackOnly(spec) { // a fake instance never contacts a host that is not loopback
		f.mu.Unlock()
		writeJSON(w, 400, map[string]string{"error": "the fake runs only loopback clone and tool URLs"})
		return
	}
	in.spec, in.jobID, in.nonce, in.testRoot = spec, j.Job, j.Nonce, spec.TestRoot
	in.deadline = strconv.FormatInt(parseTime(j.Deadline).Unix(), 10)
	in.State = "running"
	f.mu.Unlock()
	f.start(in)
	w.WriteHeader(202)
}

func parseTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Now().Add(time.Hour)
	}
	return t
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

// start runs the agent; its stdout lines are the instance's log. It is called again to restart the
// agent after restart-after-begin (the same root: the agent finds that the job ran and reports LOST).
func (f *fake) start(in *inst) {
	enc, _ := in.spec.Encode()
	cmd := exec.Command(f.bitc, "__agent")
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "BITC_SPEC=" + enc, "BITC_DEADLINE=" + in.deadline}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	out, _ := cmd.StdoutPipe()
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "fakeapi: start agent:", err)
		f.mu.Lock()
		in.State, in.Why = "exited", err.Error()
		in.Exit = 1
		f.mu.Unlock()
		return
	}
	f.mu.Lock()
	in.cmd = cmd
	f.mu.Unlock()
	f.agents.Add(1)
	go func() {
		defer f.agents.Done()
		sc := bufio.NewScanner(out)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			line := sc.Text()
			f.mu.Lock()
			restart, drop := false, false
			switch {
			case strings.HasPrefix(line, agent.MarkBegin+" "):
				in.Began = true
				// drop-log-tail withholds the logs from BEGIN on, so the live tail misses the end every
				// time instead of only when it polls before the job's last line
				in.dropTail = in.dropTail || in.behaviour == "drop-log-tail"
				if in.behaviour == "lost-after-begin" {
					in.lostPending = true
				}
				restart = in.behaviour == "restart-after-begin"
				in.behaviour = map[bool]string{true: "ok", false: in.behaviour}[restart] // restart once
			case strings.HasPrefix(line, agent.MarkExit+" "):
				if m, ok := agent.ParseMarker(line, in.nonce); ok {
					// The agent keeps serving its results after the job ends, so the EXIT marker (printed
					// once result.json is committed) is what ends the instance's job.
					in.exitSeen, in.State = true, "exited"
					in.exitCode, _ = strconv.Atoi(m.Arg)
					in.Exit = in.exitCode
				}
				if in.behaviour == "forge-result" {
					forge(in)
				}
				drop = in.behaviour == "drop-log-tail"
			}
			if !drop {
				in.log = append(in.log, line)
			}
			if restart {
				f.mu.Unlock()
				killGroup(cmd)
				_ = cmd.Wait()
				f.start(in)
				return
			}
			f.mu.Unlock()
		}
		_ = cmd.Wait()
		f.mu.Lock()
		defer f.mu.Unlock()
		if in.cmd != cmd {
			return // restarted
		}
		in.State = "exited"
		if in.exitSeen {
			in.Exit = in.exitCode
		} else if cmd.ProcessState != nil {
			in.Exit = cmd.ProcessState.ExitCode()
		}
	}()
}

// forge rewrites result.json with exit 0 and a MAC made with another key.
func forge(in *inst) {
	matches, _ := filepath.Glob(filepath.Join(in.testRoot, "run", "*", "result.json"))
	for _, m := range matches {
		r := agent.Sign([]byte("a forged key a forged key a forg"), agent.ResultFile{Exit: 0})
		b, _ := json.Marshal(r)
		_ = os.WriteFile(m, b, 0o600)
	}
}
