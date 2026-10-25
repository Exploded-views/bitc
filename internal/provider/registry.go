package provider

import (
	"context"
	"io"

	"github.com/Exploded-views/bitc/internal/config"
	"github.com/Exploded-views/bitc/internal/redact"
)

// SelfDeleteEnv is what the agent's self-delete hook gets (from PID 1's env at boot).
type SelfDeleteEnv struct {
	ResourceID string
	ScopedKey  string // the pod-scoped key the provider injects; never logged
	APIBase    string
}

// Factory registers a provider type (spec 8.5).
type Factory struct {
	Endpoints  func(raw map[string]any) ([]string, error)
	New        func(core *config.Provider, eff *config.Effective) (Provider, error)
	SelfDelete func(ctx context.Context, env SelfDeleteEnv) error
	TestOnly   bool // accepted only with a [test] table (the fake)
	// FallbackCap is the latest boundary the type declares for these provider-specific keys
	// (Caps.FallbackUntil); nil means UntilResource. A built-in's host default is this value.
	FallbackCap func(raw map[string]any) Boundary
	// DefaultResource: the host default is UntilResource whatever FallbackCap says (a plugin).
	DefaultResource bool
	// SendEnv is the type's default caller-env level for these provider-specific keys; nil means
	// EnvForwarded (a plugin). MultiJob reports whether its resources serve more than one job.
	SendEnv  func(raw map[string]any) EnvLevel
	MultiJob func(raw map[string]any) bool
	// Validate checks the provider-specific keys against the decoded core section (its name
	// prefix and credential too) and returns the host-file problems, each as the text after
	// `providers.<type>.`; nil means none.
	Validate func(core *config.Provider) []string
	// Fixed: a fixed-capacity type (providers delta C.3); MaxLiveDefault is its max_live default.
	Fixed          bool
	MaxLiveDefault func(raw map[string]any) int
}

var factories = map[string]Factory{}

// Register adds a provider type; called from provider packages' init.
func Register(typ string, f Factory) {
	factories[typ] = f
	pt := config.ProviderType{Endpoints: f.Endpoints, TestOnly: f.TestOnly, DefaultResource: f.DefaultResource}
	if f.FallbackCap != nil {
		pt.FallbackCap = func(raw map[string]any) string { return f.FallbackCap(raw).String() }
	}
	if f.SendEnv != nil {
		pt.SendEnv = func(raw map[string]any) string { return f.SendEnv(raw).String() }
	}
	pt.MultiJob = f.MultiJob
	pt.Validate = f.Validate
	pt.Fixed, pt.MaxLiveDefault = f.Fixed, f.MaxLiveDefault
	config.RegisterProviderType(typ, pt)
}

func Get(typ string) (Factory, bool) { f, ok := factories[typ]; return f, ok }

var pluginFactory *Factory

// RegisterPlugin installs the factory every plugin section uses (called from the plugin
// package's init); a plugin type is a config section with a plugin table, not a registered type.
func RegisterPlugin(f Factory) { pluginFactory = &f }

// Lookup finds the factory for a configured provider section: the plugin factory for a section
// with a plugin table, else the built-in registered under typ.
func Lookup(typ string, core *config.Provider) (Factory, bool) {
	if core.Plugin != nil {
		if pluginFactory == nil {
			return Factory{}, false
		}
		return *pluginFactory, true
	}
	return Get(typ)
}

// Starter is implemented by a provider that must be started before use (a plugin process:
// hello, configure). Walk, doctor and sweep call Start once, right after the constructor.
type Starter interface {
	Start(ctx context.Context, rt Runtime) error
}

// Runtime is what a Starter needs from the host: the redactor every secret joins before it is
// written to the child, the stderr for debug lines, and whether BITC_LOG=debug is set.
type Runtime struct {
	Red    *redact.Set
	Stderr io.Writer
	Debug  bool
}
