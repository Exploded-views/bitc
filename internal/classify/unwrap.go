package classify

import (
	"path/filepath"
	"regexp"
	"strings"
)

// Simple is one simple command found inside an argv. Script is the script name when the
// command runs a package-manager script (npm/pnpm/yarn), else "".
type Simple struct {
	Argv   []string
	Script string
}

// Markers stand in for words bitc cannot or need not resolve literally.
const (
	pwdMarker    = "\x00PWD\x00"    // $PWD, ${PWD}, $(pwd), `pwd`
	opaqueMarker = "\x00OPAQUE\x00" // any other substitution: a path argument that is not repo-wide
)

var assignRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)

// Unwrap returns the simple commands argv runs (spec 6.7). notes are debug notes (an
// unparsable sh -c string is checked only by its leading simple command).
func Unwrap(argv []string) ([]Simple, []string) {
	var out []Simple
	var notes []string
	unwrap(normalizeLiteral(argv), 0, &out, &notes)
	return out, notes
}

func normalizeLiteral(argv []string) []string {
	out := make([]string, len(argv))
	for i, a := range argv {
		switch {
		case a == "$PWD" || a == "${PWD}" || a == "$(pwd)" || a == "`pwd`":
			out[i] = pwdMarker
		case strings.HasPrefix(a, "$PWD/"):
			out[i] = pwdMarker + a[len("$PWD"):]
		case strings.HasPrefix(a, "${PWD}/"):
			out[i] = pwdMarker + a[len("${PWD}"):]
		default:
			out[i] = a
		}
	}
	return out
}

// options: name -> takes a value. A flag not listed takes no value.
var wrapperOpts = map[string]map[string]bool{
	"env":     {"-u": true, "--unset": true, "-C": true, "--chdir": true, "-i": false, "-0": false, "--ignore-environment": false, "-v": false},
	"nice":    {"-n": true, "--adjustment": true},
	"time":    {"-f": true, "--format": true, "-o": true, "--output": true, "-p": false, "-v": false, "-a": false},
	"timeout": {"-k": true, "--kill-after": true, "-s": true, "--signal": true, "--preserve-status": false, "--foreground": false, "-v": false},
	"ionice":  {"-c": true, "--class": true, "-n": true, "--classdata": true, "-p": true, "--pid": true, "-P": true, "--pgid": true, "-u": true, "--uid": true, "-t": false},
	"stdbuf":  {"-i": true, "--input": true, "-o": true, "--output": true, "-e": true, "--error": true},
	"exec":    {"-a": true, "-c": false, "-l": false},
	"command": {"-p": false},
	"nohup":   {},
	"setsid":  {"-f": false, "-w": false, "-c": false, "--fork": false, "--wait": false, "--ctty": false},
}

