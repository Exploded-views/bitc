// Package plugin is the adapter that runs an external provider executable (spec 8.5, providers
// delta A) behind the Provider interface: one pinned process per bitc process and type, NDJSON
// on its stdin and stdout, secrets on fd 3. Every rail stays in core; a plugin is an API client.
package plugin

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/Exploded-views/bitc/internal/provider"
)

// protoErr is a protocol error (A.6): malformed frame, missing field, wrong type, bad value.
// Unsupported marks a hello whose protocol bitc does not speak: a config error (A.8).
type protoErr struct {
	Why         string
	Unsupported bool
}

func (e *protoErr) Error() string { return "plugin protocol error: " + e.Why }

func protoErrf(format string, a ...any) *protoErr { return &protoErr{Why: fmt.Sprintf(format, a...)} }

// helloInfo is the decoded hello result (A.4.4).
type helloInfo struct {
	Version string
	Caps    provider.Caps
	Secrets []string
}

// parseHello decodes a hello result for plugin type typ. `fixed` and `multi_job` are required
// on the wire; caps.multi_job is Caps.MultiJob (providers delta S.4), and a plugin's
// Caps.SendEnv is always "forwarded" (S.2). `fixed` is wired into Caps once H.2 adds Caps.Fixed.
func parseHello(raw []byte, typ string) (helloInfo, error) {
	var h struct {
		Protocol      *int    `json:"protocol"`
		Type          *string `json:"type"`
		PluginVersion *string `json:"plugin_version"`
		Caps          *struct {
			LiveLogs      *bool   `json:"live_logs"`
			Artifacts     *bool   `json:"artifacts"`
			Spend         *bool   `json:"spend"`
			SelfDelete    *bool   `json:"self_delete"`
			FallbackUntil *string `json:"fallback_until"`
			Fixed         *bool   `json:"fixed"`
			MultiJob      *bool   `json:"multi_job"`
		} `json:"caps"`
		Secrets []string `json:"secrets"`
	}
	if err := json.Unmarshal(raw, &h); err != nil {
		return helloInfo{}, protoErrf("hello: %v", err)
	}
	if h.Protocol == nil || h.Type == nil || h.PluginVersion == nil || h.Caps == nil {
		return helloInfo{}, protoErrf("hello: protocol, type, plugin_version and caps are required")
	}
	if *h.Protocol != 1 {
		return helloInfo{}, &protoErr{Why: fmt.Sprintf("speaks protocol %d, bitc supports [1]", *h.Protocol), Unsupported: true}
	}
	if *h.Type != typ {
		return helloInfo{}, protoErrf("hello: type %q, want %q", *h.Type, typ)
	}
	c := h.Caps
	if c.LiveLogs == nil || c.Artifacts == nil || c.Spend == nil || c.SelfDelete == nil || c.FallbackUntil == nil || c.Fixed == nil || c.MultiJob == nil {
		return helloInfo{}, protoErrf("hello: caps needs live_logs, artifacts, spend, self_delete, fallback_until, fixed and multi_job")
	}
	b, ok := provider.ParseBoundary(*c.FallbackUntil)
	if !ok {
		return helloInfo{}, protoErrf("hello: caps.fallback_until %q is not resource or attach", *c.FallbackUntil)
	}
	return helloInfo{Version: *h.PluginVersion, Secrets: h.Secrets,
		Caps: provider.Caps{LiveLogs: *c.LiveLogs, Artifacts: *c.Artifacts, Spend: *c.Spend, SelfDelete: *c.SelfDelete, FallbackUntil: b,
			SendEnv: provider.EnvForwarded, MultiJob: *c.MultiJob}}, nil
}

// configInfo is the decoded configure result (A.4.5).
type configInfo struct {
	Endpoints, Arch []string
	MaxName         int
}

// minMaxName is the shortest name core can build without cutting anything but the pool (A.4.5).
const minMaxName = 60

// parseConfigure decodes a configure result: all three fields are required and max_name is at
// least 60 (else *provider.ErrConfig, the A.8 text).
func parseConfigure(raw []byte, typ string) (configInfo, error) {
	var c struct {
		Endpoints *[]string `json:"endpoints"`
		Arch      *[]string `json:"arch"`
		MaxName   *int      `json:"max_name"`
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return configInfo{}, protoErrf("configure: %v", err)
	}
	if c.Endpoints == nil || c.Arch == nil || c.MaxName == nil {
		return configInfo{}, protoErrf("configure: endpoints, arch and max_name are required")
	}
	if *c.MaxName < minMaxName {
		return configInfo{}, &provider.ErrConfig{Why: fmt.Sprintf("providers.%s.plugin: max_name %d is below %d, the shortest name bitc builds", typ, *c.MaxName, minMaxName)}
	}
	return configInfo{Endpoints: *c.Endpoints, Arch: *c.Arch, MaxName: *c.MaxName}, nil
}

// errObj is the error object of a final frame (A.6).
type errObj struct {
	Class      string    `json:"class"`
	Why        string    `json:"why"`
	ID         string    `json:"id"`
	Step       string    `json:"step"`
	SetupClass string    `json:"setup_class"`
	Inner      *errInner `json:"inner"`
}

type errInner struct {
	Class string `json:"class"`
	Why   string `json:"why"`
}

// parseErrObj decodes an error object; one that is not a JSON object, or has no class, is a
// protocol error.
func parseErrObj(raw []byte) (*errObj, error) {
	var e errObj
	if err := json.Unmarshal(raw, &e); err != nil {
		return nil, protoErrf("error object: %v", err)
	}
	if e.Class == "" {
		return nil, protoErrf("error object has no class")
	}
	return &e, nil
}

