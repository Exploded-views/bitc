// Package config loads and merges bitc's TOML files (spec 5). model.go is the effective
// configuration every other package reads.
package config

import "time"

// Flags are bitc's global flags (spec 3).
type Flags struct {
	ConfigFile string // --config
	StateDir   string // --state-dir
	Verbose    int    // -v (repeatable)
	JSON       bool   // --json
}

// LoadOptions are Load's inputs.
type LoadOptions struct {
	Flags Flags
	Env   []string // the caller's environment, KEY=VALUE
	Cwd   string   // os.Getwd() of the caller

	RepoFile string // `config check --repo FILE`: use FILE as the repo file
}

// RunFlags are the narrowing flags of `bitc run` (spec 3.1, 5.3).
type RunFlags struct {
	Wait      string    // --wait raw value, "" if absent
	Local     bool      // --local
	Provider  string    // --provider P
	Artifacts *[]string // --artifacts a,b (nil if absent)
}

// Size is a byte count ("<int><KiB|MiB|GiB|TiB>").
type Size int64

// Micros is US dollars in micro-dollars.
type Micros int64

// Template is a string with {name} substitutions (spec 5.7).
type Template struct{ Raw string }

// Vars are the values templates may use; integer vars are decimal strings.
type Vars map[string]string

// Effective is the merged, validated configuration for one invocation.
type Effective struct {
	HostFile   string // "" if no host file exists
	RepoFile   string // "" if no repo file applies
	Worktree   string // canonical work-tree top (5.1)
	WT8        string // 8 hex of sha256(Worktree)
	Cwd        string // canonical cwd
	LogicalCwd string // $PWD when it names Cwd, else Cwd

	Host         Host
	Pools        map[string]*Pool // canonical name -> pool
	Groups       map[string]Group
	Priority     Priority
	Detach       Detach
	RemoteLimits RemoteLimits
	Providers    map[string]*Provider // provider type -> section
	Repos        []RepoGrant
	Grant        *RepoGrant  // first [[repos]] entry matching Worktree, or nil
	Remote       *RepoRemote // the repo's [remote] table, or nil
	Vars         Vars        // merged [vars], host wins
	Compat       *Compat
	Test         *Test
	EnvProvider  string            // BITC_PROVIDER ("" or "local")
	EnvArtifacts *[]string         // BITC_ARTIFACTS (nil if unset)
	GlobalFlags  Flags             // the global flags Load was given
	LogLevel     string            // BITC_LOG: "info" (default), "debug" or "quiet" (1.2b sets it; -v raises info to debug, 1.7)
	Sources      map[string]string // effective key -> "default" | file path | "env BITC_X" | "flag --x"
	Secrets      []string          // credential values loaded, for redaction

	DefaultPool *Pool // template for ad-hoc pools when Host.UnknownPools == "default"

	Warnings []string // non-fatal config notes, one line each; `config check` prints them to stderr
	run      runState
}

type Host struct {
	StateDir          string
	Poll              time.Duration
	MemoryFloor       Size
	ReservationWindow time.Duration
	UnknownPools      string // "refuse" | "default"
	Grace             time.Duration
	MaxSignalWait     time.Duration
	HistoryMax        Size
	Retention         time.Duration
}

type Pool struct {
	Name             string
	AdHoc            bool
	Slots            int
	Wait             time.Duration
	Memory           Size           // 0: no memory admission
	Groups           map[string]int // group -> units consumed
	Providers        []string       // effective list, host order
	DefaultsEnvUnset []string       // [defaults].env_unset (both layers): applied first
	EnvDefault       map[string]Template
	Env              map[string]Template
	EnvUnset         []string // pool env_unset (both layers): applied after Env
	Refuse           []RefuseRule
	OnSignal         string // "terminate" | "forward-and-wait"
	Hooks            Hooks
	RemoteSize       map[string]string // provider type -> size key
	RemoteTimeout    time.Duration
	Remote           *PoolRemote // repo [pools.<p>.remote], nil if absent
}

type Hooks struct {
	PreRun  []Hook // host first
	PostRun []Hook // host first
	Env     *Hook  // at most one
	Wrap    []WrapRule
}

type Hook struct {
	Name       string // "pre_run" | "env" | "post_run"
	Argv       []Template
	Timeout    time.Duration
	RefuseExit []int
	Layer      string // "host" | "repo"
}

type WrapRule struct {
	WhenWorktree string // glob
	Argv         []Template
}

type RefuseRule struct {
	Tool             string
	When             string // "always" | "repo-wide"
	UnlessArgs       []string
	SkipOptionValues []string
	NpmScript        []string
	ArgvPrefix       []string
	Reason           string
}

type Group struct {
	Name  string
	Units int
}

type Priority struct {
	HighCwdContains []string
	HighCwd         []string
}

type Detach struct{ WaitTimeout time.Duration }

type RemoteLimits struct {
	Worktrees       []string
	ForwardEnvAllow []string
	ForwardEnvForce []string
	OnFailure       string // "next" | "stop"
}

