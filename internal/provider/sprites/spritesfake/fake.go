// Package spritesfake is a strict fake of the Fly Sprites API for tests (sprites-provider.md 8.1):
// an httptest REST server plus a WSS exec endpoint. A Sprite is a directory <root>/<name>/overlay;
// exec runs `bitc __agent ...` as a local unprivileged process in its own process group with
// TestRoot = the overlay. The fake never contacts any host, accepts only loopback clients, and
// rejects what the real API would: unknown JSON fields, unknown query parameters, a wrong token.
//
// Modelled: policies (stored, served by GET), the organization on every Sprite response, pause
// (no exec and no Task for IdleAfter: the Sprite goes warm and its processes are SIGSTOPped; any
// exec wakes it), cold drop (SIGKILL of everything, exec sessions cut), automatic checkpoints
// (copies of the overlay that exclude the tmpfs /run), exec that outlives its connection only for
// max_run_after_disconnect, and a Tasks API on /.sprite/api.sock.
package spritesfake

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/Exploded-views/bitc/internal/agent"
	"github.com/Exploded-views/bitc/internal/wsmini"
)

// Options configure a Fake. The zero value is usable except for Bin, which agent execs need.
type Options struct {
	Bin      string // the bitc binary that runs `__agent` (BuildBitc)
	Org      string // default "sandbox-org"
	Token    string // default Org + "/fake-token"
	AgentSHA string // when set, an agent exec whose sha check names another sha exits 1
	// IdleAfter: no exec and no live Task for this long pauses a Sprite (warm). Default 30s.
	IdleAfter time.Duration
	// ColdAfter: a Sprite warm this long goes cold (everything killed). 0 = never.
	ColdAfter time.Duration
	// AutoCheckpointEvery: period of the automatic checkpoints; 0 = never (take them with
	// AutoCheckpoint).
	AutoCheckpointEvery time.Duration
	// SessionsDontHold: an open exec session is not activity, only a live Task is (the worst case
	// of probe 9.17).
	SessionsDontHold bool
	PageSize         int // list page size, default 100
	// Exec runs a command that is not an agent command; nil means exit 127.
	Exec func(c ExecContext) int
}

// ExecContext is a non-agent exec handed to Options.Exec.
type ExecContext struct {
	Sprite, Overlay string
	Cmd             []string
	Stdin           io.Reader
	Stdout, Stderr  io.Writer
}

// Call is one recorded API request.
type Call struct {
	Method, Path, RawQuery, Auth string
	Body                         []byte
}

// ExecCall is one recorded exec upgrade: the query and every stdin byte the client sent.
type ExecCall struct {
	Sprite string
	Query  url.Values
	Stdin  []byte
	MaxRun time.Duration
}

type proc struct {
	cmd  *exec.Cmd
	done chan struct{}
}

type task struct{ expires time.Time }

type sprite struct {
	id, name, org string
	created       time.Time
	status        string
	labels        map[string]string
	dir, overlay  string
	netPolicy     *json.RawMessage
	resources     *json.RawMessage
	procs         []*proc
	sessions      int
	tasks         map[string]task
	lastActive    time.Time
	warmSince     time.Time
	ckpts         []string
	nckpt         int
	ncp           int // manual checkpoints taken
	static        bool
	sock          net.Listener
	sockDir       string
	cut           []func() // closers of the live exec connections
}

// Fake is the fake API.
type Fake struct {
	t       testing.TB
	o       Options
	root    string
	srv     *httptest.Server
	mu      sync.Mutex
	sprites map[string]*sprite
	calls   []Call
	execs   []*ExecCall
	once    map[string]int
	seq     int
	wg      sync.WaitGroup
	quit    chan struct{}
	closed  bool
	pauses  map[string]int      // pauses per Sprite name, kept after a delete
	taskLog map[string][]string // Tasks API events per Sprite name, kept after a delete
	conns   []string            // Connectors of the org (GET /v1/oauth/connections)
	// survivors: checkpoint ids kept after a Sprite's delete (the checkpoints-survive-delete
	// behaviour, the worst case of probe 9.25), by Sprite name.
	survivors map[string][]string
	// beforeExecJoin, when set (under mu), runs in an exec connection's goroutine just before it
	// joins wg: a seam for the tests that order that join against close.
	beforeExecJoin func()
	// beforeCreate, when set (under mu), runs in a create request just before it takes mu to add
	// the Sprite: the same kind of seam, for the create path.
	beforeCreate func()
	// beforeAgentJoin, when set (under mu), runs in an agent exec's launch goroutine, with its
	// process started and registered, just before it joins wg.
	beforeAgentJoin func()
}