var shells = map[string]bool{"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true}

func unwrap(argv []string, depth int, out *[]Simple, notes *[]string) {
	if depth > 16 {
		return
	}
	for len(argv) > 0 && assignRE.MatchString(argv[0]) {
		argv = argv[1:]
	}
	if len(argv) == 0 {
		return
	}
	base := filepath.Base(argv[0])
	switch {
	case wrapperOpts[base] != nil:
		if base == "command" && commandLookupOnly(argv[1:]) {
			return // command -v only prints
		}
		if base == "env" {
			if str, after, ok := envSplitString(argv[1:]); ok {
				cmds, err := parseShell(str)
				if err != nil {
					// spec 6.7: not parsed further, but the leading command is still checked
					*notes = append(*notes, "cannot parse the env -S string; checked its leading command only: "+err.Error())
					unwrap(leadingWords(str), depth+1, out, notes)
					return
				}
				for i, c := range cmds {
					if i == len(cmds)-1 {
						c = append(c, after...) // env -S STR ARGS: ARGS follow STR's words
					}
					unwrap(c, depth+1, out, notes)
				}
				return
			}
		}
		rest := skipOptions(argv[1:], wrapperOpts[base])
		if base == "env" {
			for len(rest) > 0 && assignRE.MatchString(rest[0]) {
				rest = rest[1:]
			}
		}
		if base == "timeout" && len(rest) > 0 {
			rest = rest[1:] // DURATION
		}
		unwrap(rest, depth+1, out, notes)
	case shells[base]:
		script, ok := shellScript(argv[1:])
		if !ok {
			*out = append(*out, Simple{Argv: argv})
			return
		}
		cmds, err := parseShell(script)
		if err != nil {
			// spec 6.7: not parsed further, but the leading command is still checked
			*notes = append(*notes, "cannot parse the sh -c string; checked its leading command only: "+err.Error())
			unwrap(leadingWords(script), depth+1, out, notes)
			return
		}
		for _, c := range cmds {
			unwrap(c, depth+1, out, notes)
		}
	case base == "npx" || base == "pnpx" || base == "bunx":
		rest, shellStr := npxArgs(argv[1:])
		if shellStr != "" {
			unwrap([]string{"sh", "-c", shellStr}, depth+1, out, notes)
			return
		}
		if len(rest) > 0 {
			rest[0] = stripVersion(rest[0])
		}
		unwrap(rest, depth+1, out, notes)
	case base == "npm" || base == "pnpm" || base == "yarn":
		pmArgs(base, argv, depth, out, notes)
	case base == "node" || base == "nodejs":
		if s := nodeScript(argv[1:]); s != nil {
			*out = append(*out, Simple{Argv: s})
			return
		}
		*out = append(*out, Simple{Argv: argv})
	default:
		cp := append([]string{stripVersion(base)}, argv[1:]...)
		*out = append(*out, Simple{Argv: cp})
	}
}

// skipOptions drops leading options (and the values of those that take one).
func skipOptions(args []string, opts map[string]bool) []string {
	for len(args) > 0 && strings.HasPrefix(args[0], "-") && args[0] != "-" {
		a := args[0]
		if a == "--" {
			return args[1:]
		}
		name, _, glued := strings.Cut(a, "=")
		takes, known := opts[name]
		if known && takes && !glued {
			if len(args) < 2 {
				return nil
			}
			args = args[2:]
			continue
		}
		if !known && len(name) > 2 && name[0] == '-' && name[1] != '-' {
			if takes, ok := opts[name[:2]]; ok && takes { // glued short option: -n10, -k5
				args = args[1:]
				continue
			}
		}
		args = args[1:]
	}
	return args
}

// shellValueOpts are shell options that take a value: `bash -o pipefail -c STR`.
var shellValueOpts = map[string]bool{"-o": true, "+o": true, "-O": true, "+O": true, "--rcfile": true, "--init-file": true}

// shellScript finds the string of `sh -c STR` (option clusters such as -lc, -ec count; the
// values of -o/+o/-O/+O/--rcfile/--init-file are skipped).
func shellScript(args []string) (string, bool) {
	sawC := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		opt := strings.HasPrefix(a, "-") || strings.HasPrefix(a, "+")
		switch {
		case shellValueOpts[a]:
			i++
		case !sawC && strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && strings.Contains(a, "c"):
			sawC = true
		case !opt && sawC:
			return a, true
		case !opt:
			return "", false
		}
	}
	return "", false
}

// envSplitString finds `env [opts] -S STR [ARGS]` (also -SSTR and --split-string=STR); STR is
// split like a shell string and ARGS follow its words.
func envSplitString(args []string) (str string, rest []string, ok bool) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-S" || a == "--split-string":
			if i+1 < len(args) {
				return args[i+1], args[i+2:], true
			}
			return "", nil, false
		case strings.HasPrefix(a, "--split-string="):
			return strings.TrimPrefix(a, "--split-string="), args[i+1:], true
		case strings.HasPrefix(a, "-S"):
			return a[2:], args[i+1:], true
		case a == "-u" || a == "--unset" || a == "-C" || a == "--chdir":
			i++
		case a == "--" || !strings.HasPrefix(a, "-") || a == "-":
			return "", nil, false // env stops at the first non-option: no -S
		}
	}
	return "", nil, false
}

// npxArgs skips the options of npx / npm exec (values of -p/--package and -w/--workspace/--prefix too).
func npxArgs(args []string) (rest []string, shellStr string) {
	for len(args) > 0 {
		a := args[0]
		switch {
		case a == "--":
			return args[1:], ""
		case a == "-c" || a == "--call":
			if len(args) > 1 {
				return nil, args[1]
			}
			return nil, ""
		case a == "-p" || a == "--package" || a == "-w" || a == "--workspace" || a == "--prefix":
			args = args[min(2, len(args)):]
		case strings.HasPrefix(a, "-"):
			args = args[1:]
		default:
			return args, ""
		}
	}
	return nil, ""
}

func stripVersion(tool string) string {
	if i := strings.LastIndex(tool, "@"); i > 0 {
		return tool[:i]
	}
	return tool
}

var npmValueOpts = map[string]bool{"--prefix": true, "-w": true, "--workspace": true, "-C": true, "--dir": true, "--filter": true, "-F": true, "--cwd": true,
	"--loglevel": true, "--cache": true, "--registry": true, "--userconfig": true, "--globalconfig": true}

// runValueOpts are the options after `run` whose value is not the script name.
var runValueOpts = map[string]bool{"-w": true, "--workspace": true, "--prefix": true, "--filter": true, "-F": true, "--dir": true, "-C": true, "--cwd": true}

