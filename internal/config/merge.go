package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// dur, size, positive parse a value and record a problem naming file, key and quoted value.
func dur(p *problems, file, key string, s *string, def time.Duration) time.Duration {
	if s == nil {
		return def
	}
	d, err := ParseDuration(*s)
	if err != nil {
		p.addf("%s: %s = %q %v", file, key, *s, err)
		return def
	}
	return d
}

func size(p *problems, file, key string, s *string, def Size) Size {
	if s == nil {
		return def
	}
	v, err := ParseSize(*s)
	if err != nil {
		p.addf("%s: %s = %q %v", file, key, *s, err)
		return def
	}
	return v
}

func atLeast1(p *problems, file, key string, v *int64, def int) int {
	if v == nil {
		return def
	}
	if *v < 1 {
		p.addf("%s: %s = %d must be at least 1", file, key, *v)
		return def
	}
	return int(*v)
}

func enum(p *problems, file, key string, s *string, def string, allowed ...string) string {
	if s == nil {
		return def
	}
	for _, a := range allowed {
		if *s == a {
			return a
		}
	}
	p.addf("%s: %s = %q must be one of %s", file, key, *s, strings.Join(allowed, ", "))
	return def
}

// applyHost fills host-only sections from the host layer and built-in defaults (spec 5.4).
func applyHost(p *problems, e *Effective, host layer, env map[string]string) {
	f, s := host.file, host.schema
	h := s.Host
	if h == nil {
		h = &hostSchema{}
	}
	stateBase := firstNonEmpty(env["XDG_STATE_HOME"], filepath.Join(env["HOME"], ".local", "state"))
	e.Host = Host{
		StateDir:          filepath.Join(stateBase, "bitc"),
		Poll:              dur(p, f, "host.poll", h.Poll, 5*time.Second),
		MemoryFloor:       size(p, f, "host.memory_floor", h.MemoryFloor, 6<<30),
		ReservationWindow: dur(p, f, "host.reservation_window", h.ReservationWindow, 60*time.Second),
		UnknownPools:      enum(p, f, "host.unknown_pools", h.UnknownPools, "refuse", "refuse", "default"),
		Grace:             dur(p, f, "host.grace", h.Grace, 10*time.Second),
		MaxSignalWait:     dur(p, f, "host.max_signal_wait", h.MaxSignalWait, 10*time.Minute),
		HistoryMax:        size(p, f, "host.history_max", h.HistoryMax, 50<<20),
		Retention:         dur(p, f, "host.retention", h.Retention, 14*24*time.Hour),
	}
	e.Sources["host.state_dir"] = "default"
	if h.StateDir != nil {
		if *h.StateDir == "" {
			p.addf("%s: host.state_dir = \"\" (an empty string never means the default)", f)
		} else {
			e.Host.StateDir = expandHome(*h.StateDir, env)
			e.Sources["host.state_dir"] = f
		}
	}
	for g, gs := range s.Groups {
		canon := CanonName(g)
		if !ValidPoolName(g) {
			p.addf("%s: group name %q is invalid", f, g)
		}
		if _, dup := e.Groups[canon]; dup {
			p.addf("%s: groups %s: same group spelled twice", f, canon)
		}
		if gs.Units == nil {
			p.addf("%s: groups.%s.units is required", f, g)
			continue
		}
		e.Groups[canon] = Group{Name: canon, Units: atLeast1(p, f, "groups."+g+".units", gs.Units, 1)}
	}
	if s.Priority != nil {
		e.Priority = Priority{HighCwdContains: s.Priority.HighCwdContains, HighCwd: s.Priority.HighCwd}
		for _, g := range s.Priority.HighCwd {
			if err := ValidGlob(g); err != nil {
				p.addf("%s: priority.high_cwd %q: %v", f, g, err)
			}
		}
	}
	e.Detach.WaitTimeout = 9 * time.Minute
	if s.Detach != nil {
		e.Detach.WaitTimeout = dur(p, f, "detach.wait_timeout", s.Detach.WaitTimeout, 9*time.Minute)
	}
	e.RemoteLimits.OnFailure = "next"
	if rl := s.RemoteLimits; rl != nil {
		e.RemoteLimits = RemoteLimits{Worktrees: rl.Worktrees, ForwardEnvAllow: rl.ForwardEnvAllow,
			ForwardEnvForce: rl.ForwardEnvForce,
			OnFailure:       enum(p, f, "remote_limits.on_failure", rl.OnFailure, "next", "next", "stop")}
	}
	for i, r := range s.Repos {
		if r.Match == "" {
			p.addf("%s: repos[%d].match is required", f, i)
		}
		g := RepoGrant(r)
		if g.DeployKeyFile != "" {
			g.DeployKeyFile = expandHome(g.DeployKeyFile, env)
			if why := keyFileProblem(g.DeployKeyFile); why != "" {
				p.addf("%s: [[repos]] deploy_key_file: %s", f, why)
			}
		}
		e.Repos = append(e.Repos, g)
	}
	if c := s.Compat; c != nil {
		if c.LegacyDir == "" {
			p.addf("%s: compat.legacy_dir is required", f)
		}
		cg := map[string]string{}
		for g, stem := range c.Groups {
			canon := CanonName(g)
			if !ValidPoolName(g) {
				p.addf("%s: compat.groups key %q is invalid", f, g)
			} else if _, ok := e.Groups[canon]; !ok {
				p.addf("%s: compat.groups.%s names no declared group", f, g)
			}
			if _, dup := cg[canon]; dup {
				p.addf("%s: compat.groups %s: same group spelled twice", f, canon)
			}
			if !ValidPoolName(stem) {
				p.addf("%s: compat.groups.%s: legacy lock stem %q is invalid", f, g, stem)
			}
			cg[canon] = stem
		}
		e.Compat = &Compat{LegacyDir: expandHome(c.LegacyDir, env), HoldEnv: c.HoldEnv, NamePrefix: c.NamePrefix, Groups: cg}
	}
	if t := s.Test; t != nil {
		e.Test = &Test{}
		if t.ProcfsRoot != nil {
			e.Test.ProcfsRoot = *t.ProcfsRoot
		}
		if t.TimeScale != nil {
			if *t.TimeScale <= 0 || *t.TimeScale > 1 {
				p.addf("%s: test.time_scale = %v must be in (0, 1]", f, *t.TimeScale)
			}
			e.Test.TimeScale = *t.TimeScale
		}
		if t.HTTPResults != nil {
			e.Test.HTTPResults = *t.HTTPResults
		}
		if t.AgentURL != nil {
			e.Test.AgentURL = *t.AgentURL
		}
		if t.AccountingDir != nil {
			e.Test.AccountingDir = *t.AccountingDir
		}
		if t.FixedSecrets != nil {
			e.Test.FixedSecrets = *t.FixedSecrets
		}
		if t.SSHExec != nil {
			if *t.SSHExec != "local" {
				p.addf("%s: test.ssh_exec = %q must be \"local\"", f, *t.SSHExec)
			}
			e.Test.SSHExec = *t.SSHExec
		}
		if t.SSHGate != nil {
			e.Test.SSHGate = *t.SSHGate
		}
		if t.SSHRoot != nil {
			e.Test.SSHRoot = *t.SSHRoot
		}
		if t.SSHSshd != nil {
			e.Test.SSHSshd = *t.SSHSshd
		}
	}
	decodeProviders(p, e, f, s.Providers, env)
}

