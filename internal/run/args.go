package run

import (
	"strings"

	"github.com/Exploded-views/bitc/internal/exit"
)

const runUsage = "usage: bitc run [--detach] [--name N] [--log PATH] [--wait DUR] [--local | --provider P] [--artifacts a,b] [--result PATH] <pool> [--] <cmd> [args...]"

// RunArgs are the parsed `bitc run` arguments (spec 3.1). Values stay raw; the config layer
// validates them (5.2).
type RunArgs struct {
	Detach    bool
	Name      string
	Log       string
	Wait      string
	Local     bool
	Provider  string
	Artifacts *[]string
	Result    string
	Pool      string
	Argv      []string
}

// ParseRunArgs parses flags (before <pool>), the pool, an optional "--" and the argv.
func ParseRunArgs(args []string) (RunArgs, error) {
	var ra RunArgs
	i := 0
	value := func(flag string) (string, error) {
		if i+1 >= len(args) {
			return "", exit.Usagef("%s needs a value\n%s", flag, runUsage)
		}
		i++
		if args[i] == "" && flag != "--artifacts" { // an empty string never means the default (spec 5.2)
			return "", exit.Usagef("%s needs a non-empty value\n%s", flag, runUsage)
		}
		return args[i], nil
	}
	for ; i < len(args) && strings.HasPrefix(args[i], "-") && args[i] != "--"; i++ {
		var err error
		switch f := args[i]; f {
		case "--detach":
			ra.Detach = true
		case "--local":
			ra.Local = true
		case "--name":
			ra.Name, err = value(f)
		case "--log":
			ra.Log, err = value(f)
		case "--wait":
			ra.Wait, err = value(f)
		case "--provider":
			ra.Provider, err = value(f)
		case "--result":
			ra.Result, err = value(f)
		case "--artifacts":
			var v string
			if v, err = value(f); err == nil {
				list := []string{}
				for _, a := range strings.Split(v, ",") {
					if a = strings.TrimRight(a, "/"); a != "" {
						list = append(list, a)
					}
				}
				ra.Artifacts = &list
			}
		default:
			return ra, exit.Usagef("unknown flag %s\n%s", f, runUsage)
		}
		if err != nil {
			return ra, err
		}
	}
	if ra.Local && ra.Provider != "" {
		return ra, exit.Usagef("--local and --provider exclude each other")
	}
	if i >= len(args) || args[i] == "--" {
		return ra, exit.Usagef("%s", runUsage)
	}
	ra.Pool = args[i]
	i++
	if i < len(args) && args[i] == "--" {
		i++
	}
	if i >= len(args) {
		return ra, exit.Usagef("%s", runUsage)
	}
	ra.Argv = args[i:]
	return ra, nil
}