// plain maps the classes every method may answer (A.6), nil for the others.
func (e *errObj) plain(why string) error {
	switch e.Class {
	case "ineligible":
		return &provider.ErrIneligible{Why: why}
	case "transient":
		return &provider.ErrTransient{Why: why}
	case "unavailable":
		return &provider.ErrUnavailable{Why: why}
	case "unavailable_local":
		return &provider.ErrUnavailable{Why: why, Local: true}
	}
	return nil
}

// mapError maps a classified error of method (not launch) to the 8.1 type (A.6). An unknown
// class, or `pre_attach` / `attached` outside launch, is a protocol error.
func mapError(method string, e *errObj) error {
	if err := e.plain(e.Why); err != nil {
		return err
	}
	switch e.Class {
	case "setup":
		return &provider.ErrSetup{Step: e.Step, Class: e.SetupClass, Line: e.Why}
	case "config":
		return &provider.ErrConfig{Why: e.Why} // the adapter prefixes providers.<t>:
	case "pre_attach", "attached":
		return protoErrf("error class %q outside launch", e.Class)
	}
	return protoErrf("unknown error class %q", e.Class)
}

// wireJob is the protocol-1 job object (A.5.1): snake_case keys, hex B/K/M, RFC 3339 UTC times,
// whole seconds. DeploySecret and the deploy key never appear in it.
type wireJob struct {
	Job         string        `json:"job"`
	Pool        string        `json:"pool"`
	Cwd         string        `json:"cwd"`
	CloneURL    string        `json:"clone_url"`
	Ref         string        `json:"ref"`
	Sha         string        `json:"sha"`
	SizeKey     string        `json:"size_key"`
	RepoID      string        `json:"repo_id"`
	Install     string        `json:"install"`
	Name        string        `json:"name"`
	Owner       string        `json:"owner"`
	AgentURL    string        `json:"agent_url"`
	AgentSHA256 string        `json:"agent_sha256"`
	Argv        []string      `json:"argv"`
	Env         []string      `json:"env"`
	TimeoutS    int64         `json:"timeout_s"`
	Created     string        `json:"created"`
	Deadline    string        `json:"deadline"`
	Expiry      string        `json:"expiry"`
	Bearer      string        `json:"bearer"`
	MacKey      string        `json:"mac_key"`
	Nonce       string        `json:"nonce"`
	Artifacts   []wireArtfact `json:"artifacts"`
	Packages    []string      `json:"packages"`
	Tools       []wireTool    `json:"tools"`
	Setup       []wireStep    `json:"setup"`
	Services    []wireService `json:"services"`
	RootSteps   bool          `json:"root_steps"`
	Test        *wireTest     `json:"test,omitempty"`
}

type wireArtfact struct {
	Path    string `json:"path"`
	Replace bool   `json:"replace"`
}
type wireTool struct {
	Name   string `json:"name"`
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
	Arch   string `json:"arch"`
}
type wireStep struct {
	Name  string   `json:"name"`
	RunAs string   `json:"run_as"`
	Argv  []string `json:"argv"`
}
type wireService struct {
	Name         string   `json:"name"`
	RunAs        string   `json:"run_as"`
	Argv         []string `json:"argv"`
	Ready        []string `json:"ready"`
	ReadyTimeout int64    `json:"ready_timeout_s"`
}
type wireTest struct {
	Unprivileged bool   `json:"unprivileged"`
	TestRoot     string `json:"test_root"`
}

func ts(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05Z") }

func nz[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return slices.Clone(s)
}

func toWire(j *provider.JobSpec) wireJob {
	w := wireJob{Job: j.Job, Pool: j.Pool, Cwd: j.Cwd, CloneURL: j.CloneURL, Ref: j.Ref, Sha: j.Sha, SizeKey: j.SizeKey, RepoID: j.RepoID,
		Install: j.Install, Name: j.Name, Owner: j.Owner, AgentURL: j.AgentURL, AgentSHA256: j.AgentSHA256,
		Argv: nz(j.Argv), Env: nz(j.Env), TimeoutS: int64(j.Timeout / time.Second),
		Created: ts(j.Created), Deadline: ts(j.Deadline), Expiry: ts(j.Expiry),
		Bearer: hex.EncodeToString(j.Bearer[:]), MacKey: hex.EncodeToString(j.MacKey[:]), Nonce: hex.EncodeToString(j.Nonce[:]),
		Artifacts: []wireArtfact{}, Packages: nz(j.Packages), Tools: []wireTool{}, Setup: []wireStep{}, Services: []wireService{}, RootSteps: j.RootSteps}
	for _, a := range j.Artifacts {
		w.Artifacts = append(w.Artifacts, wireArtfact{a.Path, a.Replace})
	}
	for _, t := range j.Tools {
		w.Tools = append(w.Tools, wireTool{t.Name, t.URL, t.SHA256, t.Arch})
	}
	for _, s := range j.Setup {
		w.Setup = append(w.Setup, wireStep{s.Name, s.RunAs, nz(s.Argv)})
	}
	for _, s := range j.Services {
		w.Services = append(w.Services, wireService{s.Name, s.RunAs, nz(s.Argv), nz(s.Ready), int64(s.ReadyTimeout / time.Second)})
	}
	if j.Unprivileged || j.TestRoot != "" {
		w.Test = &wireTest{j.Unprivileged, j.TestRoot}
	}
	return w
}

// marshalJob renders the job object of a request.
func marshalJob(j *provider.JobSpec) ([]byte, error) { return json.Marshal(toWire(j)) }
