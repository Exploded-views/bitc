package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/Exploded-views/bitc/internal/config"
	"github.com/Exploded-views/bitc/internal/exit"
	"github.com/Exploded-views/bitc/internal/state"
	"github.com/Exploded-views/bitc/internal/status"
	"github.com/Exploded-views/bitc/internal/version"
)

func init() {
	Register("version", func(_ context.Context, g config.Flags, args []string, std Std) int {
		asJSON := g.JSON || (len(args) == 1 && args[0] == "--json")
		if asJSON {
			b, _ := json.Marshal(map[string]any{"version": version.Current(), "commit": version.Commit,
				"state_schema": state.SchemaVersion, "config_schema": 1, "release": version.Release == "true"})
			fmt.Fprintln(std.Out, string(b))
			return 0
		}
		fmt.Fprintf(std.Out, "bitc %s (commit %s) state schema %d config schema 1 release=%s\n",
			version.Current(), orNone(version.Commit), state.SchemaVersion, version.Release)
		return 0
	})
	Register("status", func(_ context.Context, g config.Flags, args []string, std Std) int {
		asJSON, pools := g.JSON, []string{}
		for _, a := range args {
			switch {
			case a == "--json": // `bitc status --json` (spec 3.3) as well as `bitc --json status`
				asJSON = true
			case strings.HasPrefix(a, "-"):
				return usageLine(std, "usage: bitc status [--json] [POOL...]")
			default:
				pools = append(pools, a)
			}
		}
		e, l, code := loadState(g, std, false)
		if code != 0 {
			return code
		}
		for _, p := range pools {
			if e.Pools[config.CanonName(p)] == nil {
				fmt.Fprintln(std.Err, exit.As(exit.Usagef("status: unknown pool %q", p)).Line)
				return exit.Usage
			}
		}
		r := status.Snapshot(e, l, pools)
		if asJSON {
			b, _ := json.MarshalIndent(r, "", "  ")
			fmt.Fprintln(std.Out, string(b))
			return 0
		}
		r.Text(std.Out)
		return 0
	})
	Register("sweep", func(ctx context.Context, g config.Flags, args []string, std Std) int {
		dry, foreign, provider := false, false, ""
		for i := 0; i < len(args); i++ {
			switch args[i] {
			case "--dry-run":
				dry = true
			case "--foreign":
				foreign = true
			case "--provider":
				if i+1 >= len(args) {
					return usageLine(std, sweepUsage)
				}
				provider, i = args[i+1], i+1
			default:
				return usageLine(std, sweepUsage)
			}
		}
		e, l, code := loadState(g, std, true)
		if code != 0 {
			return code
		}
		_ = status.Sweep(e, l, dry, std.Out)
		for _, t := range config.UnconfiguredLeftovers(e) { // nothing configured can sweep these
			fmt.Fprintln(std.Err, "bitc: remote "+config.LeftoverHint(t))
		}
		if status.RemoteSweep != nil {
			status.RemoteSweep(ctx, e, dry, provider, foreign, std.Out)
		}
		return 0
	})
	Register("doctor", func(_ context.Context, g config.Flags, args []string, std Std) int {
		remote := false
		for _, a := range args {
			if a != "--remote" {
				return usageLine(std, "usage: bitc doctor [--remote]")
			}
			remote = true
		}
		e, l, code := loadState(g, std, remote)
		if code != 0 {
			return code
		}
		failed := status.Doctor(e, l, std.Out)
		if remote && status.RemoteDoctor != nil {
			f, unusable := status.RemoteDoctor(e, std.Out)
			if unusable {
				return exit.Config
			}
			failed = f || failed
		}
		if failed {
			return exit.Unavailable
		}
		return 0
	})
}

const sweepUsage = "usage: bitc sweep [--dry-run] [--provider P] [--foreign]"

// usageLine prints a usage line and returns 64.
func usageLine(std Std, line string) int {
	fmt.Fprintln(std.Err, line)
	return exit.Usage
}

// loadState loads the config and opens the state dir; cleanup marks sweep and doctor --remote,
// whose refusal of remote providers says how to clean up (config.LoadOptions.Cleanup).
func loadState(g config.Flags, std Std, cleanup bool) (*config.Effective, state.Layout, int) {
	cwd, _ := os.Getwd()
	e, err := config.Load(config.LoadOptions{Flags: g, Env: os.Environ(), Cwd: cwd, Cleanup: cleanup})
	if err != nil {
		fmt.Fprintln(std.Err, exit.As(err).Line)
		return nil, state.Layout{}, exit.Code(err)
	}
	l, err := state.Open(e.Host.StateDir)
	if err != nil {
		fmt.Fprintln(std.Err, exit.As(err).Line)
		return nil, state.Layout{}, exit.Code(err)
	}
	return e, l, 0
}