// layerEnv resolves one layer's defaults < pool maps.
func layerEnv(defaults, pool map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range defaults {
		out[k] = v
	}
	for k, v := range pool {
		out[k] = v
	}
	return out
}

func mergeHostWins(repo, host map[string]string) map[string]Template {
	out := map[string]Template{}
	for k, v := range repo {
		out[k] = Template{Raw: v}
	}
	for k, v := range host {
		out[k] = Template{Raw: v}
	}
	return out
}

// noteSources records, per merged key, the file that supplied it (host wins), so template
// errors and `config explain` name the right file (spec 5.2).
func noteSources(e *Effective, prefix, repoFile, hostFile string, repo, host map[string]string) {
	for k := range repo {
		e.Sources[prefix+"."+k] = repoFile
	}
	for k := range host {
		e.Sources[prefix+"."+k] = hostFile
	}
}

func union(lists ...[]string) []string {
	seen := map[string]bool{}
	var out []string
	for _, l := range lists {
		for _, s := range l {
			if !seen[s] {
				seen[s] = true
				out = append(out, s)
			}
		}
	}
	return out
}

func hook(p *problems, file, key, layer string, h *hookSchema) *Hook {
	if h == nil {
		return nil
	}
	if len(h.Argv) == 0 {
		p.addf("%s: %s.argv is required", file, key)
	}
	out := &Hook{Name: key[strings.LastIndex(key, ".")+1:], Timeout: dur(p, file, key+".timeout", h.Timeout, 60*time.Second),
		RefuseExit: []int{64}, Layer: layer}
	for _, a := range h.Argv {
		out.Argv = append(out.Argv, Template{Raw: a})
	}
	if h.RefuseExit != nil {
		out.RefuseExit = nil
		for _, c := range h.RefuseExit {
			if c < 1 || c > 255 {
				p.addf("%s: %s.refuse_exit has %d (1-255)", file, key, c)
			}
			out.RefuseExit = append(out.RefuseExit, int(c))
		}
	}
	return out
}

