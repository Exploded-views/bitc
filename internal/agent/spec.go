// Package agent is the remote side of a job (spec 8.4), the same binary as the host side.
package agent

import (
	"encoding/base64"
	"encoding/json"
	"time"
)

// Spec is what the host sends in BITC_SPEC (base64 JSON). Secrets B and K travel here and in
// nothing else; the agent moves them out of its environment at boot.
type Spec struct {
	Job          string    `json:"job"`
	Pool         string    `json:"pool"`
	Argv         []string  `json:"argv"`
	Cwd          string    `json:"cwd"`
	Env          []string  `json:"env"`
	CloneURL     string    `json:"clone_url"`
	Ref          string    `json:"ref"`
	Sha          string    `json:"sha"`
	KnownHosts   string    `json:"known_hosts"`
	Timeout      Duration  `json:"timeout"`
	Packages     []string  `json:"packages,omitempty"`
	Tools        []Tool    `json:"tools,omitempty"`
	Setup        []Step    `json:"setup,omitempty"`
	Services     []Service `json:"services,omitempty"`
	RootSteps    bool      `json:"root_steps"`
	Artifacts    []string  `json:"artifacts,omitempty"`
	Port         int       `json:"port"`
	Bearer       string    `json:"bearer"`                 // hex B
	MacKey       string    `json:"mac_key"`                // hex K
	Nonce        string    `json:"nonce"`                  // hex M
	Unprivileged bool      `json:"unprivileged,omitempty"` // host [test] only
	TestRoot     string    `json:"test_root,omitempty"`    // the RunPod fake's per-pod dir
}

type Tool struct{ Name, URL, SHA256 string }
type Step struct {
	Name  string   `json:"name"`
	RunAs string   `json:"run_as"`
	Argv  []string `json:"argv"`
}
type Service struct {
	Name         string   `json:"name"`
	RunAs        string   `json:"run_as"`
	Argv         []string `json:"argv"`
	Ready        []string `json:"ready"`
	ReadyTimeout Duration `json:"ready_timeout"`
}

// Duration marshals as a Go duration string.
type Duration time.Duration

func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(time.Duration(d).String()) }
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	*d = Duration(v)
	return err
}

// Encode is the BITC_SPEC value.
func (s Spec) Encode() (string, error) {
	b, err := json.Marshal(s)
	return base64.StdEncoding.EncodeToString(b), err
}

// DecodeSpec parses a BITC_SPEC value.
func DecodeSpec(v string) (Spec, error) {
	var s Spec
	b, err := base64.StdEncoding.DecodeString(v)
	if err != nil {
		return s, err
	}
	return s, json.Unmarshal(b, &s)
}
