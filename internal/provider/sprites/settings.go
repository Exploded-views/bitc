package sprites

import (
	"fmt"
	"math"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Exploded-views/bitc/internal/config"
)

// Size is one entry of [providers.sprites.sizes].
type Size struct {
	Memory     config.Size
	USDPerHour config.Micros // the computed price, raised to the host's usd_per_hour when that is higher
}

// Rates are the platform prices in micro-dollars per hour (sprites-provider.md 5.1).
type Rates struct{ CPU, Memory, HotStorage, ColdStorage config.Micros }

// Settings are the provider-specific keys of [providers.sprites] (sprites-provider.md 7).
type Settings struct {
	Org, APIBase                  string
	Sizes                         map[string]Size
	Rates                         Rates
	DiskBound, MemoryBound        config.Size
	EgressAllow, EgressBuildAllow []string
}

// vcpus is fixed on Sprites (sprites-provider.md 5.1); maxDisk is the 100 GB quota.
const (
	vcpus   = 8
	maxDisk = config.Size(100 << 30)
	// maxNameLen: name_prefix and a size key share what is left of 63 characters once the
	// install16, job8, E and tag8 fields and the separators are placed (2.1).
	maxPrefixAndSize = 16
)

var defaultRates = Rates{CPU: 38500, Memory: 21875, HotStorage: 683, ColdStorage: 27}