// mergePools builds every pool from the host layer, narrowed and extended by the repo layer.
func mergePools(p *problems, e *Effective, host layer, repo *layer) {
	hs := host.schema
	hd := hs.Defaults
	if hd == nil {
		hd = &defaultsSchema{}
	}
	var rs fileSchema
	rd := &defaultsSchema{}
	if repo != nil {
		rs = repo.schema
		if rs.Defaults != nil {
			rd = rs.Defaults
		}
		for k, v := range rs.Vars {
			setVar(p, e, repo.file, k, v)
		}
		e.Remote = repoRemote(p, repo.file, rs.Remote)
	}
	for k, v := range hs.Vars {
		setVar(p, e, host.file, k, v)
	}
	defSlots := atLeast1(p, host.file, "defaults.slots", hd.Slots, 1)
	defWait := dur(p, host.file, "defaults.wait", hd.Wait, 10*time.Minute)
	var repoDefWait time.Duration // the repo's narrowing [defaults].wait (valid only when repoDefSet)
	repoDefSet := false
	if repo != nil && rd.Wait != nil {
		w := dur(p, repo.file, "defaults.wait", rd.Wait, defWait)
		if w > defWait {
			p.addf("%s: defaults.wait = %q may only narrow the host's %s", repo.file, *rd.Wait, defWait)
		} else {
			defWait = w
			repoDefWait, repoDefSet = w, true
		}
	}
	build := func(name string, hp, rp *poolSchema) *Pool {
		if hp == nil {
			hp = &poolSchema{}
		}
		if rp == nil {
			rp = &poolSchema{}
		}
		key := "pools." + name
		pool := &Pool{Name: name, Slots: atLeast1(p, host.file, key+".slots", hp.Slots, defSlots),
			Wait: dur(p, host.file, key+".wait", hp.Wait, defWait), Groups: map[string]int{},
			Providers: []string{"local"}, OnSignal: "terminate", RemoteTimeout: 90 * time.Minute}
		if rp.Wait != nil {
			w := dur(p, repo.file, key+".wait", rp.Wait, pool.Wait)
			if w > pool.Wait {
				p.addf("%s: %s.wait = %q may only narrow the host's %s", repo.file, key, *rp.Wait, pool.Wait)
			} else {
				pool.Wait = w
			}
		} else if repoDefSet && repoDefWait < pool.Wait {
			pool.Wait = repoDefWait // the repo's [defaults].wait narrows an explicit host pool wait too
		}
		pool.Memory = size(p, host.file, key+".memory", hp.Memory, 0)
		for g, units := range hp.Groups {
			gc := CanonName(g)
			grp, ok := e.Groups[gc]
			switch {
			case !ok:
				p.addf("%s: %s.groups names %s, which is not a [groups] entry", host.file, key, g)
			case units < 1 || int(units) > grp.Units:
				p.addf("%s: %s.groups.%s = %d must be 1..%d", host.file, key, g, units, grp.Units)
			}
			pool.Groups[gc] = int(units)
		}
		if hp.Providers != nil {
			pool.Providers = hp.Providers
		}
		for _, t := range pool.Providers {
			if t != "local" && e.Providers[t] == nil {
				p.addf("%s: %s.providers names %q, which is not local or a configured [providers.<type>]", host.file, key, t)
			}
		}
		pool.Providers = without(pool.Providers, rp.ProvidersExclude)
		if hp.RemoteLimits != nil {
			pool.RemoteSize = hp.RemoteLimits.Size
			pool.RemoteTimeout = dur(p, host.file, key+".remote_limits.timeout", hp.RemoteLimits.Timeout, 90*time.Minute)
			for t, sk := range pool.RemoteSize {
				if !ValidSizeKey(sk) {
					p.addf("%s: %s.remote_limits.size.%s = %q is not a size key [a-z0-9]{1,10}", host.file, key, t, sk)
				}
				if pr := e.Providers[t]; pr != nil && pr.Plugin != nil {
					if _, ok := pr.Sizes[sk]; !ok { // a plugin prices only the sizes the host lists (A.7.1)
						p.addf("%s: providers.%s.sizes: a usd_per_hour per size is required for a plugin", host.file, t)
					}
				}
			}
		}
		repoEnv, hostEnv := layerEnv(rd.Env, rp.Env), layerEnv(hd.Env, hp.Env)
		repoDef, hostDef := layerEnv(rd.EnvDefault, rp.EnvDefault), layerEnv(hd.EnvDefault, hp.EnvDefault)
		pool.Env = mergeHostWins(repoEnv, hostEnv)
		pool.EnvDefault = mergeHostWins(repoDef, hostDef)
		noteSources(e, key+".env", fileOf(repo), host.file, repoEnv, hostEnv)
		noteSources(e, key+".env_default", fileOf(repo), host.file, repoDef, hostDef)
		pool.DefaultsEnvUnset = union(hd.EnvUnset, rd.EnvUnset)
		pool.EnvUnset = union(hp.EnvUnset, rp.EnvUnset)
		for _, m := range []map[string]Template{pool.Env, pool.EnvDefault} {
			for k := range m {
				if !ValidEnvName(k) {
					p.addf("%s.env: %q is not a valid variable name (or starts with BITC_)", key, k)
				}
			}
		}
		for _, l := range []struct {
			file string
			r    []refuseSchema
		}{{host.file, hp.Refuse}, {fileOf(repo), rp.Refuse}} {
			for i, r := range l.r {
				pool.Refuse = append(pool.Refuse, refuseRule(p, l.file, fmt.Sprintf("%s.refuse[%d]", key, i), r))
			}
		}
		pool.OnSignal = enum(p, host.file, key+".on_signal", hp.OnSignal, pool.OnSignal, "terminate", "forward-and-wait")
		if rp.OnSignal != nil {
			pool.OnSignal = enum(p, repo.file, key+".on_signal", rp.OnSignal, pool.OnSignal, "terminate", "forward-and-wait")
		}
		addHooks(p, e, &pool.Hooks, host.file, key, "host", hp.Hooks)
		if repo != nil {
			addHooks(p, e, &pool.Hooks, repo.file, key, "repo", rp.Hooks)
		}
		if rp.Remote != nil {
			pool.Remote = poolRemote(p, repo.file, key+".remote", rp.Remote)
			if pool.Remote.Timeout > 0 && pool.Remote.Timeout < pool.RemoteTimeout {
				pool.RemoteTimeout = pool.Remote.Timeout
			} else if pool.Remote.Timeout > pool.RemoteTimeout {
				p.addf("%s: %s.remote.timeout may only narrow the host's %s", repo.file, key, pool.RemoteTimeout)
			}
		}
		return pool
	}
	for raw, hp := range hs.Pools {
		if !ValidPoolName(raw) {
			p.addf("%s: pool name %q is invalid ([A-Za-z0-9_-]+)", host.file, raw)
			continue
		}
		name := CanonName(raw)
		if _, dup := e.Pools[name]; dup {
			p.addf("%s: pools %s: same pool spelled twice ('-' and '_' are the same name)", host.file, name)
			continue
		}
		if _, isGroup := e.Groups[name]; isGroup {
			p.addf("%s: %s is both a pool and a group", host.file, name)
		}
		e.Pools[name] = build(name, hp, findPool(rs.Pools, name))
	}
	if e.Host.UnknownPools == "default" {
		e.DefaultPool = build("", nil, nil)
	}
	for raw := range rs.Pools {
		if _, ok := e.Pools[CanonName(raw)]; !ok {
			e.Warnings = append(e.Warnings, fmt.Sprintf("%s: [pools.%s] is ignored: the host file has no such pool", repo.file, raw))
		}
	}
}

