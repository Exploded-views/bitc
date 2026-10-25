package config

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// ProviderType is what core needs from a provider before M2: its endpoints, for the [test]
// gate. Provider packages register their types from init.
type ProviderType struct {
	Endpoints func(raw map[string]any) ([]string, error)
	TestOnly  bool // accepted only with a [test] table (the fake provider)
	// FallbackCap is the latest fallback boundary the type declares ("resource" or "attach", spec
	// 8.2) for these provider-specific keys; nil means "resource".
	FallbackCap func(raw map[string]any) string
	// DefaultResource: fallback_until defaults to "resource" whatever FallbackCap says (a plugin
	// is trusted to attach only after the host opts in, spec 8.2).
	DefaultResource bool
	// SendEnv is the type's default caller-env level for these provider-specific keys ("none",
	// "forwarded" or "forced"); nil means "forwarded" (a plugin). MultiJob reports whether its
	// resources serve more than one job (providers delta S.4); nil means false.
	SendEnv  func(raw map[string]any) string
	MultiJob func(raw map[string]any) bool
	// Validate checks the provider-specific keys once the core keys are decoded; each message is
	// the text after `providers.<type>.` (a 78 at load).
	Validate func(core *Provider) []string
	// Fixed: a fixed-capacity type (ssh, providers delta C.3). It has no API key, daily_usd is
	// the type's own rule (Validate), and max_live defaults to MaxLiveDefault of its keys.
	Fixed          bool
	MaxLiveDefault func(raw map[string]any) int
}

// IsFixedType reports whether typ is a registered fixed-capacity provider type.
func IsFixedType(typ string) bool { return providerTypes[typ].Fixed }

var providerTypes = map[string]ProviderType{}

// RegisterProviderType adds or replaces a provider type (called from provider packages' init).
func RegisterProviderType(name string, t ProviderType) { providerTypes[name] = t }

