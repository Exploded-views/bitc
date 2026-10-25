package conformance

// The SH rows of the ssh provider (providers delta C.11, task H.2): bitc runs against the sshfake
// gate (`[test] ssh_exec = "local"`), which plays the host and scripts what the ssh client would
// print and exit with. Nothing here opens a socket, uses ssh or needs root.

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Exploded-views/bitc/conformance/harness"
	"github.com/Exploded-views/bitc/internal/lock"
	"github.com/Exploded-views/bitc/internal/provider/ssh/sshfake"
)

func shWire(b []byte) []byte {
	out := make([]byte, 4, 4+len(b))
	binary.BigEndian.PutUint32(out, uint32(len(b)))
	return append(out, b...)
}

// shHostKey is a syntactically valid ssh-ed25519 line (the bytes are not a real point).
func shHostKey() string {
	blob := append(shWire([]byte("ssh-ed25519")), shWire(make([]byte, 32))...)
	return "ssh-ed25519 " + base64.StdEncoding.EncodeToString(blob)
}

// shKey writes an unencrypted OpenSSH private key container (not a real key), mode 0600.
func shKey(t *testing.T, path, cipher string) {
	t.Helper()
	body := append([]byte("openssh-key-v1\x00"), shWire([]byte(cipher))...)
	body = append(body, shWire([]byte("none"))...)
	body = append(body, shWire(nil)...)
	body = append(body, 0, 0, 0, 1)
	body = append(body, shWire([]byte("pub"))...)
	body = append(body, shWire([]byte("priv"))...)
	enc := base64.StdEncoding.EncodeToString(body)
	if err := os.WriteFile(path, []byte("-----BEGIN OPENSSH PRIVATE KEY-----\n"+enc+"\n-----END OPENSSH PRIVATE KEY-----\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
}

type shOpts struct {
	slots    int                       // fake host and [providers.ssh] slots (default 2)
	maxLive  int                       // default = slots
	arch     string                    // configured arch (default amd64)
	hostArch string                    // what the fake host reports (default = arch)
	prov     func(m map[string]string) // edits the [providers.ssh] keys, as TOML value text
	extra    string                    // raw lines appended to [providers.ssh]
	host     string                    // extra host-file text after the ssh section
	limits   string                    // extra [remote_limits] text
	repo     string                    // extra repo-file text (after [pools.heavy.remote])
	poolRem  string                    // extra lines inside [pools.heavy.remote]
	order    string                    // the pool's providers (default `"ssh", "local"`)
	size     string                    // the ssh size key (default s1)
	pool     string                    // the pool's name (default heavy)
	fake     bool                      // also configure the fake provider (named in order)
	seams    []string
}

type shEnv struct {
	*fbEnv
	root  string // the fake host's state
	key   string
	slots int
}

func shRepo(t *testing.T, h *harness.H, o shOpts) *shEnv {
	t.Helper()
	if o.slots == 0 {
		o.slots = 2
	}
	if o.maxLive == 0 {
		o.maxLive = o.slots
	}
	if o.arch == "" {
		o.arch = "amd64"
	}
	if o.hostArch == "" {
		o.hostArch = o.arch
	}
	if o.order == "" {
		o.order = `"ssh", "local"`
	}
	if o.size == "" {
		o.size = "s1"
	}
	if o.pool == "" {
		o.pool = "heavy"
	}
	e := &shEnv{root: filepath.Join(h.Root, "sshhost"), key: filepath.Join(h.Root, "vm_ed25519"), slots: o.slots}
	shKey(t, e.key, "none")
	if err := sshfake.Init(e.root, sshfake.Config{Slots: o.slots, Arch: o.hostArch}); err != nil {
		t.Fatal(err)
	}
	h.Seam("time_scale = 0.05")
	h.Seam("agent_url = \"http://127.0.0.1:9/bitc\"")
	h.Seam("ssh_exec = \"local\"")
	h.Seam("ssh_gate = " + harness.Q(harness.SSHFakeGate))
	h.Seam("ssh_root = " + harness.Q(e.root))
	for _, s := range o.seams {
		h.Seam(s)
	}
	m := map[string]string{
		"host": `"127.0.0.1"`, "port": "22022", "user": `"bitc"`, "host_key": harness.Q(shHostKey()),
		"identity_file": harness.Q(e.key), "arch": harness.Q(o.arch), "slots": fmt.Sprint(o.slots), "max_live": fmt.Sprint(o.maxLive),
		"resolver": `"9.9.9.9"`, "start_timeout": `"2m"`,
		"sizes": "{ s1 = { slots = 1 }, s2 = { slots = 2 } }",
	}
	if o.prov != nil {
		o.prov(m)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var prov strings.Builder
	prov.WriteString("[providers.ssh]\n")
	for _, k := range keys {
		prov.WriteString(k + " = " + m[k] + "\n")
	}
	prov.WriteString(o.extra)
	sizes := `ssh = "` + o.size + `"`
	fake := ""
	if o.fake {
		sizes += `, fake = "small"`
		fst := filepath.Join(h.Root, "fakestate")
		if err := os.MkdirAll(fst, 0o700); err != nil {
			t.Fatal(err)
		}
		fake = "\n[providers.fake]\ndaily_usd = 25\nmax_live = 6\nusd_per_hour = 0.24\nfallback_until_cap = \"attach\"\nstate_dir = " + harness.Q(fst) + "\nmode = \"run\"\n"
	}
	h.HostConfig("[remote_limits]\nworktrees = [\"**/app\"]\nforward_env_allow = [\"CI\", \"DEPLOY_TOKEN\"]\nforward_env_force = [\"DEPLOY_TOKEN\"]\n" + o.limits +
		"[pools." + o.pool + "]\nslots = 2\nproviders = [" + o.order + "]\nremote_limits = { size = { " + sizes + " }, timeout = \"60m\" }\nwait = \"5s\"\n" +
		prov.String() + fake + o.host)
	wt := h.Repo("app", map[string]string{".bitc.toml": "version = 1\n[pools." + o.pool + ".remote]\nallow = [[\"sh\", \"*\"], [\"echo\", \"*\"]]\n" + o.poolRem + "\n" + o.repo})
	h.Cwd = wt
	t.Cleanup(func() { e.checkTheRealGate(t) })
	e.fbEnv = &fbEnv{h: h, wt: wt, acct: filepath.Join(h.Root, "account", "remote", "ssh")}
	return e
}

func (e *shEnv) script(t *testing.T, rules ...sshfake.Rule) {
	t.Helper()
	if err := sshfake.SetScript(e.root, rules); err != nil {
		t.Fatal(err)
	}
}

func (e *shEnv) calls() []sshfake.Call { return sshfake.Calls(e.root) }

func (e *shEnv) ops() []string {
	var out []string
	for _, c := range e.calls() {
		out = append(out, c.Op)
	}
	return out
}

// verbs are the command lines of one op, in order.
func (e *shEnv) verbs(op string) []string {
	var out []string
	for _, c := range e.calls() {
		if c.Op == op {
			out = append(out, c.Verb)
		}
	}
	return out
}

func (e *shEnv) count(op string) int { return len(e.verbs(op)) }

// slotLock opens a controller-side slot lock the way a second process would.
func (e *shEnv) slotLock(t *testing.T, i int) *lock.File {
	t.Helper()
	f, err := lock.Open(filepath.Join(e.acct, fmt.Sprintf("slot.%d.lock", i)))
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// hold takes slot i's lock for the test.
func (e *shEnv) hold(t *testing.T, i int) {
	t.Helper()
	f := e.slotLock(t, i)
	ok, err := f.TryLock()
	if err != nil || !ok {
		t.Fatalf("cannot hold slot %d: %v %v", i, ok, err)
	}
	t.Cleanup(func() { _ = f.Unlock(); f.Close() })
}

// free reports whether slot i's lock can be taken now.
func (e *shEnv) free(t *testing.T, i int) bool {
	t.Helper()
	f := e.slotLock(t, i)
	defer f.Close()
	ok, err := f.TryLock()
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		_ = f.Unlock()
	}
	return ok
}

// ledgerNum is a numeric field of the first ledger line of an event.
func (e *shEnv) ledgerNum(t *testing.T, event, field string) float64 {
	t.Helper()
	ls := e.ledger(t, event)
	if len(ls) == 0 {
		t.Fatalf("no ledger %s line", event)
	}
	v, _ := ls[0][field].(float64)
	return v
}

func (e *shEnv) pauses(t *testing.T) int {
	n := 0
	for _, l := range lines(t, filepath.Join(e.acct, "failures.jsonl")) {
		if strings.Contains(l, `"kind":"pause"`) {
			n++
		}
	}
	return n
}

var shEcho = []string{"run", "heavy", "--", "echo", "on-ssh"}

// ranOnSSH: the job's output came back and nothing ran locally.
func shRanOnSSH(t *testing.T, r harness.Result) {
	t.Helper()
	if r.Code != 0 || r.Stdout != "on-ssh\n" || strings.Contains(r.Stderr, "locally") {
		t.Fatalf("not run on ssh: %+v", r)
	}
}

// shFellBack: the job ran locally with a note holding want.
func shFellBack(t *testing.T, e *shEnv, r harness.Result, want string) {
	t.Helper()
	if r.Code != 0 || r.Stdout != "on-ssh\n" || !strings.Contains(r.Stderr, "bitc: ssh: running heavy locally: ") || !strings.Contains(r.Stderr, want) {
		t.Fatalf("want a local fallback noting %q: %+v", want, r)
	}
}

// ---- SH1

func TestSH1_ConfigCasesAre78WithTheirMessages(t *testing.T) {
	h := harness.New(t)
	shRepo(t, h, shOpts{})
	if r := h.Run("config", "check"); r.Code != 0 {
		t.Fatalf("a valid ssh section: %+v", r)
	}
	if got := levelOf(t, h, "ssh"); got != `providers.ssh.send_env = "forwarded"  # default` {
		t.Fatalf("send_env default: %q", got)
	}
	cases := []struct {
		name string
		o    shOpts
		pre  func(t *testing.T, e *shEnv)
		want string
	}{
		{"host", shOpts{prov: func(m map[string]string) { m["host"] = `"-oProxyCommand=x"` }}, nil, `providers.ssh.host: invalid: "-oProxyCommand=x"`},
		{"host at", shOpts{prov: func(m map[string]string) { m["host"] = `"u@h"` }}, nil, `providers.ssh.host: invalid: "u@h"`},
		{"user", shOpts{prov: func(m map[string]string) { m["user"] = `"Bad User"` }}, nil, `providers.ssh.user: invalid: "Bad User"`},
		{"port", shOpts{prov: func(m map[string]string) { m["port"] = "70000" }}, nil, `providers.ssh.port: out of range: 70000`},
		{"host_key missing", shOpts{prov: func(m map[string]string) { delete(m, "host_key") }}, nil, `providers.ssh.host_key: required`},
		{"host_key dss", shOpts{prov: func(m map[string]string) { m["host_key"] = `"ssh-dss AAAA"` }}, nil, `providers.ssh.host_key: ssh-dss is not accepted`},
		{"identity mode", shOpts{}, func(t *testing.T, e *shEnv) { _ = os.Chmod(e.key, 0o640) }, `providers.ssh.identity_file: must be owned by you and mode 600`},
		{"identity encrypted", shOpts{}, func(t *testing.T, e *shEnv) { shKey(t, e.key, "aes256-ctr") }, `providers.ssh.identity_file: is encrypted`},
		{"identity missing", shOpts{prov: func(m map[string]string) { m["identity_file"] = `"/nonexistent/id"` }}, nil, `providers.ssh.identity_file: /nonexistent/id: no such file`},
		{"api_key", shOpts{extra: "api_key = \"k\"\n"}, nil, `providers.ssh.api_key: unknown key for ssh (use identity_file)`},
		{"arch", shOpts{prov: func(m map[string]string) { m["arch"] = `"mips"` }}, nil, `providers.ssh.arch: must be "amd64" or "arm64": "mips"`},
		{"slots", shOpts{prov: func(m map[string]string) { m["slots"] = "65" }}, nil, `providers.ssh.slots: out of range: 65`},
		{"max_live", shOpts{slots: 2, maxLive: 3}, nil, `providers.ssh.max_live: 3 exceeds slots 2`},
		{"size slots", shOpts{prov: func(m map[string]string) { m["sizes"] = "{ s1 = { slots = 3 } }" }}, nil, `providers.ssh.sizes.s1.slots: out of range: 3`},
		{"priced without cap", shOpts{extra: "usd_per_hour = 1.5\n"}, nil, `providers.ssh.daily_usd: required when usd_per_hour > 0`},
		{"cap on unpriced", shOpts{extra: "daily_usd = 5\n"}, nil, `providers.ssh.daily_usd: not applicable to an unpriced provider`},
		{"proxy_jump", shOpts{extra: "proxy_jump = \"bastion\"\n"}, nil, `providers.ssh.proxy_jump: not supported in phase 1`},
		{"resolver", shOpts{prov: func(m map[string]string) { m["resolver"] = `"dns.example"` }}, nil, `providers.ssh.resolver: required, an IPv4 address: "dns.example"`},
		{"netns", shOpts{extra: "netns = false\n"}, nil, `providers.ssh.netns: false requires slots = 1`},
		{"memory_per_slot", shOpts{extra: "memory_per_slot = \"128MiB\"\n"}, nil, `providers.ssh.memory_per_slot: out of range: 128MiB`},
		{"send_env", shOpts{extra: "send_env = \"all\"\n"}, nil, `providers.ssh.send_env: must be "none", "forwarded" or "forced": "all"`},
		{"fallback_until", shOpts{extra: "fallback_until = \"later\"\n"}, nil, `providers.ssh.fallback_until: must be "resource" or "attach": "later"`},
		{"build without module_dir", shOpts{extra: "agent = { source = \"build\" }\n"}, nil, `providers.ssh.agent.module_dir: required when source is "build"`},
		{"unknown key", shOpts{extra: "reuse = true\n"}, nil, `providers.ssh.reuse: unknown key`},
	}
	for _, c := range cases {
		h := harness.New(t)
		e := shRepo(t, h, c.o)
		if c.pre != nil {
			c.pre(t, e)
		}
		if r := h.Run("config", "check"); r.Code != 78 || !strings.Contains(r.Stderr, "bitc: ") || !strings.Contains(r.Stderr, ": "+c.want) {
			t.Errorf("%s: want 78 with %q, got %+v", c.name, c.want, r)
		}
	}
	// ssh needs no daily_usd while it is unpriced, and a priced one needs it
	h2 := harness.New(t)
	shRepo(t, h2, shOpts{extra: "usd_per_hour = 1.5\ndaily_usd = 20\n"})
	if r := h2.Run("config", "check"); r.Code != 0 {
		t.Errorf("a priced ssh section with daily_usd: %+v", r)
	}
	// max_live defaults to slots
	h3 := harness.New(t)
	e3 := shRepo(t, h3, shOpts{slots: 3, prov: func(m map[string]string) { delete(m, "max_live") }})
	_ = e3
	if r := h3.Run("config", "check"); r.Code != 0 {
		t.Errorf("max_live default: %+v", r)
	}
}

// ---- SH3

func TestSH3_SlotsLowestFreeAllOrNoneAndBusy(t *testing.T) {
	t.Run("lowest free", func(t *testing.T) {
		h := harness.New(t)
		e := shRepo(t, h, shOpts{})
		e.hold(t, 1) // another process of this install runs a job in slot 1
		shRanOnSSH(t, h.Run(shEcho...))
		if v := e.verbs("prepare"); len(v) != 1 || !strings.Contains(v[0], "slot-boot --prepare 2 ") {
			t.Fatalf("prepare verbs %q, want slot 2", v)
		}
		if !e.free(t, 2) {
			t.Error("the finished job's slot lock is still held")
		}
	})
	t.Run("slot 1 when nothing is held", func(t *testing.T) {
		h := harness.New(t)
		e := shRepo(t, h, shOpts{})
		shRanOnSSH(t, h.Run(shEcho...))
		if v := e.verbs("prepare"); len(v) != 1 || !strings.Contains(v[0], "slot-boot --prepare 1 ") {
			t.Fatalf("prepare verbs %q, want slot 1", v)
		}
	})
	t.Run("all taken is a fallback note", func(t *testing.T) {
		h := harness.New(t)
		e := shRepo(t, h, shOpts{})
		e.hold(t, 1)
		e.hold(t, 2)
		shFellBack(t, e, h.Run(shEcho...), "2 of 2 slots busy")
		if e.count("prepare") != 0 {
			t.Fatalf("prepared with every slot taken: %v", e.ops())
		}
	})
	t.Run("all or none", func(t *testing.T) {
		h := harness.New(t)
		e := shRepo(t, h, shOpts{size: "s2"})
		e.hold(t, 1)
		shFellBack(t, e, h.Run(shEcho...), "1 of 2 slots busy")
		if e.count("prepare") != 0 {
			t.Fatalf("a two-slot job prepared with one slot taken: %v", e.ops())
		}
		if !e.free(t, 2) {
			t.Error("slot 2 stayed locked after the refused two-slot job")
		}
	})
	t.Run("a two-slot job takes both", func(t *testing.T) {
		h := harness.New(t)
		e := shRepo(t, h, shOpts{size: "s2"})
		shRanOnSSH(t, h.Run(shEcho...))
		if v := e.verbs("prepare"); len(v) != 1 || !strings.Contains(v[0], "slot-boot --prepare 1+2 ") {
			t.Fatalf("prepare verbs %q, want slots 1+2", v)
		}
	})
	t.Run("max_live", func(t *testing.T) {
		h := harness.New(t)
		e := shRepo(t, h, shOpts{maxLive: 1})
		p := h.Start("run", "heavy", "--", "sh", "-c", "sleep 4; echo on-ssh")
		h.Until(func() bool { return len(e.reservations(t)) == 1 && e.count("run") == 1 }, 30*time.Second)
		shFellBack(t, e, h.Run(shEcho...), "1 live of max_live 1")
		if r := p.Wait(); r.Code != 0 || r.Stdout != "on-ssh\n" {
			t.Fatalf("the first job: %+v", r)
		}
	})
	t.Run("busy on the host is transient", func(t *testing.T) {
		h := harness.New(t)
		e := shRepo(t, h, shOpts{slots: 1, prov: func(m map[string]string) { m["sizes"] = "{ s1 = { slots = 1 } }" }})
		if err := sshfake.SetOwner(e.root, 1, sshfake.SlotOwner{Name: "other-controller-job", Nonce: strings.Repeat("a", 32),
			Created: time.Now().Unix(), LeaseUntil: time.Now().Add(time.Hour).Unix()}); err != nil {
			t.Fatal(err)
		}
		shFellBack(t, e, h.Run(shEcho...), "slot 1 busy on host")
		if e.count("teardown") != 0 || len(e.reservations(t)) != 0 || e.failures(t) != 0 {
			t.Fatalf("a refused lease left traces: %v, %v, %d failures", e.ops(), e.reservations(t), e.failures(t))
		}
		if !e.free(t, 1) {
			t.Error("the controller lock of the refused slot is still held")
		}
	})
}

// ---- SH4

func TestSH4_CostIsZeroByDefaultAndRailsStillRefuse(t *testing.T) {
	t.Run("unpriced", func(t *testing.T) {
		h := harness.New(t)
		e := shRepo(t, h, shOpts{})
		shRanOnSSH(t, h.Run(shEcho...))
		l, end := e.ledger(t, "launch"), e.ledger(t, "end")
		if len(l) != 1 || len(end) != 1 {
			t.Fatalf("ledger %v / %v", l, end)
		}
		if est, _ := l[0]["est_usd"].(float64); est != 0 {
			t.Errorf("estimate %v, want 0", est)
		}
		if cost, _ := end[0]["cost_usd"].(float64); cost != 0 || end[0]["created"] != true {
			t.Errorf("end line %v, want cost 0 and created", end[0])
		}
	})
	t.Run("priced", func(t *testing.T) {
		h := harness.New(t)
		e := shRepo(t, h, shOpts{extra: "usd_per_hour = 3.6\ndaily_usd = 100\n"})
		shRanOnSSH(t, h.Run(shEcho...))
		est := e.ledgerNum(t, "launch", "est_usd")
		// start_timeout 2m + timeout 60m + 15m grace = 77 minutes at $3.60/h
		if est < 4.61 || est > 4.63 {
			t.Fatalf("estimate %v, want about 4.62", est)
		}
		if e.ledgerNum(t, "end", "cost_usd") <= 0 {
			t.Errorf("a priced job costs nothing: %v", e.ledger(t, "end"))
		}
	})
	t.Run("a priced section keeps the daily comparison", func(t *testing.T) {
		h := harness.New(t)
		e := shRepo(t, h, shOpts{extra: "usd_per_hour = 3.6\ndaily_usd = 0.01\n"})
		shFellBack(t, e, h.Run(shEcho...), "daily cap $0.01")
		if e.count("prepare") != 0 {
			t.Fatalf("launched over the cap: %v", e.ops())
		}
	})
	t.Run("a list error refuses", func(t *testing.T) {
		h := harness.New(t)
		e := shRepo(t, h, shOpts{})
		e.script(t, sshfake.Rule{Op: "list", Times: -1, Exit: 69, Stderr: "bitc agent: boom\n"})
		shFellBack(t, e, h.Run(shEcho...), "ssh 127.0.0.1: list failed: ")
		if e.count("prepare") != 0 {
			t.Fatalf("launched with the list failing: %v", e.ops())
		}
	})
	t.Run("the breaker pauses it", func(t *testing.T) {
		h := harness.New(t)
		e := shRepo(t, h, shOpts{})
		e.script(t, sshfake.Rule{Op: "probe", Times: 1, Exit: 255, Stderr: "Host key verification failed.\n"})
		shFellBack(t, e, h.Run(shEcho...), "host key verification failed")
		before := len(e.calls())
		r := h.Run(shEcho...)
		if r.Code != 0 || r.Stdout != "on-ssh\n" || !strings.Contains(r.Stderr, "ssh is paused: ") {
			t.Fatalf("second job: %+v", r)
		}
		if len(e.calls()) != before {
			t.Errorf("a paused provider was called: %v", e.ops()[before:])
		}
	})
}

// ---- SH5

func TestSH5_ArchMismatchAndPerArchEligibility(t *testing.T) {
	const tool = "[remote]\ntools = [{ name = \"node\", url = \"https://nodejs.org/dist/node-x64.tar.xz\", sha256 = \"" +
		"0000000000000000000000000000000000000000000000000000000000000000\" }]\n"
	t.Run("the host is another arch", func(t *testing.T) {
		h := harness.New(t)
		e := shRepo(t, h, shOpts{arch: "arm64", hostArch: "amd64", poolRem: "arch = [\"arm64\"]"})
		shFellBack(t, e, h.Run(shEcho...), "host arch amd64, configured arm64")
		if e.count("prepare") != 0 || e.count("install") != 0 {
			t.Fatalf("used a host of the wrong arch: %v", e.ops())
		}
		if e.pauses(t) != 1 {
			t.Errorf("want one pause record, got %d", e.pauses(t))
		}
	})
	t.Run("the pool does not allow the arch", func(t *testing.T) {
		h := harness.New(t)
		e := shRepo(t, h, shOpts{arch: "arm64"})
		shFellBack(t, e, h.Run(shEcho...), `arch arm64 not allowed by pool (arch = ["amd64"])`)
		if len(e.calls()) != 0 {
			t.Fatalf("a network-free check called the host: %v", e.ops())
		}
	})
	t.Run("a tool without an arch entry", func(t *testing.T) {
		h := harness.New(t)
		e := shRepo(t, h, shOpts{arch: "arm64", poolRem: "arch = [\"arm64\"]", repo: tool})
		shFellBack(t, e, h.Run(shEcho...), "tool node has no arm64 entry")
		if len(e.calls()) != 0 {
			t.Fatalf("a network-free check called the host: %v", e.ops())
		}
	})
	t.Run("an arm64 pool on an arm64 host", func(t *testing.T) {
		h := harness.New(t)
		e := shRepo(t, h, shOpts{arch: "arm64", poolRem: "arch = [\"arm64\"]"})
		shRanOnSSH(t, h.Run(shEcho...))
		_ = e
	})
}

// ---- SH6

func TestSH6_PreResourceFailuresMoveOn(t *testing.T) {
	refused := "ssh: connect to host 127.0.0.1 port 22022: Connection refused\n"
	cases := []struct {
		name string
		rule sshfake.Rule
		want string
	}{
		{"connect", sshfake.Rule{Op: "probe", Times: -1, Exit: 255, Stderr: refused}, "connection refused"},
		{"probe exit", sshfake.Rule{Op: "probe", Times: -1, Exit: 69, Stderr: "gate: broken\n"}, "probe"},
		{"probe garbage", sshfake.Rule{Op: "probe", Times: -1, Exit: 0, Stdout: "not json\n"}, "probe"},
		{"install", sshfake.Rule{Op: "install", Times: -1, Exit: 69, Stderr: "no space left\n"}, "install"},
		{"check-host", sshfake.Rule{Op: "check-host", Times: -1, Exit: 69, Stdout: "fail ssh-host-mounts: /Users (virtiofs) visible; disable host file sharing\n"}, "fail ssh-host-mounts: /Users"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := harness.New(t)
			e := shRepo(t, h, shOpts{})
			e.script(t, c.rule)
			shFellBack(t, e, h.Run(shEcho...), c.want)
			if e.count("prepare") != 0 || e.count("run") != 0 || e.count("teardown") != 0 {
				t.Fatalf("a pre-resource failure reached the slot: %v", e.ops())
			}
			if len(e.reservations(t)) != 0 || !e.free(t, 1) {
				t.Errorf("traces left: reservations %v", e.reservations(t))
			}
		})
	}
	t.Run("a check-host failure is a pause", func(t *testing.T) {
		h := harness.New(t)
		e := shRepo(t, h, shOpts{})
		e.script(t, cases[4].rule)
		h.Run(shEcho...)
		if e.pauses(t) != 1 || e.failures(t) != 0 {
			t.Errorf("pauses %d, failures %d, want 1 and 0", e.pauses(t), e.failures(t))
		}
	})
	t.Run("an unreachable host is no breaker record", func(t *testing.T) {
		h := harness.New(t)
		e := shRepo(t, h, shOpts{})
		e.script(t, cases[0].rule)
		h.Run(shEcho...)
		if e.pauses(t) != 0 || e.failures(t) != 0 {
			t.Errorf("pauses %d, failures %d, want none", e.pauses(t), e.failures(t))
		}
	})
}

func TestSH6_APrepareFailurePastSessionSetupIsPreAttach(t *testing.T) {
	// the owner file exists on the host when the failure comes (After): the slot must be torn down by name
	boom := sshfake.Rule{Op: "prepare", Times: 1, Exit: 69, Stderr: "bitc agent: boom\n", After: true}
	t.Run("one failure: torn down, retried once on a fresh name", func(t *testing.T) {
		h := harness.New(t)
		e := shRepo(t, h, shOpts{})
		e.script(t, boom)
		shRanOnSSH(t, h.Run(shEcho...))
		if e.count("prepare") != 2 || e.count("teardown") < 1 {
			t.Fatalf("ops %v, want two prepares and a teardown between", e.ops())
		}
		prep, td := e.verbs("prepare"), e.verbs("teardown")
		name1 := strings.Fields(prep[0])[5]
		if !strings.HasSuffix(td[0], " "+name1) {
			t.Errorf("teardown %q is not by the first attempt's name %q", td[0], name1)
		}
		if name2 := strings.Fields(prep[1])[5]; name2 == name1 {
			t.Errorf("the retry reused the name %q", name1)
		}
		if ends := e.ledger(t, "end"); len(ends) != 2 || ends[0]["exit"] != "fallback" || ends[1]["exit"] != "0" {
			t.Errorf("ledger ends %v", ends)
		}
		if e.failures(t) != 1 {
			t.Errorf("the discarded slot is one breaker failure, got %d", e.failures(t))
		}
	})
	t.Run("two failures: next provider", func(t *testing.T) {
		h := harness.New(t)
		e := shRepo(t, h, shOpts{})
		b2 := boom
		b2.Times = 2
		e.script(t, b2)
		shFellBack(t, e, h.Run(shEcho...), "torn down before attach")
		if e.count("prepare") != 2 || e.count("run") != 0 {
			t.Fatalf("ops %v", e.ops())
		}
	})
	t.Run("under resource: 75, torn down, no retry", func(t *testing.T) {
		h := harness.New(t)
		e := shRepo(t, h, shOpts{extra: "fallback_until = \"resource\"\n"})
		e.script(t, boom)
		r := h.Run("run", "heavy", "--", "sh", "-c", "echo ran > ran-locally")
		if r.Code != 75 || !strings.Contains(r.Stderr, "STARVED heavy (ssh: ") || ranLocally(e.fbEnv) {
			t.Fatalf("%+v", r)
		}
		if e.count("prepare") != 1 || e.count("teardown") != 1 {
			t.Fatalf("ops %v", e.ops())
		}
	})
	t.Run("a package missing on the host is ineligible", func(t *testing.T) {
		h := harness.New(t)
		e := shRepo(t, h, shOpts{repo: "[remote]\npackages = [\"libfoo\"]\n"})
		e.script(t, sshfake.Rule{Op: "prepare", Times: -1, Exit: 75, After: true, Stdout: "provision: installing 1 packages and 0 tools\nsetup step packages failed: package libfoo not installed on this host\n"})
		shFellBack(t, e, h.Run(shEcho...), "package libfoo not installed on 127.0.0.1")
		if e.count("teardown") != 1 {
			t.Fatalf("ops %v", e.ops())
		}
	})
}

// ---- SH9

func TestSH9_PerJobValuesAndTheSendEnvCheck(t *testing.T) {
	repo := "[remote]\nforward_env = [\"DEPLOY_TOKEN\", \"CI\"]\n"
	t.Run("a forced name is ineligible at the default level, with no connect", func(t *testing.T) {
		h := harness.New(t)
		e := shRepo(t, h, shOpts{repo: repo})
		r := h.RunWith([]string{"DEPLOY_TOKEN=forced-canary-1", "CI=1"}, shEcho...)
		if r.Code != 0 || !strings.Contains(r.Stderr, `bitc: ssh: running heavy locally: forced env not sent to ssh (providers.ssh.send_env = "forwarded"): DEPLOY_TOKEN`) {
			t.Fatalf("%+v", r)
		}
		if len(e.calls()) != 0 {
			t.Fatalf("connected to the host: %v", e.ops())
		}
	})
	t.Run("send_env none refuses any forwarded name", func(t *testing.T) {
		h := harness.New(t)
		e := shRepo(t, h, shOpts{repo: repo, extra: "send_env = \"none\"\n"})
		r := h.RunWith([]string{"CI=1"}, shEcho...)
		if r.Code != 0 || !strings.Contains(r.Stderr, `forwarded env not sent to ssh (providers.ssh.send_env = "none"): CI`) {
			t.Fatalf("%+v", r)
		}
		if len(e.calls()) != 0 {
			t.Fatalf("connected to the host: %v", e.ops())
		}
		// and a job with no forwarded name runs
		shRanOnSSH(t, h.Run(shEcho...))
	})
	t.Run("a plain name crosses at the default level", func(t *testing.T) {
		h := harness.New(t)
		shRepo(t, h, shOpts{repo: repo})
		r := h.RunWith([]string{"CI=plain-value"}, "run", "heavy", "--", "sh", "-c", "echo $CI")
		if r.Code != 0 || r.Stdout != "plain-value\n" || strings.Contains(r.Stderr, "locally") {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("canaries reach the host only inside the run payload", func(t *testing.T) {
		h := harness.New(t)
		e := shRepo(t, h, shOpts{repo: repo, extra: "send_env = \"forced\"\n", seams: []string{"fixed_secrets = true"}})
		r := h.RunWith([]string{"DEPLOY_TOKEN=forced-canary-1", "CI=1"}, "run", "heavy", "--", "sh", "-c", "echo $DEPLOY_TOKEN")
		if r.Code != 0 || r.Stdout != "forced-canary-1\n" || strings.Contains(r.Stderr, "locally") {
			t.Fatalf("%+v", r)
		}
		canaries := []string{"forced-canary-1", strings.Repeat("0b", 32), strings.Repeat("0c", 32), strings.Repeat("0d", 32)}
		for _, c := range e.calls() {
			for _, v := range canaries {
				for _, form := range shForms(v) {
					if strings.Contains(c.Verb, form) {
						t.Errorf("%q is in the argv of %s: %s", v, c.Op, c.Verb)
					}
					if c.Op != "run" && strings.Contains(c.Data, form) {
						t.Errorf("%q crossed in %s, before the run payload", v, c.Op)
					}
				}
			}
		}
		if run := e.verbs("run"); len(run) != 1 {
			t.Fatalf("run verbs %v", run)
		}
		var data string
		for _, c := range e.calls() {
			if c.Op == "run" {
				data = c.Data
			}
		}
		if !strings.Contains(data, "forced-canary-1") {
			t.Errorf("the run payload does not carry the forwarded value")
		}
	})
}

// shForms is v raw, hex and base64.
func shForms(v string) []string {
	return []string{v, hex.EncodeToString([]byte(v)), base64.StdEncoding.EncodeToString([]byte(v))}
}

// ---- SH12

func TestSH12_TheRunPayloadIsOutAndTheLinkFails(t *testing.T) {
	cases := []struct {
		name string
		rule sshfake.Rule
	}{
		{"the connection drops", sshfake.Rule{Op: "run", Times: -1, Read: true, Exit: 255, Stderr: "client_loop: send disconnect: Broken pipe\n"}},
		{"no started acknowledgement", sshfake.Rule{Op: "run", Times: -1, Read: true, Exit: 0}},
		{"truncated payload refused", sshfake.Rule{Op: "run", Times: -1, Read: true, Exit: 65, Stdout: "refused: truncated payload\n"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := harness.New(t)
			e := shRepo(t, h, shOpts{fake: true, order: `"ssh", "fake", "local"`})
			e.script(t, c.rule)
			r := h.Run("run", "heavy", "--", "sh", "-c", "echo ran > ran-locally")
			if r.Code != 75 || !strings.Contains(r.Stderr, "STARVED heavy (ssh: ") || !strings.Contains(r.Stderr, " after attach)") ||
				strings.Contains(r.Stderr, "trying") || strings.Contains(r.Stderr, "locally") || ranLocally(e.fbEnv) {
				t.Fatalf("%+v", r)
			}
			prep, td := e.verbs("prepare"), e.verbs("teardown")
			if len(prep) != 1 || len(td) != 1 || e.count("run") != 1 {
				t.Fatalf("ops %v, want one prepare, one run and a teardown", e.ops())
			}
			if name := strings.Fields(prep[0])[5]; !strings.HasSuffix(td[0], " "+name) {
				t.Errorf("teardown %q is not by name %q", td[0], name)
			}
			if _, err := os.Stat(filepath.Join(h.Root, "fakestate", "attempts")); err == nil {
				t.Error("the next provider was tried after attach")
			}
			if !e.free(t, 1) {
				t.Error("the slot lock is held after the walk ended")
			}
		})
	}
}

// ---- SH13

func TestSH13_ARunThatNeverReachedTheHostIsPreAttach(t *testing.T) {
	lines := map[string]string{
		"refused":  "ssh: connect to host 127.0.0.1 port 22022: Connection refused\n",
		"timeout":  "ssh: connect to host 127.0.0.1 port 22022: Connection timed out\n",
		"host key": "Host key verification failed.\n",
	}
	for _, name := range []string{"refused", "timeout", "host key"} {
		stderr := lines[name]
		t.Run(name+" once: torn down, then retried", func(t *testing.T) {
			h := harness.New(t)
			e := shRepo(t, h, shOpts{})
			e.script(t, sshfake.Rule{Op: "run", Times: 1, Read: true, Exit: 255, Stderr: stderr})
			r := h.Run(shEcho...)
			if name == "host key" {
				// never retried: the next provider, after one pause record
				shFellBack(t, e, r, "torn down before attach")
				if e.pauses(t) != 1 {
					t.Errorf("pauses %d, want 1", e.pauses(t))
				}
			} else {
				shRanOnSSH(t, r)
				if e.count("prepare") != 2 || e.count("run") != 2 {
					t.Fatalf("ops %v, want the attempt retried once", e.ops())
				}
			}
			if td := e.verbs("teardown"); len(td) < 1 || !strings.HasSuffix(td[0], " "+strings.Fields(e.verbs("prepare")[0])[5]) {
				t.Errorf("teardown verbs %q, want the first attempt by name", td)
			}
		})
		t.Run(name+" always: the next provider", func(t *testing.T) {
			h := harness.New(t)
			e := shRepo(t, h, shOpts{})
			e.script(t, sshfake.Rule{Op: "run", Times: -1, Read: true, Exit: 255, Stderr: stderr})
			shFellBack(t, e, h.Run(shEcho...), "torn down before attach")
			if strings.Contains(h.Run(plain...).Stderr, "STARVED") {
				t.Error("75 after a pre-attach failure")
			}
		})
	}
	t.Run("under resource it is 75", func(t *testing.T) {
		h := harness.New(t)
		e := shRepo(t, h, shOpts{extra: "fallback_until = \"resource\"\n"})
		e.script(t, sshfake.Rule{Op: "run", Times: 1, Read: true, Exit: 255, Stderr: lines["refused"]})
		r := h.Run("run", "heavy", "--", "sh", "-c", "echo ran > ran-locally")
		if r.Code != 75 || ranLocally(e.fbEnv) || e.count("teardown") != 1 {
			t.Fatalf("%+v, ops %v", r, e.ops())
		}
	})
}

// ---- SH19

func TestSH19_ServicesAndRootStepsAreIneligible(t *testing.T) {
	svc := "[remote]\nservices = [{ name = \"db\", argv = [\"true\"], ready = [\"true\"] }]\n"
	root := "[remote]\nsetup = [{ name = \"prep\", run_as = \"root\", argv = [\"true\"] }]\n"
	grant := "\n[[repos]]\nmatch = \"**/app\"\nroot_steps = true\n"
	for _, c := range []struct{ name, repo, host, want string }{
		{"services", svc, "", "services are not run on ssh hosts"},
		{"root steps", root, grant, "root steps are not run on ssh hosts"},
	} {
		t.Run(c.name+" with local next", func(t *testing.T) {
			h := harness.New(t)
			e := shRepo(t, h, shOpts{repo: c.repo, host: c.host})
			shFellBack(t, e, h.Run(shEcho...), "running heavy locally: "+c.want)
			if len(e.calls()) != 0 {
				t.Fatalf("connected to the host: %v", e.ops())
			}
		})
		t.Run(c.name+" with a remote provider next", func(t *testing.T) {
			h := harness.New(t)
			e := shRepo(t, h, shOpts{repo: c.repo, host: c.host, fake: true, order: `"ssh", "fake", "local"`})
			r := h.Run(shEcho...)
			// core's D3 line for an ineligible job whose next provider is remote
			if r.Code != 0 || !strings.Contains(r.Stderr, "bitc: ssh: heavy: not eligible for remote: "+c.want) || !strings.Contains(r.Stderr, "bitc: fake: heavy job") {
				t.Fatalf("want the not-eligible note, then the next provider: %+v", r)
			}
			if len(e.calls()) != 0 {
				t.Fatalf("connected to the host: %v", e.ops())
			}
		})
	}
}

// ---- SH21

func TestSH21_APrepareThatNeverGotPastSessionSetup(t *testing.T) {
	for name, line := range map[string]string{
		"connect timeout": "ssh: connect to host 127.0.0.1 port 22022: Connection timed out\n",
		"kex reset":       "kex_exchange_identification: read: Connection reset by peer\n",
	} {
		t.Run(name, func(t *testing.T) {
			h := harness.New(t)
			e := shRepo(t, h, shOpts{fake: true, order: `"ssh", "fake", "local"`})
			e.script(t, sshfake.Rule{Op: "prepare", Times: -1, Exit: 255, Stderr: line})
			p := h.Start("run", "heavy", "--", "sh", "-c", "sleep 3; echo on-ssh")
			// the first job runs on the next provider; its ssh slot lock was released when the walk moved on
			h.Until(func() bool { _, err := os.Stat(filepath.Join(h.Root, "fakestate", "attempts")); return err == nil }, 30*time.Second)
			if !e.free(t, 1) {
				t.Error("the slot lock is still held while the job runs on the next provider")
			}
			if r := p.Wait(); r.Code != 0 || r.Stdout != "on-ssh\n" || !strings.Contains(r.Stderr, "bitc: ssh: heavy: trying fake: ") {
				t.Fatalf("%+v", r)
			}
			if e.count("teardown") != 0 || len(e.reservations(t)) != 0 {
				t.Errorf("no teardown, no orphan: ops %v, reservations %v", e.ops(), e.reservations(t))
			}
			if e.failures(t) != 0 || e.pauses(t) != 0 {
				t.Errorf("no breaker record: failures %d, pauses %d", e.failures(t), e.pauses(t))
			}
			if e.count("prepare") != 1 {
				t.Errorf("a pre-resource failure is not retried: %v", e.ops())
			}
		})
	}
	t.Run("an on-demand host that is off", func(t *testing.T) {
		h := harness.New(t)
		e := shRepo(t, h, shOpts{})
		e.script(t, sshfake.Rule{Op: "probe", Times: -1, Exit: 255, Stderr: "ssh: connect to host 127.0.0.1 port 22022: Connection refused\n"},
			sshfake.Rule{Op: "list", Times: -1, Exit: 255, Stderr: "ssh: connect to host 127.0.0.1 port 22022: Connection refused\n"})
		for i := 0; i < 4; i++ { // more than breaker.failures: a sleeping host never trips it
			shFellBack(t, e, h.Run(shEcho...), "connection refused")
		}
		if e.failures(t) != 0 || e.pauses(t) != 0 || strings.Contains(h.Run(plain...).Stderr, "paused") {
			t.Errorf("an unreachable host left breaker records: failures %d, pauses %d", e.failures(t), e.pauses(t))
		}
	})
	t.Run("a host key mismatch is a pause", func(t *testing.T) {
		h := harness.New(t)
		e := shRepo(t, h, shOpts{})
		e.script(t, sshfake.Rule{Op: "prepare", Times: 1, Exit: 255, Stderr: "Host key verification failed.\n"})
		shFellBack(t, e, h.Run(shEcho...), "host key verification failed")
		if e.pauses(t) != 1 || e.failures(t) != 0 || e.count("teardown") != 0 {
			t.Errorf("pauses %d, failures %d, ops %v", e.pauses(t), e.failures(t), e.ops())
		}
	})
}

var _ = sha256.Sum256