func fileOf(l *layer) string {
	if l == nil {
		return ""
	}
	return l.file
}

func findPool(m map[string]*poolSchema, canon string) *poolSchema {
	for k, v := range m {
		if CanonName(k) == canon {
			return v
		}
	}
	return nil
}

func without(list, drop []string) []string {
	var out []string
	for _, s := range list {
		keep := true
		for _, d := range drop {
			if s == d {
				keep = false
			}
		}
		if keep {
			out = append(out, s)
		}
	}
	return out
}

func setVar(p *problems, e *Effective, file, k string, v any) {
	if !varRE.MatchString(k) || HostOnlyVars[k] || RemoteOnlyVars[k] || k == "pool" || k == "job" {
		p.addf("%s: vars.%s is not a valid or free variable name", file, k)
		return
	}
	switch x := v.(type) {
	case int64:
		e.Vars[k] = fmt.Sprint(x)
	case string:
		e.Vars[k] = x
	default:
		p.addf("%s: vars.%s must be a string or an integer", file, k)
	}
}

func refuseRule(p *problems, file, key string, r refuseSchema) RefuseRule {
	kinds := 0
	if r.Tool != "" {
		kinds++
	}
	if r.NpmScript != nil {
		kinds++
	}
	if r.ArgvPrefix != nil {
		kinds++
	}
	if kinds != 1 {
		p.addf("%s: %s needs exactly one of tool, npm_script, argv_prefix", file, key)
	}
	if r.Reason == "" {
		p.addf("%s: %s.reason is required", file, key)
	}
	when := r.When
	if when == "" {
		when = "always"
	}
	if when != "always" && when != "repo-wide" {
		p.addf("%s: %s.when = %q must be always or repo-wide", file, key, r.When)
	}
	return RefuseRule{Tool: r.Tool, When: when, UnlessArgs: r.UnlessArgs, SkipOptionValues: r.SkipOptionValues,
		NpmScript: r.NpmScript, ArgvPrefix: r.ArgvPrefix, Reason: r.Reason}
}

