// Package liveprobe is the Sprites probe kit: a scripted, capped runner that answers
// the open items of docs/design/sprites-provider.md section 9 against the live Sprites API.
// Only maintainers run it live; the tests drive it against
// spritesfake and httptest only.
//
// Hard rails, all in code: the token comes from a 0600 file and never reaches a log or the
// report; every Sprite is named bitcproof-s0-<run>-<n> and carries the owner label; at most one
// Sprite is live at any time; every Sprite is deleted when its probe ends, on error, on a signal
// and in a final sweep; Sprite-seconds are priced and capped; no real secret is ever sent.
package liveprobe

import (
	"context"
	"io"
	"net/url"
	"time"

	"github.com/Exploded-views/bitc/internal/provider/sprites"
)

// NamePrefix starts the name of every Sprite the kit creates.
const NamePrefix = "bitcproof-s0-"

// Probe statuses.
const (
	Answered    = "answered"
	AssumeWorst = "assume-worst"
	Skipped     = "skipped"
	Error       = "error"
)

// Outcome is what a probe returns.
type Outcome struct {
	Status   string
	Finding  string
	Evidence map[string]any
}

// Result is the JSON line of one probe.
type Result struct {
	ID            int            `json:"id"`
	Status        string         `json:"status"`
	Finding       string         `json:"finding"`
	Evidence      map[string]any `json:"evidence"`
	SpriteSeconds float64        `json:"sprite_seconds"`
}

// SpriteRec is one Sprite the run created.
type SpriteRec struct {
	Name          string    `json:"name"`
	Created       time.Time `json:"created"`
	Deleted       time.Time `json:"deleted,omitempty"`
	DeleteOK      bool      `json:"delete_confirmed"`
	Seconds       float64   `json:"seconds"`
	LeftoverFound bool      `json:"leftover,omitempty"`
}

// Report is everything the run learned.
type Report struct {
	Run         string
	Results     []Result
	Sprites     []SpriteRec
	Spent       float64  // USD
	Leftovers   []string // Sprites the final sweep found
	Preexisting []string // Sprites of an earlier run found and deleted before the first create
	Fatal       string   // a failure that stopped the run before any probe
}

// Probe is one item of design section 9.
type Probe struct {
	ID    int
	Title string
	// Group names the Sprite the probe shares; "" means it needs none.
	Group string
	// Order sorts the probes of one group.
	Order int
	// Worst is the declared worst-case duration of the probe itself (its context is cut there).
	Worst time.Duration
	// Own marks a probe that makes and deletes Sprites itself (it has no group Sprite); its whole
	// duration counts as Sprite time.
	Own bool
	// Warm marks the warm-reuse items (run after the phase-1 items).
	Warm bool
	// Skip, when it returns a reason, skips the probe before any Sprite is made for it.
	Skip func(Config) string
	Run  func(ctx context.Context, e *Env) Outcome
}

// GroupSpec is how a group's Sprite is made: the initial network policy domains and the memory
// limit of the resources policy.
type GroupSpec struct {
	Domains []string
	MemMB   int
}

// GroupOverhead is the declared worst case of making a Sprite, setting its policies and
// deleting it.
const GroupOverhead = 90 * time.Second

// API is what the kit needs of a Sprites client. *sprites.Client satisfies it.
type API interface {
	Org() string
	Secrets() []string
	CreateSprite(ctx context.Context, name string, labels map[string]string) (sprites.Sprite, error)
	GetSprite(ctx context.Context, name string) (sprites.Sprite, error)
	ListSprites(ctx context.Context, prefix string) ([]sprites.Sprite, error)
	DeleteSprite(ctx context.Context, name string) error
	ApplyBuildPolicies(ctx context.Context, name string, build sprites.NetworkPolicy, res sprites.Resources) error
	ApplyJobPolicy(ctx context.Context, name string, job sprites.NetworkPolicy) error
	Run(ctx context.Context, name string, r sprites.ExecRequest, stdin []byte) (sprites.ExecResult, error)
	Exec(ctx context.Context, name string, r sprites.ExecRequest) (*sprites.Session, error)
	Raw(ctx context.Context, method, path string, q url.Values, body any) (int, []byte)
	Organization(ctx context.Context) (sprites.OrgInfo, error)
	ListConnections(ctx context.Context) ([]string, error)
	ListCheckpoints(ctx context.Context, name string) ([]sprites.Checkpoint, error)
	CreateCheckpoint(ctx context.Context, name, comment string) (sprites.Checkpoint, error)
	GetCheckpoint(ctx context.Context, name, id string) (sprites.Checkpoint, error)
	RestoreCheckpoint(ctx context.Context, name, id string) error
	DeleteCheckpoint(ctx context.Context, name, id string) error
	FSRead(ctx context.Context, name, path string, w io.Writer) (int64, error)
	FSWrite(ctx context.Context, name, path string, data []byte) error
}

var _ API = (*sprites.Client)(nil)
