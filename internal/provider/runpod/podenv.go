package runpod

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// pod_env (spec 8.7): host-set variables merged into the pod env at create. One table fixes what
// a name may not be; each message names the key, never the value.
const (
	podEnvMaxEntries = 16
	podEnvMaxValue   = 256 // bytes
)

var podEnvNameRE = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)

// podEnvRefused is the refused-name table: the exact names, the prefixes and the suffixes.
var podEnvRefused = struct{ Exact, Prefixes, Suffixes []string }{
	Exact: []string{"PATH", "HOME", "SHELL", "USER", "PUBLIC_KEY", "JUPYTER_PASSWORD", "WGETRC"},
	Prefixes: []string{
		"BITC_",   // the agent's boot keys and the BITC_OWNER tag
		"RUNPOD_", // the pod id, the scoped key and the account secret references
		"LD_", "SSH_", "SSL_", "GIT_", "GO", "CGO_", "CURL_",
	},
	Suffixes: []string{"_PROXY"},
}

// decodePodEnv checks the pod_env table; the result is a copy.
func decodePodEnv(v any) (map[string]string, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("pod_env must be a table of NAME = \"value\"")
	}
	if len(m) > podEnvMaxEntries {
		return nil, fmt.Errorf("pod_env has %d entries (at most %d)", len(m), podEnvMaxEntries)
	}
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sort.Strings(names) // the same first error every run
	out := make(map[string]string, len(m))
	for _, name := range names {
		if !podEnvNameRE.MatchString(name) {
			return nil, fmt.Errorf("pod_env key %s must match ^[A-Z][A-Z0-9_]{0,63}$", strconv.Quote(name))
		}
		if why := podEnvRefusedName(name); why != "" {
			return nil, fmt.Errorf("pod_env.%s: %s", name, why)
		}
		val, ok := m[name].(string)
		switch {
		case !ok:
			return nil, fmt.Errorf("pod_env.%s must be a string", name)
		case len(val) > podEnvMaxValue:
			return nil, fmt.Errorf("pod_env.%s: the value is over %d bytes", name, podEnvMaxValue)
		case strings.ContainsAny(val, "\r\n"):
			return nil, fmt.Errorf("pod_env.%s: the value has a line break", name)
		case strings.Contains(val, "{{"):
			return nil, fmt.Errorf("pod_env.%s: the value has {{ (a reference to a RunPod secret is not allowed)", name)
		}
		out[name] = val
	}
	return out, nil
}

// podEnvRefusedName is why name may not be set, or "".
func podEnvRefusedName(name string) string {
	for _, x := range podEnvRefused.Exact {
		if name == x {
			return "the name is reserved"
		}
	}
	for _, p := range podEnvRefused.Prefixes {
		if strings.HasPrefix(name, p) {
			return "names starting " + p + " are reserved"
		}
	}
	for _, s := range podEnvRefused.Suffixes {
		if strings.HasSuffix(name, s) {
			return "names ending " + s + " are reserved"
		}
	}
	return ""
}
