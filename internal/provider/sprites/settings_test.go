package sprites

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Exploded-views/bitc/internal/config"
)

func baseRaw() map[string]any {
	return map[string]any{"org": "sandbox-org", "sizes": map[string]any{"cpu8": map[string]any{"memory": "16GiB"}}}
}

func TestDecodeSettingsDefaults(t *testing.T) {
	s, errs := DecodeSettings(baseRaw(), "bitc")
	if len(errs) != 0 {
		t.Fatalf("errors: %v", errs)
	}
	if s.Org != "sandbox-org" || s.APIBase != "https://api.sprites.dev" || s.DiskBound != 100<<30 || s.MemoryBound != 16<<30 {
		t.Fatalf("defaults: %+v", s)
	}
	if s.Rates != (Rates{CPU: 38500, Memory: 21875, HotStorage: 683, ColdStorage: 27}) {
		t.Fatalf("rates: %+v", s.Rates)
	}
	if !reflect.DeepEqual(s.EgressAllow, []string{"github.com", "codeload.github.com", "objects.githubusercontent.com", "registry.npmjs.org"}) ||
		!reflect.DeepEqual(s.EgressBuildAllow, []string{"archive.ubuntu.com", "security.ubuntu.com", "ports.ubuntu.com"}) {
		t.Fatalf("egress: %v / %v", s.EgressAllow, s.EgressBuildAllow)
	}
	if z := s.Sizes["cpu8"]; z.Memory != 16<<30 {
		t.Fatalf("size: %+v", z)
	}
}

func TestDecodeSettingsEveryValidationCase(t *testing.T) {
	with := func(k string, v any) map[string]any { m := baseRaw(); m[k] = v; return m }
	size := func(t map[string]any) map[string]any {
		return map[string]any{"org": "sandbox-org", "sizes": map[string]any{"cpu8": t}}
	}
	for _, c := range []struct {
		name string
		raw  map[string]any
		want string
	}{
		{"no org", map[string]any{"sizes": baseRaw()["sizes"]}, "org: required"},
		{"empty org", with("org", ""), "org: required"},
		{"no sizes", map[string]any{"org": "sandbox-org"}, "sizes: at least one size with memory is required"},
		{"empty sizes", with("sizes", map[string]any{}), "sizes: at least one size with memory is required"},
		{"size without memory", size(map[string]any{}), "sizes: at least one size with memory is required"},
		{"vcpu 4", size(map[string]any{"memory": "8GiB", "vcpu": int64(4)}), "sizes.cpu8.vcpu: Sprites have 8 vCPU (got 4)"},
		{"memory over the bound", size(map[string]any{"memory": "32GiB"}), "sizes.cpu8.memory: exceeds memory_bound 16GiB"},
		{"bad memory", size(map[string]any{"memory": "lots"}), "sizes.cpu8.memory: must be a size like 512MiB or 6GiB (units KiB MiB GiB TiB)"},
		{"unknown size key", size(map[string]any{"memory": "8GiB", "ram": "1GiB"}), "sizes.cpu8.ram: unknown key"},
		{"star in egress", with("egress_allow", []any{"*"}), `egress_allow: "*" not allowed (explicit domains only)`},
		{"include in egress", with("egress_allow", []any{"include"}), `egress_allow: "include" not allowed (explicit domains only)`},
		{"ip in build egress", with("egress_build_allow", []any{"10.0.0.1"}), `egress_build_allow: "10.0.0.1" not allowed (explicit domains only)`},
		{"invalid domain", with("egress_allow", []any{"not a domain"}), `egress_allow: "not a domain" not allowed (explicit domains only)`},
		{"egress not a list", with("egress_allow", "github.com"), "egress_allow: must be a list of domains"},
		{"disk over quota", with("disk_bound", "200GiB"), "disk_bound: exceeds the 100 GB Sprite quota"},
		{"warm reuse key", with("reuse", true), "reuse: unknown key"},
		{"max_warm", with("max_warm", int64(2)), "max_warm: unknown key"},
		{"idle_ttl", with("idle_ttl", "10m"), "idle_ttl: unknown key"},
		{"bad rate", with("rates", map[string]any{"cpu": "cheap"}), "rates.cpu: must be a number of dollars per hour >= 0 with at most 6 decimals"},
		{"unknown rate", with("rates", map[string]any{"gpu": 1.0}), "rates.gpu: unknown key"},
		{"api_base http", with("api_base", "http://api.example.com"), `api_base: "http://api.example.com" must be an https URL (http only for a loopback host)`},
	} {
		_, errs := DecodeSettings(c.raw, "bitc")
		found := false
		for _, e := range errs {
			found = found || e == c.want
		}
		if !found {
			t.Errorf("%s: want %q, got %q", c.name, c.want, errs)
		}
	}
	// name_prefix and the size key share 16 characters
	long := map[string]any{"org": "sandbox-org", "sizes": map[string]any{"abcdefghij": map[string]any{"memory": "8GiB"}}}
	want := "sizes.abcdefghij: name_prefix and size key together exceed 16 characters (resource names are limited to 63)"
	if _, errs := DecodeSettings(long, "bitcbitcbi"); len(errs) == 0 || errs[0] != want {
		t.Errorf("name too long: %q", errs)
	}
	if _, errs := DecodeSettings(long, "bitcbi"); len(errs) != 0 {
		t.Errorf("16 characters are fine: %q", errs)
	}
	// loopback http is fine, and a valid configuration has no message at all
	if _, errs := DecodeSettings(with("api_base", "http://127.0.0.1:9999"), "bitc"); len(errs) != 0 {
		t.Errorf("loopback http: %v", errs)
	}
}