var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// New starts a fake. It is torn down (every process killed, every Sprite removed) at test end.
func New(t testing.TB, o Options) *Fake {
	t.Helper()
	if o.Org == "" {
		o.Org = "sandbox-org"
	}
	if o.Token == "" {
		o.Token = o.Org + "/fake-token"
	}
	if o.IdleAfter <= 0 {
		o.IdleAfter = 30 * time.Second
	}
	if o.PageSize <= 0 {
		o.PageSize = 100
	}
	root, err := os.MkdirTemp("", "spf")
	if err != nil {
		t.Fatal(err)
	}
	f := &Fake{t: t, o: o, root: root, sprites: map[string]*sprite{}, once: map[string]int{}, quit: make(chan struct{}),
		pauses: map[string]int{}, taskLog: map[string][]string{}, survivors: map[string][]string{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	f.wg.Add(1)
	go f.janitor()
	t.Cleanup(f.close)
	return f
}

func (f *Fake) close() {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return
	}
	f.closed = true
	close(f.quit)
	var all []*sprite
	for _, s := range f.sprites {
		all = append(all, s)
	}
	f.mu.Unlock()
	for _, s := range all {
		f.mu.Lock()
		f.killAll(s)
		f.cutAll(s)
		f.mu.Unlock()
		if s.sock != nil {
			s.sock.Close()
		}
	}
	f.srv.CloseClientConnections()
	done := make(chan struct{})
	go func() { f.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
	}
	f.srv.Close()
	for _, s := range all {
		if s.sockDir != "" {
			_ = os.RemoveAll(s.sockDir)
		}
	}
	_ = os.RemoveAll(f.root)
}

// URL is the API base (http, loopback). Token and Org are what a client must use.
func (f *Fake) URL() string   { return f.srv.URL }
func (f *Fake) Token() string { return f.o.Token }
func (f *Fake) Org() string   { return f.o.Org }

var (
	bitcOnce sync.Once
	bitcPath string
	bitcErr  error
)

// BuildBitc builds ./cmd/bitc once per test process and returns the binary's path.
func BuildBitc(t testing.TB) string {
	t.Helper()
	bitcOnce.Do(func() {
		_, file, _, _ := runtime.Caller(0)
		modRoot := filepath.Join(filepath.Dir(file), "..", "..", "..", "..")
		dir, err := os.MkdirTemp("", "spbitc")
		if err != nil {
			bitcErr = err
			return
		}
		bitcPath = filepath.Join(dir, "bitc")
		c := exec.Command("go", "build", "-o", bitcPath, "./cmd/bitc")
		c.Dir = modRoot
		c.Env = append(os.Environ(), "CGO_ENABLED=0")
		if out, err := c.CombinedOutput(); err != nil {
			bitcErr = fmt.Errorf("build bitc: %w\n%s", err, out)
		}
	})
	if bitcErr != nil {
		t.Fatal(bitcErr)
	}
	return bitcPath
}

// Behave arms a behaviour once; BehaveN arms it n times (n < 0: until the test ends).
//
//	429-create             POST /v1/sprites answers 429 sprite_creation_rate_limited (nothing made)
//	concurrent-limit       POST /v1/sprites answers 403 concurrent_sprite_limit_exceeded
//	max-sprites-exceeded   POST /v1/sprites answers 400 "max sprites per org exceeded"
//	403-restricted         POST /v1/sprites answers 403 "Your account is restricted"
//	401                    every request answers 401
//	create-then-500        POST /v1/sprites makes the Sprite, then answers 500
//	create-then-500-lookup-fails  as above; the next GET of a Sprite and the next list answer 500
//	create-500-nothing     POST /v1/sprites answers 500 and makes nothing
//	policy-get-mismatch    GET of a policy answers an empty policy
//	wrong-org              Sprite responses name another organization
//	labels-unsupported     a create carrying labels answers 400 (unknown field labels)
//	delete-500             DELETE of a Sprite answers 500 and does nothing
//	list-500               GET /v1/sprites answers 500
//	drop-exec-socket       the exec upgrade is answered 101 and the socket is then cut
//	exec-upgrade-4xx       the exec upgrade is answered 400 (before 101)
//	exec-upgrade-5xx       the exec upgrade is answered 503 (before 101)
//	exec-upgrade-timeout   the exec request is read and never answered
//	cold-drop-mid-job      the Sprite goes cold when the attach's boot-stdin agent first writes to stdout
//	no-org                 Sprite responses carry no organization
//	policy-500             a policy POST answers 500 (the policy is not set)
//	provision-infra-fails  `__agent provision` answers like the agent's infra failure: last line
//	                       `setup step packages failed: ...`, exit 75
//	provision-repo-fails   as above for the repo's own failure: `setup step tl failed: ...`, exit 71
//	attach-4xx             the exec that starts `__agent --boot-stdin` is answered 400 (before 101)
//	attach-timeout         that exec is read and never answered
//	drop-tail              the next `__agent tail` exec is answered 101 and the socket is then cut
//	attach-truncate        the attach's agent gets only the first half of its boot payload, so it
//	                       refuses it (`refused: truncated payload`, exit 65) and starts nothing
//	forge-result           the next time the attach's agent reports the job ended, result.json is
//	                       rewritten with exit 0 and a MAC made with another key
//	lax-names              a create accepts any name (the API might not reject what the client calls invalid)
//	checkpoints-survive-delete  the next Sprite delete leaves its checkpoints listable and
//	                       readable by id (the worst case of probe 9.25); restore stays 404
//
// An unknown name fails the test. The attach-* behaviours spare every other exec, so the
// provisioning before the attach is untouched.
func (f *Fake) Behave(b string) { f.BehaveN(b, 1) }

var behaviours = map[string]bool{"429-create": true, "concurrent-limit": true, "max-sprites-exceeded": true, "403-restricted": true,
	"401": true, "create-then-500": true, "create-then-500-lookup-fails": true, "create-500-nothing": true, "policy-get-mismatch": true,
	"wrong-org": true, "labels-unsupported": true, "delete-500": true, "list-500": true, "drop-exec-socket": true,
	"exec-upgrade-4xx": true, "exec-upgrade-5xx": true, "exec-upgrade-timeout": true, "cold-drop-mid-job": true, "no-org": true,
	"policy-500": true, "provision-infra-fails": true, "provision-repo-fails": true, "attach-4xx": true, "attach-timeout": true, "drop-tail": true, "attach-truncate": true, "forge-result": true, "checkpoints-survive-delete": true, "lax-names": true}

// KnownBehaviour reports whether b is a documented behaviour name.
func (f *Fake) KnownBehaviour(b string) bool { return behaviours[b] }

func (f *Fake) BehaveN(b string, n int) {
	if !behaviours[b] {
		f.t.Fatalf("spritesfake: unknown behaviour %q", b)
	}
	f.mu.Lock()
	f.once[b] = n
	f.mu.Unlock()
}

// take consumes one use of b (callers hold f.mu).
func (f *Fake) take(b string) bool {
	switch n := f.once[b]; {
	case n < 0:
		return true
	case n > 0:
		f.once[b]--
		return true
	}
	return false
}

// Calls returns every API request so far, in order (an exec upgrade is a GET .../exec).
func (f *Fake) Calls() []Call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Call(nil), f.calls...)
}

// Execs returns every exec upgrade so far with the stdin it received.
func (f *Fake) Execs() []ExecCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []ExecCall
	for _, e := range f.execs {
		c := *e
		c.Stdin = append([]byte(nil), e.Stdin...)
		out = append(out, c)
	}
	return out
}

