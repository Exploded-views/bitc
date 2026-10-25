package cli

import (
	"context"

	"github.com/Exploded-views/bitc/internal/agent"
	"github.com/Exploded-views/bitc/internal/config"
)

func init() {
	Register("__agent", func(_ context.Context, _ config.Flags, args []string, std Std) int {
		return agent.Main(args, std.Out)
	})
}
