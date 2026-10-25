package admit

import (
	"crypto/subtle"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/Exploded-views/bitc/internal/config"
	"github.com/Exploded-views/bitc/internal/lock"
	"github.com/Exploded-views/bitc/internal/proc"
	"github.com/Exploded-views/bitc/internal/state"
)

// VerifyHold checks a BITC_HOLD claim (spec 6.6): the ticket exists in this state dir under a
// bare name, is flocked now, holds the same token, and its (pid, start) is an ancestor of
// this process. Anything else is ignored silently.
func VerifyHold(l state.Layout, value string) (*TicketFile, bool) {
	parts := strings.Split(value, ":")
	if len(parts) != 3 || !config.ValidPoolName(parts[0]) || !ticketNameRE.MatchString(parts[1]) || parts[2] == "" {
		return nil, false
	}
	pool := config.CanonName(parts[0])
	path := filepath.Join(l.Tickets(pool), parts[1])
	if !lock.Held(path) {
		return nil, false
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	var t Ticket
	if json.Unmarshal(b, &t) != nil || t.State != "running" {
		return nil, false
	}
	if subtle.ConstantTimeCompare([]byte(t.Token), []byte(parts[2])) != 1 {
		return nil, false
	}
	if !proc.IsAncestor(t.Pid, t.Start) {
		return nil, false
	}
	return &TicketFile{Name: parts[1], Pool: pool, T: t}, true
}

// IsHigh reports whether the caller is high priority: a high_cwd_contains entry is a literal
// substring of the logical or canonical cwd, or a high_cwd glob matches either (spec 6.2).
func IsHigh(e *config.Effective) bool {
	for _, frag := range e.Priority.HighCwdContains {
		if frag != "" && (strings.Contains(e.LogicalCwd, frag) || strings.Contains(e.Cwd, frag)) {
			return true
		}
	}
	for _, g := range e.Priority.HighCwd {
		if ok, _ := config.Match(g, e.LogicalCwd); ok {
			return true
		}
		if ok, _ := config.Match(g, e.Cwd); ok {
			return true
		}
	}
	return false
}
