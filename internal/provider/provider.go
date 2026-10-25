// Package provider is the interface every remote provider implements (spec 8.1).
package provider

import (
	"context"
	"io"
	"time"

	"github.com/Exploded-views/bitc/internal/config"
)

// Boundary is how late a failed job may still move on to the next provider (spec 8.2).
type Boundary uint8

const (
	UntilResource Boundary = iota // fall back only before a resource exists
	UntilAttach                   // also after a resource exists, until job input reaches it
)

// String is the host-file spelling: "resource" or "attach".
func (b Boundary) String() string {
	if b == UntilAttach {
		return "attach"
	}
	return "resource"
}

// ParseBoundary reads "resource" or "attach".
func ParseBoundary(s string) (Boundary, bool) {
	switch s {
	case "resource":
		return UntilResource, true
	case "attach":
		return UntilAttach, true
	}
	return UntilResource, false
}

// EnvLevel is how much of the caller's forwarded env may reach a provider (providers delta S.2).
// The order is EnvNone < EnvForwarded < EnvForced.
type EnvLevel uint8

const (
	EnvNone      EnvLevel = iota // no caller env crosses
	EnvForwarded                 // plain names cross; forced names never do
	EnvForced                    // forced names may cross too
)

// String is the host-file spelling: "none", "forwarded" or "forced".
func (l EnvLevel) String() string {
	switch l {
	case EnvNone:
		return "none"
	case EnvForced:
		return "forced"
	}
	return "forwarded"
}

// ParseEnvLevel reads "none", "forwarded" or "forced".
func ParseEnvLevel(s string) (EnvLevel, bool) {
	switch s {
	case "none":
		return EnvNone, true
	case "forwarded":
		return EnvForwarded, true
	case "forced":
		return EnvForced, true
	}
	return EnvForwarded, false
}

type Caps struct {
	LiveLogs, Artifacts, Spend, SelfDelete bool
	// Fixed: fixed capacity (slots on a host the owner already has, providers delta C.3). The
	// credential is the provider's own, daily_usd is needed only when it is priced, and the
	// daily comparison is skipped while it is not.
	Fixed         bool
	FallbackUntil Boundary // the latest boundary this provider supports
	// SendEnv is the provider's default caller-env level (the host key send_env overrides it).
	SendEnv EnvLevel
	// MultiJob: a resource serves more than one job, so forced names never reach it (S.4).
	MultiJob bool
}

type Info struct {
	Type      string
	Caps      Caps
	Endpoints []string // every host the provider talks to (API and results)
	// Arch lists the linux architectures the provider can run ("amd64", "arm64"); empty means
	// amd64 only (spec 8.1, providers delta C.4).
	Arch []string
	// MaxName is the longest resource name the provider accepts, at most 63 (0 means 63). A
	// plugin declares it in configure (providers delta A.4.5).
	MaxName int
}

// JobSpec is one remote job (spec 8.1).
type JobSpec struct {
	Job, Pool    string
	Argv         []string // argv[0] reduced to its basename
	Cwd          string   // relative to the work tree
	Env          []string // remote env, KEY=VALUE
	CloneURL     string
	Ref          string
	Sha          string
	SizeKey      string
	Timeout      time.Duration
	Created      time.Time
	Deadline     time.Time // D = create + start_timeout + timeout
	Expiry       time.Time // E = D + 15m
	Artifacts    []config.Artifact
	Packages     []string
	Tools        []config.Tool // only the entries for Arch
	Arch         string        // the linux architecture the job runs on (set by preflight)
	Setup        []config.Step
	Services     []config.Service
	RootSteps    bool
	RepoID       string
	DeploySecret string
	// DeployKeyFile is the path of the repo's read-only deploy key on the host ([[repos]]
	// deploy_key_file) for a provider that delivers the key itself (Sprites); the bytes are read
	// by that provider's Launch and never held in the job object.
	DeployKeyFile  string
	Bearer, MacKey [32]byte // B and K (spec 8.4)
	Nonce          [32]byte // M
	Install        string   // install16
	Name           string   // resource name, set by the provider's naming rule
	MaxName        int      // the longest name Name may have (Info.MaxName; 0 means 63)
	DeployKey      string   // the deploy key bytes for a plugin's launch secrets; never in the job object
	Owner          string   // OwnerTag(install secret, Name)
	AgentURL       string
	AgentSHA256    string
	AgentBuild     *AgentBuild // agent source "build": the pod builds the agent (spec 8.4 step 1); AgentURL is then empty
	Unprivileged   bool        // test runs only ([test] set): the agent runs as a plain process
	TestRoot       string      // test runs only: the fake pod's private dir
}

// AgentBuild is a pinned agent build: the pod fetches Ref (the hidden ref holding Commit, set by
// the transport), builds in Image and runs the result only if its sha256 is SHA256.
type AgentBuild struct {
	Commit, Ref, Image, SHA256 string
}

type Handle struct {
	ID, Name   string
	ResultsURL string // base URL of the results server, ".../v1/"
	// Created is when the resource began to bill, when that is before Launch returned (Sprites:
	// the create's 201, ahead of provisioning); zero means Launch's return.
	Created time.Time
}

type Phase string

