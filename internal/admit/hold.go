// Package admit decides when a command may start (spec 6.2, 6.6).
package admit

import (
	"crypto/rand"
	"encoding/hex"
	"io"
	"time"

	"github.com/Exploded-views/bitc/internal/config"
	"github.com/Exploded-views/bitc/internal/state"
)

// Request is one admission attempt loop for one pool.
type Request struct {
	Layout     state.Layout
	Eff        *config.Effective
	Pool       *config.Pool
	Argv       []string
	Job        string
	CallerHold string    // the caller's BITC_HOLD ("" if unset)
	CallerEnv  []string  // for compat claims (12)
	Stderr     io.Writer // warnings and reap notes
	Now        func() time.Time
	OnReap     func(pool, job string) // called after a dead hold's group is reaped (history "reaped")
}

// Hold is an admitted ticket: slot and group units taken, command about to run.
type Hold struct {
	Pool    string
	Slot    int // 1..N
	Ticket  string
	Token   string // 32 hex
	Nested  bool
	Depth   int // 0 for a top-level hold
	Units   map[string][]int
	WaitMS  int64
	Started time.Time

	ExtraEnv []string // extra KEY=VALUE for the command ([compat] hold_env)

	release    func()
	setProcess func(pid, pgid int, leaderStart uint64) error
}

// EnvValue is BITC_HOLD: "<pool>:<ticket>:<token>" (spec 6.3).
func (h *Hold) EnvValue() string { return h.Pool + ":" + h.Ticket + ":" + h.Token }

// SetProcess records the command in the ticket for orphan reaping: its pid and start time, and
// its process group (0 when the command shares the caller's group, a TTY run: then only the
// command and its descendants may be reaped, never the caller's group).
func (h *Hold) SetProcess(pid, pgid int, leaderStart uint64) error {
	if h.setProcess == nil {
		return nil
	}
	return h.setProcess(pid, pgid, leaderStart)
}

// Release ends the hold; it is idempotent.
func (h *Hold) Release() {
	if h.release != nil {
		h.release()
		h.release = nil
	}
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
