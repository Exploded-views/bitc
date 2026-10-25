package ssh

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Exploded-views/bitc/internal/config"
)

// validCore is a decoded [providers.ssh] section that passes every check.
func validCore(t *testing.T) *config.Provider {
	t.Helper()
	return &config.Provider{Type: "ssh", Fixed: true, NamePrefix: "bitc", MaxLive: 1, Raw: map[string]any{
		"host": "127.0.0.1", "port": int64(22022), "user": "bitc", "host_key": edKey(),
		"identity_file": privateKeyFile(t, "none"), "arch": "arm64", "slots": int64(1), "resolver": "9.9.9.9",
	}}
}

func TestDecodeSettingsDefaultsAndValues(t *testing.T) {
	c := validCore(t)
	s, errs := DecodeSettings(c)
	if len(errs) != 0 {
		t.Fatalf("errors: %v", errs)
	}
	if s.Host != "127.0.0.1" || s.Port != 22022 || s.User != "bitc" || s.Arch != "arm64" || s.Slots != 1 || s.Resolver != "9.9.9.9" ||
		s.HostKeyType != "ssh-ed25519" || s.ConnectTimeout != 5*time.Second || !s.Netns || s.USDPerHour != 0 {
		t.Fatalf("settings %+v", s)
	}
	if z, ok := s.Sizes["s1"]; !ok || z.Slots != 1 {
		t.Fatalf("no sizes key means one size s1 of one slot: %+v", s.Sizes)
	}
	delete(c.Raw, "port")
	delete(c.Raw, "user")
	delete(c.Raw, "slots")
	if s, errs = DecodeSettings(c); len(errs) != 0 || s.Port != 22 || s.User != "bitc" || s.Slots != 1 {
		t.Fatalf("defaults %+v %v", s, errs)
	}
}

func TestDecodeSettingsReadsTheOptionalKeys(t *testing.T) {
	c := validCore(t)
	c.Raw["slots"], c.Raw["usd_per_hour"], c.Raw["connect_timeout"] = int64(4), 0.25, "9s"
	c.Raw["memory_per_slot"], c.Raw["netns"] = "16GiB", false
	c.Raw["sizes"] = map[string]any{"s1": map[string]any{"slots": int64(1)}, "big": map[string]any{"slots": int64(2)}}
	c.MaxLive, c.DailyUSD = 2, config.Micros(3_000_000)
	s, errs := DecodeSettings(c)
	// netns = false with slots > 1 is an error; everything else must have been read
	if !slices.Equal(errs, []string{"netns: false requires slots = 1"}) {
		t.Fatalf("errors %v", errs)
	}
	if s.Slots != 4 || s.USDPerHour != 250_000 || s.ConnectTimeout != 9*time.Second || s.MemoryPerSlot != 16<<30 || s.Netns ||
		s.Sizes["big"].Slots != 2 || len(s.Sizes) != 2 {
		t.Fatalf("settings %+v", s)
	}
}

