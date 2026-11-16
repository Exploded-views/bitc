package cli

import (
	"context"
	"os"

	"github.com/Exploded-views/bitc/internal/config"
	"github.com/Exploded-views/bitc/internal/container"
)

func init() {
	// __container-guard <runtime> <name> is the process a containerized run starts to remove its
	// container when bitc is killed: it waits on its fd 3, bitc's lifeline pipe, and runs
	// `<runtime> rm -f <name>` when that pipe closes without a "done". It is hidden.
	Register("__container-guard", func(_ context.Context, _ config.Flags, args []string, _ Std) int {
		return container.GuardMain(args, os.NewFile(container.LifelineFD, "lifeline"))
	})
}