// Live returns the names of the Sprites the API holds.
func (f *Fake) Live() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for n := range f.sprites {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Status is "cold", "warm" or "running"; "" when the Sprite does not exist.
func (f *Fake) Status(name string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s := f.sprites[name]; s != nil {
		return s.status
	}
	return ""
}

// Overlay is the Sprite's disk root ("" when it does not exist).
func (f *Fake) Overlay(name string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s := f.sprites[name]; s != nil {
		return s.overlay
	}
	return ""
}

// Checkpoints are the directories of the Sprite's checkpoints.
func (f *Fake) Checkpoints(name string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s := f.sprites[name]; s != nil {
		return append([]string(nil), s.ckpts...)
	}
	return nil
}

// Policies returns the stored network and resources policies as JSON ("" when unset).
func (f *Fake) Policies(name string) (network, resources string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s := f.sprites[name]; s != nil {
		if s.netPolicy != nil {
			network = string(*s.netPolicy)
		}
		if s.resources != nil {
			resources = string(*s.resources)
		}
	}
	return
}

// SockPath is the path of the Sprite's Tasks API socket inside the overlay.
func (f *Fake) SockPath(name string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s := f.sprites[name]; s != nil {
		return filepath.Join(s.overlay, ".sprite", "api.sock")
	}
	return ""
}

// AddSprite lists a Sprite with no process and no disk (another install's, an expired one).
func (f *Fake) AddSprite(name, org string, created time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	f.sprites[name] = &sprite{id: fmt.Sprintf("sp%d", f.seq), name: name, org: org, created: created.UTC(), status: "cold", static: true, tasks: map[string]task{}}
}

// AddConnection adds a Connector (an OAuth connection) to the org.
func (f *Fake) AddConnection(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.conns = append(f.conns, name)
}

// Wake runs a cold or warm Sprite as an exec would.
func (f *Fake) Wake(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s := f.sprites[name]; s != nil {
		f.wake(s)
	}
}

// ColdDrop kills everything on the Sprite and cuts its exec sessions (the platform dropping a
// Sprite to cold mid-job). The disk stays.
func (f *Fake) ColdDrop(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s := f.sprites[name]; s != nil {
		f.coldDrop(s)
	}
}

// Pause makes the Sprite warm now: its processes are stopped (as after IdleAfter).
func (f *Fake) Pause(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s := f.sprites[name]; s != nil {
		f.pause(s)
	}
}

// AutoCheckpoint takes an automatic checkpoint now and returns its directory.
func (f *Fake) AutoCheckpoint(name string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s := f.sprites[name]; s != nil {
		return f.autoCheckpoint(s)
	}
	return ""
}

// ---------------------------------------------------------------------------------------------
// state machine (callers hold f.mu)

func (f *Fake) wake(s *sprite) {
	if s.status == "warm" {
		for _, p := range s.procs {
			signalTree(p, syscall.SIGCONT)
		}
	}
	s.status = "running"
	s.lastActive = time.Now()
}

func (f *Fake) pause(s *sprite) {
	if s.status != "running" {
		return
	}
	s.status, s.warmSince = "warm", time.Now()
	f.pauses[s.name]++
	for _, p := range s.procs {
		signalTree(p, syscall.SIGSTOP)
	}
}

func (f *Fake) killAll(s *sprite) {
	for _, p := range s.procs {
		signalTree(p, syscall.SIGKILL)
	}
}

func (f *Fake) cutAll(s *sprite) {
	cut := s.cut
	s.cut = nil
	for _, c := range cut {
		c()
	}
}

func (f *Fake) coldDrop(s *sprite) {
	// cut first: a cold drop ends every session without an exit status, and a process killed
	// first could be reaped and its 137 sent before the cut on a slow machine
	f.cutAll(s)
	f.killAll(s)
	s.status = "cold"
	s.tasks = map[string]task{}
}

func (f *Fake) active(s *sprite, now time.Time) bool {
	if s.sessions > 0 && !f.o.SessionsDontHold {
		return true
	}
	for _, t := range s.tasks {
		if t.expires.After(now) {
			return true
		}
	}
	return false
}

func (f *Fake) janitor() {
	defer f.wg.Done()
	tick := min(f.o.IdleAfter/5, 100*time.Millisecond)
	tick = max(tick, 5*time.Millisecond)
	tk := time.NewTicker(tick)
	defer tk.Stop()
	lastCk := time.Now()
	for {
		select {
		case <-f.quit:
			return
		case now := <-tk.C:
			f.mu.Lock()
			for _, s := range f.sprites {
				if s.static {
					continue
				}
				switch {
				case s.status == "running" && f.active(s, now):
					s.lastActive = now
				case s.status == "running" && now.Sub(s.lastActive) >= f.o.IdleAfter:
					f.pause(s)
				case s.status == "warm" && f.o.ColdAfter > 0 && now.Sub(s.warmSince) >= f.o.ColdAfter:
					f.coldDrop(s)
				}
				if f.o.AutoCheckpointEvery > 0 && now.Sub(lastCk) >= f.o.AutoCheckpointEvery {
					f.autoCheckpoint(s)
				}
			}
			if f.o.AutoCheckpointEvery > 0 && now.Sub(lastCk) >= f.o.AutoCheckpointEvery {
				lastCk = now
			}
			f.mu.Unlock()
		}
	}
}

// autoCheckpoint copies the overlay without the tmpfs /run (probe 9.5: tmpfs is assumed kept out
// of checkpoints; the worst case for secrecy would be the opposite, which SP10 also tests by the
// key never being outside /run).
func (f *Fake) autoCheckpoint(s *sprite) string {
	if s.static {
		return ""
	}
	s.nckpt++
	dst := filepath.Join(s.dir, "checkpoints", "auto-"+strconv.Itoa(s.nckpt))
	_ = copyTree(s.overlay, dst, func(rel string) bool {
		return rel == "run" || strings.HasPrefix(rel, "run/") || rel == ".sprite/api.sock"
	})
	s.ckpts = append(s.ckpts, dst)
	return dst
}

func copyTree(src, dst string, skip func(rel string) bool) error {
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(src, p)
		if rel == "." {
			return nil
		}
		if skip(rel) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		to := filepath.Join(dst, rel)
		switch {
		case d.IsDir():
			return os.MkdirAll(to, 0o755)
		case d.Type()&fs.ModeSymlink != 0:
			if l, err := os.Readlink(p); err == nil {
				_ = os.Symlink(l, to)
			}
		case d.Type()&fs.ModeSocket != 0:
			// sockets are not checkpointed
		case d.Type().IsRegular():
			in, err := os.Open(p)
			if err != nil {
				return nil
			}
			defer in.Close()
			out, err := os.OpenFile(to, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o644)
			if err != nil {
				return nil
			}
			defer out.Close()
			_, _ = io.Copy(out, in)
		}
		return nil
	})
}

// signalTree signals the process, its process group and every descendant found in /proc.
func signalTree(p *proc, sig syscall.Signal) {
	if p.cmd.Process == nil {
		return
	}
	pid := p.cmd.Process.Pid
	for _, d := range descendants(pid) {
		_ = syscall.Kill(d, sig)
	}
	_ = syscall.Kill(-pid, sig)
	_ = syscall.Kill(pid, sig)
}

func descendants(root int) []int {
	ents, _ := os.ReadDir("/proc")
	children := map[int][]int{}
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		b, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue
		}
		// pid (comm) state ppid ...: comm may hold spaces and parentheses
		i := bytes.LastIndexByte(b, ')')
		if i < 0 {
			continue
		}
		fields := strings.Fields(string(b[i+1:]))
		if len(fields) < 2 {
			continue
		}
		ppid, _ := strconv.Atoi(fields[1])
		children[ppid] = append(children[ppid], pid)
	}
	var out []int
	stack := []int{root}
	for len(stack) > 0 {
		p := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		for _, c := range children[p] {
			out = append(out, c)
			stack = append(stack, c)
		}
	}
	return out
}

// ---------------------------------------------------------------------------------------------
// REST