// addHooks appends one layer's hooks; pre_run/post_run/env carry their Layer, and each wrap
// rule's file goes into e.Sources["<key>.hooks.wrap[i]"] for template messages.
func addHooks(p *problems, e *Effective, h *Hooks, file, key, layer string, hs *hooksSchema) {
	if hs == nil {
		return
	}
	if x := hook(p, file, key+".hooks.pre_run", layer, hs.PreRun); x != nil {
		h.PreRun = append(h.PreRun, *x)
	}
	if x := hook(p, file, key+".hooks.post_run", layer, hs.PostRun); x != nil {
		h.PostRun = append(h.PostRun, *x)
	}
	if x := hook(p, file, key+".hooks.env", layer, hs.Env); x != nil {
		if h.Env != nil {
			p.addf("%s: %s.hooks.env: only one env hook per pool (host and repo both set one)", file, key)
		}
		h.Env = x
	}
	for i, w := range hs.Wrap {
		if err := ValidGlob(w.WhenWorktree); err != nil || w.WhenWorktree == "" || len(w.Argv) == 0 {
			p.addf("%s: %s.hooks.wrap[%d] needs a valid when_worktree glob and an argv", file, key, i)
		}
		r := WrapRule{WhenWorktree: w.WhenWorktree}
		for _, a := range w.Argv {
			r.Argv = append(r.Argv, Template{Raw: a})
		}
		e.Sources[fmt.Sprintf("%s.hooks.wrap[%d]", key, len(h.Wrap))] = file
		h.Wrap = append(h.Wrap, r)
	}
}

