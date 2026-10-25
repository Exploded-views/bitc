package cli

import (
	"context"
	"fmt"
	"os"
	"strconv"

	"github.com/Exploded-views/bitc/internal/admit"
	"github.com/Exploded-views/bitc/internal/config"
	"github.com/Exploded-views/bitc/internal/exit"
	"github.com/Exploded-views/bitc/internal/run"
)

func init() {
	RegisterConfigSub("hook-env", func(ctx context.Context, g config.Flags, args []string, std Std) int {
		if len(args) != 3 || args[1] != "--slot" {
			fmt.Fprintln(std.Err, "usage: bitc config hook-env <pool> --slot N")
			return exit.Usage
		}
		slot, err := config.ParseInt(args[2])
		if err != nil || slot < 1 {
			fmt.Fprintf(std.Err, "bitc: --slot %q must be 1..N\n", args[2])
			return exit.Usage
		}
		e, err := loadFor(g, "")
		if err != nil {
			fmt.Fprintln(std.Err, exit.As(err).Line)
			return exit.Code(err)
		}
		pool, err := e.Resolve(args[0])
		if err != nil {
			fmt.Fprintln(std.Err, exit.As(err).Line)
			return exit.Code(err)
		}
		if int(slot) > pool.Slots || pool.Hooks.Env == nil {
			fmt.Fprintf(std.Err, "bitc: pool %s has %d slots and %s env hook\n", pool.Name, pool.Slots, map[bool]string{true: "an", false: "no"}[pool.Hooks.Env != nil])
			return exit.Usage
		}
		hold := &admit.Hold{Pool: pool.Name, Slot: int(slot), Ticket: "hook-env", Token: "hook-env"}
		vars := run.TemplateVars(e, pool, hold, "hook-env")
		in := run.EnvInput{Caller: os.Environ(), Eff: e, Pool: pool, Hold: hold, Job: "hook-env", Vars: vars}
		env, err := run.BaseEnv(in)
		if err == nil {
			var kv []run.KV
			kv, err = run.RunHook(ctx, run.HookCall{Kind: run.EnvHook, Hook: *pool.Hooks.Env, Pool: pool.Name, Vars: vars, Env: env, Worktree: e.Worktree, Stderr: std.Err})
			for _, p := range kv {
				fmt.Fprintf(std.Out, "%s=%s\n", p.Name, strconv.Quote(p.Value))
			}
		}
		if err != nil {
			fmt.Fprintln(std.Err, exit.As(err).Line)
			return exit.Code(err)
		}
		return 0
	})
}
