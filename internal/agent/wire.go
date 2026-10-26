package agent

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
)

// The host-agent wire types and framing. They are portable: a non-Linux client build encodes
// boot payloads and reads tail frames for a Linux agent; the agent itself is Linux-only.

// BootPayload is what stage 1 hands stage 2 on fd 3 (spec 8.4 step 1). The environment path
// builds it from BITC_*; --boot-stdin reads it from a length-prefixed frame on stdin.
type BootPayload struct {
	Spec        string `json:"spec"`
	Deadline    int64  `json:"deadline"`
	DeployKey   string `json:"deploy_key"`
	Provider    string `json:"provider"`
	ResourceID  string `json:"resource_id"`
	ScopedKey   string `json:"scoped_key"`
	APIBase     string `json:"api_base"`
	ResultsRoot string `json:"results_root,omitempty"` // --boot-stdin only; default /run/bitc
	// KeepAlive asks for a Tasks API task held for the job (--boot-stdin on Sprites only).
	KeepAlive *KeepAlive  `json:"keep_alive,omitempty"`
	Code      *CodeUpload `json:"code,omitempty"`
}

// KeepAlive is the lifetime of the keep-alive task and how often it is renewed.
type KeepAlive struct {
	Expire Duration `json:"expire"`
	Renew  Duration `json:"renew"`
}

// MaxBootPayload is the largest boot payload body the agent accepts (8 MiB).
const MaxBootPayload = 8 << 20

var (
	ErrTruncated = errors.New("truncated payload")
	ErrTooLarge  = errors.New("payload over 8 MiB")
	ErrBadArg    = errors.New("bad argument")
	ErrNotFound  = errors.New("not found")
	ErrOffset    = errors.New("offset beyond the end")
)

// CodeUpload says that exactly Len bytes of a git bundle (SHA256 is its hash, Ref the ref it
// carries) follow the frame on the same stream. ssh upload transport only.
type CodeUpload struct {
	Kind   string `json:"kind"`
	Len    int64  `json:"len"`
	SHA256 string `json:"sha256"`
	Ref    string `json:"ref"`
}

// Frame is the boot payload frame: an 8-byte big-endian length, then that many bytes of JSON.
func (p BootPayload) Frame() ([]byte, error) { return frameJSON(p) }

// frameJSON is an 8-byte big-endian length followed by v as JSON.
func frameJSON(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	if len(b) > MaxBootPayload {
		return nil, ErrTooLarge
	}
	out := make([]byte, 8, 8+len(b))
	binary.BigEndian.PutUint64(out, uint64(len(b)))
	return append(out, b...), nil
}

// ReadBootPayload reads one frame. EOF before the declared length, a zero length or unparsable
// JSON is ErrTruncated: the agent starts nothing (spec 8.4, providers delta C.5).
func ReadBootPayload(r io.Reader) (BootPayload, error) {
	var p BootPayload
	return p, readFrame(r, &p)
}

// readFrame reads one frame into v.
func readFrame(r io.Reader, v any) error {
	var head [8]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return ErrTruncated
	}
	n := binary.BigEndian.Uint64(head[:])
	if n > MaxBootPayload {
		return ErrTooLarge
	}
	if n == 0 {
		return ErrTruncated
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		return ErrTruncated
	}
	if json.Unmarshal(body, v) != nil {
		return ErrTruncated
	}
	return nil
}

// WithoutKey returns the payload with the deploy key taken out, and the key. Stage 1 keeps the
// rest in the memfd that survives the re-exec and passes the key in a second one that stage 2
// reads and closes before any repo code runs.
func (p BootPayload) WithoutKey() (BootPayload, string) {
	key := p.DeployKey
	p.DeployKey = ""
	return p, key
}

// Frame is one line of `__agent tail`: data of a result file at an offset (Off is omitted when
// 0), a heartbeat carrying whether the agent is alive, or the final done.
type Frame struct {
	File  string `json:"file,omitempty"`
	Off   int64  `json:"off,omitempty"`
	Data  []byte `json:"data,omitempty"`
	HB    bool   `json:"hb,omitempty"`
	Alive *bool  `json:"alive,omitempty"`
	Done  bool   `json:"done,omitempty"`
}

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
