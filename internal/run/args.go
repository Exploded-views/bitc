package run

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/Exploded-views/bitc/internal/config"
	"github.com/Exploded-views/bitc/internal/exit"
)

const runUsage = "usage: bitc run [--detach] [--name N] [--log PATH] [--wait DUR] [--units G=K]... [--local | --provider P] [--artifacts a,b] [--result PATH] <pool> [--] <cmd> [args...]"

// RunArgs are the parsed `bitc run` arguments (spec 3.1). Values stay raw; the config layer
// validates them (5.2).
type RunArgs struct {
	Detach    bool
	Name      string
	Log       string
	Wait      string
	Units     map[string]int // --units G=K (repeatable), group canonical; nil if absent
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
		case "--units":
			var v string
			if v, err = value(f); err == nil {
				err = ra.addUnits(v)
			}
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

var unitsRE = regexp.MustCompile(`^([A-Za-z0-9_-]+)=([1-9][0-9]*)$`)

// addUnits records one --units G=K. The group name is canonical; K is a positive integer with no
// leading zero. Whether the pool lists the group and K fits the limits is for the config layer.
func (ra *RunArgs) addUnits(v string) error {
	m := unitsRE.FindStringSubmatch(v)
	if m == nil {
		return exit.Usagef("--units %q must be <group>=<k>, k a positive integer\n%s", v, runUsage)
	}
	g := config.CanonName(m[1])
	k, err := strconv.Atoi(m[2])
	if err != nil {
		return exit.Usagef("--units %s: k %s is too large", g, m[2])
	}
	if _, dup := ra.Units[g]; dup {
		return exit.Usagef("--units %s given twice", g)
	}
	if ra.Units == nil {
		ra.Units = map[string]int{}
	}
	ra.Units[g] = k
	return nil
}
