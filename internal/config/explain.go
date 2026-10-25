package config

import (
	"fmt"
	"sort"
	"strings"
)

// Explain lists effective values with their source (spec 3). Credentials print <redacted>.
func Explain(e *Effective, key string) []string {
	src := func(k string) string {
		if s := e.Sources[k]; s != "" {
			return s
		}
		if e.HostFile != "" {
			return e.HostFile + " or default"
		}
		return "default"
	}
	var out []string
	add := func(k string, v any) {
		if key == "" || k == key || strings.HasPrefix(k, key+".") {
			out = append(out, fmt.Sprintf("%s = %v  # %s", k, v, src(k)))
		}
	}
	h := e.Host
	add("host.state_dir", fmt.Sprintf("%q", h.StateDir))
	add("host.poll", h.Poll)
	add("host.memory_floor", h.MemoryFloor)
	add("host.reservation_window", h.ReservationWindow)
	add("host.unknown_pools", h.UnknownPools)
	add("host.grace", h.Grace)
	add("host.max_signal_wait", h.MaxSignalWait)
	add("host.history_max", h.HistoryMax)
	add("host.retention", h.Retention)
	add("detach.wait_timeout", e.Detach.WaitTimeout)
	for _, g := range sortedKeys(e.Groups) {
		add("groups."+g+".units", e.Groups[g].Units)
	}
	for _, n := range sortedKeys(e.Pools) {
		p := e.Pools[n]
		k := "pools." + n
		add(k+".slots", p.Slots)
		add(k+".wait", p.Wait)
		add(k+".memory", p.Memory)
		add(k+".providers", fmt.Sprintf("%q", p.Providers))
		add(k+".on_signal", p.OnSignal)
		for _, ek := range sortedKeys(p.Env) {
			add(k+".env."+ek, fmt.Sprintf("%q", p.Env[ek].Raw))
		}
		for _, ek := range sortedKeys(p.EnvDefault) {
			add(k+".env_default."+ek, fmt.Sprintf("%q", p.EnvDefault[ek].Raw))
		}
		if len(p.EnvUnset) > 0 {
			add(k+".env_unset", fmt.Sprintf("%q", p.EnvUnset))
		}
	}
	for _, t := range sortedKeys(e.Providers) {
		pr := e.Providers[t]
		k := "providers." + t
		if pr.APIKey != "" {
			add(k+".api_key", "<redacted>")
		}
		add(k+".name_prefix", pr.NamePrefix)
		add(k+".daily_usd", pr.DailyUSD.Dollars())
		add(k+".max_live", pr.MaxLive)
		if sk := k + ".send_env"; key == "" || sk == key || strings.HasPrefix(sk, key+".") {
			out = append(out, fmt.Sprintf("%s = %q  # %s", sk, pr.SendEnv, pr.SendEnvSource)) // default or host (S.5)
		}
		if pl := pr.Plugin; pl != nil { // never a secret value: the files are named, not read
			add(k+".plugin.path", pl.Path)
			add(k+".plugin.sha256", pl.SHA256)
			if len(pl.EnvPass) > 0 {
				add(k+".plugin.env_pass", fmt.Sprintf("%q", pl.EnvPass))
			}
			for _, n := range sortedKeys(pl.SecretFiles) {
				add(k+".plugin.secret_files."+n, pl.SecretFiles[n])
			}
			for _, n := range sortedKeys(pr.Sizes) {
				add(k+".sizes."+n+".usd_per_hour", pr.Sizes[n].Dollars())
			}
		}
		keys := make([]string, 0, len(pr.Raw))
		for rk := range pr.Raw {
			keys = append(keys, rk)
		}
		sort.Strings(keys)
		for _, rk := range keys {
			add(k+"."+rk, fmt.Sprintf("%v", pr.Raw[rk]))
		}
	}
	return out
}
