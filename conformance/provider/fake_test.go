package provider

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/Exploded-views/bitc/conformance/harness"
	"github.com/Exploded-views/bitc/internal/config"
	"github.com/Exploded-views/bitc/internal/provider"
	_ "github.com/Exploded-views/bitc/internal/provider/fake"
)

// fakeTarget drives the test-only fake provider by rewriting its mode key.
type fakeTarget struct {
	h     *harness.H
	state string // the fake's state_dir (torn.log)
}

func (f *fakeTarget) Name() string { return "fake" }

func (f *fakeTarget) Configure(t *testing.T, h *harness.H) {
	f.h, f.state = h, filepath.Join(h.Root, "fakestate")
	if err := os.MkdirAll(f.state, 0o700); err != nil {
		t.Fatal(err)
	}
	h.HostConfig("[remote_limits]\nworktrees = [\"**/app\"]\nforward_env_allow = [\"CANARY_VALUE\"]\n" +
		"[pools.heavy]\nproviders = [\"fake\", \"local\"]\nremote_limits = { size = { fake = \"small\" } }\nwait = \"5s\"\n" +
		"[providers.fake]\ndaily_usd = 1\nmax_live = 6\nfallback_until_cap = \"attach\"\nstate_dir = " + harness.Q(f.state) + "\nmode = \"run\"\n")
	h.Cwd = h.Repo("app", map[string]string{".bitc.toml": "version = 1\n[pools.heavy.remote]\nallow = [[\"sh\", \"*\"], [\"echo\", \"*\"]]\n[remote]\nforward_env = [\"CANARY_VALUE\"]\n"})
}

var modeRE = regexp.MustCompile(`(?m)^mode = ".*"$`)

func (f *fakeTarget) Behave(t *testing.T, b string) {
	mode := map[string]string{"ok": "run", "create-fails": "transient", "setup-fails-after-create": "setup-fails",
		"setup-fails-repo-after-create": "setup-fails-repo", "lost-after-begin": "lost", "preattach-fails": "preattach-fails",
		"paginate-2": ""}[b]
	if mode == "" && b != "paginate-2" {
		t.Fatalf("the fake provider cannot produce %q", b)
	}
	cfg, err := os.ReadFile(f.h.Config)
	if err != nil {
		t.Fatal(err)
	}
	if b == "paginate-2" {
		f.h.Write(f.h.Config, modeRE.ReplaceAllStringFunc(string(cfg), func(m string) string { return m + "\nlist_page = 2" }), 0o600)
		return
	}
	f.h.Write(f.h.Config, modeRE.ReplaceAllString(string(cfg), "mode = "+harness.Q(mode)), 0o600)
}

func (f *fakeTarget) Calls() []string { return nil } // no API: it runs the job in-process

func (f *fakeTarget) TornDown() []string {
	b, _ := os.ReadFile(filepath.Join(f.state, "torn.log"))
	return strings.Fields(string(b))
}

func (f *fakeTarget) Supports(s string) bool {
	return map[string]bool{"launch": true, "fallback": true, "setup-fails-after-create": true,
		"setup-fails-repo-after-create": true, "lost": true, "cap": true, "breaker": true}[s]
}

func TestPV5_ProviderSuiteOnFake(t *testing.T) { RunAll(t, &fakeTarget{}, harness.New) }

func (f *fakeTarget) SetSpend(t *testing.T, h *harness.H, usd float64) {
	cfg, err := os.ReadFile(h.Config)
	if err != nil {
		t.Fatal(err)
	}
	add := func(m string) string { return m + "\nspend_usd = " + strconv.FormatFloat(usd, 'f', 2, 64) }
	h.Write(h.Config, modeRE.ReplaceAllStringFunc(string(cfg), add), 0o600)
}

func (f *fakeTarget) DeclaresAttach() bool { return true }

func (f *fakeTarget) APICalls(t *testing.T) []APICall {
	var out []APICall
	for _, l := range strings.Split(strings.TrimSpace(readFile(filepath.Join(f.state, "calls.jsonl"))), "\n") {
		var c struct {
			Op     string
			Attach bool
			Data   string
		}
		if json.Unmarshal([]byte(l), &c) == nil && c.Op != "" {
			out = append(out, APICall{Op: c.Op, Attach: c.Attach, Data: c.Data})
		}
	}
	return out
}

// Canaries: the bearer, results key and nonce of every launch (hex), as the fake received them.
func (f *fakeTarget) Canaries(t *testing.T) []string {
	return strings.Fields(readFile(filepath.Join(f.state, "canaries.log")))
}

func readFile(p string) string { b, _ := os.ReadFile(p); return string(b) }

func (f *fakeTarget) ListNames(t *testing.T, _ *harness.H) []string {
	core := &config.Provider{Type: "fake", NamePrefix: "bitc", Raw: map[string]any{"state_dir": f.state, "list_page": int64(2)}}
	p, err := provider.Get("fake")
	if !err {
		t.Fatal("no fake provider")
	}
	prov, nerr := p.New(core, &config.Effective{})
	if nerr != nil {
		t.Fatal(nerr)
	}
	rs, lerr := prov.List(context.Background(), []string{"bitc-"})
	if lerr != nil {
		t.Fatal(lerr)
	}
	var names []string
	for _, r := range rs {
		names = append(names, r.Name)
	}
	return names
}

func (f *fakeTarget) LiveNames(t *testing.T) []string {
	var rs []struct{ Name string }
	_ = json.Unmarshal([]byte(readFile(filepath.Join(f.state, "resources.json"))), &rs)
	var names []string
	for _, r := range rs {
		names = append(names, r.Name)
	}
	return names
}

// FB15 (design A.10.2): a hex canary (B, K, M from canaries.log) must also be caught as base64 of
// its raw bytes, and as the raw bytes themselves.
func TestFB15_CanaryFormsCoverTheRawBytesOfAHexCanary(t *testing.T) {
	raw := []byte("0123456789abcdef0123456789abcdef")
	hexed := hex.EncodeToString(raw)
	forms := canaryForms(hexed)
	for name, want := range map[string]string{
		"raw bytes":      string(raw),
		"hex text":       hexed,
		"std":            base64.StdEncoding.EncodeToString(raw),
		"std unpadded":   base64.RawStdEncoding.EncodeToString(raw),
		"url":            base64.URLEncoding.EncodeToString(raw),
		"url unpadded":   base64.RawURLEncoding.EncodeToString(raw),
		"base64 of text": base64.StdEncoding.EncodeToString([]byte(hexed)),
	} {
		if !slices.Contains(forms, want) {
			t.Errorf("%s: %q is not among the forms %q", name, want, forms)
		}
	}
	if got := canaryForms("ARGV-CANARY-7f3a9c"); len(got) != 6 {
		t.Errorf("a non-hex canary has %d forms, want 6", len(got))
	}
}
