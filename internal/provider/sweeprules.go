package provider

import (
	"context"
	"time"
)

// SweepRules is implemented by a provider whose resources are one per job and die with it (a
// Sprite): the sweep then names them with Noun, also deletes an own resource that no live
// reservation on this host accounts for once it is older than GoneAfter ("job process gone"),
// and the sweep timer is mandatory (doctor --remote fails when no sweep ran in the last
// TimerEvery x 2).
type SweepRules interface {
	SweepRules() SweepPolicy
}

// SweepPolicy is a provider's sweep behaviour beyond "own, past E, owner tag verifies".
type SweepPolicy struct {
	Noun          string        // "sprite": the word in `swept sprite <name> (...)`
	GoneAfter     time.Duration // an own resource with no reservation older than this is deleted; 0 = never
	TimerRequired bool          // doctor --remote fails when no sweep ran in the last 30 minutes
	TimerAdvice   string        // else, a doctor check of this name warns when none did (the provider self-deletes)
}

// DoctorLine is one line of a provider's own `doctor --remote` checks.
type DoctorLine struct{ Level, Check, Detail string }

// Doctorer is implemented by a provider that adds its own `doctor --remote` checks.
type Doctorer interface {
	DoctorLines(ctx context.Context) []DoctorLine
}

// SlotStatus is a fixed-capacity provider's slots as `status` shows them: how many it owns and
// which groups are held now (by any controller; Install tells whose).
type SlotStatus struct {
	Total int
	Held  []SlotHold
}

// SlotHold is one held slot group.
type SlotHold struct {
	Slots   []int
	Name    string
	Install string
	Until   time.Time // the lease's end
}

// SlotReporter is implemented by a fixed-capacity provider; `status` adds a slots section.
type SlotReporter interface {
	SlotStatus(ctx context.Context) (SlotStatus, error)
}

// ForeignOrg is implemented by the error of a listing that failed because a resource belongs to
// another organization: the sweep, doctor and status name it, and never touch it.
type ForeignOrg interface {
	error
	ForeignOrg() (name, org string)
}
