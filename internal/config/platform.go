package config

import (
	"runtime"
	"sort"
)

// platformGOOS is the OS Load checks the config against (tests stand in another).
var platformGOOS = runtime.GOOS

// checkPlatform refuses, off Linux, the keys a non-Linux client build cannot honour: a pool's
// memory admission (/proc/meminfo, cgroup v2) and every remote provider (their agent, sweep and
// money rails are exercised on Linux only). A darwin build runs local pools (docs/spec.md 1).
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
	types := make([]string, 0, len(e.Providers))
	for t := range e.Providers {
		types = append(types, t)
	}
	sort.Strings(types)
	for _, t := range types {
		p.addf("%s: providers.%s: a remote provider runs on Linux only (this is a %s build)", file, t, goos)
	}
}