// SP3: the price of a size from the rates, the memory at max(memory, memory_bound).
func TestPriceFromRatesAndTheMemoryBound(t *testing.T) {
	s, errs := DecodeSettings(baseRaw(), "bitc")
	if len(errs) != 0 {
		t.Fatal(errs)
	}
	// 8 x 0.0385 + 17.18 GB x 0.021875 + 107.38 GB x 0.000683 = 0.757154 (design 5.1: $0.7571/h)
	if got := s.Price("cpu8"); got != 757154 {
		t.Fatalf("16GiB bound: %d micros", got)
	}
	if got := s.EstimateFor("cpu8", 55*time.Minute); got != 694057 {
		t.Fatalf("the 55 minute example: %d micros ($0.6940)", got)
	}
	// a size below the bound is still priced at the bound (until probe 9.6 proves limit_mb)
	small := baseRaw()
	small["sizes"] = map[string]any{"cpu8": map[string]any{"memory": "4GiB"}}
	if s, _ := DecodeSettings(small, "bitc"); s.Price("cpu8") != 757154 {
		t.Fatalf("4GiB size: %d", s.Price("cpu8"))
	}
	// with the bound at 8 GiB: $0.5692/h
	small["memory_bound"] = "8GiB"
	if s, _ := DecodeSettings(small, "bitc"); s.Price("cpu8") != 569247 {
		t.Fatalf("8GiB bound: %d", s.Price("cpu8"))
	}
}

func TestUsdPerHourOnlyRaisesThePrice(t *testing.T) {
	raw := map[string]any{"org": "sandbox-org", "sizes": map[string]any{
		"low":  map[string]any{"memory": "16GiB", "usd_per_hour": 0.5},
		"high": map[string]any{"memory": "16GiB", "usd_per_hour": 2.0},
	}}
	s, errs := DecodeSettings(raw, "bitc")
	if len(errs) != 0 {
		t.Fatal(errs)
	}
	if s.Price("low") != 757154 || s.Price("high") != config.Micros(2_000_000) {
		t.Fatalf("low %d high %d", s.Price("low"), s.Price("high"))
	}
	if s.Price("nosuch") != s.Highest() || s.Highest() != 2_000_000 {
		t.Fatalf("unknown size %d, highest %d", s.Price("nosuch"), s.Highest())
	}
}

func TestRatesAcceptSixDecimals(t *testing.T) {
	raw := baseRaw()
	raw["rates"] = map[string]any{"cpu": 0.05, "memory": 0.025001, "hot_storage": int64(1), "cold_storage": 0.000027}
	s, errs := DecodeSettings(raw, "bitc")
	if len(errs) != 0 || s.Rates != (Rates{CPU: 50000, Memory: 25001, HotStorage: 1_000_000, ColdStorage: 27}) {
		t.Fatalf("%+v %v", s.Rates, errs)
	}
	if !strings.Contains(strings.Join(func() []string {
		_, e := DecodeSettings(map[string]any{"org": "o", "sizes": baseRaw()["sizes"], "rates": map[string]any{"cpu": 0.0000001}}, "bitc")
		return e
	}(), "|"), "rates.cpu") {
		t.Fatal("seven decimals accepted")
	}
}
