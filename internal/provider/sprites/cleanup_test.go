package sprites

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Exploded-views/bitc/internal/config"
	"github.com/Exploded-views/bitc/internal/provider"
	"github.com/Exploded-views/bitc/internal/provider/sprites/spritesfake"
	"github.com/Exploded-views/bitc/internal/state"
)

// The cleanup-only handle lists, checks owner tags from the name (no call) and deletes; the
// factory builds it, and only list, delete (and nothing else) reach the API.
func TestCleanerListsOwnerChecksAndDeletesOnly(t *testing.T) {
	f := spritesfake.New(t, spritesfake.Options{})
	acct := t.TempDir()
	if err := os.Chmod(acct, 0o700); err != nil { // a private accounting dir, as state requires
		t.Fatal(err)
	}
	secret, err := state.InstallSecret(acct)
	if err != nil {
		t.Fatal(err)
	}
	eff := &config.Effective{Test: &config.Test{TimeScale: 0.05, AccountingDir: acct}}
	raw := map[string]any{"org": f.Org(), "api_base": f.URL(), "sizes": map[string]any{"cpu8": map[string]any{"memory": "16GiB"}}}
	core := &config.Provider{Type: "sprites", NamePrefix: "bitc", APIKey: f.Token(), Raw: raw, StartTimeout: time.Minute, CleanupOnly: true}
	fac, _ := provider.Get("sprites")
	c, err := fac.NewCleaner(core, eff)
	if err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-time.Hour)
	own := EncodeName(secret, "bitc", state.Install16(secret), "cpu8", "0000aaaa", past)
	forged := own[:len(own)-1] + map[bool]string{true: "1", false: "0"}[!strings.HasSuffix(own, "1")]
	f.AddSprite(own, f.Org(), past)
	f.AddSprite(forged, f.Org(), past)
	ctx := context.Background()
	list, err := c.List(ctx, []string{"bitc-"})
	if err != nil || len(list) != 2 || list[0].Install != state.Install16(secret) {
		t.Fatalf("list %+v %v", list, err)
	}
	if tag, err := c.Owner(ctx, own); err != nil || !provider.OwnerMatches(secret, own, tag) {
		t.Fatalf("own: %q %v", tag, err)
	}
	if tag, err := c.Owner(ctx, forged); err != nil || tag != "" {
		t.Fatalf("forged: %q %v", tag, err)
	}
	if err := c.Teardown(ctx, own); err != nil {
		t.Fatal(err)
	}
	if live := f.Live(); len(live) != 1 || live[0] != forged {
		t.Fatalf("live %v", live)
	}
	if sr, ok := c.(provider.SweepRules); !ok || !sr.SweepRules().TimerRequired || sr.SweepRules().GoneAfter != time.Minute {
		t.Fatalf("sweep rules %v", ok)
	}
	if _, ok := c.(provider.Provider); ok {
		t.Fatal("the cleanup handle is a whole provider")
	}
	for _, call := range f.Calls() {
		list := call.Method == "GET" && call.Path == "/v1/sprites"
		del := call.Method == "DELETE" && call.Path == "/v1/sprites/"+own
		if !list && !del {
			t.Errorf("call %s %s", call.Method, call.Path)
		}
	}
	// no token, bad settings: errors, never a handle
	if _, err := NewCleaner(&config.Provider{Type: "sprites", NamePrefix: "bitc", Raw: raw}, eff); err == nil {
		t.Fatal("no token")
	}
	if _, err := NewCleaner(&config.Provider{Type: "sprites", NamePrefix: "bitc", APIKey: f.Token(), Raw: map[string]any{}}, eff); err == nil {
		t.Fatal("no org or sizes")
	}
	if _, err := fac.NewCleaner(&config.Provider{Type: "sprites", NamePrefix: "bitc", Raw: raw}, eff); err == nil {
		t.Fatal("factory, no token")
	}
}

// The cleanup handle's error paths: a 5xx list is retried once, a second one is the error; a
// failed delete is reported; an unusable accounting dir fails the owner check (no tag).
func TestCleanerErrorPaths(t *testing.T) {
	f := spritesfake.New(t, spritesfake.Options{})
	acct := filepath.Join(t.TempDir(), "acct")
	eff := &config.Effective{Test: &config.Test{TimeScale: 0.05, AccountingDir: acct}}
	raw := map[string]any{"org": f.Org(), "api_base": f.URL(), "sizes": map[string]any{"cpu8": map[string]any{"memory": "16GiB"}}}
	c, err := NewCleaner(&config.Provider{Type: "sprites", NamePrefix: "bitc", APIKey: f.Token(), Raw: raw, CleanupOnly: true}, eff)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	f.Behave("list-500")
	if _, err := c.List(ctx, []string{"bitc-"}); err != nil {
		t.Fatalf("one 5xx is retried: %v", err)
	}
	f.BehaveN("list-500", 2)
	if _, err := c.List(ctx, []string{"bitc-"}); err == nil {
		t.Fatal("two 5xx: no error")
	}
	f.AddSprite("bitc-x", f.Org(), time.Now())
	f.Behave("delete-500")
	if err := c.Teardown(ctx, "bitc-x"); err == nil {
		t.Fatal("a failed delete: no error")
	}
	if err := os.WriteFile(acct, []byte("not a dir"), 0o600); err != nil {
		t.Fatal(err)
	}
	if tag, err := c.Owner(ctx, "bitc-x"); err == nil || tag != "" {
		t.Fatalf("unusable accounting dir: %q %v", tag, err)
	}
}

// The factory's endpoint and constructor hooks, and the whole provider's owner check on an
// unusable accounting dir (its install secret cannot be read: no tag).
func TestFactoryHooksAndOwnerWithoutASecret(t *testing.T) {
	f := spritesfake.New(t, spritesfake.Options{})
	fac, _ := provider.Get("sprites")
	raw := map[string]any{"org": f.Org(), "api_base": f.URL(), "sizes": map[string]any{"cpu8": map[string]any{"memory": "16GiB"}}}
	if eps, err := fac.Endpoints(raw); err != nil || len(eps) != 1 || eps[0] != f.URL() {
		t.Fatalf("endpoints %v %v", eps, err)
	}
	acct := filepath.Join(t.TempDir(), "acct")
	if err := os.WriteFile(acct, []byte("not a dir"), 0o600); err != nil {
		t.Fatal(err)
	}
	eff := &config.Effective{Test: &config.Test{TimeScale: 0.05, AccountingDir: acct}}
	p, err := fac.New(&config.Provider{Type: "sprites", NamePrefix: "bitc", APIKey: f.Token(), Raw: raw}, eff)
	if err != nil {
		t.Fatal(err)
	}
	if tag, err := p.Owner(context.Background(), "bitc-x"); err == nil || tag != "" {
		t.Fatalf("owner without a secret: %q %v", tag, err)
	}
	if _, err := NewCleaner(&config.Provider{Type: "sprites", NamePrefix: "bitc", APIKey: f.Token(), Raw: map[string]any{"org": f.Org(), "api_base": "::not a url", "sizes": raw["sizes"]}}, eff); err == nil {
		t.Fatal("a bad api_base: no error")
	}
}
