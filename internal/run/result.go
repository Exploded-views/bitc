package run

import (
	"encoding/json"
	"os/exec"
	"strings"

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
	Head        string `json:"head,omitempty"`       // git rev-parse HEAD of the run's work tree at hold start
	TreeClean   *bool  `json:"tree_clean,omitempty"` // git status --porcelain was empty then; absent outside a git tree
	Cwd         string `json:"cwd,omitempty"`        // the run's working directory
}

// treeState reports the git HEAD of the work tree containing dir and whether its tree is clean
// (git status --porcelain empty, ignored files excluded). Outside a git work tree, or when git
// cannot say, head is "" and clean is nil.
func treeState(dir string, env []string) (head string, clean *bool) {
	git := func(args ...string) (string, bool) {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = env
		out, err := cmd.Output()
		return strings.TrimSpace(string(out)), err == nil
	}
	head, ok := git("rev-parse", "HEAD")
	if !ok {
		return "", nil
	}
	st, ok := git("status", "--porcelain")
	if !ok {
		return head, nil
	}
	c := st == ""
	return head, &c
}

// WriteResult writes r atomically (temp + rename, 0600).
func WriteResult(path string, r Result) error {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return state.WriteFileAtomic(path, append(b, '\n'))
}
