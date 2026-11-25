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
	Exact: []string{"PATH", "HOME", "SHELL", "USER", "PUBLIC_KEY", "JUPYTER_PASSWORD", "WGETRC",
		// read by the start stub's shell, git, curl, OpenSSL and the C library before the agent clears the env
		"XDG_CONFIG_HOME", "IFS", "SYSTEM_WGETRC", "OPENSSL_CONF", "OPENSSL_MODULES", "SSLKEYLOGFILE",
		"GLIBC_TUNABLES", "HOSTALIASES", "RES_OPTIONS", "TMPDIR", "ENV", "BASH_ENV", "PS4",
		"LOCALDOMAIN"}, // the resolver's search list
	Prefixes: []string{
		"BITC_",   // the agent's boot keys and the BITC_OWNER tag
		"RUNPOD_", // the pod id, the scoped key and the account secret references
		"LD_", "SSH_", "SSL_", "GIT_", "GO", "CGO_", "CURL_",
		"MALLOC_", // glibc malloc tuning
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
			return nil, fmt.Errorf("pod_env key %s must match ^[A-Z][A-Z0-9_]{0,63}$", podEnvKeyEcho(name))
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
		case hasControlByte(val):
			return nil, fmt.Errorf("pod_env.%s: the value has a control character", name)
		case strings.Contains(val, "{{"):
			return nil, fmt.Errorf("pod_env.%s: the value has {{ (a reference to a RunPod secret is not allowed)", name)
		}
		out[name] = val
	}
	return out, nil
}

// podEnvKeyEcho quotes an invalid key for an error: cut at the first byte outside [A-Za-z0-9_] (a
// pasted NAME=value, "NAME: value" or "NAME value" never echoes the value) and at 32 bytes, with
// "..." after the quote when anything was cut.
func podEnvKeyEcho(name string) string {
	const maxEcho = 32
	cut := false
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c == '_' || c >= '0' && c <= '9' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' {
			continue
		}
		name, cut = name[:i], true
		break
	}
	if len(name) > maxEcho {
		name, cut = name[:maxEcho], true
	}
	q := strconv.Quote(name)
	if cut {
		q += "..."
	}
	return q
}

// hasControlByte reports a C0 control character (0x00-0x1f) or DEL (0x7f) in s.
func hasControlByte(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] == 0x7f {
			return true
		}
	}
	return false
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