// SH1's cases at the settings level: each is one message, the text after `providers.ssh.`.
func TestDecodeSettingsValidation(t *testing.T) {
	cases := []struct {
		name string
		mut  func(c *config.Provider)
		want string
	}{
		{"host empty", func(c *config.Provider) { c.Raw["host"] = "" }, `host: invalid: ""`},
		{"host dash", func(c *config.Provider) { c.Raw["host"] = "-oProxyCommand=x" }, `host: invalid: "-oProxyCommand=x"`},
		{"host space", func(c *config.Provider) { c.Raw["host"] = "a b" }, `host: invalid: "a b"`},
		{"host at", func(c *config.Provider) { c.Raw["host"] = "u@h" }, `host: invalid: "u@h"`},
		{"host missing", func(c *config.Provider) { delete(c.Raw, "host") }, `host: invalid: ""`},
		{"user", func(c *config.Provider) { c.Raw["user"] = "Bad User" }, `user: invalid: "Bad User"`},
		{"port low", func(c *config.Provider) { c.Raw["port"] = int64(0) }, `port: out of range: 0`},
		{"port high", func(c *config.Provider) { c.Raw["port"] = int64(70000) }, `port: out of range: 70000`},
		{"host_key missing", func(c *config.Provider) { delete(c.Raw, "host_key") }, `host_key: required`},
		{"host_key garbage", func(c *config.Provider) { c.Raw["host_key"] = "nope" }, `host_key: unparsable`},
		{"host_key rsa", func(c *config.Provider) { c.Raw["host_key"] = rsaKey(2048) }, `host_key: ssh-rsa key is 2048 bits, 3072 or more required`},
		{"host_key dss", func(c *config.Provider) { c.Raw["host_key"] = "ssh-dss AAAA" }, `host_key: ssh-dss is not accepted`},
		{"identity missing key", func(c *config.Provider) { delete(c.Raw, "identity_file") }, `identity_file: required`},
		{"identity not found", func(c *config.Provider) { c.Raw["identity_file"] = "/nonexistent/id" }, `identity_file: /nonexistent/id: no such file`},
		{"identity mode", func(c *config.Provider) { _ = os.Chmod(c.Raw["identity_file"].(string), 0o640) },
			`identity_file: must be owned by you and mode 600`},
		{"identity encrypted", func(c *config.Provider) { c.Raw["identity_file"] = privateKeyFile(t, "aes256-ctr") }, `identity_file: is encrypted (the key must have no passphrase)`},
		{"api_key", func(c *config.Provider) { c.APIKey = "k" }, `api_key: unknown key for ssh (use identity_file)`},
		{"api_key_file", func(c *config.Provider) { c.APIKeyFile = "/x" }, `api_key: unknown key for ssh (use identity_file)`},
		{"arch missing", func(c *config.Provider) { delete(c.Raw, "arch") }, `arch: must be "amd64" or "arm64": ""`},
		{"arch bad", func(c *config.Provider) { c.Raw["arch"] = "riscv" }, `arch: must be "amd64" or "arm64": "riscv"`},
		{"slots zero", func(c *config.Provider) { c.Raw["slots"] = int64(0) }, `slots: out of range: 0`},
		{"slots 65", func(c *config.Provider) { c.Raw["slots"] = int64(65) }, `slots: out of range: 65`},
		{"max_live", func(c *config.Provider) { c.Raw["slots"] = int64(2); c.MaxLive = 3 }, `max_live: 3 exceeds slots 2`},
		{"size slots low", func(c *config.Provider) {
			c.Raw["sizes"] = map[string]any{"s1": map[string]any{"slots": int64(0)}}
		}, `sizes.s1.slots: out of range: 0`},
		{"size slots high", func(c *config.Provider) {
			c.Raw["sizes"] = map[string]any{"s1": map[string]any{"slots": int64(2)}}
		}, `sizes.s1.slots: out of range: 2`},
		{"size key", func(c *config.Provider) {
			c.Raw["sizes"] = map[string]any{"Big Size": map[string]any{"slots": int64(1)}}
		}, `sizes.Big Size: size key must match [a-z0-9]{1,10}`},
		{"size unknown key", func(c *config.Provider) {
			c.Raw["sizes"] = map[string]any{"s1": map[string]any{"slots": int64(1), "memory": "1GiB"}}
		}, `sizes.s1.memory: unknown key`},
		{"priced without cap", func(c *config.Provider) { c.Raw["usd_per_hour"] = 1.5 }, `daily_usd: required when usd_per_hour > 0`},
		{"cap on unpriced", func(c *config.Provider) { c.DailyUSD = config.Micros(5_000_000) }, `daily_usd: not applicable to an unpriced provider`},
		{"usd negative", func(c *config.Provider) { c.Raw["usd_per_hour"] = -1.0 }, `usd_per_hour: must be a number of dollars >= 0`},
		{"proxy_jump", func(c *config.Provider) { c.Raw["proxy_jump"] = "bastion" }, `proxy_jump: not supported in phase 1`},
		{"proxy_host_key", func(c *config.Provider) { c.Raw["proxy_host_key"] = edKey() }, `proxy_jump: not supported in phase 1`},
		{"resolver missing", func(c *config.Provider) { delete(c.Raw, "resolver") }, `resolver: required, an IPv4 address: ""`},
		{"resolver name", func(c *config.Provider) { c.Raw["resolver"] = "dns.example" }, `resolver: required, an IPv4 address: "dns.example"`},
		{"resolver v6", func(c *config.Provider) { c.Raw["resolver"] = "2606:4700:4700::1111" }, `resolver: required, an IPv4 address: "2606:4700:4700::1111"`},
		{"netns false slots 2", func(c *config.Provider) { c.Raw["netns"] = false; c.Raw["slots"] = int64(2); c.MaxLive = 1 }, `netns: false requires slots = 1`},
		{"memory big", func(c *config.Provider) {
			c.Raw["slots"] = int64(2)
			c.MaxLive = 1
			c.Raw["memory_per_slot"] = "600GiB"
		}, `memory_per_slot: out of range: 600GiB`},
		{"memory small", func(c *config.Provider) { c.Raw["memory_per_slot"] = "256MiB" }, `memory_per_slot: out of range: 256MiB`},
		{"connect_timeout", func(c *config.Provider) { c.Raw["connect_timeout"] = "soon" }, `connect_timeout: "soon" must be a duration like 30s, 10m or 1h30m (units ms s m h d; no bare number, sign, fraction or leading zero)`},
		{"unknown key", func(c *config.Provider) { c.Raw["reuse"] = true }, `reuse: unknown key`},
		{"build without module_dir", func(c *config.Provider) { c.Agent.Source = "build" }, `agent.module_dir: required when source is "build"`},
	}
	for _, c := range cases {
		core := validCore(t)
		c.mut(core)
		_, errs := DecodeSettings(core)
		if !slices.Contains(errs, c.want) {
			t.Errorf("%s: want %q, got %q", c.name, c.want, errs)
		}
	}
}

func TestDecodeSettingsPricedWithACapIsFine(t *testing.T) {
	c := validCore(t)
	c.Raw["usd_per_hour"] = 2.0
	c.DailyUSD = config.Micros(10_000_000)
	if s, errs := DecodeSettings(c); len(errs) != 0 || s.USDPerHour != 2_000_000 {
		t.Fatalf("%+v %v", s, errs)
	}
}

func TestDecodeSettingsExpandsTheIdentityHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	k := privateKeyFile(t, "none")
	if err := os.MkdirAll(filepath.Join(home, ".config"), 0o700); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(k)
	if err := os.WriteFile(filepath.Join(home, ".config", "id"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	c := validCore(t)
	c.Raw["identity_file"] = "~/.config/id"
	s, errs := DecodeSettings(c)
	if len(errs) != 0 || !strings.HasPrefix(s.IdentityFile, home) {
		t.Fatalf("%q %v", s.IdentityFile, errs)
	}
}