type Provider struct {
	Type         string
	Fixed        bool // fixed capacity: slots, no API key (providers delta C.3)
	NamePrefix   string
	APIKey       string // never printed
	APIKeyFile   string
	DailyUSD     Micros
	MaxLive      int
	LaunchWait   time.Duration
	StartTimeout time.Duration
	Breaker      Breaker
	Transport    Transport
	Agent        Agent
	Artifacts    ArtifactLimits
	// FallbackUntil is the effective fallback boundary, "resource" or "attach" (spec 8.2): the
	// host key fallback_until, never later than the type's declaration (FallbackCap).
	FallbackUntil string
	FallbackCap   string
	// SendEnv is the effective caller-env level, "none", "forwarded" or "forced" (providers delta
	// S.2); SendEnvSource is "default" or "host". MultiJob: the type's resources serve more than
	// one job (S.4).
	SendEnv       string
	SendEnvSource string
	MultiJob      bool
	Raw           map[string]any // provider-specific keys, decoded by the provider (8.5)
	Endpoints     []string       // hosts the provider talks to (8.1), for the [test] gate
	// Plugin is set for a plugin type (spec 8.5, providers delta A.2): the pinned executable.
	// Sizes then holds the required host price per size (A.7.1) and SizesRaw the sizes table as
	// decoded, both passed to the plugin's configure.
	Plugin   *PluginPin
	Sizes    map[string]Micros
	SizesRaw map[string]any
}

// PluginPin is the host-only plugin table of a provider section (A.2): an absolute path and a
// sha256, env names passed through, and secret files read at spawn and sent on fd 3.
type PluginPin struct {
	Path        string
	SHA256      string
	EnvPass     []string
	SecretFiles map[string]string // name -> file (home expanded), 0600 and owned by the user
}

type Breaker struct {
	Failures int
	Window   time.Duration
}

type Transport struct {
	Remote     string
	RefPrefix  string
	KnownHosts string // OpenSSH known_hosts text
	// Kind is TransportGitPush (the default) or TransportUpload (ssh only: the commit goes to
	// the host as a git bundle on the gate session). MaxBundle bounds that bundle.
	Kind      string
	MaxBundle Size
}

// The transport kinds, and the bounds of transport.max_bundle.
const (
	TransportGitPush = "git-push"
	TransportUpload  = "upload"
	DefaultMaxBundle = Size(1 << 30)
	MaxBundleLimit   = Size(8 << 30)
)

type Agent struct {
	Source string // "release" | "url" | "build"
	URL    string
	Path   string
	Commit string // build: the pinned commit of the job repo (40 hex)
	Image  string // build: the pod image, <name>@sha256:<64 hex>
	SHA256 string // build: the sha256 the built binary must have
	// Paths maps an architecture to the local file whose sha256 is sent (source = "url"); the
	// legacy Path is folded into Paths["amd64"] at load.
	Paths map[string]string
	// ModuleDir is a bitc source tree the host builds the agent from (source = "build" without
	// commit, image and sha256; providers delta C.4 item 3).
	ModuleDir string
}

type ArtifactLimits struct {
	MaxCompressed Size
	MaxUnpacked   Size
	MaxEntries    int
}

type RepoGrant struct {
	Match         string
	Config        string
	CloneURL      string
	DeploySecret  string
	DeployKeyFile string // ~ expanded; a 0600 file owned by the user (checked at load; may not exist yet)
	RootSteps     bool
}

type RepoRemote struct {
	ForwardEnv    []string
	ForbidIgnored []string
	AllowIgnored  []string
	Env           map[string]Template
	EnvUnset      []string
	Packages      []string
	Tools         []Tool
	Setup         []Step
	Services      []Service
}

type PoolRemote struct {
	Allow     [][]string
	Artifacts []Artifact
	Timeout   time.Duration
	Env       map[string]Template
	EnvUnset  []string
	Arch      []string // linux architectures the pool's jobs may run on; default ["amd64"] (spec 8.4)
}

// Tool is a sha256-pinned download; Arch is "amd64" or "arm64" (default "amd64").
type Tool struct{ Name, Arch, URL, SHA256 string }

type Step struct {
	Name  string
	RunAs string // "root" | "job"
	Argv  []string
}

type Service struct {
	Name         string
	RunAs        string
	Argv         []string
	Ready        []string
	ReadyTimeout time.Duration
}

type Artifact struct {
	Path    string
	Replace bool
}

type Compat struct {
	LegacyDir  string
	HoldEnv    string
	NamePrefix string
	Groups     map[string]string // bitc group -> legacy lock stem
}

type Test struct {
	ProcfsRoot    string
	TimeScale     float64 // (0, 1]; 0 means 1
	HTTPResults   bool
	AgentURL      string
	AccountingDir string
	// FixedSecrets makes the job's B, K and M fixed values (see remote.jobSecrets), so a suite that
	// cannot see them in the clear can still look for them in what a plugin or fake recorded.
	FixedSecrets bool
	// SSHExec = "local" makes the ssh provider run the executable SSHGate (with SSHRoot as the
	// fake host's state) instead of the system ssh client (providers delta C.11, sshfake).
	SSHExec, SSHGate, SSHRoot string
	// SSHSshd names the program doctor runs in place of `sshd -T` for the tunnel account check
	// (providers delta C.6.3), so a test needs no root and no sshd.
	SSHSshd string
}

// Scale applies [test] time_scale to a duration bitc waits on.
func (e *Effective) Scale(d time.Duration) time.Duration {
	if e == nil || e.Test == nil || e.Test.TimeScale <= 0 || e.Test.TimeScale >= 1 {
		return d
	}
	return time.Duration(float64(d) * e.Test.TimeScale)
}
