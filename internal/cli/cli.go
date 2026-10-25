// Package cli dispatches bitc's subcommands. Each subcommand registers itself from its own
// file (cmd_*.go), so tasks never edit a shared table.
package cli

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"sort"
	"strings"

	"github.com/Exploded-views/bitc/internal/config"
	"github.com/Exploded-views/bitc/internal/exit"
)

// Std is the process's standard files.
type Std struct{ In, Out, Err *os.File }

// Command runs one subcommand and returns its exit code.
type Command func(ctx context.Context, g config.Flags, args []string, std Std) int

var commands = map[string]Command{}

// Register adds a subcommand; names starting "__" are hidden from usage.
func Register(name string, c Command) { commands[name] = c }

// Main parses global flags, dispatches, and turns any panic into exit 69 (spec 4).
func Main(args []string, std Std) (code int) {
	defer func() {
		if r := recover(); r != nil {
			e := exit.Internal(r)
			fmt.Fprintln(std.Err, e.Line)
			code = e.Code
		}
	}()
	if err := PlatformCheck(runtime.GOOS); err != nil {
		fmt.Fprintln(std.Err, exit.As(err).Line)
		return exit.Unavailable
	}
	g, rest, err := parseGlobal(args)
	if err != nil {
		fmt.Fprintln(std.Err, exit.As(err).Line)
		return exit.Code(err)
	}
	if len(rest) == 0 {
		fmt.Fprintln(std.Err, usage())
		return exit.Usage
	}
	cmd, ok := commands[rest[0]]
	if !ok {
		fmt.Fprintf(std.Err, "bitc: unknown command %q\n%s\n", rest[0], usage())
		return exit.Usage
	}
	return cmd(context.Background(), g, rest[1:], std)
}

// PlatformCheck is 69 off Linux (spec 1: /proc is required).
func PlatformCheck(goos string) error {
	if goos != "linux" {
		return exit.Unavailablef("bitc runs only on Linux (this is %s)", goos)
	}
	return nil
}

func parseGlobal(args []string) (config.Flags, []string, error) {
	var g config.Flags
	i := 0
	for ; i < len(args) && strings.HasPrefix(args[i], "-"); i++ {
		switch args[i] {
		case "-v":
			g.Verbose++
		case "--json":
			g.JSON = true
		case "--config", "--state-dir":
			if i+1 >= len(args) {
				return g, nil, exit.Usagef("%s needs a value", args[i])
			}
			if args[i] == "--config" {
				g.ConfigFile = args[i+1]
			} else {
				g.StateDir = args[i+1]
			}
			i++
		default:
			return g, nil, exit.Usagef("unknown global flag %s", args[i])
		}
	}
	return g, args[i:], nil
}

func usage() string {
	var names []string
	for n := range commands {
		if !strings.HasPrefix(n, "__") {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	return "usage: bitc [--config FILE] [--state-dir DIR] [-v] [--json] <" + strings.Join(names, "|") + "> ..."
}
