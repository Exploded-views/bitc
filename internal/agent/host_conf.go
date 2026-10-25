package agent

import (
	"fmt"
	"net/netip"
	"path/filepath"
	"strconv"
	"strings"
)

// HostConf is /etc/bitc/host.conf (providers delta C.5): what the agent and the setup script
// share about one ssh host.
type HostConf struct {
	Slots         int
	Resolver      string
	Gateway       string
	Netns         bool
	MemoryPerSlot int64 // bytes; 0 means no memory.max
	SecretPaths   []string
	MaxBundle     int64 // bytes; 0 means the 1 GiB default
}

// ipv4Literal reports whether s is a dotted IPv4 address and nothing else.
func ipv4Literal(s string) bool {
	a, err := netip.ParseAddr(s)
	return err == nil && a.Is4() && a.String() == s
}

// ParseHostConf parses key=value lines ('#' comments, blank lines ignored). Keys: slots (1-64),
// resolver and gateway (IPv4 literals), netns (true|false, default true; false needs slots=1),
// memory_per_slot (bytes, or with a KiB/MiB/GiB/TiB suffix), max_bundle (the same sizes, at most
// 8GiB; optional, default 1GiB), secret_paths (absolute paths
// separated by spaces).
func ParseHostConf(b []byte) (HostConf, error) {
	c := HostConf{Netns: true}
	seen := map[string]bool{}
	for n, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			return c, fmt.Errorf("host.conf line %d: not key=value", n+1)
		}
		if seen[k] {
			return c, fmt.Errorf("host.conf: %s set twice", k)
		}
		seen[k] = true
		switch k {
		case "slots":
			i, err := strconv.Atoi(v)
			if err != nil || i < 1 || i > 64 {
				return c, fmt.Errorf("host.conf: slots out of range: %q", v)
			}
			c.Slots = i
		case "resolver":
			if !ipv4Literal(v) {
				return c, fmt.Errorf("host.conf: resolver must be an IPv4 address: %q", v)
			}
			c.Resolver = v
		case "gateway":
			if !ipv4Literal(v) {
				return c, fmt.Errorf("host.conf: gateway must be an IPv4 address: %q", v)
			}
			c.Gateway = v
		case "netns":
			if v != "true" && v != "false" {
				return c, fmt.Errorf("host.conf: netns must be true or false: %q", v)
			}
			c.Netns = v == "true"
		case "memory_per_slot":
			m, err := parseBytes(v)
			if err != nil {
				return c, fmt.Errorf("host.conf: memory_per_slot: %w", err)
			}
			c.MemoryPerSlot = m
		case "max_bundle":
			m, err := parseBytes(v)
			if err != nil || m > maxMaxBundle {
				return c, fmt.Errorf("host.conf: max_bundle must be a size of at most 8GiB: %q", v)
			}
			c.MaxBundle = m
		case "secret_paths":
			for _, p := range strings.Fields(v) {
				if !filepath.IsAbs(p) || filepath.Clean(p) != p {
					return c, fmt.Errorf("host.conf: secret_paths entry must be a clean absolute path: %q", p)
				}
				c.SecretPaths = append(c.SecretPaths, p)
			}
		default:
			return c, fmt.Errorf("host.conf: unknown key %q", k)
		}
	}
	for _, k := range []string{"slots", "resolver", "gateway"} {
		if !seen[k] {
			return c, fmt.Errorf("host.conf: %s is required", k)
		}
	}
	if !c.Netns && c.Slots != 1 {
		return c, fmt.Errorf("host.conf: netns=false requires slots=1")
	}
	return c, nil
}

func parseBytes(v string) (int64, error) {
	mult := int64(1)
	for suf, m := range map[string]int64{"KiB": 1 << 10, "MiB": 1 << 20, "GiB": 1 << 30, "TiB": 1 << 40} {
		if s, ok := strings.CutSuffix(v, suf); ok {
			v, mult = s, m
			break
		}
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 1 || n > (1<<62)/mult {
		return 0, fmt.Errorf("not a size: %q", v)
	}
	return n * mult, nil
}
