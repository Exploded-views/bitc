package config

import (
	"testing"
)

func init() {
	RegisterProviderType("fbattach", ProviderType{
		Endpoints:   func(map[string]any) ([]string, error) { return nil, nil },
		FallbackCap: func(map[string]any) string { return "attach" },
	})
	RegisterProviderType("fbresource", ProviderType{
		Endpoints: func(map[string]any) ([]string, error) { return nil, nil },
	})
	RegisterProviderType("fbplugin", ProviderType{
		Endpoints:       func(map[string]any) ([]string, error) { return nil, nil },
		FallbackCap:     func(map[string]any) string { return "attach" },
		DefaultResource: true,
	})
}

func fbHost(typ, extra string) string {
	return "version = 1\n[providers." + typ + "]\ndaily_usd = 1\n" + extra
}

// FB1 (config part): the default is the declaration for a built-in and "resource" for a plugin;
// the host may narrow, and a plugin may opt in up to its declaration.
func TestFallbackUntilDefaultsAndNarrowing(t *testing.T) {
	for _, c := range []struct{ typ, extra, want, cap string }{
		{"fbattach", "", "attach", "attach"},
		{"fbattach", "fallback_until = \"resource\"\n", "resource", "attach"},
		{"fbresource", "", "resource", "resource"},
		{"fbresource", "fallback_until = \"resource\"\n", "resource", "resource"},
		{"fbplugin", "", "resource", "attach"},
		{"fbplugin", "fallback_until = \"attach\"\n", "attach", "attach"},
	} {
		e := mustLoad(t, newFixture(t, fbHost(c.typ, c.extra), ""))
		got := e.Providers[c.typ]
		if got.FallbackUntil != c.want || got.FallbackCap != c.cap {
			t.Errorf("%s %q: effective %q cap %q, want %q cap %q", c.typ, c.extra, got.FallbackUntil, got.FallbackCap, c.want, c.cap)
		}
	}
}

func TestFallbackUntilValidation78(t *testing.T) {
	for _, c := range []struct{ typ, extra, msg string }{
		{"fbattach", "fallback_until = \"later\"\n", `providers.fbattach.fallback_until: must be "resource" or "attach": "later"`},
		{"fbattach", "fallback_until = 3\n", `providers.fbattach.fallback_until: must be "resource" or "attach"`},
		{"fbresource", "fallback_until = \"attach\"\n", `providers.fbresource.fallback_until: "attach" is wider than fbresource allows ("resource")`},
	} {
		_, err := newFixture(t, fbHost(c.typ, c.extra), "").load()
		expect78(t, err, c.msg)
	}
}

func TestFallbackUntilIsHostOnly(t *testing.T) {
	f := newFixture(t, fbHost("fbattach", ""), "version = 1\n[providers.fbattach]\nfallback_until = \"attach\"\n")
	_, err := f.load()
	// a repo file may not hold any [providers] table, and the boundary key is named (spec 8.2)
	expect78(t, err, "providers is a host-only key", "providers.fbattach.fallback_until: host-only key")
}
