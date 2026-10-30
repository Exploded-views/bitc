package cli

import (
	"context"
	"fmt"
	"os"

	"github.com/Exploded-views/bitc/internal/config"
	"github.com/Exploded-views/bitc/internal/exit"
	"github.com/Exploded-views/bitc/internal/remote"
	"github.com/Exploded-views/bitc/internal/state"
)

var configSubs = map[string]Command{}

// RegisterConfigSub adds `bitc config <name>` (hook-env registers from cmd_hookenv.go).
func RegisterConfigSub(name string, c Command) { configSubs[name] = c }

func init() {
	RegisterConfigSub("check", configCheck)
	RegisterConfigSub("explain", configExplain)
	Register("config", func(ctx context.Context, g config.Flags, args []string, std Std) int {
		if len(args) == 0 || configSubs[args[0]] == nil {
			fmt.Fprintln(std.Err, "usage: bitc config check [--repo FILE] | explain [key] | hook-env <pool> --slot N")
			return exit.Usage
		}
		return configSubs[args[0]](ctx, g, args[1:], std)
	})
}

func loadFor(g config.Flags, repo string) (*config.Effective, error) {
	cwd, _ := os.Getwd()
	return config.Load(config.LoadOptions{Flags: g, Env: os.Environ(), Cwd: cwd, RepoFile: repo})
}

func configCheck(ctx context.Context, g config.Flags, args []string, std Std) int {
	repo := ""
	if len(args) == 2 && args[0] == "--repo" {
		repo = args[1]
	} else if len(args) != 0 {
		fmt.Fprintln(std.Err, "usage: bitc config check [--repo FILE]")
		return exit.Usage
	}
	e, err := loadFor(g, repo)
	if err != nil {
		fmt.Fprintln(std.Err, exit.As(err).Line)
		return exit.Code(err)
	}
	for _, w := range e.Warnings {
		fmt.Fprintln(std.Err, "bitc: "+w)
	}
	if err := state.Check(e.Host.StateDir); err != nil {
		fmt.Fprintln(std.Err, exit.As(err).Line)
		return exit.Code(err)
	}
	if err := remote.CheckPlugins(ctx, e, std.Err); err != nil { // spawns each plugin: hello, configure (providers delta A.3.1)
		fmt.Fprintln(std.Err, exit.As(err).Line)
		return exit.Code(err)
	}
	fmt.Fprintf(std.Out, "ok host %s repo %s\n", orNone(e.HostFile), orNone(e.RepoFile))
	return 0
}

func configExplain(_ context.Context, g config.Flags, args []string, std Std) int {
	if len(args) > 1 {
		fmt.Fprintln(std.Err, "usage: bitc config explain [key]")
		return exit.Usage
	}
	e, err := loadFor(g, "")
	if err != nil {
		fmt.Fprintln(std.Err, exit.As(err).Line)
		return exit.Code(err)
	}
	key := ""
	if len(args) == 1 {
		key = args[0]
	}
	for _, l := range config.Explain(e, key) {
		fmt.Fprintln(std.Out, l)
	}
	return 0
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}
