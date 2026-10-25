package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Exploded-views/bitc/internal/config"
	"github.com/Exploded-views/bitc/internal/provider"
	"github.com/Exploded-views/bitc/internal/provider/plugin/pin"
	"github.com/Exploded-views/bitc/internal/redact"
)

var stubBin string

// TestMain builds the stub plugin once, and doubles as the __plugin-exec shim: the adapter starts
// plugins as `/proc/self/exe __plugin-exec <path>`, which in this process is the test binary.
func TestMain(m *testing.M) {
	if len(os.Args) > 2 && os.Args[1] == "__plugin-exec" {
		os.Exit(ExecShim(os.Args[2:]))
	}
	dir, err := os.MkdirTemp("", "plugin-stub-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	stubBin = filepath.Join(dir, "stub")
	build := exec.Command("go", "build", "-trimpath", "-o", stubBin, "github.com/Exploded-views/bitc/internal/provider/plugin/stubplugin")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "build stubplugin: %v\n%s", err, out)
		os.Exit(2)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// rig is one adapter over the stub plugin in a private directory.
type rig struct {
	t                *testing.T
	dir, bin         string
	state, acct, own string // state dir, accounting dir, <state>/plugins/stub
	core             *config.Provider
	eff              *config.Effective
	p                *Provider
	red              *redact.Set
	log              *syncBuf
	debug            bool
}

type rigOpt func(*rig)

func raw(k string, v any) rigOpt { return func(r *rig) { r.core.Raw[k] = v } }
func fault(f string) rigOpt      { return raw("fault", f) }

func newRig(t *testing.T, opts ...rigOpt) *rig {
	t.Helper()
	dir := t.TempDir()
	r := &rig{t: t, dir: dir, state: filepath.Join(dir, "state"), acct: filepath.Join(dir, "account"), red: redact.New(), log: &syncBuf{}}
	r.own = filepath.Join(r.state, "plugins", "stub")
	r.bin = filepath.Join(dir, "bitc-provider-stub")
	b, err := os.ReadFile(stubBin)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(r.bin, b, 0o755); err != nil {
		t.Fatal(err)
	}
	sum, _ := pin.SumFile(r.bin)
	r.core = &config.Provider{Type: "stub", NamePrefix: "bitc", APIKey: "sk-test-key-1", DailyUSD: 5_000_000, MaxLive: 2,
		LaunchWait: time.Minute, StartTimeout: 2 * time.Minute, Breaker: config.Breaker{Failures: 3, Window: time.Hour},
		FallbackUntil: "resource", FallbackCap: "attach",
		Plugin:   &config.PluginPin{Path: r.bin, SHA256: sum, EnvPass: []string{"STUB_FALLBACK", "STUB_DECLARE", "STUB_HELLO_FAULT", "STUB_SPEND", "STUB_MULTI_JOB"}, SecretFiles: map[string]string{}},
		Sizes:    map[string]config.Micros{"cpu8": 600_000},
		SizesRaw: map[string]any{"cpu8": map[string]any{"usd_per_hour": 0.6}},
		Raw:      map[string]any{"region": "eu"}}
	r.eff = &config.Effective{Host: config.Host{StateDir: r.state}, Test: &config.Test{AccountingDir: r.acct, TimeScale: 0.02}}
	for _, o := range opts {
		o(r)
	}
	r.p, err = New(r.core, r.eff)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.p.Close() })
	return r
}

func (r *rig) rt() provider.Runtime {
	return provider.Runtime{Red: r.red, Stderr: r.log, Debug: r.debug}
}

func (r *rig) start() {
	r.t.Helper()
	if err := r.p.Start(context.Background(), r.rt()); err != nil {
		r.t.Fatalf("Start: %v", err)
	}
}

func (r *rig) startErr() error { return r.p.Start(context.Background(), r.rt()) }

// job is a launchable job with a name and owner as core would set them.
func (r *rig) job() *provider.JobSpec {
	exp := time.Now().Add(time.Hour).Truncate(time.Second)
	name := provider.ResourceNameMax("bitc", "0123456789abcdef", "heavy", "cpu8", "job-1", exp, 63)
	var b, k, m [32]byte
	for i := range b {
		b[i], k[i], m[i] = byte(0x10+i), byte(0x40+i), byte(0x80+i)
	}
	return &provider.JobSpec{Job: "job-1", Pool: "heavy", Argv: []string{"echo", "x"}, SizeKey: "cpu8", Timeout: time.Hour, Created: time.Now(),
		Deadline: exp.Add(-15 * time.Minute), Expiry: exp, Bearer: b, MacKey: k, Nonce: m, Install: "0123456789abcdef", Name: name,
		Owner: provider.OwnerTag([]byte("install-secret"), name), AgentURL: "https://example.com/agent", AgentSHA256: strings.Repeat("ab", 32)}
}

func (r *rig) read(name string) []byte {
	r.t.Helper()
	b, err := os.ReadFile(filepath.Join(r.own, name))
	if err != nil {
		r.t.Fatalf("%s: %v", name, err)
	}
	return b
}

func (r *rig) exists(name string) bool {
	_, err := os.Stat(filepath.Join(r.own, name))
	return err == nil
}

type diagT struct {
	Env        []string `json:"env"`
	Args       []string `json:"args"`
	Tmpdir     string   `json:"tmpdir"`
	Pid, Pgid  int
	NoNewPrivs string   `json:"nonewprivs"`
	Fds        []string `json:"fds"`
	Secrets    string   `json:"secrets"`
	Cmdline    string   `json:"cmdline"`
	Exe        string   `json:"exe"`
}

func (r *rig) diag() diagT {
	r.t.Helper()
	var d diagT
	if err := json.Unmarshal(r.read("diag.json"), &d); err != nil {
		r.t.Fatal(err)
	}
	var m map[string]any
	_ = json.Unmarshal(r.read("diag.json"), &m)
	d.Pid, d.Pgid = int(m["pid"].(float64)), int(m["pgid"].(float64))
	return d
}

// resources is the stub's own view of what it holds.
func (r *rig) resources() []map[string]any {
	var rs []map[string]any
	if b, err := os.ReadFile(filepath.Join(r.own, "res.json")); err == nil {
		_ = json.Unmarshal(b, &rs)
	}
	return rs
}

func (r *rig) torn() string {
	b, _ := os.ReadFile(filepath.Join(r.own, "torn.log"))
	return string(b)
}

// recSink records what Watch reports.
type recSink struct {
	mu     sync.Mutex
	phases []string
	setup  []string
	out    map[provider.Stream]*bytes.Buffer
}

func newSink() *recSink {
	return &recSink{out: map[provider.Stream]*bytes.Buffer{provider.Stdout: {}, provider.Stderr: {}}}
}
func (s *recSink) Phase(p provider.Phase, why string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.phases = append(s.phases, string(p))
}
func (s *recSink) Setup(l string) { s.mu.Lock(); defer s.mu.Unlock(); s.setup = append(s.setup, l) }
func (s *recSink) Out(st provider.Stream, b []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.out[st].Write(b)
}

var _ io.Writer = (*syncBuf)(nil)
