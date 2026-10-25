package remote

import (
	"slices"
	"sort"
	"strings"

	"github.com/Exploded-views/bitc/internal/provider"
	"github.com/Exploded-views/bitc/internal/run"
)

// CallerEnvNames splits the job's forwarded names (providers delta S.1): the names in the repo's
// forward_env, in the host's forward_env_allow and set in the caller's environment (an empty
// value counts as set), into forced ones (listed in forward_env_force; a secret-like name must
// be, or load refused it) and plain ones. Both lists are sorted; no value is read.
func CallerEnvNames(p *run.Prepared, caller []string) (forced, plain []string) {
	if p.Eff.Remote == nil {
		return nil, nil
	}
	set := map[string]bool{}
	for _, kv := range caller {
		k, _, _ := strings.Cut(kv, "=")
		set[k] = true
	}
	for _, n := range p.Eff.Remote.ForwardEnv {
		if !set[n] || !slices.Contains(p.Eff.RemoteLimits.ForwardEnvAllow, n) || slices.Contains(forced, n) || slices.Contains(plain, n) {
			continue
		}
		if slices.Contains(p.Eff.RemoteLimits.ForwardEnvForce, n) {
			forced = append(forced, n)
		} else {
			plain = append(plain, n)
		}
	}
	sort.Strings(forced)
	sort.Strings(plain)
	return forced, plain
}

// SendEnvCheck is the caller-env preflight check (providers delta S.3): the job needs "none" with
// no forwarded name, "forced" with at least one forced name, else "forwarded"; a provider whose
// effective level is lower, or a multi-job one (S.4, which never takes a forced name, whatever
// its setting), cannot take it. Nothing is stripped to make a job fit; the note names the names,
// never a value.
func SendEnvCheck(typ string, level provider.EnvLevel, multiJob bool, forced, plain []string) error {
	if multiJob && level > provider.EnvForwarded {
		level = provider.EnvForwarded
	}
	var names []string
	switch {
	case len(forced) > 0 && level < provider.EnvForced:
		names = forced
		if level == provider.EnvNone {
			names = append(slices.Clone(forced), plain...)
		}
	case len(plain) > 0 && level < provider.EnvForwarded:
		names = plain
	default:
		return nil
	}
	sort.Strings(names)
	kind := "forwarded"
	if level == provider.EnvForwarded {
		kind = "forced"
	}
	return ineligible("%s env not sent to %s (providers.%s.send_env = %q): %s", kind, typ, typ, level.String(), strings.Join(names, ", "))
}