func (f *Fake) serve(w http.ResponseWriter, r *http.Request) {
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		w.WriteHeader(403)
		return
	}
	var body []byte
	if r.Body != nil && r.Method != "GET" {
		body, _ = io.ReadAll(io.LimitReader(r.Body, 1<<20))
	}
	f.mu.Lock()
	f.calls = append(f.calls, Call{Method: r.Method, Path: r.URL.Path, RawQuery: r.URL.RawQuery, Auth: r.Header.Get("Authorization"), Body: body})
	unauth := r.Header.Get("Authorization") != "Bearer "+f.o.Token || f.take("401")
	f.mu.Unlock()
	if unauth {
		jsonErr(w, 401, "unauthorized", "bad or missing token")
		return
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if r.URL.Path == "/v1/oauth/connections" && r.Method == "GET" {
		f.mu.Lock()
		items := []map[string]string{}
		for i, n := range f.conns {
			items = append(items, map[string]string{"id": fmt.Sprintf("conn%d", i+1), "name": n})
		}
		f.mu.Unlock()
		writeJSON(w, 200, map[string]any{"connections": items})
		return
	}
	if r.URL.Path == "/v1/organization" && r.Method == "GET" {
		writeJSON(w, 200, map[string]any{"slug": f.o.Org, "name": f.o.Org})
		return
	}
	if len(parts) < 2 || parts[0] != "v1" || parts[1] != "sprites" {
		jsonErr(w, 404, "not_found", "no such route")
		return
	}
	switch {
	case len(parts) == 2 && r.Method == "POST":
		f.create(w, body)
	case len(parts) == 2 && r.Method == "GET":
		f.list(w, r)
	case len(parts) == 3 && (r.Method == "GET" || r.Method == "DELETE"):
		f.one(w, r.Method, parts[2])
	case len(parts) == 5 && parts[3] == "policy" && (r.Method == "GET" || r.Method == "POST"):
		f.policy(w, r.Method, parts[2], parts[4], body)
	case len(parts) == 4 && parts[3] == "checkpoints" && r.Method == "GET":
		f.checkpoints(w, parts[2])
	case len(parts) == 4 && parts[3] == "checkpoint" && r.Method == "POST":
		f.ckptCreate(w, parts[2])
	case len(parts) == 5 && parts[3] == "checkpoints" && (r.Method == "GET" || r.Method == "DELETE"):
		f.ckptOne(w, r.Method, parts[2], parts[4])
	case len(parts) == 6 && parts[3] == "checkpoints" && parts[5] == "restore" && r.Method == "POST":
		f.ckptRestore(w, parts[2], parts[4])
	case len(parts) == 5 && parts[3] == "fs" && parts[4] == "read" && r.Method == "GET",
		len(parts) == 5 && parts[3] == "fs" && parts[4] == "write" && r.Method == "PUT":
		f.fsFile(w, r, parts[2], parts[4], body)
	case len(parts) == 4 && parts[3] == "exec" && r.Method == "GET":
		f.execWS(w, r, parts[2])
	default:
		jsonErr(w, 404, "not_found", "no such route")
	}
}

func jsonErr(w http.ResponseWriter, code int, c, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": c, "message": msg})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// strict decodes body into v and refuses an unknown field or trailing data.
func strict(body []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return fmt.Errorf("trailing data")
	}
	return nil
}

func (f *Fake) view(s *sprite) map[string]any {
	org := s.org
	if f.once["wrong-org"] != 0 {
		org = "other-org"
	}
	m := map[string]any{"id": s.id, "name": s.name, "status": s.status, "created_at": s.created.Format(time.RFC3339Nano)}
	if len(s.labels) > 0 {
		m["labels"] = s.labels
	}
	if f.once["no-org"] == 0 {
		m["organization"] = org
	}
	return m
}

func (f *Fake) create(w http.ResponseWriter, body []byte) {
	var req struct {
		Name   string            `json:"name"`
		Labels map[string]string `json:"labels"`
	}
	f.mu.Lock()
	switch {
	case f.take("429-create"):
		f.mu.Unlock()
		jsonErr(w, 429, "sprite_creation_rate_limited", "too many sprites created")
		return
	case f.take("concurrent-limit"):
		f.mu.Unlock()
		jsonErr(w, 403, "concurrent_sprite_limit_exceeded", "too many running sprites")
		return
	case f.take("max-sprites-exceeded"):
		f.mu.Unlock()
		jsonErr(w, 400, "bad_request", "max sprites per org exceeded")
		return
	case f.take("403-restricted"):
		f.mu.Unlock()
		jsonErr(w, 403, "forbidden", "Your account is restricted")
		return
	case f.take("create-500-nothing"):
		f.mu.Unlock()
		jsonErr(w, 500, "internal", "try again")
		return
	}
	labelsOff := f.once["labels-unsupported"] != 0
	lax := f.once["lax-names"] != 0
	beforeCreate := f.beforeCreate
	f.mu.Unlock()
	if err := strict(body, &req); err != nil {
		jsonErr(w, 400, "bad_request", "invalid body: "+firstLine(err.Error()))
		return
	}
	if labelsOff && len(req.Labels) > 0 {
		f.mu.Lock()
		f.take("labels-unsupported")
		f.mu.Unlock()
		jsonErr(w, 400, "bad_request", "unknown field labels")
		return
	}
	if !lax && !nameRE.MatchString(req.Name) {
		jsonErr(w, 400, "bad_request", "invalid sprite name")
		return
	}
	if beforeCreate != nil {
		beforeCreate()
	}
	f.mu.Lock()
	if f.closed {
		// close has swept the Sprites: a new one would start a server nobody stops or waits for
		f.mu.Unlock()
		jsonErr(w, 503, "unavailable", "the fake is closing")
		return
	}
	if _, ok := f.sprites[req.Name]; ok {
		f.mu.Unlock()
		jsonErr(w, 409, "conflict", "sprite name already exists")
		return
	}
	f.seq++
	s := &sprite{id: fmt.Sprintf("sp%d", f.seq), name: req.Name, org: f.o.Org, created: time.Now().UTC(), status: "running",
		labels: req.Labels, tasks: map[string]task{}, lastActive: time.Now()}
	s.dir = filepath.Join(f.root, s.name)
	s.overlay = filepath.Join(s.dir, "overlay")
	if err := os.MkdirAll(filepath.Join(s.overlay, ".sprite"), 0o755); err != nil {
		f.mu.Unlock()
		jsonErr(w, 500, "internal", "fake disk")
		return
	}
	f.startSock(s)
	f.sprites[s.name] = s
	if f.take("create-then-500") {
		f.mu.Unlock()
		jsonErr(w, 500, "internal", "try again")
		return
	}
	if f.take("create-then-500-lookup-fails") {
		f.once["get-500"], f.once["list-500"] = 1, 1
		f.mu.Unlock()
		jsonErr(w, 500, "internal", "try again")
		return
	}
	v := f.view(s)
	f.mu.Unlock()
	writeJSON(w, 201, v)
}

func firstLine(s string) string { s, _, _ = strings.Cut(s, "\n"); return s }

