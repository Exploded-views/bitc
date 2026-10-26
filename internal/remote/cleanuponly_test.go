package remote

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/Exploded-views/bitc/internal/config"
	"github.com/Exploded-views/bitc/internal/provider"
	_ "github.com/Exploded-views/bitc/internal/provider/sprites"
	"github.com/Exploded-views/bitc/internal/run"
)

// Defense in depth: a CleanupOnly core (Sprites in a cleanup) never becomes a whole provider,
// through NewProvider (sweep, doctor, status) or through Walk (a job), whatever loaded it.
func TestCleanupOnlyCoreBuildsNoProvider(t *testing.T) {
	raw := map[string]any{"org": "o", "api_base": "http://127.0.0.1:9", "sizes": map[string]any{"cpu8": map[string]any{"memory": "16GiB"}}}
	core := &config.Provider{Type: "sprites", NamePrefix: "bitc", APIKey: "o/token", Raw: raw, CleanupOnly: true}
	eff := &config.Effective{Providers: map[string]*config.Provider{"sprites": core}}
	p, err := NewProvider(context.Background(), "sprites", core, eff, provider.Runtime{})
	if p != nil || err == nil || !strings.Contains(err.Error(), "sprites jobs are refused in this release") {
		t.Fatalf("NewProvider: %v, %v", p, err)
	}
	_, handled, err := Walk(context.Background(), run.Invocation{}, &run.Prepared{Eff: eff}, "sprites", io.Discard)
	var inel *provider.ErrIneligible
	if handled || !errors.As(err, &inel) || !strings.Contains(err.Error(), "sprites jobs are refused in this release") {
		t.Fatalf("Walk: handled %v, %v", handled, err)
	}
}
