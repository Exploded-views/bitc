package liveprobe

import (
	"errors"
	"io"
	"strings"
	"testing"
)

func TestParseFlagsDefaults(t *testing.T) {
	c, err := ParseFlags([]string{"--org", "sandbox-org", "--token-file", "/x/t"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if c.MaxUSD != 0.90 || c.PricePerHour != 0.7571 || c.MaxMinutes != 75 || c.DryRun || c.Probes != nil || c.Org != "sandbox-org" || c.TokenFile != "/x/t" {
		t.Fatalf("defaults: %+v", c)
	}
	if !runIDRE.MatchString(c.RunID) || len(c.RunID) != 8 {
		t.Fatalf("random run id %q", c.RunID)
	}
}

func TestParseFlagsProbeSelection(t *testing.T) {
	c, err := ParseFlags([]string{"--org", "o", "--token-file", "t", "--probes", "24, 13,15", "--dry-run", "--run", "abc1", "--dns-canary-domain", "canary.example.org"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Probes) != 3 || c.Probes[0] != 24 || c.Probes[1] != 13 || c.Probes[2] != 15 || !c.DryRun || c.RunID != "abc1" || c.DNSCanary != "canary.example.org" {
		t.Fatalf("%+v", c)
	}
}

func TestParseFlagsRefusals(t *testing.T) {
	ok := []string{"--org", "o", "--token-file", "t"}
	for name, extra := range map[string][]string{
		"unknown probe":    {"--probes", "26"},
		"zero probe":       {"--probes", "0"},
		"repeated probe":   {"--probes", "3,3"},
		"word probe":       {"--probes", "x"},
		"over the hard $":  {"--max-usd", "1.5"},
		"zero $":           {"--max-usd", "0"},
		"negative price":   {"--price-per-hour", "-1"},
		"zero minutes":     {"--max-minutes", "0"},
		"too many minutes": {"--max-minutes", "500"},
		"max-live 2":       {"--max-live", "2"},
		"bad run id":       {"--run", "UPPER"},
		"bad canary":       {"--dns-canary-domain", "not a domain"},
		"stray argument":   {"extra"},
		"unknown flag":     {"--token", "abc"},
	} {
		if _, err := ParseFlags(append(append([]string{}, ok...), extra...), io.Discard); !errors.Is(err, ErrUsage) {
			t.Errorf("%s: err = %v, want a usage error", name, err)
		}
	}
	if _, err := ParseFlags([]string{"--token-file", "t"}, io.Discard); err == nil || !strings.Contains(err.Error(), "--org is required") {
		t.Errorf("no --org: %v", err)
	}
	if _, err := ParseFlags([]string{"--org", "o"}, io.Discard); err == nil || !strings.Contains(err.Error(), "--token-file is required") {
		t.Errorf("no --token-file: %v", err)
	}
}
