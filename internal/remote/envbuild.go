package remote

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/Exploded-views/bitc/internal/config"
	"github.com/Exploded-views/bitc/internal/run"
)

// RemoteEnv builds the remote env from empty (spec 8.4).
func RemoteEnv(p *run.Prepared, caller []string) ([]string, error) {
	return remoteEnv(p.Eff, p.Pool, p.Job, p.Eff.Worktree, caller)
}

func remoteEnv(eff *config.Effective, pool *config.Pool, job, wt string, caller []string) ([]string, error) {
	vars := config.Vars{}
	for k, v := range eff.Vars {
		vars[k] = v
	}
	vars["pool"], vars["job"] = pool.Name, job
	y, w := time.Now().UTC().ISOWeek()
	vars["week"] = fmt.Sprintf("%d-W%02d", y, w)

	var order []string
	m := map[string]string{}
	set := func(k, v string) {
		if _, ok := m[k]; !ok {
			order = append(order, k)
		}
		m[k] = v
	}
	unset := func(keys []string) {
		for _, k := range keys {
			delete(m, k)
			order = slices.DeleteFunc(order, func(o string) bool { return o == k })
		}
	}
	callerMap := map[string]string{}
	for _, kv := range caller {
		k, v, _ := strings.Cut(kv, "=")
		callerMap[k] = v
	}
	var repoEnv, poolRemoteEnv map[string]config.Template
	var repoUnset, poolRemoteUnset, forward []string
	if eff.Remote != nil {
		repoEnv, repoUnset, forward = eff.Remote.Env, eff.Remote.EnvUnset, eff.Remote.ForwardEnv
	}
	if pool.Remote != nil {
		poolRemoteEnv, poolRemoteUnset = pool.Remote.Env, pool.Remote.EnvUnset
	}
	for _, n := range forward { // repo forward_env intersected with the host allow list
		if v, ok := callerMap[n]; ok && slices.Contains(eff.RemoteLimits.ForwardEnvAllow, n) {
			set(n, v)
		}
	}
	// A pool key the remote profile overrides or unsets may use a host-only variable, which load
	// allowed exactly because the profile replaces it (5.7): its expansion error is not one.
	replaced := func(k string) bool {
		_, a := repoEnv[k]
		_, b := poolRemoteEnv[k]
		return a || b || slices.Contains(repoUnset, k) || slices.Contains(poolRemoteUnset, k)
	}
	apply := func(env map[string]config.Template, skipReplaced, onlyUnset bool) error {
		keys := make([]string, 0, len(env))
		for k := range env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if _, has := m[k]; onlyUnset && has {
				continue
			}
			v, err := expandRemote(env[k], vars, wt)
			if err != nil && skipReplaced && replaced(k) {
				continue
			}
			if err != nil {
				return fmt.Errorf("%s: %w", k, err)
			}
			set(k, v)
		}
		return nil
	}
	unset(pool.DefaultsEnvUnset)
	if err := apply(pool.EnvDefault, true, true); err != nil {
		return nil, err
	}
	if err := apply(pool.Env, true, false); err != nil {
		return nil, err
	}
	unset(pool.EnvUnset)
	if err := apply(repoEnv, false, false); err != nil {
		return nil, err
	}
	unset(repoUnset)
	if err := apply(poolRemoteEnv, false, false); err != nil {
		return nil, err
	}
	unset(poolRemoteUnset)
	var out []string
	for _, k := range order {
		if v, ok := m[k]; ok {
			out = append(out, k+"="+v)
		}
	}
	return out, nil
}

// expandRemote also expands the remote-only {week} and {sha256:<path>} (16 hex of a
// work-tree file) on the host at launch (spec 5.7).
func expandRemote(t config.Template, vars config.Vars, wt string) (string, error) {
	parts, err := config.ParseTemplate(t.Raw)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, p := range parts {
		switch p.Var {
		case "":
			b.WriteString(p.Lit)
		case "week":
			b.WriteString(vars["week"])
		case "sha256":
			data, err := os.ReadFile(filepath.Join(wt, filepath.Clean("/"+p.Arg)))
			if err != nil {
				return "", err
			}
			sum := sha256.Sum256(data)
			b.WriteString(hex.EncodeToString(sum[:])[:16])
		default:
			v, err := config.Template{Raw: "{" + p.Var + p.Op + p.Arg + "}"}.Expand(vars)
			if err != nil {
				return "", err
			}
			b.WriteString(v)
		}
	}
	return b.String(), nil
}
