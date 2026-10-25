// Package ssh is the fixed-capacity provider (providers delta C): jobs run on one host the owner
// already has, reached over the system OpenSSH client, with a number of slots instead of a price.
package ssh

import (
	"encoding/base64"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/Exploded-views/bitc/internal/config"
)

// Size is one entry of [providers.ssh] sizes: how many slots a job of that size takes.
type Size struct{ Slots int }

// Settings are the provider-specific keys of [providers.ssh] (providers delta C.2), decoded.
type Settings struct {
	Host, User, HostKey, HostKeyType, IdentityFile, Arch, Resolver string
	Port, Slots                                                    int
	USDPerHour                                                     config.Micros // accounting only
	ConnectTimeout                                                 time.Duration
	MemoryPerSlot                                                  config.Size // 0: no memory.max
	Netns                                                          bool
	Sizes                                                          map[string]Size
}

var (
	userRE = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)
	// connect_timeout is bounded so a typo cannot make every launch wait for minutes
	maxConnectTimeout = time.Minute
)

// DecodeSettings decodes the provider-specific keys strictly and applies the checks of providers
// delta C.10. It also reads the core fields the rules depend on (max_live, daily_usd, api_key).
// Each message is the text after `providers.ssh.`.
func DecodeSettings(core *config.Provider) (Settings, []string) {
	raw := core.Raw
	s := Settings{Port: 22, User: "bitc", Slots: 1, ConnectTimeout: 5 * time.Second, Netns: true, Sizes: map[string]Size{}}
	var errs []string
	bad := func(format string, a ...any) { errs = append(errs, fmt.Sprintf(format, a...)) }

	known := map[string]bool{"host": true, "port": true, "user": true, "host_key": true, "identity_file": true, "arch": true, "slots": true,
		"usd_per_hour": true, "connect_timeout": true, "memory_per_slot": true, "resolver": true, "netns": true, "sizes": true,
		"proxy_jump": true, "proxy_host_key": true}
	keys := make([]string, 0, len(raw))
	for k := range raw {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !known[k] {
			bad("%s: unknown key", k)
		}
	}

	// host
	host, _ := raw["host"].(string)
	if host == "" || strings.HasPrefix(host, "-") || strings.ContainsAny(host, " \t\r\n@") {
		bad("host: invalid: %q", host)
	}
	s.Host = host
	// user
	if v, ok := raw["user"]; ok {
		u, _ := v.(string)
		if !userRE.MatchString(u) {
			bad("user: invalid: %q", u)
		}
		s.User = u
	}
	// port
	if v, ok := raw["port"]; ok {
		n, isInt := v.(int64)
		if !isInt || n < 1 || n > 65535 {
			bad("port: out of range: %v", v)
		} else {
			s.Port = int(n)
		}
	}
	// host_key
	hk, _ := raw["host_key"].(string)
	if typ, err := ParseHostKey(hk); err != nil {
		bad("host_key: %v", err)
	} else {
		s.HostKey, s.HostKeyType = strings.TrimSpace(hk), typ
	}
	// identity_file
	if v, ok := raw["identity_file"].(string); !ok || v == "" {
		bad("identity_file: required")
	} else {
		path := expandHome(v)
		if why := checkIdentity(path); why != "" {
			bad("identity_file: %s", why)
		}
		s.IdentityFile = path
	}
	// the credential is identity_file: an API key is a mistake
	if core.APIKey != "" || core.APIKeyFile != "" {
		bad("api_key: unknown key for ssh (use identity_file)")
	}
	for _, k := range []string{"proxy_jump", "proxy_host_key"} {
		if _, ok := raw[k]; ok {
			bad("proxy_jump: not supported in phase 1")
			break
		}
	}
	// arch
	switch a, _ := raw["arch"].(string); a {
	case "amd64", "arm64":
		s.Arch = a
	default:
		bad("arch: must be \"amd64\" or \"arm64\": %q", a)
	}
	// slots
	slotsOK := true
	if v, ok := raw["slots"]; ok {
		n, isInt := v.(int64)
		if !isInt || n < 1 || n > 64 {
			bad("slots: out of range: %v", v)
			slotsOK = false
		} else {
			s.Slots = int(n)
		}
	}
	if slotsOK && core.MaxLive > s.Slots {
		bad("max_live: %d exceeds slots %d", core.MaxLive, s.Slots)
	}
	// sizes
	if m, ok := raw["sizes"].(map[string]any); ok && len(m) > 0 {
		names := make([]string, 0, len(m))
		for n := range m {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			z, isTable := m[n].(map[string]any)
			if !isTable {
				bad("sizes.%s: must be a table", n)
				continue
			}
			if !config.ValidSizeKey(n) {
				bad("sizes.%s: size key must match [a-z0-9]{1,10}", n)
				continue
			}
			zk := make([]string, 0, len(z))
			for k := range z {
				zk = append(zk, k)
			}
			sort.Strings(zk)
			for _, k := range zk {
				if k != "slots" {
					bad("sizes.%s.%s: unknown key", n, k)
				}
			}
			v, set := z["slots"]
			c, isInt := v.(int64)
			switch {
			case !set:
				bad("sizes.%s.slots: required", n)
			case !isInt || c < 1 || c > int64(s.Slots):
				bad("sizes.%s.slots: out of range: %v", n, v)
			default:
				s.Sizes[n] = Size{Slots: int(c)}
			}
		}
	} else if !ok || len(m) == 0 {
		s.Sizes["s1"] = Size{Slots: 1}
	}
	// usd_per_hour and daily_usd
	if v, ok := raw["usd_per_hour"]; ok {
		m, err := config.ParseMoney(v)
		if err != nil {
			bad("usd_per_hour: %v", err)
		}
		s.USDPerHour = m
	}
	switch {
	case s.USDPerHour > 0 && core.DailyUSD == 0:
		bad("daily_usd: required when usd_per_hour > 0")
	case s.USDPerHour == 0 && core.DailyUSD != 0:
		bad("daily_usd: not applicable to an unpriced provider")
	}
	// resolver: an IPv4 literal outside the VM (C.8.7)
	r, _ := raw["resolver"].(string)
	if ip := net.ParseIP(r); ip == nil || ip.To4() == nil || strings.Contains(r, ":") {
		bad("resolver: required, an IPv4 address: %q", r)
	}
	s.Resolver = r
	// netns
	if v, ok := raw["netns"]; ok {
		b, isBool := v.(bool)
		if !isBool {
			bad("netns: must be true or false")
		} else {
			s.Netns = b
		}
	}
	if !s.Netns && s.Slots > 1 {
		bad("netns: false requires slots = 1")
	}
	// connect_timeout
	if v, ok := raw["connect_timeout"]; ok {
		str, _ := v.(string)
		d, err := config.ParseDuration(str)
		switch {
		case err != nil:
			bad("connect_timeout: %q %v", str, err)
		case d <= 0 || d > maxConnectTimeout:
			bad("connect_timeout: out of range: %q", str)
		default:
			s.ConnectTimeout = d
		}
	}
	// memory_per_slot
	if v, ok := raw["memory_per_slot"]; ok {
		str, _ := v.(string)
		n, err := config.ParseSize(str)
		switch {
		case err != nil:
			bad("memory_per_slot: %q %v", str, err)
		case n < 512<<20 || int64(n)*int64(s.Slots) > 1<<40:
			bad("memory_per_slot: out of range: %s", str)
		default:
			s.MemoryPerSlot = n
		}
	}
	// the agent comes from bytes the host builds or holds, never from a pinned pod build
	if core.Agent.Source == "build" && core.Agent.ModuleDir == "" {
		bad("agent.module_dir: required when source is \"build\"")
	}
	return s, errs
}

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		home := os.Getenv("HOME")
		if home == "" {
			if h, err := os.UserHomeDir(); err == nil {
				home = h
			}
		}
		return filepath.Join(home, strings.TrimPrefix(p, "~"))
	}
	return p
}

