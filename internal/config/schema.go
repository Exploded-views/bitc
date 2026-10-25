package config

// fileSchema is the superset of the host and repo files (spec 5). Pointers and maps tell
// "absent" from "set"; class tags drive the layer checks and the generated reference.
type fileSchema struct {
	Version      *int64                    `toml:"version" bitc:"file,required"`
	MinVersion   *string                   `toml:"min_version" bitc:"file"`
	Host         *hostSchema               `toml:"host" bitc:"host"`
	Defaults     *defaultsSchema           `toml:"defaults" bitc:"project"`
	Pools        map[string]*poolSchema    `toml:"pools" bitc:"project"`
	Groups       map[string]*groupSchema   `toml:"groups" bitc:"host"`
	Priority     *prioritySchema           `toml:"priority" bitc:"host"`
	Detach       *detachSchema             `toml:"detach" bitc:"host"`
	Vars         map[string]any            `toml:"vars" bitc:"project"`
	RemoteLimits *remoteLimitsSchema       `toml:"remote_limits" bitc:"host"`
	Providers    map[string]map[string]any `toml:"providers" bitc:"host"`
	Repos        []repoGrantSchema         `toml:"repos" bitc:"host"`
	Compat       *compatSchema             `toml:"compat" bitc:"host"`
	Test         *testSchema               `toml:"test" bitc:"host"`
	Remote       *repoRemoteSchema         `toml:"remote" bitc:"repo"`
}

type hostSchema struct {
	StateDir          *string `toml:"state_dir" bitc:"host,default=$XDG_STATE_HOME/bitc"`
	Poll              *string `toml:"poll" bitc:"host,default=5s"`
	MemoryFloor       *string `toml:"memory_floor" bitc:"host,default=6GiB"`
	ReservationWindow *string `toml:"reservation_window" bitc:"host,default=60s"`
	UnknownPools      *string `toml:"unknown_pools" bitc:"host,default=refuse"`
	Grace             *string `toml:"grace" bitc:"host,default=10s"`
	MaxSignalWait     *string `toml:"max_signal_wait" bitc:"host,default=10m"`
	HistoryMax        *string `toml:"history_max" bitc:"host,default=50MiB"`
	Retention         *string `toml:"retention" bitc:"host,default=14d"`
}

type defaultsSchema struct {
	Slots      *int64            `toml:"slots" bitc:"host,default=1"`
	Wait       *string           `toml:"wait" bitc:"narrow,default=10m"`
	Env        map[string]string `toml:"env" bitc:"project"`
	EnvDefault map[string]string `toml:"env_default" bitc:"project"`
	EnvUnset   []string          `toml:"env_unset" bitc:"project"`
}

type poolSchema struct {
	Slots            *int64                  `toml:"slots" bitc:"host,default=1"`
	Wait             *string                 `toml:"wait" bitc:"narrow,default=10m"`
	Memory           *string                 `toml:"memory" bitc:"host"`
	Groups           map[string]int64        `toml:"groups" bitc:"host"`
	Providers        []string                `toml:"providers" bitc:"host,default=[\"local\"]"`
	ProvidersExclude []string                `toml:"providers_exclude" bitc:"repo"`
	RemoteLimits     *poolRemoteLimitsSchema `toml:"remote_limits" bitc:"host"`
	Env              map[string]string       `toml:"env" bitc:"project"`
	EnvDefault       map[string]string       `toml:"env_default" bitc:"project"`
	EnvUnset         []string                `toml:"env_unset" bitc:"project"`
	Refuse           []refuseSchema          `toml:"refuse" bitc:"project"`
	OnSignal         *string                 `toml:"on_signal" bitc:"project,default=terminate"`
	Hooks            *hooksSchema            `toml:"hooks" bitc:"project"`
	Remote           *poolRemoteSchema       `toml:"remote" bitc:"repo"`
}

type poolRemoteLimitsSchema struct {
	Size    map[string]string `toml:"size" bitc:"host"`
	Timeout *string           `toml:"timeout" bitc:"host,default=90m"`
}

type groupSchema struct {
	Units *int64 `toml:"units" bitc:"host,required"`
}

type prioritySchema struct {
	HighCwdContains []string `toml:"high_cwd_contains" bitc:"host,default=[]"`
	HighCwd         []string `toml:"high_cwd" bitc:"host,default=[]"`
}

type detachSchema struct {
	WaitTimeout *string `toml:"wait_timeout" bitc:"host,default=9m"`
}

type remoteLimitsSchema struct {
	Worktrees       []string `toml:"worktrees" bitc:"host,default=[]"`
	ForwardEnvAllow []string `toml:"forward_env_allow" bitc:"host,default=[]"`
	ForwardEnvForce []string `toml:"forward_env_force" bitc:"host,default=[]"`
	OnFailure       *string  `toml:"on_failure" bitc:"host,default=next"`
}

type repoGrantSchema struct {
	Match        string `toml:"match" bitc:"host,required"`
	Config       string `toml:"config" bitc:"host"`
	CloneURL     string `toml:"clone_url" bitc:"host"`
	DeploySecret string `toml:"deploy_secret" bitc:"host"`
	// DeployKeyFile is a 0600 file holding the repo's read-only deploy key, for a provider that
	// delivers the key itself (Sprites, sprites-provider.md decision 3).
	DeployKeyFile string `toml:"deploy_key_file" bitc:"host"`
	RootSteps     bool   `toml:"root_steps" bitc:"host,default=false"`
}

