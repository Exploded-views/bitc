package run

import (
	"encoding/json"

	"github.com/Exploded-views/bitc/internal/state"
)

// Result is result.json (spec 4).
type Result struct {
	Origin      string `json:"origin"`  // "bitc" | "command"
	Outcome     string `json:"outcome"` // ok | command-failed | starved | refused | config | unavailable | interrupted | lost | internal
	ExitCode    int    `json:"exit_code"`
	Reason      string `json:"reason,omitempty"`
	Pool        string `json:"pool"`
	Slot        int    `json:"slot,omitempty"`
	Provider    string `json:"provider"`
	Job         string `json:"job"`
	Generation  int    `json:"generation,omitempty"`
	HoldStarted string `json:"hold_started,omitempty"`
	WaitMS      int64  `json:"wait_ms"`
	RunMS       int64  `json:"run_ms"`
}

// WriteResult writes r atomically (temp + rename, 0600).
func WriteResult(path string, r Result) error {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return state.WriteFileAtomic(path, append(b, '\n'))
}
