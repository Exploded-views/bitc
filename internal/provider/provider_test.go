package provider

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/Exploded-views/bitc/internal/config"
)

func TestResourceNameFitsWithLongestPrefixAndSize(t *testing.T) {
	n := ResourceName("abcdefghij", "0123456789abcdef", strings.Repeat("p", 80), "abcdefghij", "job", time.Unix(1_800_000_000, 0))
	if len(n) > maxName || !strings.Contains(n, "-0123456789abcdef-p") {
		t.Fatalf("%d chars: %s", len(n), n)
	}
}

func TestOwnerTag(t *testing.T) {
	s := []byte("0123456789abcdef0123456789abcdef")
	tag := OwnerTag(s, "bitc-x-lint-cpu8-deadbeef-1700000000")
	if len(tag) != 64 || !OwnerMatches(s, "bitc-x-lint-cpu8-deadbeef-1700000000", tag) || OwnerMatches(s, "other", tag) {
		t.Fatal(tag)
	}
}

func TestResourceNameMaxKeepsJob8AndExpiryAtTheShortestLimit(t *testing.T) {
	exp := time.Unix(1_900_000_000, 0)
	pool := strings.Repeat("p", 40) + "-heavy"
	sum := sha256.Sum256([]byte("job-1"))
	job8 := hex.EncodeToString(sum[:])[:8]
	for _, limit := range []int{60, 61, 63} {
		got := ResourceNameMax("abcdefghij", "0123456789abcdef", pool, "cpu8cpu8cp", "job-1", exp, limit)
		if len(got) > limit || !strings.HasSuffix(got, "-cpu8cpu8cp-"+job8+"-1900000000") || !strings.HasPrefix(got, "abcdefghij-0123456789abcdef-p") {
			t.Errorf("limit %d: %q (%d chars)", limit, got, len(got))
		}
	}
	short := ResourceNameMax("abcdefghij", "0123456789abcdef", pool, "cpu8cpu8cp", "job-1", exp, 60)
	if len(short) != 60 {
		t.Errorf("limit 60 should use all 60 characters, got %d: %q", len(short), short)
	}
	if got, want := ResourceName("bitc", "0123456789abcdef", "lint", "cpu8", "job-1", exp), ResourceNameMax("bitc", "0123456789abcdef", "lint", "cpu8", "job-1", exp, 63); got != want {
		t.Errorf("ResourceName %q differs from the 63 limit %q", got, want)
	}
}

func TestLookupUsesThePluginFactoryOnlyForAPluginSection(t *testing.T) {
	built := false
	Register("lookuptype", Factory{Endpoints: func(map[string]any) ([]string, error) { return nil, nil },
		New: func(*config.Provider, *config.Effective) (Provider, error) { built = true; return nil, nil }})
	plug := false
	RegisterPlugin(Factory{New: func(*config.Provider, *config.Effective) (Provider, error) { plug = true; return nil, nil }})
	f, ok := Lookup("lookuptype", &config.Provider{})
	if !ok {
		t.Fatal("built-in not found")
	}
	_, _ = f.New(nil, nil)
	if !built || plug {
		t.Fatalf("built-in lookup: built %v plugin %v", built, plug)
	}
	f, ok = Lookup("modal", &config.Provider{Plugin: &config.PluginPin{}})
	if !ok {
		t.Fatal("plugin section not found")
	}
	_, _ = f.New(nil, nil)
	if !plug {
		t.Fatal("a plugin section did not use the plugin factory")
	}
	if _, ok := Lookup("unknown", &config.Provider{}); ok {
		t.Fatal("an unknown type without a plugin table was found")
	}
}