// npmScriptAliases are the npm subcommands that run a package.json script.
var npmScriptAliases = map[string]bool{"test": true, "t": true, "tst": true, "start": true, "stop": true, "restart": true}

// pmArgs handles npm/pnpm/yarn: exec/x/dlx unwrap to the tool; run/run-script mark a script.
func pmArgs(pm string, argv []string, depth int, out *[]Simple, notes *[]string) {
	args := argv[1:]
	for len(args) > 0 && strings.HasPrefix(args[0], "-") {
		if npmValueOpts[args[0]] && len(args) > 1 {
			args = args[2:]
		} else {
			args = args[1:]
		}
	}
	if len(args) == 0 {
		*out = append(*out, Simple{Argv: argv})
		return
	}
	sub := args[0]
	switch {
	case sub == "exec" || sub == "x" || (pm == "pnpm" && sub == "dlx") || (pm == "yarn" && sub == "dlx"):
		rest, shellStr := npxArgs(args[1:])
		if shellStr != "" {
			unwrap([]string{"sh", "-c", shellStr}, depth+1, out, notes)
			return
		}
		if len(rest) > 0 {
			rest[0] = stripVersion(rest[0])
		}
		unwrap(rest, depth+1, out, notes)
	case sub == "run" || sub == "run-script" || sub == "rum" || sub == "urn":
		rest := args[1:]
		for len(rest) > 0 && strings.HasPrefix(rest[0], "-") {
			if rest[0] == "--" {
				rest = rest[1:]
				break
			}
			if runValueOpts[rest[0]] && len(rest) > 1 {
				rest = rest[2:]
			} else {
				rest = rest[1:]
			}
		}
		s := Simple{Argv: argv}
		if len(rest) > 0 {
			s.Script = rest[0]
		}
		*out = append(*out, s)
	case (pm == "pnpm" || pm == "yarn") && !pmBuiltin[sub], pm == "npm" && npmScriptAliases[sub]:
		if sub == "t" || sub == "tst" {
			sub = "test" // npm aliases of npm test
		}
		*out = append(*out, Simple{Argv: argv, Script: sub})
	default:
		*out = append(*out, Simple{Argv: argv})
	}
}

// pmBuiltin are pnpm/yarn subcommands that are not scripts.
var pmBuiltin = map[string]bool{"install": true, "i": true, "add": true, "remove": true, "update": true, "up": true,
	"why": true, "list": true, "ls": true, "outdated": true, "publish": true, "pack": true, "link": true,
	"unlink": true, "init": true, "create": true, "store": true, "config": true, "info": true, "audit": true,
	"exec": true, "dlx": true, "run": true, "workspace": true, "workspaces": true, "version": true}

var nodeValueOpts = map[string]bool{"-r": true, "--require": true, "--import": true, "--loader": true, "--experimental-loader": true, "-C": true, "--conditions": true, "--title": true, "--env-file": true, "--inspect-port": true}

// nodeEvalCluster reports a short-option cluster such as -pe or -ep: node evaluates a string.
// Only the letters of node's own short flags count, so -title is not a cluster.
func nodeEvalCluster(a string) bool {
	if len(a) < 3 || a[0] != '-' || a[1] == '-' || strings.Trim(a[1:], "epic") != "" {
		return false
	}
	return strings.ContainsAny(a[1:], "ep")
}

// commandLookupOnly reports `command -v/-V` option clusters (-pv, -pV, -Vp): they only print.
func commandLookupOnly(args []string) bool {
	for _, a := range args {
		if a == "--" || !strings.HasPrefix(a, "-") || a == "-" {
			return false
		}
		if strings.Trim(a[1:], "pvV") == "" && strings.ContainsAny(a[1:], "vV") {
			return true
		}
	}
	return false
}

// nodeScript returns [tool, args...] for `node <opts> <path>/<tool>(.js)`, or nil.
func nodeScript(args []string) []string {
	for len(args) > 0 && strings.HasPrefix(args[0], "-") {
		if args[0] == "-e" || args[0] == "--eval" || args[0] == "-p" || args[0] == "--print" || args[0] == "-v" || args[0] == "--version" || nodeEvalCluster(args[0]) {
			return nil
		}
		if nodeValueOpts[args[0]] {
			args = args[min(2, len(args)):]
		} else {
			args = args[1:]
		}
	}
	if len(args) == 0 {
		return nil
	}
	tool := filepath.Base(args[0])
	for _, ext := range []string{".js", ".mjs", ".cjs"} {
		tool = strings.TrimSuffix(tool, ext)
	}
	return append([]string{tool}, args[1:]...)
}
