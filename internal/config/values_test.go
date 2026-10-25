package config

import (
	"testing"
	"time"
)

func TestParseInt(t *testing.T) {
	for _, ok := range []string{"0", "1", "10", "65535"} {
		if _, err := ParseInt(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "010", "08", "-1", "+1", "1.5", "1e3", "10MB", " 1", "1 ", "0x10", "99999999999999999999"} {
		if _, err := ParseInt(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestParseDuration(t *testing.T) {
	cases := map[string]time.Duration{
		"0s": 0, "100ms": 100 * time.Millisecond, "5s": 5 * time.Second, "10m": 10 * time.Minute,
		"1h30m": 90 * time.Minute, "2d": 48 * time.Hour, "1h0m5s": time.Hour + 5*time.Second,
	}
	for in, want := range cases {
		if got, err := ParseDuration(in); err != nil || got != want {
			t.Errorf("%q: %v %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "5", "-5s", "0.5h", "05s", "5 s", "5S", "1h-5m", "1w", "s", "10m5"} {
		if _, err := ParseDuration(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestParseSize(t *testing.T) {
	cases := map[string]Size{"0GiB": 0, "1KiB": 1024, "6GiB": 6 << 30, "200MiB": 200 << 20, "1TiB": 1 << 40}
	for in, want := range cases {
		if got, err := ParseSize(in); err != nil || got != want {
			t.Errorf("%q: %v %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "6", "6GB", "6MB", "06GiB", "6gib", "1.5GiB", "-1GiB", "6 GiB"} {
		if _, err := ParseSize(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if s := Size(6 << 30).String(); s != "6GiB" {
		t.Errorf("String %q", s)
	}
	if s := Size(1536 << 20).String(); s != "1536MiB" {
		t.Errorf("String %q", s)
	}
}

func TestParseMoney(t *testing.T) {
	cases := []struct {
		in   any
		want Micros
	}{{int64(25), 25_000_000}, {0.24, 240_000}, {12.3456, 12_345_600}, {int64(0), 0}}
	for _, c := range cases {
		if got, err := ParseMoney(c.in); err != nil || got != c.want {
			t.Errorf("%v: %v %v", c.in, got, err)
		}
	}
	for _, bad := range []any{-1.0, int64(-1), 0.12345, "25", nil} {
		if _, err := ParseMoney(bad); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}
	if d := Micros(12_345_600).Dollars(); d != "$12.35" {
		t.Errorf("Dollars %q", d)
	}
}

func TestNames(t *testing.T) {
	if !ValidPoolName("release_tag-2") || ValidPoolName("a/b") || ValidPoolName("") || ValidPoolName("x y") {
		t.Fatal("pool names")
	}
	if CanonName("release_tag") != "release-tag" || CanonName("a_b_c") != "a-b-c" {
		t.Fatal("canon")
	}
	if !ValidSizeKey("cpu8") || !ValidSizeKey("abcdefghij") || ValidSizeKey("abcdefghijk") || ValidSizeKey("cpu-8") || ValidSizeKey("CPU8") {
		t.Fatal("size keys")
	}
	if !ValidEnvName("NODE_OPTIONS") || !ValidEnvName("_x") || ValidEnvName("1X") || ValidEnvName("BITC_X") || ValidEnvName("A-B") {
		t.Fatal("env names")
	}
}
