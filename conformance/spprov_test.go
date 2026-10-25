package conformance

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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

	"github.com/Exploded-views/bitc/conformance/harness"
	"github.com/Exploded-views/bitc/internal/agent"
	"github.com/Exploded-views/bitc/internal/provider/runpod/runpodfake"
	"github.com/Exploded-views/bitc/internal/provider/sprites"
	"github.com/Exploded-views/bitc/internal/provider/sprites/spritesfake"
	"github.com/Exploded-views/bitc/internal/state"
)

// The SP family at the provider and walk level: bitc runs against spritesfake (and
// runpodfake where a row moves on to RunPod), with this binary as the agent in every Sprite.

type spOpts struct {
	runpod bool                // pool: sprites, runpod, local (else sprites, local)
	prov   string              // extra [providers.sprites] text
	limits string              // extra [remote_limits] text
	repo   string              // extra repo file text
	fake   spritesfake.Options // Bin and AgentSHA are filled in
	key    string              // a deploy key: written 0600 and granted to the repo
	start  string              // start_timeout (default 2m)
	tmo    string              // pool timeout (default 60m)
	daily  string              // daily_usd (default 10)
}

type spEnv struct {
	*fbEnv
	f     *spritesfake.Fake
	rp    *runpodfake.Fake
	token string
	key   string
}

var (
	spShaOnce sync.Once
	spShaVal  string
)

// spAgentSHA is the sha256 of the bitc binary under test: what the host sends as the agent's sha.
func spAgentSHA(t testing.TB) string {
	t.Helper()
	spShaOnce.Do(func() {
		b, err := os.ReadFile(harness.Bin)
		if err != nil {
			panic(err)
		}
		s := sha256.Sum256(b)
		spShaVal = hex.EncodeToString(s[:])
	})
	return spShaVal
}

func spRepo(t *testing.T, h *harness.H, o spOpts) *spEnv {
	t.Helper()
	o.fake.Bin, o.fake.AgentSHA = harness.Bin, spAgentSHA(t)
	if o.fake.Exec == nil { // the stub exec that installs the agent: the fake's agent is this binary already
		o.fake.Exec = func(spritesfake.ExecContext) int { return 0 }
	}
	e := &spEnv{f: spritesfake.New(t, o.fake)}
	e.token = filepath.Join(h.Root, "sprites.token")
	h.Write(e.token, e.f.Token()+"\n", 0o600)
	h.Seam("time_scale = 0.05")
	h.Seam("agent_url = \"http://127.0.0.1:9/bitc\"")
	order, sizes, extra := `"sprites", "local"`, `sprites = "cpu8"`, ""
	if o.runpod {
		e.rp = runpodfake.New(t, runpodfake.Options{Bin: harness.Bin})
		h.Seam("http_results = true")
		order, sizes = `"sprites", "runpod", "local"`, `sprites = "cpu8", runpod = "cpu8"`
		extra = "[providers.runpod]\napi_key = \"sk-test-0123456789\"\ndaily_usd = 25\nstart_timeout = \"2m\"\nimage = \"debian\"\n" +
			"api_base = " + harness.Q(e.rp.URL()) + "\nproxy_domain = " + harness.Q(e.rp.ProxyAddr()) + "\n" +
			"sizes = { cpu8 = { cpu_type = \"cpu3c\", vcpu = 8, usd_per_hour = 0.24 } }\n"
	}
	start, tmo, daily := firstOf(o.start, "2m"), firstOf(o.tmo, "60m"), firstOf(o.daily, "10")
	grant := ""
	if o.key != "" {
		e.key = filepath.Join(h.Root, "deploy.key")
		h.Write(e.key, o.key+"\n", 0o600)
		grant = "[[repos]]\nmatch = \"**/app\"\ndeploy_key_file = " + harness.Q(e.key) + "\n"
	}
	h.HostConfig("[remote_limits]\nworktrees = [\"**/app\"]\nforward_env_allow = [\"CI\", \"DEPLOY_TOKEN\"]\nforward_env_force = [\"DEPLOY_TOKEN\"]\n" + o.limits +
		"[pools.heavy]\nslots = 2\nproviders = [" + order + "]\nremote_limits = { size = { " + sizes + " }, timeout = " + harness.Q(tmo) + " }\nwait = \"5s\"\n" +
		"[providers.sprites]\napi_key_file = " + harness.Q(e.token) + "\norg = " + harness.Q(e.f.Org()) + "\ndaily_usd = " + daily + "\nmax_live = 2\n" +
		"start_timeout = " + harness.Q(start) + "\napi_base = " + harness.Q(e.f.URL()) + "\nsizes = { cpu8 = { memory = \"16GiB\" } }\n" + o.prov +
		extra + grant)
	wt := h.Repo("app", map[string]string{".bitc.toml": "version = 1\n[pools.heavy.remote]\nallow = [[\"sh\", \"*\"], [\"echo\", \"*\"]]\n" + o.repo})
	h.Cwd = wt
	e.fbEnv = &fbEnv{h: h, wt: wt, acct: filepath.Join(h.Root, "account", "remote", "sprites")}
	return e
}