// DecodeSettings decodes the provider-specific keys strictly. The messages come back in key
// order, each as the text after `providers.sprites.`; namePrefix is the core name_prefix, which
// bounds the size keys (resource names stay within 63 characters).
func DecodeSettings(raw map[string]any, namePrefix string) (Settings, []string) {
	s := Settings{APIBase: DefaultAPIBase, Rates: defaultRates, DiskBound: maxDisk, MemoryBound: 16 << 30, Sizes: map[string]Size{},
		EgressAllow:      []string{"github.com", "codeload.github.com", "objects.githubusercontent.com", "registry.npmjs.org"},
		EgressBuildAllow: []string{"archive.ubuntu.com", "security.ubuntu.com", "ports.ubuntu.com"}}
	var errs []string
	bad := func(format string, a ...any) { errs = append(errs, fmt.Sprintf(format, a...)) }
	keys := make([]string, 0, len(raw))
	for k := range raw {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	// scalar keys first: the size checks need memory_bound
	for _, k := range keys {
		switch k {
		case "org":
			v, ok := raw[k].(string)
			if !ok || v == "" {
				bad("org: required")
			}
			s.Org = v
		case "api_base":
			v, ok := raw[k].(string)
			if !ok || !baseURLOK(v) {
				bad("api_base: %s must be an https URL (http only for a loopback host)", quote(raw[k]))
				continue
			}
			s.APIBase = v
		case "disk_bound", "memory_bound":
			v, _ := raw[k].(string)
			n, err := config.ParseSize(v)
			if err != nil {
				bad("%s: %v", k, err)
				continue
			}
			if k == "disk_bound" {
				if n > maxDisk {
					bad("disk_bound: exceeds the 100 GB Sprite quota")
					continue
				}
				s.DiskBound = n
			} else {
				s.MemoryBound = n
			}
		case "egress_allow", "egress_build_allow":
			list, ok := raw[k].([]any)
			if !ok {
				bad("%s: must be a list of domains", k)
				continue
			}
			var out []string
			okAll := true
			for _, x := range list {
				d, isStr := x.(string)
				if !isStr || !domainRE.MatchString(d) {
					bad("%s: %s not allowed (explicit domains only)", k, quote(x))
					okAll = false
					continue
				}
				out = append(out, d)
			}
			if okAll {
				if k == "egress_allow" {
					s.EgressAllow = out
				} else {
					s.EgressBuildAllow = out
				}
			}
		case "rates":
			m, ok := raw[k].(map[string]any)
			if !ok {
				bad("rates: must be a table")
				continue
			}
			rkeys := make([]string, 0, len(m))
			for rk := range m {
				rkeys = append(rkeys, rk)
			}
			sort.Strings(rkeys)
			for _, rk := range rkeys {
				dst := map[string]*config.Micros{"cpu": &s.Rates.CPU, "memory": &s.Rates.Memory, "hot_storage": &s.Rates.HotStorage, "cold_storage": &s.Rates.ColdStorage}[rk]
				if dst == nil {
					bad("rates.%s: unknown key", rk)
					continue
				}
				v, ok := parseRate(m[rk])
				if !ok {
					bad("rates.%s: must be a number of dollars per hour >= 0 with at most 6 decimals", rk)
					continue
				}
				*dst = v
			}
		case "sizes":
		default:
			bad("%s: unknown key", k)
		}
	}
	if _, set := raw["org"]; !set {
		bad("org: required")
	}
	if m, ok := raw["sizes"].(map[string]any); ok {
		names := make([]string, 0, len(m))
		for n := range m {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			z, ok := m[n].(map[string]any)
			if !ok {
				bad("sizes.%s: must be a table", n)
				continue
			}
			if !config.ValidSizeKey(n) {
				bad("sizes.%s: size key must match [a-z0-9]{1,10}", n)
				continue
			}
			if len(namePrefix)+len(n) > maxPrefixAndSize {
				bad("sizes.%s: name_prefix and size key together exceed %d characters (resource names are limited to 63)", n, maxPrefixAndSize)
				continue
			}
			zkeys := make([]string, 0, len(z))
			for k := range z {
				zkeys = append(zkeys, k)
			}
			sort.Strings(zkeys)
			var sz Size
			var override config.Micros
			haveMem := false
			for _, k := range zkeys {
				switch k {
				case "memory":
					v, _ := z[k].(string)
					mem, err := config.ParseSize(v)
					switch {
					case err != nil:
						bad("sizes.%s.memory: %v", n, err)
					case mem > s.MemoryBound:
						bad("sizes.%s.memory: exceeds memory_bound %s", n, s.MemoryBound)
					default:
						sz.Memory, haveMem = mem, true
					}
				case "vcpu":
					if v, ok := z[k].(int64); !ok || v != vcpus {
						bad("sizes.%s.vcpu: Sprites have %d vCPU (got %v)", n, vcpus, z[k])
					}
				case "usd_per_hour":
					v, err := config.ParseMoney(z[k])
					if err != nil {
						bad("sizes.%s.usd_per_hour: %v", n, err)
						continue
					}
					override = v
				default:
					bad("sizes.%s.%s: unknown key", n, k)
				}
			}
			if !haveMem {
				if _, set := z["memory"]; !set {
					bad("sizes: at least one size with memory is required")
				}
				continue
			}
			sz.USDPerHour = max(s.computed(sz.Memory), override)
			s.Sizes[n] = sz
		}
	}
	if len(s.Sizes) == 0 && !contains(errs, "sizes: at least one size with memory is required") {
		bad("sizes: at least one size with memory is required")
	}
	// the price of a size depends on rates and bounds read above: price again with the final values
	for n, z := range s.Sizes {
		var override config.Micros
		if m, ok := raw["sizes"].(map[string]any); ok {
			if t, ok := m[n].(map[string]any); ok && t["usd_per_hour"] != nil {
				override, _ = config.ParseMoney(t["usd_per_hour"])
			}
		}
		z.USDPerHour = max(s.computed(z.Memory), override)
		s.Sizes[n] = z
	}
	return s, errs
}

func contains(l []string, x string) bool {
	for _, e := range l {
		if e == x {
			return true
		}
	}
	return false
}

func quote(v any) string {
	if s, ok := v.(string); ok {
		return strconv.Quote(s)
	}
	return fmt.Sprintf("%v", v)
}

func baseURLOK(v string) bool {
	u, err := url.Parse(v)
	if err != nil || u.Host == "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" {
		return false
	}
	switch u.Scheme {
	case "https":
		return true
	case "http":
		return loopbackHost(u.Hostname())
	}
	return false
}

// parseRate reads a TOML number of dollars per hour with at most six decimals as micro-dollars.
func parseRate(v any) (config.Micros, bool) {
	switch n := v.(type) {
	case int64:
		if n < 0 || n > math.MaxInt64/1_000_000 {
			return 0, false
		}
		return config.Micros(n * 1_000_000), true
	case float64:
		if math.IsNaN(n) || math.IsInf(n, 0) || n < 0 || n > 1e6 {
			return 0, false
		}
		txt := strconv.FormatFloat(n, 'f', -1, 64)
		if i := strings.IndexByte(txt, '.'); i >= 0 && len(txt)-i-1 > 6 {
			return 0, false
		}
		return config.Micros(math.Round(n * 1_000_000)), true
	}
	return 0, false
}

// gbCost prices b bytes (decimal GB, rounded up to 0.01) at rate micro-dollars per GB-hour.
func gbCost(b config.Size, rate config.Micros) config.Micros {
	u := (int64(b) + 9_999_999) / 10_000_000 // hundredths of a GB, rounded up
	return config.Micros((u*int64(rate) + 50) / 100)
}

// computed is the hourly price of a Sprite whose memory limit is mem (5.1): 8 vCPU, the memory
// at max(mem, memory_bound) and the disk at disk_bound.
func (s Settings) computed(mem config.Size) config.Micros {
	return vcpus*s.Rates.CPU + gbCost(max(mem, s.MemoryBound), s.Rates.Memory) + gbCost(s.DiskBound, s.Rates.HotStorage)
}

// Price is the USD per hour of a size; an unknown key gets the highest price.
func (s Settings) Price(size string) config.Micros {
	if z, ok := s.Sizes[size]; ok {
		return z.USDPerHour
	}
	return s.Highest()
}

// Highest is the most expensive size's price.
func (s Settings) Highest() config.Micros {
	var h config.Micros
	for _, z := range s.Sizes {
		h = max(h, z.USDPerHour)
	}
	return h
}

// EstimateFor is the price of size over d.
func (s Settings) EstimateFor(size string, d time.Duration) config.Micros {
	return config.Micros(float64(s.Price(size)) * d.Hours())
}
