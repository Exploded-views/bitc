package cli

import (
	"context"

	"github.com/Exploded-views/bitc/internal/config"
	"github.com/Exploded-views/bitc/internal/provider/plugin"
)

func init() {
	// Registers the plugin adapter for every [providers.<type>] section with a plugin table.
	// __plugin-exec is the shim the adapter starts a pinned plugin through (A.2.4). On Linux it sets
	// no-new-privs and execs the open, hashed plugin file (it never returns on success); on macOS it
	// runs the private copy as a child, forwards signals and returns the plugin's exit status. It
	// is hidden.
	Register("__plugin-exec", func(_ context.Context, _ config.Flags, args []string, _ Std) int {
		return plugin.ExecShim(args)
	})
}
