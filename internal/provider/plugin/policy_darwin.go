//go:build darwin

package plugin

// darwinPolicy is what the darwin shim does about privilege escalation by the plugin, which macOS
// cannot express as Linux's PR_SET_NO_NEW_PRIVS.
type darwinPolicy int

const (
	darwinAccept  darwinPolicy = iota // run the plugin: the private copy has no setuid or setgid bit
	darwinRefuse                      // refuse every plugin (a config error, exit 78)
	darwinSandbox                     // run the plugin under a sandbox-exec profile
)

// darwinSetuidPolicy is the one seam for the darwin no_new_privs decision: refusing plugins, or
// adding a sandbox-exec profile, later is a change behind this variable. The default is accept.
// STUB (RED step): declared only so the tests compile; nothing reads it yet.
var darwinSetuidPolicy = darwinAccept
