package config

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Exploded-views/bitc/internal/exit"
)

// runState holds BITC_* usage problems (64, spec 5.2) and the run narrowing flags. It is the
// unexported field `run runState` that this task adds to Effective in model.go.
type runState struct {
	usage    []string
	wait     *time.Duration
	local    bool
	provider string
	arts     *[]string
}

func rs(e *Effective) *runState { return &e.run }

func usageErr(e *Effective) error {
	if u := rs(e).usage; len(u) > 0 {
		return exit.Usagef("%s", strings.Join(u, "; "))
	}
	return nil
}

// applyEnv applies BITC_* overrides (spec 5.6) and --state-dir. Empty values mean unset.
func applyEnv(p *problems, e *Effective, env map[string]string, o LoadOptions) {
	st := rs(e)
	if v := env["BITC_DIR"]; v != "" {
		e.Host.StateDir, e.Sources["host.state_dir"] = v, "env BITC_DIR"
	}
	if o.Flags.StateDir != "" {
		e.Host.StateDir, e.Sources["host.state_dir"] = o.Flags.StateDir, "flag --state-dir"
	}
	if !filepath.IsAbs(e.Host.StateDir) {
		e.Host.StateDir = filepath.Join(e.Cwd, e.Host.StateDir)
	}
	if v := env["BITC_POLL"]; v != "" {
		d, err := ParseDuration(v)
		switch {
		case err != nil:
			st.usage = append(st.usage, fmt.Sprintf("BITC_POLL=%q %v", v, err))
		case d < 100*time.Millisecond:
			st.usage = append(st.usage, fmt.Sprintf("BITC_POLL=%q must be at least 100ms", v))
		default:
			e.Host.Poll, e.Sources["host.poll"] = d, "env BITC_POLL"
		}
	}
	if v := env["BITC_PROVIDER"]; v != "" {
		if v != "local" {
			st.usage = append(st.usage, fmt.Sprintf("BITC_PROVIDER=%q: the only value is local", v))
		}
		e.EnvProvider = v
	}
	if v, ok := env["BITC_ARTIFACTS"]; ok {
		list := []string{}
		for _, a := range strings.Split(v, ",") {
			if a = strings.TrimRight(a, "/"); a != "" {
				list = append(list, a)
			}
		}
		e.EnvArtifacts = &list
	}
	e.LogLevel = "info"
	switch v := env["BITC_LOG"]; v {
	case "":
	case "info", "debug", "quiet":
		e.LogLevel = v
	default:
		st.usage = append(st.usage, fmt.Sprintf("BITC_LOG=%q must be info, debug or quiet", v))
	}
}

