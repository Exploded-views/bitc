package provider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Exploded-views/bitc/conformance/harness"
	"github.com/Exploded-views/bitc/internal/config"
	"github.com/Exploded-views/bitc/internal/provider/sprites"
	"github.com/Exploded-views/bitc/internal/provider/sprites/spritesfake"
)

var (
	agentShaOnce sync.Once
	agentShaVal  string
)

// agentSHA is the sha256 of the bitc binary under test: what the host sends as the agent's sha.
func agentSHA(t testing.TB) string {
	t.Helper()
	agentShaOnce.Do(func() {
		b, err := os.ReadFile(harness.Bin)
		if err != nil {
			panic(err)
		}
		s := sha256.Sum256(b)
		agentShaVal = hex.EncodeToString(s[:])
	})
	return agentShaVal
}

// spritesTarget drives the Sprites provider against its fake, with this bitc as the agent in each Sprite.
type spritesTarget struct {
	f *spritesfake.Fake
	h *harness.H
}

func (s *spritesTarget) Name() string { return "sprites" }

func (s *spritesTarget) Configure(t *testing.T, h *harness.H) {
	s.h = h
	s.f = spritesfake.New(t, spritesfake.Options{Bin: harness.Bin, AgentSHA: agentSHA(t), PageSize: 2,
		Exec: func(spritesfake.ExecContext) int { return 0 }}) // the stub that installs the agent: the fake's agent is this binary already
	token := filepath.Join(h.Root, "sprites.token")
	h.Write(token, s.f.Token()+"\n", 0o600)
	h.Seam("time_scale = 0.05")
	h.Seam("agent_url = \"http://127.0.0.1:9/bitc\"")
	h.HostConfig("[remote_limits]\nworktrees = [\"**/app\"]\n" +
		"[pools.heavy]\nslots = 2\nproviders = [\"sprites\", \"local\"]\nremote_limits = { size = { sprites = \"cpu8\" }, timeout = \"60m\" }\nwait = \"5s\"\n" +
		"[providers.sprites]\napi_key_file = " + harness.Q(token) + "\norg = " + harness.Q(s.f.Org()) + "\ndaily_usd = 10\nmax_live = 6\n" +
		"start_timeout = \"2m\"\napi_base = " + harness.Q(s.f.URL()) + "\nsizes = { cpu8 = { memory = \"16GiB\" } }\n")
	h.Cwd = h.Repo("app", map[string]string{".bitc.toml": "version = 1\n[pools.heavy.remote]\nallow = [[\"sh\", \"*\"], [\"echo\", \"*\"]]\n"})
}

func (s *spritesTarget) Behave(t *testing.T, b string) {
	switch b {
	case "ok", "paginate-2": // the fake lists in pages of two from the start
	case "create-fails":
		s.f.BehaveN("429-create", -1) // capacity: transient on every attempt, so the job moves on
	case "setup-fails-after-create": // the agent refuses its boot payload: attached, so the Sprite is deleted and the job is 75
		s.f.Behave("attach-truncate")
	case "setup-fails-repo-after-create":
		s.f.Behave("provision-repo-fails")
	case "lost-after-begin", "restart-after-begin":
		s.f.Behave("cold-drop-mid-job")
	case "drop-log-tail":
		s.f.Behave("drop-tail")
	case "forge-result":
		s.f.Behave("forge-result")
	default:
		t.Fatalf("the Sprites fake cannot produce %q", b)
	}
}

func (s *spritesTarget) Calls() []string {
	var out []string
	for _, c := range s.f.Calls() {
		out = append(out, c.Method+" "+c.Path)
	}
	return out
}

func (s *spritesTarget) TornDown() []string {
	var ids []string
	for _, c := range s.f.Calls() {
		if c.Method == "DELETE" {
			if n, ok := strings.CutPrefix(c.Path, "/v1/sprites/"); ok {
				ids = append(ids, n)
			}
		}
	}
	return ids
}

// Supports: a provisioning failure of the repo's own is simulated (its message is the fake's,
// not the repo's).
func (s *spritesTarget) Supports(sc string) bool {
	return sc != "setup-fails-repo-after-create"
}

// SetSpend: Sprites has no billing API, so the cap rests on the ledger; a daily_usd below any
// launch estimate stands for a spent day.
func (s *spritesTarget) SetSpend(t *testing.T, h *harness.H, _ float64) {
	cfg, err := os.ReadFile(h.Config)
	if err != nil {
		t.Fatal(err)
	}
	h.Write(h.Config, strings.Replace(string(cfg), "daily_usd = 10", "daily_usd = 0.01", 1), 0o600)
}

// Trip: two pre-attach discards (a Sprite and its one retry) are two breaker failures; the job moves on to local.
func (s *spritesTarget) Trip(*testing.T) int { s.f.BehaveN("provision-infra-fails", 2); return 0 }

func (s *spritesTarget) RecoversTail() {}

func (s *spritesTarget) Noun() string { return "sprite" }

func (s *spritesTarget) AddExpired(t *testing.T, h *harness.H) string {
	secret, err := os.ReadFile(filepath.Join(h.Root, "account", "install-secret"))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(secret)
	name := sprites.EncodeName(secret, "bitc", hex.EncodeToString(sum[:])[:16], "cpu8", "0000000a", time.Now().Add(-time.Hour))
	s.f.AddSprite(name, s.f.Org(), time.Now().Add(-3*time.Hour))
	return name
}

func (s *spritesTarget) ListNames(t *testing.T, _ *harness.H) []string {
	core := &config.Provider{Type: "sprites", NamePrefix: "bitc", APIKey: s.f.Token(), Raw: map[string]any{
		"org": s.f.Org(), "api_base": s.f.URL(), "sizes": map[string]any{"cpu8": map[string]any{"memory": "16GiB"}}}}
	p, err := sprites.NewProvider(core, &config.Effective{Test: &config.Test{TimeScale: 0.05, AccountingDir: t.TempDir()}})
	if err != nil {
		t.Fatal(err)
	}
	rs, err := p.List(context.Background(), []string{"bitc-"})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, x := range rs {
		names = append(names, x.Name)
	}
	return names
}

func (s *spritesTarget) LiveNames(*testing.T) []string { return s.f.Live() }

func TestSP22_ProviderSuiteOnSpritesFake(t *testing.T) { RunAll(t, &spritesTarget{}, harness.New) }

func TestPS1_ListCompletenessOnSprites(t *testing.T) {
	tg := &spritesTarget{}
	h := harness.New(t)
	tg.Configure(t, h)
	RunPS1(t, tg, h)
}
