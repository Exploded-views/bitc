package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"

	"golang.org/x/sys/unix"

	"github.com/Exploded-views/bitc/internal/provider/plugin/pin"
)

var (
	pluginTypeRE   = regexp.MustCompile(`^[a-z][a-z0-9]{0,15}$`)
	pluginSecretRE = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
	pluginShaRE    = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// builtInTypes are the provider types that are never plugins (providers delta A.1.1).
var builtInTypes = []string{"local", "runpod", "sprites", "ssh", "fake"}

// IsBuiltInType reports whether typ names a built-in provider (registered or reserved).
func IsBuiltInType(typ string) bool {
	_, reg := providerTypes[typ]
	return reg || slices.Contains(builtInTypes, typ)
}

// SecretLikeName reports whether a variable or key name looks like a secret (spec 8.4).
func SecretLikeName(name string) bool { return secretLike(name) }

// pluginSectionType is the registration of every plugin type: it talks to nothing the host can
// name before configure (A.4.5), is trusted to attach only on the host's say-so (decision 2), and
// may declare either boundary (the real declaration comes with hello).
var pluginSectionType = ProviderType{
	Endpoints:       func(map[string]any) ([]string, error) { return nil, nil },
	FallbackCap:     func(map[string]any) string { return "attach" },
	DefaultResource: true,
}

// decodePluginPin decodes the plugin table of a provider section (A.2) and the core sizes.
func decodePluginPin(p *problems, file, typ string, pr *Provider, plugin, sizes any, env map[string]string) {
	key := "providers." + typ
	m := table(p, file, key+".plugin", plugin, "path", "sha256", "env_pass", "secret_files")
	pin_ := &PluginPin{SecretFiles: map[string]string{}}
	pr.Plugin = pin_
	if m != nil {
		pin_.Path, _ = m["path"].(string)
		pin_.SHA256, _ = m["sha256"].(string)
		decodePluginPath(p, file, typ, pin_, m["path"])
		decodePluginEnvPass(p, file, key, pin_, m["env_pass"])
		decodePluginSecrets(p, file, key, pin_, m["secret_files"], env)
	}
	decodePluginSizes(p, file, key, pr, sizes)
	for _, k := range sortedRawKeys(pr.Raw) {
		if secretLike(k) {
			p.addf("%s: %s.%s: secret; set it in plugin.secret_files", file, key, k)
		}
	}
}

func sortedRawKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func decodePluginPath(p *problems, file, typ string, pl *PluginPin, rawPath any) {
	key := "providers." + typ + ".plugin"
	if _, ok := rawPath.(string); !ok || !filepath.IsAbs(pl.Path) || filepath.Base(pl.Path) != "bitc-provider-"+typ {
		p.addf("%s: %s.path: must be an absolute path ending in bitc-provider-%s: %s", file, key, typ, quoteVal(rawPath))
		return
	}
	shaOK := pluginShaRE.MatchString(pl.SHA256)
	if !shaOK {
		p.addf("%s: %s.sha256: invalid: %s", file, key, quoteVal(pl.SHA256))
	}
	if err := pin.Check(pl.Path); err != nil {
		var pe *pin.Error
		switch {
		case errors.As(err, &pe) && pe.Kind == pin.KindInsecure:
			p.addf("%s: %s.path: insecure: %s", file, key, pe.Detail)
		case errors.As(err, &pe) && pe.Kind == pin.KindPrivs:
			p.addf("%s: %s.path: insecure: setuid/setgid/capabilities: %q", file, key, pl.Path)
		case errors.As(err, &pe):
			p.addf("%s: %s.path: %s: %q", file, key, pe.Detail, pl.Path)
		default:
			p.addf("%s: %s.path: %v: %q", file, key, err, pl.Path)
		}
		return
	}
	if !shaOK {
		return
	}
	if got, err := pin.SumFile(pl.Path); err != nil {
		p.addf("%s: %s.path: %v: %q", file, key, err, pl.Path)
	} else if got != pl.SHA256 {
		p.addf("%s: %s: sha256 mismatch (want %s, file %s)", file, key, pin.Short(pl.SHA256), pin.Short(got))
	}
}

func decodePluginEnvPass(p *problems, file, key string, pl *PluginPin, v any) {
	if v == nil {
		return
	}
	list, ok := v.([]any)
	if !ok {
		p.addf("%s: %s.plugin.env_pass: must be a list of variable names", file, key)
		return
	}
	for _, x := range list {
		s, _ := x.(string)
		if !ValidEnvName(s) || secretLike(s) {
			p.addf("%s: %s.plugin.env_pass: %s not allowed", file, key, quoteVal(x))
			continue
		}
		pl.EnvPass = append(pl.EnvPass, s)
	}
}

func decodePluginSecrets(p *problems, file, key string, pl *PluginPin, v any, env map[string]string) {
	if v == nil {
		return
	}
	m, ok := v.(map[string]any)
	if !ok {
		p.addf("%s: %s.plugin.secret_files: must be a table of name = file", file, key)
		return
	}
	for _, n := range sortedRawKeys(m) {
		k := key + ".plugin.secret_files." + n
		path, ok := m[n].(string)
		switch {
		case n == "api_key" || !pluginSecretRE.MatchString(n):
			p.addf("%s: %s: name must match ^[a-z][a-z0-9_]{0,31}$ and not be api_key", file, k)
		case !ok || path == "":
			p.addf("%s: %s: must be a file path", file, k)
		default:
			path = expandHome(path, env)
			if why := secretFileProblem(path); why != "" {
				p.addf("%s: %s: %s", file, k, why)
				continue
			}
			pl.SecretFiles[n] = path
		}
	}
}

// secretFileProblem: the credential-file rules (spec 10): owned by the user, mode 0600. A missing
// file is not a load error (the plugin is then unusable at spawn), as for api_key_file.
func secretFileProblem(path string) string {
	var st unix.Stat_t
	if err := unix.Stat(path, &st); errors.Is(err, fs.ErrNotExist) {
		return ""
	} else if err != nil {
		return fmt.Sprintf("%s: %v", path, err)
	}
	if st.Uid != uint32(os.Getuid()) || st.Mode&0o077 != 0 {
		return fmt.Sprintf("%s must be owned by you and mode 600 (it is %o)", path, st.Mode&0o777)
	}
	return ""
}

func decodePluginSizes(p *problems, file, key string, pr *Provider, v any) {
	msg := func() { p.addf("%s: %s.sizes: a usd_per_hour per size is required for a plugin", file, key) }
	m, ok := v.(map[string]any)
	if !ok || len(m) == 0 {
		msg()
		return
	}
	pr.Sizes, pr.SizesRaw = map[string]Micros{}, m
	bad := false
	for _, k := range sortedRawKeys(m) {
		sz, _ := m[k].(map[string]any)
		price, err := ParseMoney(sz["usd_per_hour"])
		if !sizeKeyRE.MatchString(k) || sz == nil || err != nil || price <= 0 {
			bad = true
			continue
		}
		pr.Sizes[k] = price
	}
	if bad {
		msg()
	}
}