// checkTemplates parses every host-side template and checks its variables (spec 5.7).
func checkTemplates(p *problems, e *Effective) {
	known := func(name string) bool {
		_, isVar := e.Vars[name]
		return isVar || HostOnlyVars[name] || name == "pool" || name == "job"
	}
	integer := func(name string) bool {
		if name == "slot" || name == "slots" {
			return true
		}
		v, ok := e.Vars[name]
		return ok && intRE.MatchString(v)
	}
	// every message names the file that supplied the template, the key and the quoted value (5.2)
	check := func(file, key string, t Template) {
		bad := func(format string, a ...any) {
			p.addf("%s: %s = %q: "+format, append([]any{firstNonEmpty(file, e.HostFile), key, t.Raw}, a...)...)
		}
		parts, err := ParseTemplate(t.Raw)
		if err != nil {
			bad("%v", err)
			return
		}
		for _, pt := range parts {
			if pt.Var == "" {
				continue
			}
			if RemoteOnlyVars[pt.Var] {
				bad("{%s} is remote-only", pt.Var)
				continue
			}
			if !known(pt.Var) {
				bad("unknown template variable %s", pt.Var)
				continue
			}
			if pt.Op == "+" || pt.Op == "-" {
				if !integer(pt.Var) {
					bad("%s is not an integer", pt.Var)
				}
				if varRE.MatchString(pt.Arg) && (!known(pt.Arg) || !integer(pt.Arg)) {
					if !known(pt.Arg) {
						bad("unknown template variable %s", pt.Arg)
					} else {
						bad("%s is not an integer", pt.Arg)
					}
				}
			}
		}
	}
	layerFile := func(layer string) string {
		if layer == "repo" {
			return e.RepoFile
		}
		return e.HostFile
	}
	for _, name := range sortedKeys(e.Pools) {
		pool := e.Pools[name]
		for _, k := range sortedKeys(pool.Env) {
			key := "pools." + name + ".env." + k
			check(e.Sources[key], key, pool.Env[k])
		}
		for _, k := range sortedKeys(pool.EnvDefault) {
			key := "pools." + name + ".env_default." + k
			check(e.Sources[key], key, pool.EnvDefault[k])
		}
		for _, h := range pool.Hooks.PreRun { // pre_run runs before queueing: no slot is held (spec 6.5)
			for i, a := range h.Argv {
				names, _ := a.Names()
				for _, n := range names {
					if n == "slot" || n == "hold" {
						p.addf("pools.%s.hooks.pre_run.argv[%d] uses {%s}, but pre_run runs before a slot is held", name, i, n)
					}
				}
			}
		}
		for _, h := range append(append(append([]Hook{}, pool.Hooks.PreRun...), pool.Hooks.PostRun...), hookList(pool.Hooks.Env)...) {
			for i, a := range h.Argv {
				check(layerFile(h.Layer), fmt.Sprintf("pools.%s.hooks.%s.argv[%d]", name, h.Name, i), a)
			}
		}
		for i, w := range pool.Hooks.Wrap {
			src := e.Sources[fmt.Sprintf("pools.%s.hooks.wrap[%d]", name, i)]
			for j, a := range w.Argv {
				check(src, fmt.Sprintf("pools.%s.hooks.wrap[%d].argv[%d]", name, i, j), a)
			}
		}
	}
}

func hookList(h *Hook) []Hook {
	if h == nil {
		return nil
	}
	return []Hook{*h}
}

// checkRemoteEnv: in a remote-capable pool, env keys using host-only variables must be
// overridden or unset by the remote profile (spec 5.7); forwarded names must be valid env names and
// secret-like ones need force (8.4).
func checkRemoteEnv(p *problems, e *Effective) {
	for _, name := range sortedKeys(e.Pools) {
		pool := e.Pools[name]
		remoteListed := false
		for _, t := range pool.Providers {
			remoteListed = remoteListed || t != "local"
		}
		if !remoteListed || pool.Remote == nil {
			continue
		}
		covered := map[string]bool{}
		for _, m := range []map[string]Template{pool.Remote.Env} {
			for k := range m {
				covered[k] = true
			}
		}
		if e.Remote != nil {
			for k := range e.Remote.Env {
				covered[k] = true
			}
			for _, k := range e.Remote.EnvUnset {
				covered[k] = true
			}
		}
		for _, k := range pool.Remote.EnvUnset {
			covered[k] = true
		}
		for _, k := range sortedKeys(pool.Env) {
			names, _ := pool.Env[k].Names()
			for _, n := range names {
				if HostOnlyVars[n] && !covered[k] {
					p.addf("pools.%s.env.%s uses the host-only {%s}; set it in [pools.%s.remote] env or [remote] env, or list it in env_unset", name, k, n, name)
					break
				}
			}
		}
	}
	for _, l := range []struct {
		key   string
		names []string
	}{
		{"remote_limits.forward_env_allow", e.RemoteLimits.ForwardEnvAllow},
		{"remote_limits.forward_env_force", e.RemoteLimits.ForwardEnvForce},
	} {
		for _, n := range l.names {
			if !ValidEnvName(n) {
				p.addf("%s: %s: %q is not a valid variable name (or starts with BITC_)", e.HostFile, l.key, n)
			}
		}
	}
	if e.Remote == nil {
		return
	}
	for _, n := range e.Remote.ForwardEnv {
		if !ValidEnvName(n) {
			p.addf("%s: remote.forward_env: %q is not a valid variable name (or starts with BITC_)", e.RepoFile, n)
			continue
		}
		if secretLike(n) && !contains(e.RemoteLimits.ForwardEnvForce, n) {
			p.addf("%s: remote.forward_env names %s, which looks like a secret; the host must list it in remote_limits.forward_env_force", e.RepoFile, n)
		}
	}
}