func decodeProviders(p *problems, e *Effective, file string, sections map[string]map[string]any, env map[string]string) {
	for typ, raw := range sections {
		key := "providers." + typ
		pt, ok := providerTypes[typ]
		pluginRaw, isPlugin := raw["plugin"]
		if isPlugin {
			switch {
			case IsBuiltInType(typ):
				p.addf("%s: %s.plugin: %q is a built-in provider", file, key, typ)
				continue
			case !pluginTypeRE.MatchString(typ):
				p.addf("%s: %s: invalid provider type", file, key)
				continue
			}
			pt, ok = pluginSectionType, true
		}
		if !ok {
			p.addf("%s: %s: unknown provider type %q", file, key, typ)
			continue
		}
		if pt.TestOnly && e.Test == nil {
			p.addf("%s: %s: provider type %q is for tests only (needs a [test] table)", file, key, typ)
			continue
		}
		pr := &Provider{Type: typ, NamePrefix: "bitc", MaxLive: 2, LaunchWait: 5 * time.Minute, StartTimeout: 20 * time.Minute,
			Breaker:   Breaker{Failures: 3, Window: 60 * time.Minute},
			Transport: Transport{Remote: "origin", RefPrefix: "refs/bitc/jobs/", KnownHosts: BuiltinKnownHosts, Kind: TransportGitPush, MaxBundle: DefaultMaxBundle},
			Agent:     Agent{Source: "release"},
			Artifacts: ArtifactLimits{MaxCompressed: 200 << 20, MaxUnpacked: 2 << 30, MaxEntries: 100000},
			Raw:       map[string]any{}}
		pr.Fixed = pt.Fixed
		var fallback any
		hasFallback := false
		var sendEnv any
		hasSendEnv := false
		for k, v := range raw {
			switch k {
			case "api_key":
				pr.APIKey = str(p, file, key+"."+k, v)
			case "api_key_file":
				pr.APIKeyFile = expandHome(str(p, file, key+"."+k, v), env)
				pr.APIKey = readKeyFile(p, file, key+".api_key_file", pr.APIKeyFile)
			case "name_prefix":
				pr.NamePrefix = str(p, file, key+"."+k, v)
				if !sizeKeyRE.MatchString(pr.NamePrefix) {
					p.addf("%s: %s.name_prefix = %q must match [a-z0-9]{1,10}", file, key, pr.NamePrefix)
				}
			case "daily_usd":
				m, err := ParseMoney(v)
				if err != nil {
					p.addf("%s: %s.daily_usd = %v %v", file, key, v, err)
				}
				pr.DailyUSD = m
			case "max_live":
				n, ok := v.(int64)
				if !ok || n < 1 {
					p.addf("%s: %s.max_live must be an integer >= 1", file, key)
				}
				pr.MaxLive = int(n)
			case "launch_wait", "start_timeout":
				s := str(p, file, key+"."+k, v)
				d := dur(p, file, key+"."+k, &s, 0)
				if k == "launch_wait" {
					pr.LaunchWait = d
				} else {
					pr.StartTimeout = d
				}
			case "breaker":
				m := table(p, file, key+".breaker", v, "failures", "window")
				if n, ok := m["failures"].(int64); ok && n >= 1 {
					pr.Breaker.Failures = int(n)
				} else if m["failures"] != nil {
					p.addf("%s: %s.breaker.failures must be an integer >= 1", file, key)
				}
				if s, ok := m["window"].(string); ok {
					pr.Breaker.Window = dur(p, file, key+".breaker.window", &s, pr.Breaker.Window)
				}
			case "transport":
				m := table(p, file, key+".transport", v, "remote", "ref_prefix", "known_hosts", "kind", "max_bundle")
				if s, ok := m["remote"].(string); ok {
					pr.Transport.Remote = s
				}
				if s, ok := m["ref_prefix"].(string); ok {
					if !strings.HasPrefix(s, "refs/") || !strings.HasSuffix(s, "/") {
						p.addf("%s: %s.transport.ref_prefix = %q must start refs/ and end /", file, key, s)
					}
					pr.Transport.RefPrefix = s
				}
				if s, ok := m["known_hosts"].(string); ok {
					pr.Transport.KnownHosts = s
				}
				decodeUpload(p, file, key, typ, pr, m)
			case "agent":
				m := table(p, file, key+".agent", v, "source", "url", "path", "paths", "commit", "image", "sha256", "module_dir")
				if s, ok := m["source"].(string); ok {
					pr.Agent.Source = enum(p, file, key+".agent.source", &s, "release", "release", "url", "build")
				}
				pr.Agent.URL, _ = m["url"].(string)
				pr.Agent.Path, _ = m["path"].(string)
				pr.Agent.Commit, _ = m["commit"].(string)
				pr.Agent.Image, _ = m["image"].(string)
				pr.Agent.SHA256, _ = m["sha256"].(string)
				pr.Agent.ModuleDir, _ = m["module_dir"].(string)
				agentPaths(p, file, key, &pr.Agent, m["paths"])
				if pr.Agent.Source == "url" && (!strings.HasPrefix(pr.Agent.URL, "https://") || len(pr.Agent.Paths) == 0) {
					p.addf("%s: %s.agent with source = \"url\" needs an https url and a path", file, key)
				}
				if pr.Agent.ModuleDir != "" {
					agentModuleDir(p, file, key, &pr.Agent)
				} else if pr.Agent.Source == "build" && !pt.Fixed && (!agentCommitRE.MatchString(pr.Agent.Commit) || !agentImageRE.MatchString(pr.Agent.Image) ||
					!agentSumRE.MatchString(pr.Agent.SHA256) || pr.Agent.URL != "" || pr.Agent.Path != "") {
					p.addf("%s: %s.agent with source = \"build\" needs commit (40 lowercase hex), image (<name>@sha256:<64 hex>) and sha256 (64 lowercase hex), and no url or path", file, key)
				}
			case "fallback_until":
				fallback, hasFallback = v, true
			case "send_env":
				sendEnv, hasSendEnv = v, true
			case "plugin", "sizes":
				if !isPlugin {
					pr.Raw[k] = v
				}
			case "artifacts":
				m := table(p, file, key+".artifacts", v, "max_compressed", "max_unpacked", "max_entries")
				if s, ok := m["max_compressed"].(string); ok {
					pr.Artifacts.MaxCompressed = size(p, file, key+".artifacts.max_compressed", &s, pr.Artifacts.MaxCompressed)
				}
				if s, ok := m["max_unpacked"].(string); ok {
					pr.Artifacts.MaxUnpacked = size(p, file, key+".artifacts.max_unpacked", &s, pr.Artifacts.MaxUnpacked)
				}
				if n, ok := m["max_entries"].(int64); ok {
					pr.Artifacts.MaxEntries = int(n)
				}
			default:
				pr.Raw[k] = v
			}
		}
		if isPlugin {
			decodePluginPin(p, file, typ, pr, pluginRaw, raw["sizes"], env)
		}
		decodeFallback(p, pr, pt, file, key, fallback, hasFallback)
		decodeSendEnv(p, pr, pt, file, key, sendEnv, hasSendEnv)
		if _, ok := raw["daily_usd"]; !ok && !pt.Fixed {
			p.addf("%s: %s.daily_usd is required", file, key)
		}
		if _, ok := raw["max_live"]; !ok && pt.MaxLiveDefault != nil {
			pr.MaxLive = pt.MaxLiveDefault(pr.Raw)
		}
		if pr.APIKey != "" {
			e.Secrets = append(e.Secrets, pr.APIKey)
		}
		if pt.Validate != nil {
			for _, m := range pt.Validate(pr) {
				p.addf("%s: %s.%s", file, key, m)
			}
		}
		eps, err := pt.Endpoints(pr.Raw)
		if err != nil {
			p.addf("%s: %s: %v", file, key, err)
		}
		pr.Endpoints = eps
		e.Providers[typ] = pr
	}
}