func firstOf(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// created lists the Sprite names the host asked to create, in order.
func (e *spEnv) created(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, c := range e.f.Calls() {
		if c.Method == "POST" && c.Path == "/v1/sprites" {
			var b struct{ Name string }
			if json.Unmarshal(c.Body, &b) != nil || b.Name == "" {
				t.Fatalf("a create without a name: %s", c.Body)
			}
			out = append(out, b.Name)
		}
	}
	return out
}

// first is the first Sprite the host asked to create; the test stops when there is none.
func (e *spEnv) first(t *testing.T) string {
	t.Helper()
	n := e.created(t)
	if len(n) == 0 {
		t.Fatal("no Sprite was created")
	}
	return n[0]
}

// ledgerNum is a numeric field of the first ledger line of an event.
func (e *spEnv) ledgerNum(t *testing.T, event, field string) float64 {
	t.Helper()
	ls := e.ledger(t, event)
	if len(ls) == 0 {
		t.Fatalf("no ledger %s line", event)
	}
	v, _ := ls[0][field].(float64)
	return v
}

// deletes counts the DELETE requests for Sprites.
func (e *spEnv) deletes() int {
	n := 0
	for _, c := range e.f.Calls() {
		if c.Method == "DELETE" && strings.HasPrefix(c.Path, "/v1/sprites/") {
			n++
		}
	}
	return n
}

func (e *spEnv) rpCreates() int {
	if e.rp == nil {
		return 0
	}
	return calls(e.rp, "POST /v2/pods")
}

func (e *spEnv) secret(t *testing.T) []byte {
	t.Helper()
	b, err := state.InstallSecret(filepath.Join(e.h.Root, "account"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func spRun(script string) []string { return []string{"run", "heavy", "--", "sh", "-c", script} }

var spNameRE = regexp.MustCompile(`^bitc-[0-9a-f]{16}-cpu8-[0-9a-f]{8}-[0-9]+-[0-9a-f]{8}$`)

// waitFile waits for a host file (the fake Sprites are plain local processes).
func waitFile(t *testing.T, p string, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for {
		if _, err := os.Stat(p); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s did not appear within %s", p, d)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// SP1: the documented defaults, and every validation case of the design's 7 is a 78 with its message.
func TestSP1_SettingsDefaultsAndValidation(t *testing.T) {
	h := harness.New(t)
	spRepo(t, h, spOpts{})
	if r := h.Run("config", "check"); r.Code != 0 {
		t.Fatalf("a valid sprites section: %+v", r)
	}
	if got := levelOf(t, h, "sprites"); got != `providers.sprites.send_env = "forced"  # default` {
		t.Fatalf("send_env default: %q", got)
	}
	if explain(h, nil, "providers.sprites.org") == "" {
		t.Errorf("config does not show providers.sprites.org")
	}
	cases := []struct{ name, from, to, msg string }{
		{"no org", `org = "sandbox-org"`, ``, "providers.sprites.org: required"},
		{"no sizes", `sizes = { cpu8 = { memory = "16GiB" } }`, ``, "providers.sprites.sizes: at least one size with memory is required"},
		{"vcpu", `memory = "16GiB" }`, `memory = "16GiB", vcpu = 4 }`, "providers.sprites.sizes.cpu8.vcpu: Sprites have 8 vCPU (got 4)"},
		{"memory", `memory = "16GiB" }`, `memory = "32GiB" }`, "providers.sprites.sizes.cpu8.memory: exceeds memory_bound 16GiB"},
		{"egress star", `daily_usd = 10`, "daily_usd = 10\negress_allow = [\"*\"]", `providers.sprites.egress_allow: "*" not allowed (explicit domains only)`},
		{"build egress ip", `daily_usd = 10`, "daily_usd = 10\negress_build_allow = [\"10.0.0.1\"]", `providers.sprites.egress_build_allow: "10.0.0.1" not allowed (explicit domains only)`},
		{"disk", `daily_usd = 10`, "daily_usd = 10\ndisk_bound = \"200GiB\"", "providers.sprites.disk_bound: exceeds the 100 GB Sprite quota"},
		{"warm reuse", `daily_usd = 10`, "daily_usd = 10\nreuse = true", "providers.sprites.reuse: unknown key"},
		{"max_warm", `daily_usd = 10`, "daily_usd = 10\nmax_warm = 2", "providers.sprites.max_warm: unknown key"},
		{"fallback_until", `daily_usd = 10`, "daily_usd = 10\nfallback_until = \"later\"", `providers.sprites.fallback_until: must be "resource" or "attach": "later"`},
		{"send_env", `daily_usd = 10`, "daily_usd = 10\nsend_env = \"all\"", `providers.sprites.send_env: must be "none", "forwarded" or "forced": "all"`},
	}
	for _, c := range cases {
		h := harness.New(t)
		spRepo(t, h, spOpts{})
		cfg := readCfg(t, h)
		if !strings.Contains(cfg, c.from) {
			t.Fatalf("%s: the config has no %q", c.name, c.from)
		}
		h.HostConfig(strings.Replace(cfg, c.from, c.to, 1))
		if r := h.Run("config", "check"); r.Code != 78 || !strings.Contains(r.Stderr, "bitc: ") || !strings.Contains(r.Stderr, ": "+c.msg) {
			t.Errorf("%s: want 78 with %q, got %+v", c.name, c.msg, r)
		}
	}
	// the token's org slug must be the configured org
	h2 := harness.New(t)
	e2 := spRepo(t, h2, spOpts{})
	h2.Write(e2.token, "prod-org/secret-part\n", 0o600)
	if r := h2.Run("config", "check"); r.Code != 78 || !strings.Contains(r.Stderr, `providers.sprites.org: token belongs to "prod-org", not "sandbox-org"`) || strings.Contains(r.Stderr, "secret-part") {
		t.Errorf("token org: %+v", r)
	}
	// send_env and fallback_until are host-only keys
	h3 := harness.New(t)
	e3 := spRepo(t, h3, spOpts{})
	h3.Write(filepath.Join(e3.wt, ".bitc.toml"), "version = 1\n[providers.sprites]\nsend_env = \"none\"\n", 0o644)
	if r := h3.Run("config", "check"); r.Code != 78 || !strings.Contains(r.Stderr, "providers.sprites.send_env: host-only key") {
		t.Errorf("repo file send_env: %+v", r)
	}
	// [[repos]] deploy_key_file follows the credential-file rules
	h4 := harness.New(t)
	e4 := spRepo(t, h4, spOpts{key: "KEY"})
	if err := os.Chmod(e4.key, 0o644); err != nil {
		t.Fatal(err)
	}
	if r := h4.Run("config", "check"); r.Code != 78 || !strings.Contains(r.Stderr, "[[repos]] deploy_key_file: ") || !strings.Contains(r.Stderr, "mode 600") {
		t.Errorf("deploy_key_file mode: %+v", r)
	}
}

// SP3: the price from the rates reaches the ledger: the estimate is price x (E - now) and the end
// line's cost_usd is price x minutes.
func TestSP3_PriceReachesTheLedger(t *testing.T) {
	h := harness.New(t)
	e := spRepo(t, h, spOpts{})
	if r := h.Run(spRun("echo ok")...); r.Code != 0 || r.Stdout != "ok\n" {
		t.Fatalf("%+v", r)
	}
	launch, end := e.ledger(t, "launch"), e.ledger(t, "end")
	if len(launch) != 1 || len(end) != 1 {
		t.Fatalf("ledger: %v / %v", launch, end)
	}
	// start_timeout 2m + timeout 60m + 15m grace: E - now is 77 minutes at 0.757154 $/h
	est := launch[0]["est_usd"].(float64)
	if est < 0.9715 || est > 0.9720 {
		t.Fatalf("estimate %v, want about 0.9717 (77 min at $0.757154/h)", est)
	}
	cost, minutes := end[0]["cost_usd"].(float64), end[0]["minutes"].(float64)
	want := 0.757154 * minutes / 60
	if end[0]["created"] != true || end[0]["exit"] != "0" || cost < want*0.5 || cost > want*1.5+0.001 {
		t.Fatalf("end line %v, want cost about %.4f for %v min", end[0], want, minutes)
	}
}

// SP6: a provisioning infra failure deletes the Sprite, retries once on a fresh Sprite with a new
// name and reservation, then moves to RunPod.
func TestSP6_ProvisioningInfraFailureRetriesOnceThenMovesOn(t *testing.T) {
	h := harness.New(t)
	e := spRepo(t, h, spOpts{runpod: true})
	e.f.BehaveN("provision-infra-fails", 2)
	r := h.Run(spRun("echo on-runpod")...)
	names := e.created(t)
	if r.Code != 0 || r.Stdout != "on-runpod\n" || len(names) != 2 || names[0] == names[1] {
		t.Fatalf("exit %d, creates %v, %+v", r.Code, names, r)
	}
	for _, n := range names {
		if !spNameRE.MatchString(n) {
			t.Errorf("name %q is not <prefix>-<install16>-<size>-<job8>-<E>-<tag8>", n)
		}
	}
	want := "bitc: sprites: heavy: trying runpod: setup step packages failed: simulated package install failure; " + names[1] + " torn down before attach\n"
	if !strings.Contains(r.Stderr, want) || strings.Contains(r.Stderr, "locally") {
		t.Fatalf("want %q in\n%s", want, r.Stderr)
	}
	if got := e.f.Live(); len(got) != 0 || e.deletes() != 2 || e.rpCreates() != 1 {
		t.Fatalf("live %v, deletes %d, runpod creates %d", got, e.deletes(), e.rpCreates())
	}
	// one launch and one end (fallback, created) per attempt; two breaker failures; no reservation left
	launches, ends := e.ledger(t, "launch"), e.ledger(t, "end")
	if len(launches) != 2 || launches[0]["name"] != names[0] || launches[1]["name"] != names[1] || len(ends) != 2 {
		t.Fatalf("ledger launches %v ends %v", launches, ends)
	}
	for i, en := range ends {
		if en["exit"] != "fallback" || en["created"] != true || en["name"] != names[i] {
			t.Errorf("end %d: %v", i, en)
		}
	}
	if n := e.failures(t); n != 2 {
		t.Errorf("%d breaker failures, want 2", n)
	}
	if rs := e.reservations(t); len(rs) != 0 {
		t.Errorf("reservations left: %v", rs)
	}
	// the Sprite is torn down before the ledger end (8.3 order): the delete precedes nothing else here
}

// SP7: an exec upgrade refused with an HTTP error is pre-attach (the SP6 path); one that times out
// counts as attached: the Sprite is deleted, exit 75, and neither RunPod nor local is tried.
func TestSP7_AttachClassification(t *testing.T) {
	h := harness.New(t)
	e := spRepo(t, h, spOpts{runpod: true})
	e.f.Behave("attach-4xx")
	r := h.Run(spRun("echo after-retry")...)
	names := e.created(t)
	if r.Code != 0 || r.Stdout != "after-retry\n" || len(names) != 2 || e.rpCreates() != 0 || strings.Contains(r.Stderr, "trying") {
		t.Fatalf("a refused attach is retried on a fresh Sprite: exit %d, creates %v, %+v", r.Code, names, r)
	}
	if ends := e.ledger(t, "end"); len(ends) != 2 || ends[0]["exit"] != "fallback" || ends[0]["created"] != true || ends[1]["exit"] != "0" {
		t.Fatalf("ends: %v", ends)
	}
	if n := e.failures(t); n != 1 {
		t.Errorf("%d breaker failures, want 1 (the refused attach)", n)
	}

	h2 := harness.New(t)
	e2 := spRepo(t, h2, spOpts{runpod: true})
	e2.f.Behave("attach-timeout")
	r = h2.Run(spRun("echo never")...)
	if r.Code != 75 || !strings.Contains(r.Stderr, "STARVED heavy (sprites: ") || strings.Contains(r.Stderr, "trying") || strings.Contains(r.Stderr, "locally") {
		t.Fatalf("a timed-out attach: %+v", r)
	}
	if got := e2.created(t); len(got) != 1 {
		t.Fatalf("creates %v: an attached job is never retried", got)
	}
	if e2.rpCreates() != 0 || len(e2.f.Live()) != 0 || e2.deletes() != 1 {
		t.Fatalf("runpod creates %d, live %v, deletes %d", e2.rpCreates(), e2.f.Live(), e2.deletes())
	}

	// an agent that refuses its payload: attached, so 75 at once (not after the start timeout),
	// naming what the agent said
	h3 := harness.New(t)
	e3 := spRepo(t, h3, spOpts{runpod: true})
	e3.f.Behave("attach-truncate")
	began := time.Now()
	r = h3.Run(spRun("echo never")...)
	if r.Code != 75 || !strings.Contains(r.Stderr, "STARVED heavy (sprites: ") || !strings.Contains(r.Stderr, "refused: truncated payload") || time.Since(began) > 30*time.Second {
		t.Fatalf("a refused payload (%s): %+v", time.Since(began), r)
	}
	if e3.rpCreates() != 0 || len(e3.f.Live()) != 0 || len(e3.created(t)) != 1 {
		t.Fatalf("runpod creates %d, live %v, creates %v", e3.rpCreates(), e3.f.Live(), e3.created(t))
	}
}

// pre-attach call order and the policies, shared by SP8's provider half and SP9.
func spCallOrder(f *spritesfake.Fake) []string {
	var out []string
	for _, c := range f.Calls() {
		out = append(out, c.Method+" "+regexp.MustCompile(`^/v1/sprites/[^/]+`).ReplaceAllString(c.Path, "/v1/sprites/N"))
	}
	return out
}

// SP9 (with the provider half of SP8): the calls are create, the two policies (each read back),
// the provisioning execs, the job policy (read back), then the attach; nothing of the job, its
// code ref, its key or its forwarded values appears in any of them or in a provisioning exec.
func TestSP9_NoJobInputBeforeTheAttachExec(t *testing.T) {
	const keyCanary, forcedCanary, plainCanary, argvCanary = "CANARY-DEPLOY-KEY-8f3a1c", "forced-canary-77e1", "plain-canary-5b09", "argv-canary-5521"
	h := harness.New(t)
	e := spRepo(t, h, spOpts{key: keyCanary, repo: "[remote]\nforward_env = [\"CI\", \"DEPLOY_TOKEN\"]\npackages = [\"jq\"]\n"})
	r := h.RunWith([]string{"CI=" + plainCanary, "DEPLOY_TOKEN=" + forcedCanary}, "run", "heavy", "--", "echo", argvCanary)
	if r.Code != 0 || r.Stdout != argvCanary+"\n" {
		t.Fatalf("%+v", r)
	}
	want := []string{"GET /v1/sprites" /* the cap check's list */, "POST /v1/sprites", "POST /v1/sprites/N/policy/network", "GET /v1/sprites/N/policy/network", "POST /v1/sprites/N/policy/resources",
		"GET /v1/sprites/N/policy/resources", "GET /v1/sprites/N/exec", "GET /v1/sprites/N/exec", "POST /v1/sprites/N/policy/network",
		"GET /v1/sprites/N/policy/network", "GET /v1/sprites/N/exec"}
	got := spCallOrder(e.f)
	if len(got) < len(want) || strings.Join(got[:len(want)], "\n") != strings.Join(want, "\n") {
		t.Fatalf("call order:\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	execs := e.f.Execs()
	attach := -1
	for i, x := range execs {
		if strings.Contains(strings.Join(x.Query["cmd"], " "), "--boot-stdin") {
			attach = i
		}
	}
	if attach != 2 {
		t.Fatalf("the attach is exec #%d of %d, want #2 (stub, provision, attach)", attach, len(execs))
	}
	// the sha of the commit and the job ref
	sha := strings.TrimSpace(string(must(exec.Command("git", "-C", e.wt, "rev-parse", "HEAD").Output())))
	canaries := []string{keyCanary, forcedCanary, plainCanary, argvCanary, sha, "refs/bitc/jobs"}
	pre := e.f.Calls()[:len(want)] // through the attach's own upgrade request
	for _, c := range pre {
		blob := c.Path + "?" + c.RawQuery + string(c.Body)
		for _, k := range canaries {
			if strings.Contains(blob, k) {
				t.Errorf("job input %q in %s %s", k, c.Method, c.Path)
			}
		}
	}
	for i, x := range execs[:attach+1] {
		blob := x.Query.Encode()
		if i < attach {
			blob += string(x.Stdin)
		}
		for _, k := range canaries {
			if strings.Contains(blob, k) {
				t.Errorf("job input %q in exec #%d", k, i)
			}
		}
	}
	// provisioning carried the repo's packages and nothing else of the job
	if !bytes.Contains(execs[1].Stdin, []byte(`"packages":["jq"]`)) || bytes.Contains(execs[1].Stdin, []byte("argv")) {
		t.Errorf("provisioning stdin: %q", execs[1].Stdin)
	}
	// the attach carries all of it, on stdin only
	p, err := agent.ReadBootPayload(bytes.NewReader(execs[attach].Stdin))
	if err != nil {
		t.Fatal(err)
	}
	spec, err := agent.DecodeSpec(p.Spec)
	if err != nil {
		t.Fatal(err)
	}
	env := strings.Join(spec.Env, " ")
	if p.DeployKey != keyCanary || spec.Sha != sha || !strings.Contains(env, "DEPLOY_TOKEN="+forcedCanary) || !strings.Contains(env, "CI="+plainCanary) || strings.Join(spec.Argv, " ") != "echo "+argvCanary {
		t.Fatalf("the attach payload lacks the job: key %q sha %q env %q argv %q", p.DeployKey, spec.Sha, env, spec.Argv)
	}
	// the policies: the build policy allows the mirrors, the job policy does not
	var nets []string
	for _, c := range e.f.Calls() {
		if c.Method == "POST" && strings.HasSuffix(c.Path, "/policy/network") {
			nets = append(nets, string(c.Body))
		}
	}
	if len(nets) != 2 || !strings.Contains(nets[0], "archive.ubuntu.com") || !strings.Contains(nets[0], "github.com") || strings.Contains(nets[1], "ubuntu") || !strings.Contains(nets[1], "registry.npmjs.org") {
		t.Fatalf("network policies: %v", nets)
	}
	for _, c := range e.f.Calls() {
		if c.Method == "POST" && strings.HasSuffix(c.Path, "/policy/resources") && !strings.Contains(string(c.Body), `"limit_mb":16384`) {
			t.Errorf("resources policy %s", c.Body)
		}
	}
}

// secretsScan walks every Sprite's overlay and checkpoints for the canaries while a job runs.
type secretsScan struct {
	mu    sync.Mutex
	hits  map[string]string // canary -> first path
	files int
	stop  chan struct{}
	done  chan struct{}
}

func scanSprites(f *spritesfake.Fake, canaries []string) *secretsScan {
	s := &secretsScan{hits: map[string]string{}, stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(s.done)
		for {
			select {
			case <-s.stop:
				return
			case <-time.After(15 * time.Millisecond):
			}
			for _, n := range f.Live() {
				f.AutoCheckpoint(n)
				roots := append([]string{f.Overlay(n)}, f.Checkpoints(n)...)
				for _, root := range roots {
					_ = filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
						if err != nil || !fi.Mode().IsRegular() {
							return nil
						}
						b, _ := os.ReadFile(p)
						s.mu.Lock()
						s.files++
						for _, c := range canaries {
							if bytes.Contains(b, []byte(c)) && s.hits[c] == "" {
								s.hits[c] = p
							}
						}
						s.mu.Unlock()
						return nil
					})
				}
			}
		}
	}()
	return s
}

func (s *secretsScan) finish() { close(s.stop); <-s.done }

// SP10: the deploy key, K and the forwarded values appear in no URL, query, body, label, overlay
// file or automatic checkpoint; only the attach's stdin carries them.
func TestSP10_SecretsTravelOnStdinOnly(t *testing.T) {
	const keyCanary, forcedCanary = "CANARY-DEPLOY-KEY-61d0e2", "forced-canary-aa31"
	h := harness.New(t)
	e := spRepo(t, h, spOpts{key: keyCanary, repo: "[remote]\nforward_env = [\"DEPLOY_TOKEN\"]\n"})
	// K and the nonce are only known from the attach: scan for the canaries that are fixed up front
	scan := scanSprites(e.f, []string{keyCanary, forcedCanary})
	r := h.RunWith([]string{"DEPLOY_TOKEN=" + forcedCanary}, spRun("sleep 1; echo ${DEPLOY_TOKEN:+has-token}")...)
	scan.finish()
	if r.Code != 0 || r.Stdout != "has-token\n" {
		t.Fatalf("%+v", r)
	}
	if scan.files == 0 {
		t.Fatal("the scan never saw a file of a Sprite")
	}
	for c, p := range scan.hits {
		t.Errorf("%q found in %s", c, p)
	}
	execs := e.f.Execs()
	attach := execs[len(execs)-1]
	for _, x := range execs {
		if strings.Contains(strings.Join(x.Query["cmd"], " "), "--boot-stdin") {
			attach = x
		}
	}
	p, err := agent.ReadBootPayload(bytes.NewReader(attach.Stdin))
	if err != nil {
		t.Fatal(err)
	}
	spec, _ := agent.DecodeSpec(p.Spec)
	secrets := []string{keyCanary, forcedCanary, spec.MacKey, spec.Nonce, spec.Bearer}
	if p.DeployKey != keyCanary || len(spec.MacKey) != 64 || len(spec.Nonce) != 64 {
		t.Fatalf("payload: key %q K %q M %q", p.DeployKey, spec.MacKey, spec.Nonce)
	}
	for _, c := range e.f.Calls() {
		blob := c.Path + "?" + c.RawQuery + string(c.Body) + c.Auth
		for _, k := range secrets[:4] {
			if strings.Contains(blob, k) {
				t.Errorf("a secret in %s %s", c.Method, c.Path)
			}
		}
	}
	for i, x := range execs {
		if strings.Contains(x.Query.Encode(), keyCanary) || strings.Contains(x.Query.Encode(), forcedCanary) || strings.Contains(x.Query.Encode(), spec.MacKey) {
			t.Errorf("a secret in the query of exec #%d", i)
		}
	}
	if spec.Bearer != "" || spec.Port != -1 {
		t.Errorf("B must be omitted and the port -1 (no results server): bearer %q port %d", spec.Bearer, spec.Port)
	}
	// the Sprite's create body carries only the name and a label; nothing else persists
	for _, c := range e.f.Calls() {
		if c.Method == "POST" && c.Path == "/v1/sprites" {
			var b map[string]any
			_ = json.Unmarshal(c.Body, &b)
			for k := range b {
				if k != "name" && k != "labels" {
					t.Errorf("create body key %q (an environment would persist)", k)
				}
			}
		}
	}
}

// SP11: the Task keeps a quiet command from pausing the Sprite (even when an open exec session
// would not); a cold drop mid-job is a lost job: 75, the Sprite deleted.
func TestSP11_KeepAliveAndColdDrop(t *testing.T) {
	h := harness.New(t)
	e := spRepo(t, h, spOpts{fake: spritesfake.Options{IdleAfter: 2 * time.Second, SessionsDontHold: true}})
	// two minutes of silence at the scale of 0.05 is six seconds
	if r := h.Run(spRun("sleep 6; echo quiet-done")...); r.Code != 0 || r.Stdout != "quiet-done\n" {
		t.Fatalf("%+v", r)
	}
	name := e.first(t)
	if n := e.f.PauseCount(name); n != 0 {
		t.Fatalf("the Sprite was paused %d times under a quiet job", n)
	}
	log := e.f.TaskLog(name)
	renews := 0
	for _, l := range log {
		if strings.HasPrefix(l, "renew") {
			renews++
		}
	}
	if len(log) < 4 || log[0] != "create 15s" || log[len(log)-1] != "delete" || renews < 1 {
		t.Fatalf("task log %v, want create 15s (5m scaled), renewals, delete", log)
	}

	// a cold drop at the first line of the agent
	h2 := harness.New(t)
	e2 := spRepo(t, h2, spOpts{})
	e2.f.Behave("cold-drop-mid-job")
	r := h2.Run(spRun("echo never")...)
	if r.Code != 75 || !strings.Contains(r.Stderr, "STARVED heavy (sprites: ") || len(e2.f.Live()) != 0 {
		t.Fatalf("cold drop at the boot: %+v live %v", r, e2.f.Live())
	}

	// a cold drop while the command runs: lost after BEGIN, a breaker failure
	h3 := harness.New(t)
	e3 := spRepo(t, h3, spOpts{})
	ready := filepath.Join(h3.Root, "ready")
	p := h3.StartWith([]string{"BITC_DEBUG=watch"}, spRun(": > "+ready+"; sleep 30")...)
	waitFile(t, ready, 30*time.Second)
	e3.f.ColdDrop(e3.first(t))
	r = p.Wait()
	if r.Code != 75 || !strings.Contains(r.Stderr, "STARVED heavy (sprites: ") || !strings.Contains(r.Stderr, "lost") {
		t.Fatalf("cold drop mid-job: %+v", r)
	}
	if len(e3.f.Live()) != 0 || e3.failures(t) != 1 {
		t.Fatalf("live %v, failures %d\n%s", e3.f.Live(), e3.failures(t), r.Stderr)
	}
}

// SP12: the estimate example of 5.2 (start 10m + timeout 30m), the cap refusing at the per-host
// share, and a running Sprite and its reservation counted once.
func TestSP12_CapEstimateAndCountingOnce(t *testing.T) {
	h := harness.New(t)
	e := spRepo(t, h, spOpts{start: "10m", tmo: "30m", daily: "1.00"})
	// the estimate: start 10m + timeout 30m + 15m is 55 minutes at $0.757154/h
	if r := h.Run(spRun("echo ok")...); r.Code != 0 {
		t.Fatalf("%+v", r)
	}
	if est := e.ledgerNum(t, "launch", "est_usd"); est < 0.6938 || est > 0.6942 {
		t.Fatalf("estimate %v, want about 0.6940 (55 min at $0.757154/h)", est)
	}
	// a listed own Sprite of the same span plus this job's estimate is over the $1.00 share
	secret := e.secret(t)
	exp := time.Now().Add(55 * time.Minute)
	other := sprites.EncodeName(secret, "bitc", state.Install16(secret), "cpu8", "abcd1234", exp)
	e.f.AddSprite(other, e.f.Org(), time.Now())
	r := h.Run(spRun("echo should-run-locally")...)
	if !strings.Contains(r.Stderr, "bitc: sprites: running heavy locally: daily cap $1.00: spent $0.01 + committed $0.69 + this job $0.69") || r.Stdout != "should-run-locally\n" {
		t.Fatalf("%+v", r)
	}

	// a running Sprite and its reservation count once: 2 x 0.694 fits a $1.80 share only if
	// the first job is not counted twice (listed and reserved)
	h2 := harness.New(t)
	e2 := spRepo(t, h2, spOpts{start: "10m", tmo: "30m", daily: "1.80"})
	ready, gate := filepath.Join(h2.Root, "ready"), filepath.Join(h2.Root, "gate")
	a := h2.Start(spRun(": > " + ready + "; while [ ! -e " + gate + " ]; do sleep 0.05; done; echo a-done")...)
	waitFile(t, ready, 30*time.Second)
	rb := h2.Run(spRun("echo b-done")...)
	_ = os.WriteFile(gate, nil, 0o600)
	ra := a.Wait()
	if rb.Code != 0 || rb.Stdout != "b-done\n" || strings.Contains(rb.Stderr, "daily cap") || ra.Stdout != "a-done\n" || len(e2.created(t)) != 2 {
		t.Fatalf("b: %+v\na: %+v\ncreates %v", rb, ra, e2.created(t))
	}
}

// SP13: a signal before or after the attach deletes the Sprite and exits 128+N, with no fallback.
func TestSP13_InterruptDeletesTheSprite(t *testing.T) {
	// before the attach: the stub exec is held
	gate := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(gate) }) }
	t.Cleanup(release)
	h := harness.New(t)
	e := spRepo(t, h, spOpts{runpod: true, fake: spritesfake.Options{Exec: func(spritesfake.ExecContext) int { <-gate; return 0 }}})
	p := h.Start(spRun("echo ran > ran-locally")...)
	h.Until(func() bool { return len(e.f.Execs()) >= 1 }, 30*time.Second)
	p.Signal(syscall.SIGINT)
	r := p.Wait()
	release()
	if r.Code != 130 || strings.Contains(r.Stderr, "trying") || ranLocally(e.fbEnv) || e.rpCreates() != 0 {
		t.Fatalf("before the attach: %+v", r)
	}
	if len(e.f.Live()) != 0 {
		t.Fatalf("the Sprite is still there: %v", e.f.Live())
	}
	if ends := e.ledger(t, "end"); len(ends) != 1 || ends[0]["exit"] != "interrupted" {
		t.Fatalf("ledger ends %v", ends)
	} else if c, _ := ends[0]["created"].(bool); !c || e.ledgerNum(t, "end", "cost_usd") <= 0 {
		// the Sprite billed through provisioning (design 5.3): the end carries its minutes
		t.Fatalf("ledger end of a pre-attach interrupt %v, want created=true and cost_usd > 0", ends[0])
	}

	// before the attach, and the name lookup fails: the Sprite is not confirmed gone, so its
	// reservation stays as an orphan for the sweep (providers delta B.3.3)
	gate2 := make(chan struct{})
	var once2 sync.Once
	release2 := func() { once2.Do(func() { close(gate2) }) }
	t.Cleanup(release2)
	ho := harness.New(t)
	eo := spRepo(t, ho, spOpts{runpod: true, fake: spritesfake.Options{Exec: func(spritesfake.ExecContext) int { <-gate2; return 0 }}})
	po := ho.Start(spRun("echo ran > ran-locally")...)
	ho.Until(func() bool { return len(eo.f.Execs()) >= 1 }, 30*time.Second)
	eo.f.BehaveN("list-500", 20) // the client retries a GET
	po.Signal(syscall.SIGINT)
	ro := po.Wait()
	release2()
	if ro.Code != 130 {
		t.Fatalf("interrupt with a failing lookup: %+v", ro)
	}
	if rs := eo.reservations(t); len(rs) != 1 || !rs[0].Orphan {
		t.Fatalf("reservations %v, want one orphan", rs)
	}

	// after the attach: the command is running
	h2 := harness.New(t)
	e2 := spRepo(t, h2, spOpts{runpod: true})
	ready := filepath.Join(h2.Root, "ready")
	p2 := h2.Start(spRun(": > " + ready + "; sleep 30")...)
	waitFile(t, ready, 30*time.Second)
	p2.Signal(syscall.SIGTERM)
	r = p2.Wait()
	if r.Code != 143 || strings.Contains(r.Stderr, "trying") || e2.rpCreates() != 0 {
		t.Fatalf("after the attach: %+v", r)
	}
	if len(e2.f.Live()) != 0 || e2.deletes() != 1 {
		t.Fatalf("live %v deletes %d", e2.f.Live(), e2.deletes())
	}
	if ends := e2.ledger(t, "end"); len(ends) != 1 || ends[0]["exit"] != "interrupted" {
		t.Fatalf("ledger ends %v", ends)
	}
	if n := e2.failures(t); n != 0 {
		t.Fatalf("an interrupt wrote %d breaker records", n)
	}
}

// SP15: a repo-class provisioning failure (a tool pin) is 69: the Sprite is deleted, there is no
// retry and no fallback.
func TestSP15_ProvisioningRepoFailureIs69(t *testing.T) {
	h := harness.New(t)
	e := spRepo(t, h, spOpts{runpod: true})
	e.f.Behave("provision-repo-fails")
	r := h.Run(spRun("echo ran > ran-locally")...)
	if r.Code != 69 || !strings.Contains(r.Stderr, "bitc: sprites: heavy: setup step tl failed: simulated tool pin mismatch") || strings.Contains(r.Stderr, "trying") {
		t.Fatalf("%+v", r)
	}
	if got := e.created(t); len(got) != 1 || len(e.f.Live()) != 0 || e.deletes() != 1 || e.rpCreates() != 0 || ranLocally(e.fbEnv) {
		t.Fatalf("creates %v, live %v, deletes %d, runpod %d", got, e.f.Live(), e.deletes(), e.rpCreates())
	}
	if ends := e.ledger(t, "end"); len(ends) != 1 || ends[0]["exit"] != "69" || e.failures(t) != 0 {
		t.Fatalf("ends %v, failures %d", ends, e.failures(t))
	}
}

// SP16: an ambiguous create. Found and deleted: one retry, then the next provider. Not confirmed
// gone: left for the sweep, no retry, an orphan reservation the next launch's cap check deletes
// at once. With fallback_until = "resource" both are 75.
func TestSP16_AmbiguousCreate(t *testing.T) {
	h := harness.New(t)
	e := spRepo(t, h, spOpts{runpod: true})
	e.f.BehaveN("create-then-500", 2)
	r := h.Run(spRun("echo on-runpod")...)
	names := e.created(t)
	if r.Code != 0 || r.Stdout != "on-runpod\n" || len(names) != 2 || !strings.Contains(r.Stderr, "trying runpod: ") || !strings.Contains(r.Stderr, "; "+names[1]+" torn down before attach\n") {
		t.Fatalf("found and deleted twice: creates %v, %+v", names, r)
	}
	if len(e.f.Live()) != 0 || e.failures(t) != 2 {
		t.Fatalf("live %v, failures %d", e.f.Live(), e.failures(t))
	}

	// found and deleted once: the retry runs the job on Sprites
	h1 := harness.New(t)
	e1 := spRepo(t, h1, spOpts{runpod: true})
	e1.f.Behave("create-then-500")
	if r := h1.Run(spRun("echo on-sprites")...); r.Code != 0 || r.Stdout != "on-sprites\n" || len(e1.created(t)) != 2 || e1.rpCreates() != 0 {
		t.Fatalf("one ambiguous create: %+v creates %v", r, e1.created(t))
	}

	// the delete fails too: unconfirmed, left for the sweep, no retry
	h2 := harness.New(t)
	e2 := spRepo(t, h2, spOpts{runpod: true})
	e2.f.Behave("create-then-500")
	e2.f.Behave("delete-500")
	r = h2.Run(spRun("echo on-runpod")...)
	names = e2.created(t)
	if r.Code != 0 || r.Stdout != "on-runpod\n" || len(names) != 1 || !strings.Contains(r.Stderr, "; "+names[0]+" left for sweep\n") {
		t.Fatalf("unconfirmed: creates %v, %+v", names, r)
	}
	if live := e2.f.Live(); len(live) != 1 || live[0] != names[0] {
		t.Fatalf("live %v", live)
	}
	rs := e2.reservations(t)
	if len(rs) != 1 || rs[0].Name != names[0] || !rs[0].Orphan {
		t.Fatalf("reservations %v, want one orphan for %s", rs, names[0])
	}
	// the next Sprites launch deletes the orphan at once (its owner tag verifies), whatever its expiry
	if r := h2.Run(spRun("echo on-sprites")...); r.Code != 0 || r.Stdout != "on-sprites\n" {
		t.Fatalf("the next job: %+v", r)
	}
	for _, n := range e2.f.Live() {
		if n == names[0] {
			t.Fatalf("the orphan %s is still alive after the next launch", n)
		}
	}
	if rs := e2.reservations(t); len(rs) != 0 {
		t.Fatalf("reservations %v", rs)
	}

	// narrowed to resource: both are 75
	h3 := harness.New(t)
	e3 := spRepo(t, h3, spOpts{runpod: true, prov: "fallback_until = \"resource\"\n"})
	e3.f.Behave("create-then-500")
	r = h3.Run(spRun("echo never")...)
	if r.Code != 75 || !strings.Contains(r.Stderr, "STARVED heavy (sprites: ") || e3.rpCreates() != 0 || len(e3.created(t)) != 1 || len(e3.f.Live()) != 0 {
		t.Fatalf("narrowed: %+v creates %v", r, e3.created(t))
	}

	// a 5xx on a policy call after the 201 takes the same path
	h4 := harness.New(t)
	e4 := spRepo(t, h4, spOpts{runpod: true})
	e4.f.BehaveN("policy-500", 2)
	r = h4.Run(spRun("echo on-runpod")...)
	names = e4.created(t)
	if r.Code != 0 || r.Stdout != "on-runpod\n" || len(names) != 2 || !strings.Contains(r.Stderr, "; "+names[1]+" torn down before attach\n") || len(e4.f.Live()) != 0 {
		t.Fatalf("policy 5xx: creates %v, %+v", names, r)
	}

	// a 5xx on the cap check's list, before any create, is retried once
	h5 := harness.New(t)
	e5 := spRepo(t, h5, spOpts{runpod: true})
	e5.f.Behave("list-500")
	if r := h5.Run(spRun("echo on-sprites")...); r.Code != 0 || r.Stdout != "on-sprites\n" || e5.rpCreates() != 0 || len(e5.created(t)) != 1 {
		t.Fatalf("list 5xx: %+v", r)
	}
}

// SP17: read and tail through the Sprites exec path reject a bad job id, an unlisted file name and
// a symlink, with nothing on stdout.
func TestSP17_ReadAndTailRejectBadArguments(t *testing.T) {
	f := spritesfake.New(t, spritesfake.Options{Bin: harness.Bin, AgentSHA: spAgentSHA(t)})
	c := spClient(t, f)
	ctx := context.Background()
	name := "bitc-a"
	if _, err := c.CreateSprite(ctx, name, nil); err != nil {
		t.Fatal(err)
	}
	build, _ := sprites.AllowDomains("github.com")
	if err := c.ApplyBuildPolicies(ctx, name, build, sprites.Resources{LimitMB: 1024}); err != nil {
		t.Fatal(err)
	}
	sha := spAgentSHA(t)
	// a real job directory, then a symlink in place of its output file
	dir := filepath.Join(f.Overlay(name), "run", "j1")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(t.TempDir(), "secret")
	_ = os.WriteFile(secret, []byte("not for the host"), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "result.json"), []byte("{}"), 0o600)
	if err := os.Symlink(secret, filepath.Join(dir, "out.log")); err != nil {
		t.Fatal(err)
	}
	read := func(job, file string) (int, string, error) {
		var b bytes.Buffer
		code, err := c.Read(ctx, name, sha, job, file, 0, &b)
		return code, b.String(), err
	}
	if code, out, err := read("j1", "result.json"); err != nil || code != 0 || out != "{}" {
		t.Fatalf("a listed file: %d %q %v", code, out, err)
	}
	for _, bad := range []struct {
		job, file string
		code      int
	}{{"j1", "evil.log", 64}, {"j1", "out.log", 66}, {"j2", "result.json", 66}} {
		code, out, err := read(bad.job, bad.file)
		if err != nil || code != bad.code || out != "" {
			t.Errorf("read %s %s: exit %d (want %d), stdout %q, %v", bad.job, bad.file, code, bad.code, out, err)
		}
	}
	// ids the client refuses are never sent
	before := len(f.Execs())
	for _, id := range []string{"..", ".hidden", "a/b", "j1; id", ""} {
		if _, _, err := read(id, "out.log"); err == nil {
			t.Errorf("job id %q was accepted", id)
		}
	}
	if len(f.Execs()) != before {
		t.Errorf("a bad job id reached the Sprite")
	}
	err := c.Tail(ctx, name, sha, "..", nil, func(agent.Frame) error { return nil })
	if err == nil || len(f.Execs()) != before {
		t.Errorf("tail of a bad job id: %v", err)
	}
	// a tail of the symlink's job streams nothing from the link target
	var got []byte
	_ = c.Tail(ctx, name, sha, "j1", nil, func(fr agent.Frame) error { got = append(got, fr.Data...); return nil })
	if bytes.Contains(got, []byte("not for the host")) {
		t.Errorf("tail followed a symlink")
	}

	// through the whole host: a job whose artifact archive is swapped for a symlink to a file of
	// the Sprite is not fetched; the call ends 75 and the file's bytes reach no work tree
	h := harness.New(t)
	e := spRepo(t, h, spOpts{repo: "artifacts = [{ path = \"out\" }]\n"})
	target := tgz(t, map[string]string{"out/report.txt": "SPRITE-SIDE-SECRET"}) // a valid archive: followed, it would be applied
	stop := make(chan struct{})
	swapped := make(chan bool, 1)
	go func() { // the moment result.json exists, the archive becomes a link
		for {
			select {
			case <-stop:
				swapped <- false
				return
			case <-time.After(2 * time.Millisecond):
			}
			for _, n := range e.f.Live() {
				m, _ := filepath.Glob(filepath.Join(e.f.Overlay(n), "run", "*", "result.json"))
				if len(m) == 0 {
					continue
				}
				a := filepath.Join(filepath.Dir(m[0]), "artifacts.tar.gz")
				_ = os.Remove(a)
				swapped <- os.Symlink(target, a) == nil
				return
			}
		}
	}()
	r := h.Run(spRun("mkdir -p out; echo data > out/report.txt")...)
	close(stop)
	if !<-swapped {
		t.Fatal("the archive was never swapped")
	}
	if r.Code != 75 || !strings.Contains(r.Stderr, "artifacts: ") || strings.Contains(r.Stdout+r.Stderr, "SPRITE-SIDE-SECRET") {
		t.Fatalf("a symlinked archive: %+v", r)
	}
	if _, err := os.Stat(filepath.Join(e.wt, "out")); err == nil {
		t.Fatal("something was applied to the work tree")
	}
}

// SP18: the agent has exited after result.json; the host completes the output and the artifacts
// over exec from the idle Sprite, then deletes it.
func TestSP18_HostCompletesFromAnIdleSprite(t *testing.T) {
	h := harness.New(t)
	e := spRepo(t, h, spOpts{repo: "artifacts = [{ path = \"out\" }]\n"})
	exited := make(chan bool, 1)
	go func() { // the agent's pid dies by itself soon after result.json
		var res string
		for i := 0; i < 1500 && res == ""; i++ {
			for _, n := range e.f.Live() {
				m, _ := filepath.Glob(filepath.Join(e.f.Overlay(n), "run", "*", "result.json"))
				if len(m) > 0 {
					res = m[0]
				}
			}
			time.Sleep(10 * time.Millisecond)
		}
		if res == "" {
			exited <- false
			return
		}
		pidFile := filepath.Join(filepath.Dir(res), "agent.pid")
		for i := 0; i < 300; i++ {
			b, _ := os.ReadFile(pidFile)
			f := strings.Fields(string(b))
			if len(f) == 2 {
				pid, _ := strconv.Atoi(f[0])
				if _, err := os.Stat("/proc/" + strconv.Itoa(pid)); err != nil {
					exited <- true
					return
				}
			}
			time.Sleep(10 * time.Millisecond)
		}
		exited <- false
	}()
	r := h.Run(spRun("mkdir -p out; echo artifact-data > out/report.txt; echo stdout-line")...)
	if r.Code != 0 || r.Stdout != "stdout-line\n" {
		t.Fatalf("%+v", r)
	}
	if ok := <-exited; !ok {
		t.Fatal("the agent did not exit by itself after result.json")
	}
	b, err := os.ReadFile(filepath.Join(e.wt, "out", "report.txt"))
	if err != nil || string(b) != "artifact-data\n" {
		t.Fatalf("artifact: %q %v", b, err)
	}
	var verbs []string
	for _, x := range e.f.Execs() {
		if cmd := strings.Join(x.Query["cmd"], " "); strings.Contains(cmd, "__agent read") {
			verbs = append(verbs, cmd[strings.Index(cmd, "__agent read"):])
		}
	}
	if !strings.Contains(strings.Join(verbs, "\n"), "result.json 0") || !strings.Contains(strings.Join(verbs, "\n"), "artifacts.tar.gz 0") {
		t.Fatalf("result.json and the artifacts were not read over exec: %v", verbs)
	}
	if len(e.f.Live()) != 0 || e.deletes() != 1 {
		t.Fatalf("live %v deletes %d", e.f.Live(), e.deletes())
	}
	// no listener: the spec's port is -1 and B is omitted
	for _, x := range e.f.Execs() {
		if strings.Contains(strings.Join(x.Query["cmd"], " "), "--boot-stdin") {
			p, _ := agent.ReadBootPayload(bytes.NewReader(x.Stdin))
			spec, _ := agent.DecodeSpec(p.Spec)
			if spec.Port != -1 || spec.Bearer != "" || p.KeepAlive == nil || p.Provider != "sprites" || p.ResourceID == "" {
				t.Errorf("attach payload: port %d bearer %q keep-alive %v provider %q resource %q", spec.Port, spec.Bearer, p.KeepAlive, p.Provider, p.ResourceID)
			}
		}
	}
}

// SP19: three discarded Sprites within the window pause Sprites; the next job goes to RunPod
// without a create; 429s write no record.
func TestSP19_BreakerPausesSprites(t *testing.T) {
	h := harness.New(t)
	e := spRepo(t, h, spOpts{runpod: true})
	e.f.BehaveN("429-create", 2) // a capacity answer and its retry: nothing created, no record
	r := h.Run(spRun("echo c0")...)
	if r.Code != 0 || r.Stdout != "c0\n" || !strings.Contains(r.Stderr, "trying runpod: ") || len(e.created(t)) != 2 {
		t.Fatalf("429: %+v", r)
	}
	if e.failures(t) != 0 || strings.TrimSpace(slurp(filepath.Join(e.acct, "failures.jsonl"))) != "" {
		t.Fatalf("a 429 wrote a record: %q", slurp(filepath.Join(e.acct, "failures.jsonl")))
	}
	e.f.BehaveN("provision-infra-fails", -1)
	before := len(e.created(t))
	r = h.Run(spRun("echo c1")...) // two discarded Sprites
	if r.Code != 0 || r.Stdout != "c1\n" || len(e.created(t)) != before+2 || e.failures(t) != 2 {
		t.Fatalf("first job: creates %d, failures %d, %+v", len(e.created(t))-before, e.failures(t), r)
	}
	r = h.Run(spRun("echo c2")...) // the third discard pauses; the retry is refused
	if r.Code != 0 || r.Stdout != "c2\n" || e.failures(t) != 3 || !strings.Contains(r.Stderr, "sprites is paused: 3 failures after start in the last 1h0m0s") {
		t.Fatalf("second job: failures %d, %+v", e.failures(t), r)
	}
	before = len(e.created(t))
	r = h.Run(spRun("echo c3")...)
	if r.Code != 0 || r.Stdout != "c3\n" || len(e.created(t)) != before || !strings.Contains(r.Stderr, "bitc: sprites: heavy: trying runpod: sprites is paused: ") {
		t.Fatalf("third job: creates %d, %+v", len(e.created(t))-before, r)
	}
	if len(e.f.Live()) != 0 {
		t.Fatalf("live %v", e.f.Live())
	}
}

// SP20: send_env. The default is "forced": a forced value travels only in the attach's stdin. With
// "forwarded" a job forwarding a forced name is ineligible for Sprites (no create, no reservation)
// and runs on RunPod.
func TestSP20_SendEnv(t *testing.T) {
	const forced, plain = "forced-canary-3e8d", "plain-value-1"
	show := spRun(`echo "$CI|${DEPLOY_TOKEN:+tok}"`)
	repo := "[remote]\nforward_env = [\"CI\", \"DEPLOY_TOKEN\"]\n"
	env := []string{"CI=" + plain, "DEPLOY_TOKEN=" + forced}
	h := harness.New(t)
	e := spRepo(t, h, spOpts{runpod: true, repo: repo})
	if r := h.RunWith(env, show...); r.Code != 0 || r.Stdout != plain+"|tok\n" || strings.Contains(r.Stderr, "not eligible") {
		t.Fatalf("default: %+v", r)
	}
	if e.rpCreates() != 0 || len(e.created(t)) != 1 {
		t.Fatalf("the job did not run on Sprites: creates %v", e.created(t))
	}
	for _, c := range e.f.Calls() {
		if strings.Contains(c.Path+c.RawQuery+string(c.Body), forced) {
			t.Errorf("the forced value is in %s %s", c.Method, c.Path)
		}
	}
	for i, x := range e.f.Execs() {
		if strings.Contains(x.Query.Encode(), forced) {
			t.Errorf("the forced value is in the query of exec #%d", i)
		}
	}

	h2 := harness.New(t)
	e2 := spRepo(t, h2, spOpts{runpod: true, repo: repo, prov: "send_env = \"forwarded\"\n"})
	if got := levelOf(t, h2, "sprites"); got != `providers.sprites.send_env = "forwarded"  # host` {
		t.Fatalf("level: %q", got)
	}
	r := h2.RunWith(env, show...)
	want := `bitc: sprites: heavy: not eligible for remote: forced env not sent to sprites (providers.sprites.send_env = "forwarded"): DEPLOY_TOKEN` + "\n"
	if r.Code != 0 || r.Stdout != plain+"|tok\n" || !strings.Contains(r.Stderr, want) {
		t.Fatalf("forwarded: want %q in %+v", want, r)
	}
	if len(e2.f.Calls()) != 0 || e2.rpCreates() != 1 {
		t.Fatalf("Sprites saw %d calls, runpod creates %d", len(e2.f.Calls()), e2.rpCreates())
	}
	if rs := e2.reservations(t); len(rs) != 0 || strings.TrimSpace(slurp(filepath.Join(e2.acct, "ledger.jsonl"))) != "" {
		t.Fatalf("a reservation or ledger line for an ineligible job: %v", rs)
	}
	// plain names only: Sprites takes it
	if r := h2.RunWith(env[:1], show...); r.Code != 0 || r.Stdout != plain+"|\n" || len(e2.created(t)) != 1 {
		t.Fatalf("plain only: %+v", r)
	}
}

// SP2 (after the attach): the monitor's org guard sees another organization while the command
// runs: the Sprite is deleted at once, one pause record is written, the job is 75 (it began, so
// no fallback), and the next job does not even try to create.
func TestSP2_OrgGuardAfterTheAttach(t *testing.T) {
	h := harness.New(t)
	e := spRepo(t, h, spOpts{runpod: true})
	ready := filepath.Join(h.Root, "ready")
	p := h.Start(spRun(": > " + ready + "; sleep 60")...)
	waitFile(t, ready, 30*time.Second)
	e.f.BehaveN("wrong-org", -1)
	r := p.Wait()
	if r.Code != 75 || !strings.Contains(r.Stderr, "(org guard)") || strings.Contains(r.Stderr, "trying") || e.rpCreates() != 0 {
		t.Fatalf("wrong org after the attach: %+v", r)
	}
	e.f.BehaveN("wrong-org", 0)
	if len(e.f.Live()) != 0 || e.deletes() != 1 {
		t.Fatalf("live %v, deletes %d", e.f.Live(), e.deletes())
	}
	if n := strings.Count(slurp(filepath.Join(e.acct, "failures.jsonl")), `"kind":"pause"`); n != 1 {
		t.Fatalf("%d pause records, want 1", n)
	}
	before := len(e.created(t))
	if r := h.Run(spRun("echo again")...); len(e.created(t)) != before || !strings.Contains(r.Stderr, "trying runpod: sprites is paused: ") {
		t.Fatalf("while paused: %+v", r)
	}
}

// ledgerAt reads the ledger lines of one event from a provider's account directory.
func ledgerAt(t *testing.T, e *spEnv, provider, event string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, l := range lines(t, filepath.Join(e.h.Root, "account", "remote", provider, "ledger.jsonl")) {
		var m map[string]any
		if json.Unmarshal([]byte(l), &m) == nil && m["event"] == event {
			out = append(out, m)
		}
	}
	return out
}

// SP23: the fallback chain sprites -> runpod -> local on the two fakes. A pre-attach Sprites failure
// moves to RunPod; RunPod without capacity moves to local; an attached Sprites failure never moves
// on (75); a forced env value works on both; the ledger and the cap are kept per provider.
func TestSP23_FallbackChainSpritesRunPodLocal(t *testing.T) {
	const forced = "forced-canary-9c41"
	show := spRun(`echo "${DEPLOY_TOKEN:+tok:$DEPLOY_TOKEN}"`)
	env := []string{"DEPLOY_TOKEN=" + forced}
	repo := "[remote]\nforward_env = [\"DEPLOY_TOKEN\"]\n"

	t.Run("pre-attach Sprites failure moves to RunPod, forced env included", func(t *testing.T) {
		h := harness.New(t)
		e := spRepo(t, h, spOpts{runpod: true, repo: repo})
		e.f.BehaveN("provision-infra-fails", 2)
		r := h.RunWith(env, show...)
		if r.Code != 0 || r.Stdout != "tok:"+forced+"\n" || !strings.Contains(r.Stderr, "bitc: sprites: heavy: trying runpod: ") || strings.Contains(r.Stderr, "locally") {
			t.Fatalf("%+v", r)
		}
		if e.rpCreates() != 1 || len(e.created(t)) != 2 || len(e.f.Live()) != 0 {
			t.Fatalf("runpod creates %d, sprites %v, live %v", e.rpCreates(), e.created(t), e.f.Live())
		}
		for _, c := range e.f.Calls() {
			if strings.Contains(c.Path+c.RawQuery+string(c.Body), forced) {
				t.Errorf("the forced value is in the Sprites call %s %s", c.Method, c.Path)
			}
		}
		// the ledger is kept per provider: two fallback attempts on Sprites, one clean job on RunPod
		sEnds, rEnds := ledgerAt(t, e, "sprites", "end"), ledgerAt(t, e, "runpod", "end")
		if len(sEnds) != 2 || sEnds[0]["exit"] != "fallback" || len(rEnds) != 1 || rEnds[0]["exit"] != "0" || rEnds[0]["name"] == sEnds[0]["name"] {
			t.Fatalf("sprites ends %v, runpod ends %v", sEnds, rEnds)
		}
		if n := len(ledgerAt(t, e, "runpod", "launch")); n != 1 {
			t.Fatalf("%d runpod launches", n)
		}
	})

	t.Run("a forced env value also works on Sprites", func(t *testing.T) {
		h := harness.New(t)
		e := spRepo(t, h, spOpts{runpod: true, repo: repo})
		r := h.RunWith(env, show...)
		if r.Code != 0 || r.Stdout != "tok:"+forced+"\n" || e.rpCreates() != 0 || len(e.created(t)) != 1 || len(ledgerAt(t, e, "runpod", "launch")) != 0 {
			t.Fatalf("creates %v, runpod %d: %+v", e.created(t), e.rpCreates(), r)
		}
	})

	t.Run("RunPod without capacity moves to local", func(t *testing.T) {
		h := harness.New(t)
		e := spRepo(t, h, spOpts{runpod: true})
		e.f.BehaveN("provision-infra-fails", 2)
		e.rp.Behave("transient-create-twice") // the "no longer any instances" answer, on the create and on its one retry
		r := h.Run(spRun("echo on-local")...)
		if r.Code != 0 || r.Stdout != "on-local\n" || !strings.Contains(r.Stderr, "trying runpod: ") || !strings.Contains(r.Stderr, "(no capacity)") || !strings.Contains(r.Stderr, "running heavy locally: ") {
			t.Fatalf("%+v", r)
		}
		if got := strings.Index(r.Stderr, "trying runpod: "); got > strings.Index(r.Stderr, "running heavy locally: ") {
			t.Fatalf("local before runpod:\n%s", r.Stderr)
		}
		if len(e.f.Live()) != 0 || len(ledgerAt(t, e, "sprites", "end")) != 2 {
			t.Fatalf("live %v", e.f.Live())
		}
		// a pre-launch capacity answer is not a failure of the provider: no breaker record, no launch booked
		rpDir := filepath.Join(h.Root, "account", "remote", "runpod")
		if b, err := os.ReadFile(filepath.Join(rpDir, "failures.jsonl")); err == nil && len(strings.TrimSpace(string(b))) > 0 {
			t.Fatalf("runpod breaker records: %q", b)
		}
		// the reservation made before the create is closed as "fallback" (nothing created), never as a job
		rl, re := ledgerAt(t, e, "runpod", "launch"), ledgerAt(t, e, "runpod", "end")
		if len(rl) != len(re) || len(re) == 0 {
			t.Fatalf("runpod launches %v, ends %v", rl, re)
		}
		for _, x := range re {
			if x["exit"] != "fallback" {
				t.Fatalf("runpod end %v", x)
			}
		}
		if e.rpCreates() != 2 { // the create and its one retry, both refused: no pod exists
			t.Fatalf("runpod create calls %d", e.rpCreates())
		}
	})

	t.Run("an attached Sprites failure never moves on", func(t *testing.T) {
		h := harness.New(t)
		e := spRepo(t, h, spOpts{runpod: true})
		e.f.Behave("cold-drop-mid-job")
		r := h.Run(markLocal...)
		if r.Code != 75 || strings.Contains(r.Stderr, "trying") || strings.Contains(r.Stderr, "locally") || e.rpCreates() != 0 || ranLocally(e.fbEnv) {
			t.Fatalf("runpod creates %d: %+v", e.rpCreates(), r)
		}
		if len(e.created(t)) != 1 || len(e.f.Live()) != 0 || len(ledgerAt(t, e, "runpod", "launch")) != 0 {
			t.Fatalf("creates %v, live %v", e.created(t), e.f.Live())
		}
	})

	t.Run("the cap is per provider", func(t *testing.T) {
		h := harness.New(t)
		e := spRepo(t, h, spOpts{runpod: true, daily: "0.01"}) // below any Sprites launch estimate
		r := h.Run(spRun("echo capped")...)
		if r.Code != 0 || r.Stdout != "capped\n" || !strings.Contains(r.Stderr, "trying runpod: ") || !strings.Contains(r.Stderr, "daily cap $") || len(e.created(t)) != 0 || e.rpCreates() != 1 {
			t.Fatalf("sprites creates %v, runpod %d: %+v", e.created(t), e.rpCreates(), r)
		}
		if len(ledgerAt(t, e, "sprites", "launch")) != 0 || len(ledgerAt(t, e, "runpod", "launch")) != 1 {
			t.Fatalf("the launch was not booked on RunPod alone")
		}
	})
}