type compatSchema struct {
	LegacyDir  string            `toml:"legacy_dir" bitc:"host,required"`
	HoldEnv    string            `toml:"hold_env" bitc:"host"`
	NamePrefix string            `toml:"name_prefix" bitc:"host"`
	Groups     map[string]string `toml:"groups" bitc:"host"`
}

type testSchema struct {
	ProcfsRoot    *string  `toml:"procfs_root" bitc:"host"`
	TimeScale     *float64 `toml:"time_scale" bitc:"host"`
	HTTPResults   *bool    `toml:"http_results" bitc:"host"`
	AgentURL      *string  `toml:"agent_url" bitc:"host"`
	AccountingDir *string  `toml:"accounting_dir" bitc:"host"`
	FixedSecrets  *bool    `toml:"fixed_secrets" bitc:"host"`
	SSHExec       *string  `toml:"ssh_exec" bitc:"host"`
	SSHGate       *string  `toml:"ssh_gate" bitc:"host"`
	SSHRoot       *string  `toml:"ssh_root" bitc:"host"`
	SSHSshd       *string  `toml:"ssh_sshd" bitc:"host"`
}

type hooksSchema struct {
	PreRun  *hookSchema  `toml:"pre_run" bitc:"project"`
	PostRun *hookSchema  `toml:"post_run" bitc:"project"`
	Env     *hookSchema  `toml:"env" bitc:"project"`
	Wrap    []wrapSchema `toml:"wrap" bitc:"project"`
}

type hookSchema struct {
	Argv       []string `toml:"argv" bitc:"project,required"`
	Timeout    *string  `toml:"timeout" bitc:"project,default=60s"`
	RefuseExit []int64  `toml:"refuse_exit" bitc:"project,default=[64]"`
}

type wrapSchema struct {
	WhenWorktree string   `toml:"when_worktree" bitc:"project,required"`
	Argv         []string `toml:"argv" bitc:"project,required"`
}

type refuseSchema struct {
	Tool             string   `toml:"tool" bitc:"project"`
	When             string   `toml:"when" bitc:"project,default=always"`
	UnlessArgs       []string `toml:"unless_args" bitc:"project"`
	SkipOptionValues []string `toml:"skip_option_values" bitc:"project"`
	NpmScript        []string `toml:"npm_script" bitc:"project"`
	ArgvPrefix       []string `toml:"argv_prefix" bitc:"project"`
	Reason           string   `toml:"reason" bitc:"project,required"`
}

type repoRemoteSchema struct {
	ForwardEnv    []string          `toml:"forward_env" bitc:"repo"`
	ForbidIgnored []string          `toml:"forbid_ignored" bitc:"repo"`
	AllowIgnored  []string          `toml:"allow_ignored" bitc:"repo"`
	Env           map[string]string `toml:"env" bitc:"repo"`
	EnvUnset      []string          `toml:"env_unset" bitc:"repo"`
	Packages      []string          `toml:"packages" bitc:"repo"`
	Tools         []toolSchema      `toml:"tools" bitc:"repo"`
	Setup         []stepSchema      `toml:"setup" bitc:"repo"`
	Services      []serviceSchema   `toml:"services" bitc:"repo"`
}

type poolRemoteSchema struct {
	Allow     [][]string        `toml:"allow" bitc:"repo"`
	Artifacts []artifactSchema  `toml:"artifacts" bitc:"repo"`
	Timeout   *string           `toml:"timeout" bitc:"narrow"`
	Env       map[string]string `toml:"env" bitc:"repo"`
	EnvUnset  []string          `toml:"env_unset" bitc:"repo"`
	Arch      []string          `toml:"arch" bitc:"repo,default=[\"amd64\"]"`
}

type toolSchema struct {
	Name   string `toml:"name" bitc:"repo,required"`
	Arch   string `toml:"arch" bitc:"repo,default=amd64"`
	URL    string `toml:"url" bitc:"repo,required"`
	SHA256 string `toml:"sha256" bitc:"repo,required"`
}

type stepSchema struct {
	Name  string   `toml:"name" bitc:"repo,required"`
	RunAs string   `toml:"run_as" bitc:"repo,default=job"`
	Argv  []string `toml:"argv" bitc:"repo,required"`
}

type serviceSchema struct {
	Name         string   `toml:"name" bitc:"repo,required"`
	RunAs        string   `toml:"run_as" bitc:"repo,default=job"`
	Argv         []string `toml:"argv" bitc:"repo,required"`
	Ready        []string `toml:"ready" bitc:"repo,required"`
	ReadyTimeout *string  `toml:"ready_timeout" bitc:"repo,default=120s"`
}

type artifactSchema struct {
	Path    string `toml:"path" bitc:"repo,required"`
	Replace *bool  `toml:"replace" bitc:"repo,default=true"`
}

// providerCoreKeys are decoded by core for every [providers.<type>] (spec 8.5).
var providerCoreKeys = []struct{ Key, Default string }{
	{"api_key", ""}, {"api_key_file", ""}, {"name_prefix", "bitc"}, {"daily_usd", "required"},
	{"max_live", "2"}, {"launch_wait", "5m"}, {"start_timeout", "20m"},
	{"fallback_until", `the provider's declaration ("resource" for a plugin)`},
	{"send_env", `the provider's declaration ("forced" for runpod; "forwarded" for a plugin)`},
	{"breaker", `{failures = 3, window = "60m"}`},
	{"transport", `{remote = "origin", ref_prefix = "refs/bitc/jobs/", known_hosts = <built-in>, kind = "git-push", max_bundle = "1GiB"}`},
	{"agent", `{source = "release"}`},
	{"artifacts", `{max_compressed = "200MiB", max_unpacked = "2GiB", max_entries = 100000}`},
}
