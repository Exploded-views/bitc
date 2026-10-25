package run

import (
	"sort"
	"strconv"

	"github.com/Exploded-views/bitc/internal/admit"
	"github.com/Exploded-views/bitc/internal/config"
	"github.com/Exploded-views/bitc/internal/exit"
)

// KV is one NAME=VALUE pair from an env hook.
type KV struct{ Name, Value string }

// EnvInput is everything the job's environment derives from (spec 6.3).
type EnvInput struct {
	Caller []string
	Eff    *config.Effective
	Pool   *config.Pool
	Hold   *admit.Hold
	Job    string
	Vars   config.Vars
}

var bitcSet = []string{"BITC_POOL", "BITC_SLOT", "BITC_HOLD", "BITC_JOB", "BITC_DIR"}

// BaseEnv: caller env, minus [defaults].env_unset; env_default for unset keys; env; minus the
// pool's env_unset; then the BITC_* variables (spec 6.3). Host and repo already merged (5.3).
func BaseEnv(in EnvInput) ([]string, error) {
	env := withoutKeys(in.Caller, append(append([]string{}, in.Pool.DefaultsEnvUnset...), bitcSet...)...)
	has := func(k string) bool {
		for _, kv := range env {
			if len(kv) > len(k) && kv[:len(k)] == k && kv[len(k)] == '=' {
				return true
			}
		}
		return false
	}
	set := func(k string, t config.Template) error {
		v, err := t.Expand(in.Vars)
		if err != nil {
			return exit.Configf("pools.%s.env.%s: %v", in.Pool.Name, k, err)
		}
		env = append(withoutKeys(env, k), k+"="+v)
		return nil
	}
	for _, k := range sortedKeys(in.Pool.EnvDefault) {
		if !has(k) {
			if err := set(k, in.Pool.EnvDefault[k]); err != nil {
				return nil, err
			}
		}
	}
	for _, k := range sortedKeys(in.Pool.Env) {
		if err := set(k, in.Pool.Env[k]); err != nil {
			return nil, err
		}
	}
	env = withoutKeys(env, in.Pool.EnvUnset...)
	return append(env, BitcVars(in)...), nil
}

// BitcVars are set last and re-asserted after the env hook.
func BitcVars(in EnvInput) []string {
	vars := []string{
		"BITC_POOL=" + in.Pool.Name,
		"BITC_SLOT=" + in.Pool.Name + "." + strconv.Itoa(in.Hold.Slot),
		"BITC_HOLD=" + in.Hold.EnvValue(),
		"BITC_JOB=" + in.Job,
		"BITC_DIR=" + in.Eff.Host.StateDir,
	}
	return append(vars, in.Hold.ExtraEnv...)
}

// ApplyHookEnv applies env-hook output, then re-asserts the BITC_* variables.
func ApplyHookEnv(env []string, kv []KV, bitc []string) []string {
	for _, p := range kv {
		env = append(withoutKeys(env, p.Name), p.Name+"="+p.Value)
	}
	for _, b := range bitc {
		env = append(withoutKeys(env, b[:indexByte(b, '=')]), b)
	}
	return env
}

// TemplateVars are [vars] plus {pool} and {job}; with a hold also the host-only variables.
func TemplateVars(eff *config.Effective, pool *config.Pool, hold *admit.Hold, job string) config.Vars {
	v := config.Vars{}
	for k, x := range eff.Vars {
		v[k] = x
	}
	v["pool"], v["job"] = pool.Name, job
	v["slots"] = strconv.Itoa(pool.Slots)
	v["state_dir"], v["worktree"], v["cwd"] = eff.Host.StateDir, eff.Worktree, eff.Cwd
	if hold != nil {
		v["slot"] = strconv.Itoa(hold.Slot)
		v["hold"] = hold.EnvValue()
	}
	return v
}

func sortedKeys(m map[string]config.Template) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func withoutKeys(env []string, keys ...string) []string {
	out := make([]string, 0, len(env))
next:
	for _, kv := range env {
		for _, k := range keys {
			if len(kv) > len(k) && kv[:len(k)] == k && kv[len(k)] == '=' {
				continue next
			}
		}
		out = append(out, kv)
	}
	return out
}

func indexByte(s string, c byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			return i
		}
	}
	return len(s)
}
