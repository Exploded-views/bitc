package config

import (
	"runtime"
	"sort"
)

// platformGOOS is the OS Load checks the config against (tests stand in another).
var platformGOOS = runtime.GOOS

// clientProviders are the built-in provider types a non-Linux client build loads: ssh runs as a
// client (the agent runs on the Linux host, #4), and sprites is decoded so that it gets the same
// refusal as on Linux (#6, SpritesRefusal) and so that sweep can clean up after 0.1.0. A plugin
// provider loads too (#26). Every other table is refused for the platform: RunPod is parked.
var clientProviders = map[string]bool{"ssh": true, "sprites": true}

// platformRefusedTypes lists, sorted, the [providers.<type>] tables of a host file that goos
// cannot honour. It reads no value: only the names and the plugin key.
func platformRefusedTypes(s fileSchema, goos string) []string {
	if goos == "linux" {
		return nil
	}
	var out []string
	for t, raw := range s.Providers {
		if _, isPlugin := raw["plugin"]; !isPlugin && !clientProviders[t] {
			out = append(out, t)
		}
	}
	sort.Strings(out)
	return out
}

// checkPlatform refuses, off Linux, the keys a non-Linux client build cannot honour: a pool's
// memory admission (/proc/meminfo, cgroup v2) and the remote providers other than the ssh client
// (RunPod is parked; sprites is refused for #6 on every OS, SpritesRefusal). A darwin build runs
// local pools, exec plugin providers and the ssh provider as a client (docs/spec.md 1).
func checkPlatform(p *problems, e *Effective, goos string) {
	if goos == "linux" {
		return
	}
	file := e.HostFile
	if file == "" {
		file = "host config"
	}
	pools := make([]string, 0, len(e.Pools))
	for n := range e.Pools {
		pools = append(pools, n)
	}
	sort.Strings(pools)
	for _, n := range pools {
		if e.Pools[n].Memory > 0 {
			p.addf("%s: pools.%s.memory: memory admission runs on Linux only (this is a %s build)", file, n, goos)
		}
	}
	types := append([]string(nil), e.platformRefused...) // not decoded off Linux (applyHost)
	for t, pr := range e.Providers {
		if pr.Plugin == nil && !clientProviders[t] { // a plugin provider and the ssh client run on darwin
			types = append(types, t)
		}
	}
	sort.Strings(types)
	for _, t := range types {
		if t == "runpod" {
			p.addf("%s: providers.runpod: RunPod runs on Linux only for now (this is a %s build)", file, goos)
			continue
		}
		p.addf("%s: providers.%s: a remote provider runs on Linux only (this is a %s build)", file, t, goos)
	}
}