const (
	Booting Phase = "booting"
	Begun   Phase = "begun"
	Ended   Phase = "ended"
	Lost    Phase = "lost"
)

type Stream int

const (
	Stdout Stream = iota
	Stderr
	ResultFile // result.json on the results server
	StateFile  // state.json
)

// Sink receives what Watch observes.
type Sink interface {
	Phase(p Phase, why string)
	Setup(line string)      // a pre-BEGIN log line (printed as "pod: <line>")
	Out(s Stream, b []byte) // live command output (Caps.LiveLogs)
}

type Result struct {
	Exit  int
	Began bool
	Why   string
}

type ArtifactSpec struct {
	MaxCompressed int64
	Offset        int64
}

type Resource struct {
	ID, Name, Install, Pool, Job, SizeKey string
	USDPerHour                            config.Micros
	Created, Expires                      time.Time
	State                                 string // starting | running | exited | gone
}

type Provider interface {
	Info() Info
	Preflight(ctx context.Context, j *JobSpec) error
	Estimate(ctx context.Context, j *JobSpec) (config.Micros, error)
	Launch(ctx context.Context, j *JobSpec) (Handle, error)
	Watch(ctx context.Context, h Handle, out Sink) (Result, error)
	Output(ctx context.Context, h Handle, s Stream, off int64, w io.Writer) error
	Fetch(ctx context.Context, h Handle, a ArtifactSpec, w io.Writer) error
	Owner(ctx context.Context, id string) (string, error)
	Teardown(ctx context.Context, id string) error
	List(ctx context.Context, prefixes []string) ([]Resource, error)
	Spend(ctx context.Context) (config.Micros, error)
}

// ErrIneligible: argv, size or request too large; try the next provider (spec 8.1 table).
type ErrIneligible struct{ Why string }

// ErrTransient: capacity, 429/5xx, network, cap, max_live.
type ErrTransient struct{ Why string }

// ErrUnavailable: auth, no balance, account disabled. Local marks a cause on this host (missing
// credentials, an unusable accounting dir): it never pauses the provider (spec 8.3 breaker).
type ErrUnavailable struct {
	Why   string
	Local bool
}

func (e *ErrIneligible) Error() string  { return e.Why }
func (e *ErrTransient) Error() string   { return e.Why }
func (e *ErrUnavailable) Error() string { return e.Why }

// NothingCreated is implemented by a Launch error that proves no resource was made (an
// unreachable host, a refusal before anything ran): the core then skips its name lookup and its
// create retry, and the walk moves on at once (providers delta C.5.1, C.6).
type NothingCreated interface{ NothingCreated() bool }

// Unmade marks err as NothingCreated; errors.As still finds the class inside.
func Unmade(err error) error {
	if err == nil {
		return nil
	}
	return &unmade{err}
}

type unmade struct{ error }

func (u *unmade) Unwrap() error        { return u.error }
func (u *unmade) NothingCreated() bool { return true }

// ErrSetup is a setup failure the agent reported before BEGIN (spec 8.4, the
// __BITC_SETUP_FAILED__ marker). Watch returns it once a resource exists, so it is never a
// fallback. Class "repo" (the repo's own step, root step or service exited non-zero by itself, or
// a tool's sha256 mismatched) exits 69; "infra", "" or anything unparsable is 75 (spec 8.2, 15.9).
type ErrSetup struct{ Step, Class, Line string }

func (e *ErrSetup) Error() string { return "setup step " + e.Step + " failed: " + e.Line }

// Repo reports the repo's own failure: retrying cannot help.
func (e *ErrSetup) Repo() bool { return e.Class == "repo" }

// ErrPreAttach: Launch made a resource but failed before any job input reached it (spec 8.2).
// Err is ErrTransient, ErrUnavailable, ErrIneligible or ErrSetup.
type ErrPreAttach struct {
	ID  string
	Err error
}

// ErrAttached: Launch failed after job input reached the resource; the job never falls back.
type ErrAttached struct {
	ID  string
	Err error
	// Ambiguous marks a plugin launch with no definite outcome, or one after which the job's
	// resource was found (providers delta A.5.4, A.5.5). ID is then the resource id found, else
	// the name core assigned, and the adapter has already torn down by name. Walk writes one
	// breaker failure for it; Unconfirmed says the teardown was not confirmed, so the job's
	// reservation stays as an orphan until the sweep or E.
	Ambiguous, Unconfirmed bool
}

// ErrConfig is a configuration error found only when a provider starts, such as a plugin's
// configure answering `config` or a max_name below 60: the call ends with 78 (providers delta
// A.4.5, A.8). Why carries the host-file text after "<file>: ".
type ErrConfig struct{ Why string }

func (e *ErrConfig) Error() string { return e.Why }

func (e *ErrPreAttach) Error() string { return e.Err.Error() }
func (e *ErrPreAttach) Unwrap() error { return e.Err }
func (e *ErrAttached) Error() string  { return e.Err.Error() }
func (e *ErrAttached) Unwrap() error  { return e.Err }

// UploadRef is the ref an upload transport's bundle carries, which the host's agent fetches:
// refs/bitc/upload/<job>. It is shared by the core transport stage and the providers that upload.
func UploadRef(job string) string { return "refs/bitc/upload/" + job }