// checkIdentity returns why the identity file is unusable, or "" (providers delta C.10): a regular
// file owned by the user, mode 0600, whose key has no passphrase.
func checkIdentity(path string) string {
	st, err := os.Stat(path)
	if os.IsNotExist(err) {
		return path + ": no such file"
	}
	if err != nil {
		return path + ": " + err.Error()
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !st.Mode().IsRegular() || !ok || sys.Uid != uint32(os.Getuid()) || st.Mode().Perm()&0o077 != 0 {
		return "must be owned by you and mode 600"
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return path + ": " + err.Error()
	}
	if len(b) > 64<<10 {
		return path + ": over 64 KiB"
	}
	if encrypted(b) {
		return "is encrypted (the key must have no passphrase)"
	}
	return ""
}

// encrypted reports a passphrase-protected private key: a legacy PEM with an ENCRYPTED header, or
// an openssh-key-v1 container whose cipher is not "none".
func encrypted(b []byte) bool {
	s := string(b)
	if strings.Contains(s, "ENCRYPTED") {
		return true
	}
	const begin, end = "-----BEGIN OPENSSH PRIVATE KEY-----", "-----END OPENSSH PRIVATE KEY-----"
	i, j := strings.Index(s, begin), strings.Index(s, end)
	if i < 0 || j < i {
		return false
	}
	raw, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(s[i+len(begin):j]), ""))
	const magic = "openssh-key-v1\x00"
	if err != nil || len(raw) < len(magic) || string(raw[:len(magic)]) != magic {
		return false
	}
	cipher, _, ok := wireString(raw[len(magic):])
	return ok && string(cipher) != "none"
}
