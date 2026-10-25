// Package runpod is the RunPod provider (spec 8.7): CPU pods over REST v2.
package runpod

import (
	"fmt"
	"sort"

	"github.com/Exploded-views/bitc/internal/config"
)

type Size struct {
	CPUType    string
	VCPU       int
	USDPerHour config.Micros
}

type Settings struct {
	Image, APIBase, ProxyDomain, Cloud, DataCenter string
	Sizes                                          map[string]Size
	Disk                                           config.Size
	Port                                           int
}

// DecodeSettings decodes the provider-specific keys strictly (spec 8.5, 8.7).
func DecodeSettings(raw map[string]any) (Settings, error) {
	s := Settings{APIBase: "https://api.runpod.io", ProxyDomain: "proxy.runpod.net", Cloud: "SECURE", Disk: 40 << 30, Port: 8090, Sizes: map[string]Size{}}
	str := func(k string) (string, error) {
		v, ok := raw[k].(string)
		if !ok {
			return "", fmt.Errorf("%s must be a string", k)
		}
		return v, nil
	}
	keys := make([]string, 0, len(raw))
	for k := range raw {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var err error
	for _, k := range keys {
		switch k {
		case "image":
			s.Image, err = str(k)
		case "api_base":
			s.APIBase, err = str(k)
		case "proxy_domain":
			s.ProxyDomain, err = str(k)
		case "cloud":
			s.Cloud, err = str(k)
			if err == nil && s.Cloud != "SECURE" && s.Cloud != "COMMUNITY" {
				err = fmt.Errorf("cloud must be SECURE or COMMUNITY")
			}
		case "data_center":
			s.DataCenter, err = str(k)
		case "disk":
			var v string
			if v, err = str(k); err == nil {
				s.Disk, err = config.ParseSize(v)
			}
		case "port":
			n, ok := raw[k].(int64)
			if !ok || n < 1 || n > 65535 {
				err = fmt.Errorf("port must be 1..65535")
			}
			s.Port = int(n)
		case "sizes":
			m, ok := raw[k].(map[string]any)
			if !ok {
				return s, fmt.Errorf("sizes must be a table")
			}
			for name, v := range m {
				if !config.ValidSizeKey(name) {
					return s, fmt.Errorf("size key %q must match [a-z0-9]{1,10}", name)
				}
				t, ok := v.(map[string]any)
				ct, _ := t["cpu_type"].(string)
				vcpu, _ := t["vcpu"].(int64)
				price, perr := config.ParseMoney(t["usd_per_hour"])
				if !ok || ct == "" || vcpu < 1 || perr != nil || len(t) != 3 {
					return s, fmt.Errorf("sizes.%s needs exactly cpu_type, vcpu and usd_per_hour", name)
				}
				s.Sizes[name] = Size{CPUType: ct, VCPU: int(vcpu), USDPerHour: price}
			}
		default:
			return s, fmt.Errorf("unknown key %s", k)
		}
		if err != nil {
			return s, err
		}
	}
	if s.Image == "" {
		return s, fmt.Errorf("image is required")
	}
	if _, ok := raw["sizes"]; !ok {
		return s, fmt.Errorf("sizes is required")
	}
	return s, nil
}

// highest is the price for unknown size keys (spec 8.7).
func (s Settings) highest() config.Micros {
	var h config.Micros
	for _, z := range s.Sizes {
		if z.USDPerHour > h {
			h = z.USDPerHour
		}
	}
	return h
}