func str(p *problems, file, key string, v any) string {
	s, ok := v.(string)
	if !ok {
		p.addf("%s: %s must be a string", file, key)
	}
	return s
}

func table(p *problems, file, key string, v any, allowed ...string) map[string]any {
	m, ok := v.(map[string]any)
	if !ok {
		p.addf("%s: %s must be a table", file, key)
		return nil
	}
	for k := range m {
		found := false
		for _, a := range allowed {
			found = found || a == k
		}
		if !found {
			p.addf("%s: unknown key %s.%s", file, key, k)
		}
	}
	return m
}

// readKeyFile: the key alone on one line, 0600, owned by the user (spec 10). Never prints the
// key. A missing file is not a load error: the provider is then unusable (preflight falls back
// with "no credentials", doctor --remote fails), so example configs pass `config check`.
func readKeyFile(p *problems, file, key, path string) string {
	var st unix.Stat_t
	if err := unix.Stat(path, &st); errors.Is(err, fs.ErrNotExist) {
		return ""
	} else if err != nil {
		p.addf("%s: %s %s: %v", file, key, path, err)
		return ""
	}
	if st.Uid != uint32(os.Getuid()) || st.Mode&0o077 != 0 {
		p.addf("%s: %s %s must be owned by you and mode 600 (it is %o)", file, key, path, st.Mode&0o777)
		return ""
	}
	f, err := os.Open(path)
	if err != nil {
		p.addf("%s: %s %s: %v", file, key, path, err)
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	var lines []string
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	if len(lines) != 1 || lines[0] == "" || strings.ContainsAny(lines[0], " \t=\"'") {
		p.addf("%s: %s %s must hold the bare key alone on one line", file, key, path)
		return ""
	}
	return lines[0]
}

// checkTestGate: [test] only when every provider endpoint is a loopback literal (spec 11).
func checkTestGate(p *problems, e *Effective) {
	if e.Test == nil {
		return
	}
	for typ, pr := range e.Providers {
		for _, ep := range pr.Endpoints {
			if !loopbackLiteral(ep) {
				p.addf("%s: [test] is accepted only when every provider endpoint is a loopback literal; providers.%s talks to %s",
					e.HostFile, typ, ep)
			}
		}
	}
}

// LoopbackLiteral reports whether an endpoint (a host, host:port or URL) is a loopback IP literal.
func LoopbackLiteral(ep string) bool { return loopbackLiteral(ep) }

func loopbackLiteral(ep string) bool {
	host := ep
	if u, err := url.Parse(ep); err == nil && u.Host != "" {
		host = u.Host
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

// The agent = { source = "build" } pins (spec 5.x): all lowercase hex, the image by digest.
var (
	agentCommitRE = regexp.MustCompile(`^[0-9a-f]{40}$`)
	agentImageRE  = regexp.MustCompile(`^[^\s@]+@sha256:[0-9a-f]{64}$`)
	agentSumRE    = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// agentPaths decodes agent.paths (architecture -> local file) and folds the legacy agent.path
// into paths.amd64 (providers delta C.4 item 2). A url with {arch} may serve any listed
// architecture; without it only amd64 is served.
func agentPaths(p *problems, file, key string, a *Agent, v any) {
	if v == nil && a.Path == "" {
		return
	}
	a.Paths = map[string]string{}
	if v != nil {
		m := table(p, file, key+".agent.paths", v, Architectures...)
		for arch, x := range m {
			if !validArch(arch) {
				p.addf("%s: %s.agent.paths: invalid arch %q", file, key, arch)
				continue
			}
			if s, ok := x.(string); ok && s != "" {
				a.Paths[arch] = s
			} else {
				p.addf("%s: %s.agent.paths.%s must be a non-empty path", file, key, arch)
			}
		}
	}
	if a.Path != "" {
		if cur, ok := a.Paths["amd64"]; ok && cur != a.Path {
			p.addf("%s: %s.agent.path and agent.paths.amd64 differ", file, key)
		}
		a.Paths["amd64"] = a.Path
	}
	if a.Source == "url" && !strings.Contains(a.URL, "{arch}") {
		for arch := range a.Paths {
			if arch != "amd64" {
				p.addf("%s: %s.agent.url: {arch} is required to serve paths.%s", file, key, arch)
			}
		}
	}
}

// agentModuleDir checks agent.module_dir: only with source = "build", alone (no commit, image,
// sha256, url or path), an absolute path (~/ allowed) holding this module's go.mod.
func agentModuleDir(p *problems, file, key string, a *Agent) {
	bad := func(why string) { p.addf("%s: %s.agent.module_dir: %s", file, key, why) }
	if a.Source != "build" {
		bad(`only source = "build" takes a module_dir`)
		return
	}
	if a.Commit != "" || a.Image != "" || a.SHA256 != "" || a.URL != "" || a.Path != "" || len(a.Paths) > 0 {
		bad("cannot be combined with commit, image, sha256, url or path")
		return
	}
	dir := a.ModuleDir
	if rest, ok := strings.CutPrefix(dir, "~/"); ok {
		if home, err := os.UserHomeDir(); err == nil {
			dir = home + "/" + rest
		}
	}
	if !strings.HasPrefix(dir, "/") {
		bad("not an absolute path: " + a.ModuleDir)
		return
	}
	b, err := os.ReadFile(dir + "/go.mod")
	if err != nil {
		bad("no go.mod in " + a.ModuleDir)
		return
	}
	if first, _, _ := strings.Cut(string(b), "\n"); strings.TrimSpace(first) != "module "+ModulePath {
		bad("go.mod does not declare module " + ModulePath)
		return
	}
	a.ModuleDir = dir
}

// ModulePath is this module's path, the one a module_dir tree must declare.
const ModulePath = "github.com/Exploded-views/bitc"

// decodeFallback sets the effective fallback boundary (spec 8.2): the host key fallback_until,
// default the type's declaration (a plugin: "resource"), never later than the declaration.
func decodeFallback(p *problems, pr *Provider, pt ProviderType, file, key string, v any, set bool) {
	pr.FallbackCap = "resource"
	if pt.FallbackCap != nil {
		pr.FallbackCap = pt.FallbackCap(pr.Raw)
	}
	pr.FallbackUntil = pr.FallbackCap
	if pt.DefaultResource {
		pr.FallbackUntil = "resource"
	}
	if !set {
		return
	}
	s, _ := v.(string)
	if s != "resource" && s != "attach" {
		p.addf("%s: %s.fallback_until: must be \"resource\" or \"attach\": %s", file, key, quoteVal(v))
		return
	}
	if s == "attach" && pr.FallbackCap == "resource" {
		p.addf("%s: %s.fallback_until: \"attach\" is wider than %s allows (\"resource\")", file, key, pr.Type)
		return
	}
	pr.FallbackUntil = s
}

// decodeSendEnv sets the effective caller-env level (providers delta S.2): the host key send_env,
// default the type's declaration ("forwarded" for a plugin). A multi-job type (S.4) defaults to at
// most "forwarded" and refuses "forced".
func decodeSendEnv(p *problems, pr *Provider, pt ProviderType, file, key string, v any, set bool) {
	def := "forwarded"
	if pt.SendEnv != nil {
		def = pt.SendEnv(pr.Raw)
	}
	pr.MultiJob = pt.MultiJob != nil && pt.MultiJob(pr.Raw)
	if pr.MultiJob && def == "forced" {
		def = "forwarded"
	}
	pr.SendEnv, pr.SendEnvSource = def, "default"
	if !set {
		return
	}
	s, _ := v.(string)
	if s != "none" && s != "forwarded" && s != "forced" {
		p.addf("%s: %s.send_env: must be \"none\", \"forwarded\" or \"forced\": %s", file, key, quoteVal(v))
		return
	}
	if s == "forced" && pr.MultiJob {
		p.addf("%s: %s.send_env: \"forced\" is not allowed: %s reuses resources across jobs", file, key, pr.Type)
		return
	}
	pr.SendEnv, pr.SendEnvSource = s, "host"
}

func quoteVal(v any) string {
	if s, ok := v.(string); ok {
		return "\"" + s + "\""
	}
	return fmt.Sprintf("%v", v)
}

// decodeUpload reads transport.kind and transport.max_bundle (the upload transport, ssh only).
func decodeUpload(p *problems, file, key, typ string, pr *Provider, m map[string]any) {
	if v, ok := m["kind"]; ok {
		s, isStr := v.(string)
		switch {
		case !isStr || (s != TransportGitPush && s != TransportUpload):
			p.addf("%s: %s.transport.kind = %s must be %q or %q", file, key, quoteAny(v), TransportGitPush, TransportUpload)
		case s == TransportUpload && typ != "ssh":
			p.addf("%s: %s.transport.kind = %q is valid on ssh only", file, key, s)
		default:
			pr.Transport.Kind = s
		}
	}
	if v, ok := m["max_bundle"]; ok {
		s, isStr := v.(string)
		if !isStr {
			p.addf("%s: %s.transport.max_bundle must be a string", file, key)
			return
		}
		n, err := ParseSize(s)
		switch {
		case err != nil:
			p.addf("%s: %s.transport.max_bundle = %q %v", file, key, s, err)
		case n <= 0 || n > MaxBundleLimit:
			p.addf("%s: %s.transport.max_bundle = %q must be more than 0 and at most %s", file, key, s, MaxBundleLimit)
		default:
			pr.Transport.MaxBundle = n
		}
	}
}

func quoteAny(v any) string {
	if s, ok := v.(string); ok {
		return strconv.Quote(s)
	}
	return fmt.Sprint(v)
}
