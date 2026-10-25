package agent

import (
	"strconv"
	"strings"
)

const (
	MarkBoot        = "__BITC_BOOT__"
	MarkBegin       = "__BITC_BEGIN__"
	MarkExit        = "__BITC_EXIT__"
	MarkSetupFailed = "__BITC_SETUP_FAILED__"
	MarkLost        = "__BITC_LOST__"
)

// Marker is one parsed marker. For MarkSetupFailed, Arg is the step name, Class "repo" or
// "infra" ("" when missing or unparsable: the host treats that as infra) and Line the first
// error line (spec 8.4 step 4).
type Marker struct{ Kind, Arg, Class, Line string }

// FormatMarker renders "<kind> <nonce>[ <arg>]".
func FormatMarker(kind, nonce string, arg ...string) string {
	return strings.Join(append([]string{kind, nonce}, arg...), " ")
}

// ParseMarker: a marker counts only with this job's nonce (spec 8.4).
func ParseMarker(line, nonce string) (Marker, bool) {
	f := strings.Fields(strings.TrimRight(line, "\r"))
	if len(f) < 2 || f[1] != nonce || !strings.HasPrefix(line, f[0]) {
		return Marker{}, false
	}
	switch f[0] {
	case MarkBoot, MarkBegin, MarkLost:
		if len(f) == 2 {
			return Marker{Kind: f[0]}, true
		}
	case MarkExit:
		if len(f) == 3 {
			if n, err := strconv.Atoi(f[2]); err == nil && n >= 0 && n <= 255 {
				return Marker{Kind: f[0], Arg: f[2]}, true
			}
		}
	case MarkSetupFailed: // <name> [<class> [<first error line>]]
		if len(f) >= 3 {
			mk := Marker{Kind: f[0], Arg: f[2]}
			p := strings.SplitN(strings.TrimRight(line, "\r"), " ", 5)
			if len(p) >= 4 && (p[3] == "repo" || p[3] == "infra") {
				mk.Class = p[3]
			}
			if len(p) == 5 {
				mk.Line = p[4]
			}
			return mk, true
		}
	}
	return Marker{}, false
}
