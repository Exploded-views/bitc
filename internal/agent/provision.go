package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// ProvisionSpec is the whole input of `__agent provision` (sprites-provider.md 3): the repo's
// packages and tools and nothing that belongs to one job. Unprivileged and TestRoot are for the
// host's [test] runs only, as in Spec.
type ProvisionSpec struct {
	Packages     []string `json:"packages,omitempty"`
	Tools        []Tool   `json:"tools,omitempty"`
	Unprivileged bool     `json:"unprivileged,omitempty"`
	TestRoot     string   `json:"test_root,omitempty"`
}

// Frame is the length-prefixed JSON the host writes on stdin (the boot payload's framing).
func (p ProvisionSpec) Frame() ([]byte, error) { return frameJSON(p) }

// ReadProvisionSpec reads one frame.
func ReadProvisionSpec(r io.Reader) (ProvisionSpec, error) {
	var p ProvisionSpec
	return p, readFrame(r, &p)
}

// Exit codes of `__agent provision` besides 0: the repo's own failure and an infrastructure one.
// Anything else (64 bad input, a signal, 69, 126, 127) is read as infrastructure.
const (
	ProvisionExitRepo  = 71
	ProvisionExitInfra = 75
)

// ProvisionMarkerName is the marker file in the state root.
const ProvisionMarkerName = "provisioned.json"

// ProvisionMarker records what provisioning installed; Run skips its own installs when it matches.
type ProvisionMarker struct {
	AgentSHA256 string   `json:"agent_sha256"`
	Packages    []string `json:"packages"`
	Tools       []Tool   `json:"tools"`
}

// ProvisionOptions are the agent's environment for provisioning.
type ProvisionOptions struct {
	Stdout               io.Writer
	System               System
	ToolsRoot, StateRoot string
	HTTP                 *http.Client
	SelfSHA              func() (string, error) // the running agent's sha256 (default: hash the executable)
}

// selfSHA is the sha256 of the running agent binary.
func selfSHA() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	f, err := os.Open(exe)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Provision installs the packages and tools, creates the job user and the state root and writes
// the marker. It prints its progress on o.Stdout and returns 0, ProvisionExitRepo or
// ProvisionExitInfra. The last line of a failure is `setup step <name> failed: <why>`.
func Provision(o ProvisionOptions, ps ProvisionSpec) int {
	say := func(l string) { fmt.Fprintln(o.Stdout, l) }
	fail := func(err error) int {
		var se *stepError
		name, repo := "setup", false
		if errors.As(err, &se) {
			name, repo = se.Name, se.Repo
			if se.Tail != "" {
				say(se.Tail)
			}
		}
		say("setup step " + printable(name) + " failed: " + printable(errorLine(err)))
		if repo {
			return ProvisionExitRepo
		}
		return ProvisionExitInfra
	}
	for _, t := range ps.Tools {
		if !toolNameRE.MatchString(t.Name) {
			return fail(&stepError{Name: t.Name, Repo: true, Err: fmt.Errorf("tool name %q must match %s", t.Name, toolNameRE)})
		}
	}
	if err := ensureDir(o.StateRoot, 0o700); err != nil {
		return fail(&stepError{Name: "state", Err: err})
	}
	marker := filepath.Join(o.StateRoot, ProvisionMarkerName)
	_ = os.Remove(marker) // a failed provisioning must leave no marker of an earlier one
	say(fmt.Sprintf("provision: installing %d packages and %d tools", len(ps.Packages), len(ps.Tools)))
	if err := o.System.InstallPackages(ps.Packages); err != nil {
		return fail(&stepError{Name: "packages", Err: err})
	}
	client := o.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	for _, t := range ps.Tools {
		if _, err := installTool(client, o.ToolsRoot, t); err != nil {
			return fail(err)
		}
	}
	if _, _, _, err := o.System.EnsureJobUser(); err != nil {
		return fail(&stepError{Name: "job-user", Err: err})
	}
	sum := o.SelfSHA
	if sum == nil {
		sum = selfSHA
	}
	self, err := sum()
	if err != nil {
		return fail(&stepError{Name: "agent-sha", Err: err})
	}
	b, _ := json.Marshal(ProvisionMarker{AgentSHA256: self, Packages: ps.Packages, Tools: ps.Tools})
	tmp := marker + ".part"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fail(&stepError{Name: "marker", Err: err})
	}
	if err := os.Rename(tmp, marker); err != nil {
		return fail(&stepError{Name: "marker", Err: err})
	}
	say("provision: done")
	return 0
}

// printable keeps a one-line, printable form of s for a host-bound line.
func printable(s string) string {
	return strings.Map(func(r rune) rune {
		if r < ' ' || r == 0x7f {
			return '?'
		}
		return r
	}, s)
}

// provisioned reports whether the marker in the state root describes exactly this spec and this
// agent: then Run installs nothing itself.
func (o Options) provisioned(s Spec) bool {
	if o.StateRoot == "" {
		return false
	}
	b, err := os.ReadFile(filepath.Join(o.StateRoot, ProvisionMarkerName))
	if err != nil {
		return false
	}
	var m ProvisionMarker
	if json.Unmarshal(b, &m) != nil {
		return false
	}
	sum := o.SelfSHA
	if sum == nil {
		sum = selfSHA
	}
	self, err := sum()
	if err != nil || self != m.AgentSHA256 {
		return false
	}
	return slices.Equal(m.Packages, s.Packages) && slices.Equal(m.Tools, s.Tools)
}

// provisionVerb is `bitc __agent provision`: one ProvisionSpec frame on stdin. A refusal prints
// `refused: <why>` on stdout and exits 64 with nothing installed.
func provisionVerb(stdin io.Reader, stdout io.Writer) int {
	ps, err := ReadProvisionSpec(stdin)
	if err != nil {
		fmt.Fprintln(stdout, "refused: "+err.Error())
		return 64
	}
	o := ProvisionOptions{Stdout: stdout, System: RealSystem{}, ToolsRoot: "/opt/bitc/tools", StateRoot: "/var/lib/bitc"}
	if ps.Unprivileged && ps.TestRoot != "" {
		o.System = PlainSystem{}
		o.ToolsRoot, o.StateRoot = ps.TestRoot+"/tools", ps.TestRoot+"/var/lib/bitc"
	}
	return Provision(o, ps)
}