// secretLikeParts make a forwarded variable name secret-like (spec 8.4), matched as substrings in
// any case; such a name needs remote_limits.forward_env_force.
var secretLikeParts = []string{"TOKEN", "SECRET", "KEY", "PASSWORD", "PASS", "CREDENTIAL", "AUTH", "DSN",
	"PRIVATE", "COOKIE", "SESSION", "CERT", "DATABASE_URL", "DIRECT_URL"}

func secretLike(name string) bool {
	up := strings.ToUpper(name)
	for _, s := range secretLikeParts {
		if strings.Contains(up, s) {
			return true
		}
	}
	return false
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// ApplyRunFlags records `run` narrowing flags; Resolve applies them to the pool (5.3).
func (e *Effective) ApplyRunFlags(f RunFlags) error {
	st := rs(e)
	if f.Wait != "" {
		d, err := ParseDuration(f.Wait)
		if err != nil {
			return exit.Usagef("--wait %q %v", f.Wait, err)
		}
		st.wait = &d
	}
	st.local, st.provider, st.arts = f.Local, f.Provider, f.Artifacts
	return nil
}

// Resolve looks a pool up by canonical name (spec 6.1) and applies run narrowing.
func (e *Effective) Resolve(name string) (*Pool, error) {
	if !ValidPoolName(name) {
		return nil, exit.Usagef("invalid pool name %q", name)
	}
	canon := CanonName(name)
	src, ok := e.Pools[canon]
	var pool Pool
	switch {
	case ok:
		pool = *src
	case e.Groups[canon].Name != "":
		return nil, exit.Usagef("%s is a group, not a pool", canon)
	case e.Host.UnknownPools == "default" && e.DefaultPool != nil:
		pool = *e.DefaultPool
		pool.Name, pool.AdHoc = canon, true
	default:
		known := sortedKeys(e.Pools)
		sort.Strings(known)
		return nil, exit.Usagef("unknown pool %s (known: %s)", canon, strings.Join(known, ", "))
	}
	st := rs(e)
	if st.wait != nil {
		if *st.wait > pool.Wait {
			return nil, exit.Usagef("--wait %s is longer than pool %s's wait %s (it may only shorten it)", *st.wait, canon, pool.Wait)
		}
		pool.Wait = *st.wait
	}
	if st.local || e.EnvProvider == "local" {
		pool.Providers = []string{"local"}
	}
	if st.provider != "" {
		if st.provider == "local" || !contains(pool.Providers, st.provider) {
			return nil, exit.Usagef("--provider %s: pool %s's providers are %s", st.provider, canon, strings.Join(pool.Providers, ", "))
		}
		list := []string{st.provider}
		if contains(pool.Providers, "local") {
			list = append(list, "local")
		}
		pool.Providers = list
	}
	arts := st.arts
	if arts == nil {
		arts = e.EnvArtifacts
	}
	if arts != nil {
		declared := map[string]Artifact{}
		if pool.Remote != nil {
			for _, a := range pool.Remote.Artifacts {
				declared[a.Path] = a
			}
		}
		var sub []Artifact
		for _, a := range *arts {
			d, ok := declared[a]
			if !ok {
				return nil, exit.Usagef("artifact %q is not declared for pool %s", a, canon)
			}
			sub = append(sub, d)
		}
		if pool.Remote != nil {
			r := *pool.Remote
			r.Artifacts = sub
			pool.Remote = &r
		}
	}
	return &pool, nil
}