func repoRemote(p *problems, file string, r *repoRemoteSchema) *RepoRemote {
	if r == nil {
		return nil
	}
	out := &RepoRemote{ForwardEnv: r.ForwardEnv, ForbidIgnored: r.ForbidIgnored, AllowIgnored: r.AllowIgnored,
		Env: mergeHostWins(r.Env, nil), EnvUnset: r.EnvUnset, Packages: r.Packages}
	for _, pk := range r.Packages {
		if !packageRE.MatchString(pk) {
			p.addf("%s: remote.packages has %q (^[a-z0-9][a-z0-9+.-]*$)", file, pk)
		}
	}
	seenTool := map[string]bool{}
	for i, t := range r.Tools {
		arch := firstNonEmpty(t.Arch, "amd64")
		if !validArch(arch) {
			p.addf("%s: remote.tools: %s/%s: invalid arch (amd64 or arm64)", file, t.Name, arch)
		} else if seenTool[t.Name+"/"+arch] {
			p.addf("%s: remote.tools: %s/%s: duplicate (name, arch)", file, t.Name, arch)
		}
		seenTool[t.Name+"/"+arch] = true
		if !toolNameRE.MatchString(t.Name) {
			p.addf("%s: remote.tools[%d] name %q must match ^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$", file, i, t.Name)
		}
		if !sha256RE.MatchString(t.SHA256) || !strings.HasPrefix(t.URL, "https://") {
			p.addf("%s: remote.tools[%d] needs an https url and a 64-hex sha256", file, i)
		}
		out.Tools = append(out.Tools, Tool{Name: t.Name, Arch: firstNonEmpty(t.Arch, "amd64"), URL: t.URL, SHA256: t.SHA256})
	}
	for i, s := range r.Setup {
		runAs := firstNonEmpty(s.RunAs, "job")
		if runAs != "job" && runAs != "root" || s.Name == "" || len(s.Argv) == 0 {
			p.addf("%s: remote.setup[%d] needs name, argv and run_as job|root", file, i)
		}
		if s.Name != "" && !toolNameRE.MatchString(s.Name) {
			p.addf("%s: remote.setup[%d] name %q must match ^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$", file, i, s.Name)
		}
		out.Setup = append(out.Setup, Step{Name: s.Name, RunAs: runAs, Argv: s.Argv})
	}
	for i, s := range r.Services {
		runAs := firstNonEmpty(s.RunAs, "job")
		if runAs != "job" && runAs != "root" || s.Name == "" || len(s.Argv) == 0 || len(s.Ready) == 0 {
			p.addf("%s: remote.services[%d] needs name, argv, ready and run_as job|root", file, i)
		}
		if s.Name != "" && !toolNameRE.MatchString(s.Name) {
			p.addf("%s: remote.services[%d] name %q must match ^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$", file, i, s.Name)
		}
		out.Services = append(out.Services, Service{Name: s.Name, RunAs: runAs, Argv: s.Argv, Ready: s.Ready,
			ReadyTimeout: dur(p, file, fmt.Sprintf("remote.services[%d].ready_timeout", i), s.ReadyTimeout, 120*time.Second)})
	}
	return out
}

// Architectures are the linux architectures bitc delivers an agent for (spec 8.4 step 1).
var Architectures = []string{"amd64", "arm64"}

func validArch(a string) bool {
	for _, x := range Architectures {
		if a == x {
			return true
		}
	}
	return false
}

func poolRemote(p *problems, file, key string, r *poolRemoteSchema) *PoolRemote {
	out := &PoolRemote{Allow: r.Allow, Env: mergeHostWins(r.Env, nil), EnvUnset: r.EnvUnset,
		Timeout: dur(p, file, key+".timeout", r.Timeout, 0)}
	seen := map[string]bool{}
	for _, a := range r.Arch {
		if !validArch(a) || seen[a] {
			p.addf("%s: %s.arch: invalid: %q", file, key, a)
		}
		seen[a] = true
		out.Arch = append(out.Arch, a)
	}
	if len(out.Arch) == 0 {
		out.Arch = []string{"amd64"}
	}
	for i, a := range r.Allow {
		if len(a) == 0 {
			p.addf("%s: %s.allow[%d] is empty", file, key, i)
		}
	}
	for _, a := range r.Artifacts {
		rep := true
		if a.Replace != nil {
			rep = *a.Replace
		}
		out.Artifacts = append(out.Artifacts, Artifact{Path: strings.TrimRight(a.Path, "/"), Replace: rep})
	}
	return out
}

// keyFileProblem applies the credential-file rules (spec 10) to a deploy key file: a regular file
// owned by the user with no group or other access. A file that does not exist yet is not a load
// error (an example config checks cleanly); the provider that needs it reads it per job.
func keyFileProblem(path string) string {
	var st unix.Stat_t
	if err := unix.Stat(path, &st); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return ""
		}
		return fmt.Sprintf("%s: %v", path, err)
	}
	switch {
	case st.Mode&unix.S_IFMT != unix.S_IFREG:
		return path + " is not a regular file"
	case st.Uid != uint32(os.Getuid()) || st.Mode&0o077 != 0:
		return fmt.Sprintf("%s must be owned by you and mode 600 (it is %o)", path, st.Mode&0o777)
	}
	return ""
}
