// Package classify unwraps an argv into simple commands and applies refuse rules (spec 6.7).
package classify

import "github.com/Exploded-views/bitc/internal/config"

// Context locates the call for repo-wide checks; all paths canonical.
type Context struct {
	Cwd        string
	LogicalCwd string
	Worktree   string
}

// Check applies rules (host rules first, then repo rules, as merged) to every simple command.
func Check(rules []config.RefuseRule, argv []string, c Context) (reason string, refused bool) {
	if len(rules) == 0 {
		return "", false
	}
	cmds, _ := Unwrap(argv)
	for _, s := range cmds {
		for _, r := range rules {
			if match(r, s, c) {
				return r.Reason, true
			}
		}
	}
	return "", false
}