func (f *Fake) list(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	for k := range q {
		if k != "prefix" && k != "max_results" && k != "continuation_token" {
			jsonErr(w, 400, "bad_request", "unknown query parameter "+k)
			return
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.take("list-500") {
		jsonErr(w, 500, "internal", "try again")
		return
	}
	page := f.o.PageSize
	if m := q.Get("max_results"); m != "" {
		n, err := strconv.Atoi(m)
		if err != nil || n < 1 {
			jsonErr(w, 400, "bad_request", "bad max_results")
			return
		}
		page = min(page, n)
	}
	after := ""
	if t := q.Get("continuation_token"); t != "" {
		b, err := base64.RawURLEncoding.DecodeString(t)
		if err != nil {
			jsonErr(w, 400, "bad_request", "bad continuation_token")
			return
		}
		after = string(b)
	}
	var names []string
	for n := range f.sprites {
		if strings.HasPrefix(n, q.Get("prefix")) && n > after {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	out := map[string]any{"has_more": false}
	if len(names) > page {
		names = names[:page]
		out["has_more"] = true
		out["next_continuation_token"] = base64.RawURLEncoding.EncodeToString([]byte(names[len(names)-1]))
	}
	items := []map[string]any{}
	for _, n := range names {
		items = append(items, f.view(f.sprites[n]))
	}
	out["sprites"] = items
	writeJSON(w, 200, out)
}

func (f *Fake) one(w http.ResponseWriter, method, name string) {
	f.mu.Lock()
	if method == "GET" && f.take("get-500") {
		f.mu.Unlock()
		jsonErr(w, 500, "internal", "try again")
		return
	}
	if method == "DELETE" && f.take("delete-500") {
		f.mu.Unlock()
		jsonErr(w, 500, "internal", "try again")
		return
	}
	s := f.sprites[name]
	if s == nil {
		f.mu.Unlock()
		jsonErr(w, 404, "not_found", "no such sprite")
		return
	}
	if method == "GET" {
		v := f.view(s)
		f.mu.Unlock()
		writeJSON(w, 200, v)
		return
	}
	f.killAll(s)
	f.cutAll(s)
	delete(f.sprites, name)
	if f.take("checkpoints-survive-delete") {
		for _, c := range s.ckpts {
			f.survivors[name] = append(f.survivors[name], filepath.Base(c))
		}
	}
	sock := s.sock
	f.mu.Unlock()
	if sock != nil {
		sock.Close()
	}
	if s.sockDir != "" {
		_ = os.RemoveAll(s.sockDir)
	}
	if s.dir != "" {
		_ = os.RemoveAll(s.dir)
	}
	w.WriteHeader(204)
}

type netRule struct {
	Domain string `json:"domain"`
	Action string `json:"action"`
}
type netPolicy struct {
	Rules []netRule `json:"rules"`
}
type resPolicy struct {
	Memory *struct {
		LimitMB   int  `json:"limit_mb"`
		Autoscale bool `json:"autoscale"`
	} `json:"memory"`
}

var fakeDomainRE = regexp.MustCompile(`^(\*\.)?([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]([a-z0-9-]{0,61}[a-z0-9])?$`)

func (f *Fake) policy(w http.ResponseWriter, method, name, kind string, body []byte) {
	if kind != "network" && kind != "resources" {
		jsonErr(w, 404, "not_found", "no such policy")
		return
	}
	f.mu.Lock()
	s := f.sprites[name]
	if s == nil {
		f.mu.Unlock()
		jsonErr(w, 404, "not_found", "no such sprite")
		return
	}
	if method == "POST" && f.take("policy-500") {
		f.mu.Unlock()
		jsonErr(w, 500, "internal", "try again")
		return
	}
	if method == "GET" {
		cur := s.netPolicy
		empty := json.RawMessage(`{"rules":[]}`)
		if kind == "resources" {
			cur, empty = s.resources, json.RawMessage(`{}`)
		}
		if cur == nil || f.take("policy-get-mismatch") {
			f.mu.Unlock()
			writeJSON(w, 200, empty)
			return
		}
		raw := *cur
		f.mu.Unlock()
		writeJSON(w, 200, raw)
		return
	}
	f.mu.Unlock()
	var stored json.RawMessage
	switch kind {
	case "network":
		var p netPolicy
		if err := strict(body, &p); err != nil || p.Rules == nil {
			jsonErr(w, 400, "bad_request", "invalid network policy")
			return
		}
		for _, r := range p.Rules {
			if r.Action != "allow" || !fakeDomainRE.MatchString(r.Domain) {
				jsonErr(w, 400, "bad_request", "invalid rule: explicit allow domains only")
				return
			}
		}
		stored, _ = json.Marshal(p)
	case "resources":
		var p resPolicy
		if err := strict(body, &p); err != nil || p.Memory == nil || p.Memory.LimitMB <= 0 || p.Memory.Autoscale {
			jsonErr(w, 400, "bad_request", "invalid resources policy")
			return
		}
		stored, _ = json.Marshal(p)
	}
	f.mu.Lock()
	if s2 := f.sprites[name]; s2 != nil {
		if kind == "network" {
			s2.netPolicy = &stored
		} else {
			s2.resources = &stored
		}
	}
	f.mu.Unlock()
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func ckptView(id string) map[string]any {
	return map[string]any{"id": id, "auto": strings.HasPrefix(id, "auto-"), "created_at": time.Now().UTC().Format(time.RFC3339)}
}

func (f *Fake) checkpoints(w http.ResponseWriter, name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.sprites[name]
	var ids []string
	switch {
	case s != nil:
		for _, c := range s.ckpts {
			ids = append(ids, filepath.Base(c))
		}
	case len(f.survivors[name]) > 0:
		ids = f.survivors[name]
	default:
		jsonErr(w, 404, "not_found", "no such sprite")
		return
	}
	items := []map[string]any{}
	for _, id := range ids {
		items = append(items, ckptView(id))
	}
	writeJSON(w, 200, map[string]any{"checkpoints": items})
}

// ckptCreate takes a manual checkpoint (a copy of the overlay without the tmpfs /run).
func (f *Fake) ckptCreate(w http.ResponseWriter, name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.sprites[name]
	if s == nil || s.static {
		jsonErr(w, 404, "not_found", "no such sprite")
		return
	}
	s.ncp++
	id := "cp-" + strconv.Itoa(s.ncp)
	dst := filepath.Join(s.dir, "checkpoints", id)
	_ = copyTree(s.overlay, dst, func(rel string) bool {
		return rel == "run" || strings.HasPrefix(rel, "run/") || rel == ".sprite/api.sock"
	})
	s.ckpts = append(s.ckpts, dst)
	writeJSON(w, 200, ckptView(id))
}

func (f *Fake) ckptIndex(s *sprite, id string) int {
	for i, c := range s.ckpts {
		if filepath.Base(c) == id {
			return i
		}
	}
	return -1
}

func (f *Fake) ckptOne(w http.ResponseWriter, method, name, id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.sprites[name]
	if s == nil {
		for _, k := range f.survivors[name] {
			if k == id && method == "GET" {
				writeJSON(w, 200, ckptView(id))
				return
			}
		}
		jsonErr(w, 404, "not_found", "no such sprite")
		return
	}
	i := f.ckptIndex(s, id)
	if i < 0 {
		jsonErr(w, 404, "not_found", "no such checkpoint")
		return
	}
	if method == "GET" {
		writeJSON(w, 200, ckptView(id))
		return
	}
	_ = os.RemoveAll(s.ckpts[i])
	s.ckpts = append(s.ckpts[:i], s.ckpts[i+1:]...)
	w.WriteHeader(204)
}

// ckptRestore puts the checkpoint's files back: the overlay is emptied (but for /.sprite) and the
// checkpoint copied in. The policies are kept (the model of probe 9.2's best case).
func (f *Fake) ckptRestore(w http.ResponseWriter, name, id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.sprites[name]
	if s == nil || s.static {
		jsonErr(w, 404, "not_found", "no such sprite")
		return
	}
	i := f.ckptIndex(s, id)
	if i < 0 {
		jsonErr(w, 404, "not_found", "no such checkpoint")
		return
	}
	ents, _ := os.ReadDir(s.overlay)
	for _, e := range ents {
		if e.Name() != ".sprite" {
			_ = os.RemoveAll(filepath.Join(s.overlay, e.Name()))
		}
	}
	_ = copyTree(s.ckpts[i], s.overlay, func(string) bool { return false })
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// fsFile serves fs/read (GET) and fs/write (PUT) on the Sprite's overlay. The path is absolute
// and cleaned against the overlay's root, so it cannot leave it.
func (f *Fake) fsFile(w http.ResponseWriter, r *http.Request, name, verb string, body []byte) {
	q := r.URL.Query()
	for k := range q {
		if k != "path" && (verb != "write" || k != "mkdirParents") {
			jsonErr(w, 400, "bad_request", "unknown query parameter "+k)
			return
		}
	}
	p := q.Get("path")
	if !strings.HasPrefix(p, "/") {
		jsonErr(w, 400, "bad_request", "path must be absolute")
		return
	}
	f.mu.Lock()
	s := f.sprites[name]
	f.mu.Unlock()
	if s == nil || s.static {
		jsonErr(w, 404, "not_found", "no such sprite")
		return
	}
	full := filepath.Join(s.overlay, filepath.Clean("/"+p))
	if verb == "read" {
		b, err := os.ReadFile(full)
		if err != nil {
			jsonErr(w, 404, "not_found", "no such file")
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(b)
		return
	}
	if q.Get("mkdirParents") == "true" {
		_ = os.MkdirAll(filepath.Dir(full), 0o755)
	}
	if err := os.WriteFile(full, body, 0o644); err != nil {
		jsonErr(w, 404, "not_found", "no such directory")
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// ---------------------------------------------------------------------------------------------
// api.sock: the Tasks API (assumed shape: POST /v1/tasks {"expire":"5m"} -> {"id"}, POST
// /v1/tasks/{id} {"expire"} renews, DELETE /v1/tasks/{id}, GET /v1/tasks)

func (f *Fake) startSock(s *sprite) {
	dir, err := os.MkdirTemp("", "sps")
	if err != nil {
		return
	}
	real := filepath.Join(dir, "api.sock")
	l, err := net.Listen("unix", real)
	if err != nil {
		_ = os.RemoveAll(dir)
		return
	}
	_ = os.Symlink(real, filepath.Join(s.overlay, ".sprite", "api.sock"))
	s.sock, s.sockDir = l, dir
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/tasks", func(w http.ResponseWriter, r *http.Request) { f.tasks(w, r, s, "") })
	mux.HandleFunc("/v1/tasks/", func(w http.ResponseWriter, r *http.Request) {
		f.tasks(w, r, s, strings.TrimPrefix(r.URL.Path, "/v1/tasks/"))
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	f.wg.Add(1)
	go func() { defer f.wg.Done(); _ = srv.Serve(l) }()
}

func (f *Fake) tasks(w http.ResponseWriter, r *http.Request, s *sprite, id string) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sprites[s.name] != s {
		jsonErr(w, 404, "not_found", "gone")
		return
	}
	switch {
	case id == "" && r.Method == "GET":
		ids := []string{}
		for k := range s.tasks {
			ids = append(ids, k)
		}
		sort.Strings(ids)
		writeJSON(w, 200, map[string]any{"tasks": ids})
	case (id == "" && r.Method == "POST") || (id != "" && r.Method == "POST"):
		var req struct {
			Expire string `json:"expire"`
		}
		var d time.Duration
		err := strict(body, &req)
		if err == nil {
			d, err = time.ParseDuration(req.Expire)
		}
		if err != nil || d <= 0 {
			jsonErr(w, 400, "bad_request", "invalid task")
			return
		}
		creating := id == ""
		if creating {
			f.seq++
			id = "t" + strconv.Itoa(f.seq)
		} else if _, ok := s.tasks[id]; !ok {
			jsonErr(w, 404, "not_found", "no such task")
			return
		}
		verb := "renew "
		if creating {
			verb = "create "
		}
		f.taskLog[s.name] = append(f.taskLog[s.name], verb+req.Expire)
		s.tasks[id] = task{expires: time.Now().Add(d)}
		writeJSON(w, 200, map[string]string{"id": id})
	case id != "" && r.Method == "DELETE":
		f.taskLog[s.name] = append(f.taskLog[s.name], "delete")
		delete(s.tasks, id)
		w.WriteHeader(204)
	default:
		jsonErr(w, 404, "not_found", "no such route")
	}
}

// ---------------------------------------------------------------------------------------------
// exec

var (
	shaCheckRE = regexp.MustCompile(`^echo "([0-9a-f]{64})  /opt/bitc/bin/bitc" \| sha256sum -c - >/dev/null && exec (sudo -n )?/opt/bitc/bin/bitc __agent ([A-Za-z0-9._ -]+)$`)
	execKeys   = map[string]bool{"cmd": true, "path": true, "dir": true, "stdin": true, "tty": true, "max_run_after_disconnect": true, "env": true}
)

const (
	chStdin  = 0x00
	chStdout = 0x01
	chStderr = 0x02
	chExit   = 0x03
	chEOF    = 0x04
)

func (f *Fake) execWS(w http.ResponseWriter, r *http.Request, name string) {
	q := r.URL.Query()
	for k := range q {
		if !execKeys[k] {
			jsonErr(w, 400, "bad_request", "unknown query parameter "+k)
			return
		}
	}
	cmd := q["cmd"]
	if len(cmd) == 0 || (q.Get("path") != "" && q.Get("path") != cmd[0]) {
		jsonErr(w, 400, "bad_request", "cmd is required and path must be its first element")
		return
	}
	var maxRun time.Duration
	if v := q.Get("max_run_after_disconnect"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			jsonErr(w, 400, "bad_request", "bad max_run_after_disconnect")
			return
		}
		maxRun = d
	}
	isAttach := len(cmd) == 3 && strings.HasSuffix(cmd[2], "__agent --boot-stdin")
	isTail := len(cmd) == 3 && strings.Contains(cmd[2], "__agent tail ")
	f.mu.Lock()
	s := f.sprites[name]
	switch {
	case s == nil || s.static:
		f.mu.Unlock()
		jsonErr(w, 404, "not_found", "no such sprite")
		return
	case isAttach && f.take("attach-4xx"):
		f.mu.Unlock()
		jsonErr(w, 400, "bad_request", "rejected")
		return
	case isAttach && f.take("attach-timeout"):
		f.mu.Unlock()
		select {
		case <-r.Context().Done():
		case <-f.quit:
		}
		return
	case f.take("exec-upgrade-4xx"):
		f.mu.Unlock()
		jsonErr(w, 400, "bad_request", "rejected")
		return
	case f.take("exec-upgrade-5xx"):
		f.mu.Unlock()
		jsonErr(w, 503, "unavailable", "try again")
		return
	case f.take("exec-upgrade-timeout"):
		f.mu.Unlock()
		select {
		case <-r.Context().Done():
		case <-f.quit:
		}
		return
	}
	dropSocket := f.take("drop-exec-socket") || (isTail && f.take("drop-tail"))
	coldMid := isAttach && f.take("cold-drop-mid-job")
	truncate := isAttach && f.take("attach-truncate")
	f.wake(s)
	rec := &ExecCall{Sprite: name, Query: q, MaxRun: maxRun}
	f.execs = append(f.execs, rec)
	beforeJoin := f.beforeExecJoin
	f.mu.Unlock()

	c, err := wsmini.Upgrade(w, r, 0)
	if err != nil {
		return
	}
	if dropSocket {
		_ = c.Abort()
		return
	}
	if beforeJoin != nil {
		beforeJoin()
	}
	// close waits on wg and cuts the connections in s.cut: a connection that joins must be in both
	// before close can look, and one that comes once it has begun must not join.
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		_ = c.Abort()
		return
	}
	f.wg.Add(1)
	s.sessions++
	s.cut = append(s.cut, func() { _ = c.Abort() })
	f.mu.Unlock()
	defer f.wg.Done()
	f.runExec(c, s, rec, cmd, maxRun, coldMid, truncate)
}

func (f *Fake) runExec(c *wsmini.Conn, s *sprite, rec *ExecCall, cmd []string, maxRun time.Duration, coldMid, truncate bool) {
	ended := false
	end := func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		if !ended {
			ended = true
			s.sessions--
			s.lastActive = time.Now()
		}
	}
	defer end()
	defer func() { _ = c.Abort() }()

	send := func(ch byte, p []byte) error {
		for len(p) > 0 || ch == chExit {
			k := min(len(p), 32<<10)
			msg := append([]byte{ch}, p[:k]...)
			if err := c.WriteMessage(wsmini.OpBinary, msg); err != nil {
				return err
			}
			p = p[k:]
			if ch == chExit {
				break
			}
		}
		return nil
	}

	// stdin: messages until the process exists, then forwarded
	type start struct {
		argv      []string
		bootStdin bool // the stdin is one boot payload frame (attach) or one ProvisionSpec frame
		provision bool
		hook      bool
	}
	var st start
	if len(cmd) == 3 && cmd[0] == "sh" && cmd[1] == "-c" {
		if m := shaCheckRE.FindStringSubmatch(cmd[2]); m != nil {
			if f.o.AgentSHA != "" && m[1] != f.o.AgentSHA {
				_ = send(chStderr, []byte("sha256sum: /opt/bitc/bin/bitc: FAILED\n"))
				_ = send(chExit, []byte{1})
				_ = c.Close()
				return
			}
			args := strings.Fields(m[3])
			switch args[0] {
			case "read", "tail", "list", "teardown":
				args = append([]string{"--root", filepath.Join(s.overlay, "run")}, args...)
			}
			st.argv = append([]string{"__agent"}, args...)
			st.provision = len(args) == 1 && args[0] == "provision"
			st.bootStdin = st.provision || (len(args) == 1 && args[0] == "--boot-stdin")
		}
	}
	if st.provision {
		f.mu.Lock()
		infra, repo := f.take("provision-infra-fails"), f.take("provision-repo-fails")
		f.mu.Unlock()
		if infra || repo {
			line, code := "setup step packages failed: simulated package install failure", byte(75)
			if repo {
				line, code = "setup step tl failed: simulated tool pin mismatch", 71
			}
			_ = send(chStdout, []byte("provision: installing 0 packages and 0 tools\n"+line+"\n"))
			_ = send(chExit, []byte{code})
			_ = c.Close()
			return
		}
	}
	if st.argv == nil {
		st.hook = true
	}

	var (
		inR, inW = io.Pipe()
		stdinBuf bytes.Buffer
		startOne sync.Once
		p        *proc // set by runAgent under f.mu
		exited   = make(chan int, 1)
	)
	getP := func() *proc {
		f.mu.Lock()
		defer f.mu.Unlock()
		return p
	}
	launch := func(first []byte) {
		startOne.Do(func() {
			go func() {
				if st.hook {
					f.runHook(s, cmd, inR, send, exited)
					return
				}
				f.runAgent(s, st.argv, first, inR, send, exited, &p, coldMid, st.bootStdin)
			}()
		})
	}
	if !st.bootStdin {
		launch(nil)
	}
	// read loop
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		defer inW.Close()
		for {
			op, m, err := c.ReadMessage()
			if err != nil {
				return
			}
			if op != wsmini.OpBinary || len(m) == 0 {
				return
			}
			switch m[0] {
			case chStdin:
				f.mu.Lock()
				rec.Stdin = append(rec.Stdin, m[1:]...)
				f.mu.Unlock()
				if st.bootStdin && getP() == nil {
					stdinBuf.Write(m[1:])
					if payload, ok := completeFrame(stdinBuf.Bytes()); ok {
						if truncate {
							payload = payload[:len(payload)/2]
						} else {
							payload = f.rewriteBoot(s, payload, st.provision)
						}
						launch(payload)
						stdinBuf.Reset()
					}
					continue
				}
				if _, err := inW.Write(m[1:]); err != nil {
					return
				}
			case chEOF:
				if st.bootStdin && getP() == nil {
					launch(stdinBuf.Bytes()) // a truncated payload: the agent refuses it
				}
				inW.Close()
			default:
				return
			}
		}
	}()
	select {
	case code := <-exited:
		_ = send(chExit, []byte{byte(code)})
		_ = c.Close()
	case <-readDone:
		// disconnected: the process lives on for maxRun only
		cur := getP()
		if cur != nil {
			f.wg.Add(1)
			go func() {
				defer f.wg.Done()
				t := time.NewTimer(maxRun)
				defer t.Stop()
				select {
				case <-cur.done:
				case <-t.C:
					f.mu.Lock()
					signalTree(cur, syscall.SIGKILL)
					f.mu.Unlock()
				case <-f.quit:
				}
			}()
		}
	}
	// a process that never started, or an exec without max_run: killed at disconnect
	if maxRun == 0 {
		if cur := getP(); cur != nil {
			f.mu.Lock()
			signalTree(cur, syscall.SIGKILL)
			f.mu.Unlock()
		}
	}
}

// completeFrame returns the whole length-prefixed frame at the start of b when it is complete.
func completeFrame(b []byte) ([]byte, bool) {
	if len(b) < 8 {
		return nil, false
	}
	n := binary.BigEndian.Uint64(b[:8])
	if n > agent.MaxBootPayload {
		return b, true // the agent refuses it
	}
	if uint64(len(b)) < 8+n {
		return nil, false
	}
	return b[:8+n], true
}

// rewriteBoot points the job's spec at the Sprite's overlay (test mode: an unprivileged agent
// with TestRoot = the overlay), as runpodfake does for a pod.
func (f *Fake) rewriteBoot(s *sprite, frame []byte, provision bool) []byte {
	if len(frame) < 8 {
		return frame
	}
	if provision {
		var ps agent.ProvisionSpec
		if json.Unmarshal(frame[8:], &ps) != nil {
			return frame
		}
		ps.Unprivileged, ps.TestRoot = true, s.overlay
		if out, err := ps.Frame(); err == nil {
			return out
		}
		return frame
	}
	var p agent.BootPayload
	if json.Unmarshal(frame[8:], &p) != nil {
		return frame
	}
	spec, err := agent.DecodeSpec(p.Spec)
	if err != nil {
		return frame
	}
	spec.Unprivileged, spec.TestRoot = true, s.overlay
	enc, err := spec.Encode()
	if err != nil {
		return frame
	}
	p.Spec = enc
	out, err := p.Frame()
	if err != nil {
		return frame
	}
	return out
}

func (f *Fake) runHook(s *sprite, cmd []string, in io.Reader, send func(byte, []byte) error, exited chan<- int) {
	if f.o.Exec == nil {
		_ = send(chStderr, []byte("fake: command not available\n"))
		exited <- 127
		return
	}
	code := f.o.Exec(ExecContext{Sprite: s.name, Overlay: s.overlay, Cmd: cmd, Stdin: in,
		Stdout: chanWriter{send, chStdout}, Stderr: chanWriter{send, chStderr}})
	exited <- code
}

type chanWriter struct {
	send func(byte, []byte) error
	ch   byte
}

func (w chanWriter) Write(p []byte) (int, error) {
	if err := w.send(w.ch, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

// forgeResults rewrites every result.json under the Sprite's run directory with exit 0 and a MAC
// made with another key, before the host can be told the job ended.
func forgeResults(s *sprite) {
	matches, _ := filepath.Glob(filepath.Join(s.overlay, "run", "*", "result.json"))
	for _, m := range matches {
		b, _ := json.Marshal(agent.Sign([]byte("a forged key a forged key a forg"), agent.ResultFile{Exit: 0}))
		_ = os.WriteFile(m, b, 0o600)
	}
}

func (f *Fake) runAgent(s *sprite, argv []string, first []byte, in io.Reader, send func(byte, []byte) error, exited chan<- int, pp **proc, coldMid, bootStdin bool) {
	cmd := exec.Command(f.o.Bin, argv...)
	if f.o.Bin == "" {
		_ = send(chStderr, []byte("fake: no agent binary (Options.Bin)\n"))
		exited <- 127
		return
	}
	cmd.Dir = s.overlay
	cmd.Env = os.Environ()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdinW, err := cmd.StdinPipe()
	if err != nil {
		exited <- 126
		return
	}
	outR, outW, _ := os.Pipe()
	errR, errW, _ := os.Pipe()
	cmd.Stdout, cmd.Stderr = outW, errW
	if err := cmd.Start(); err != nil {
		outW.Close()
		errW.Close()
		outR.Close()
		errR.Close()
		_ = send(chStderr, []byte("fake: cannot start the agent\n"))
		exited <- 126
		return
	}
	outW.Close()
	errW.Close()
	pr := &proc{cmd: cmd, done: make(chan struct{})}
	f.mu.Lock()
	s.procs = append(s.procs, pr)
	*pp = pr
	beforeJoin := f.beforeAgentJoin
	f.mu.Unlock()
	if beforeJoin != nil {
		beforeJoin()
	}
	go func() {
		if len(first) > 0 {
			_, _ = stdinW.Write(first)
		}
		_, _ = io.Copy(stdinW, in)
		_ = stdinW.Close()
	}()
	var pumps sync.WaitGroup
	pump := func(r *os.File, ch byte) {
		defer pumps.Done()
		buf := make([]byte, 32<<10)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				if coldMid && ch == chStdout && bootStdin {
					f.mu.Lock()
					f.coldDrop(s)
					f.mu.Unlock()
					return
				}
				if ch == chStdout && bytes.Contains(buf[:n], []byte(agent.MarkExit+" ")) {
					f.mu.Lock()
					forged := f.take("forge-result")
					f.mu.Unlock()
					if forged {
						forgeResults(s)
					}
				}
				_ = send(ch, buf[:n])
			}
			if err != nil {
				return
			}
		}
	}
	pumps.Add(2)
	go pump(outR, chStdout)
	go pump(errR, chStderr)
	f.mu.Lock()
	closed := f.closed
	if !closed {
		f.wg.Add(1)
	} else {
		// close has begun and already swept the processes: end this one here, nobody waits for it
		signalTree(pr, syscall.SIGKILL)
	}
	f.mu.Unlock()
	if closed {
		_ = cmd.Wait()
		close(pr.done)
		outR.Close()
		errR.Close()
		select {
		case exited <- 137:
		default:
		}
		return
	}
	go func() {
		defer f.wg.Done()
		err := cmd.Wait()
		close(pr.done)
		code := 0
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
			if code < 0 {
				code = 137
			}
		}
		// The agent keeps a detached copy of stdout while the job runs, so the pipes reach EOF
		// when it is gone, after everything it wrote. A stray holder is cut after 2s.
		drained := make(chan struct{})
		go func() { pumps.Wait(); close(drained) }()
		select {
		case <-drained:
		case <-time.After(2 * time.Second):
			outR.Close()
			errR.Close()
			<-drained
		}
		select {
		case exited <- code:
		default:
		}
	}()
}

// PauseCount is how many times the Sprite was paused (kept after it is deleted).
func (f *Fake) PauseCount(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pauses[name]
}

// TaskLog is the Tasks API events the Sprite saw ("create 5m", "renew 5m", "delete"), kept after
// the Sprite is deleted.
func (f *Fake) TaskLog(name string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.taskLog[name]...)
}

// CutSessions cuts every live exec connection of the Sprite, as a network drop would; the
// processes run on (to the extent of their max_run_after_disconnect).
func (f *Fake) CutSessions(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s := f.sprites[name]; s != nil {
		f.cutAll(s)
	}
}
