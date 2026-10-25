// Package version is stamped by the build (-ldflags -X).
package version

import (
	"runtime/debug"
	"strings"
)

var (
	Version = "dev"   // SemVer without "v" for tagged builds
	Commit  = ""      // short commit
	Release = "false" // "true" only for release-pipeline builds (spec 8.4)
)

// readBuildInfo is a seam for the tests.
var readBuildInfo = debug.ReadBuildInfo

// Current is the stamped version, else the module version a `go install ...@vX` build records,
// else "dev". A pseudo-version based on v0.0.0 is what a plain `go build` of an untagged
// checkout records; it names no release, so it counts as "dev".
func Current() string {
	if Version != "dev" {
		return Version
	}
	if bi, ok := readBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" &&
		!strings.HasPrefix(bi.Main.Version, "v0.0.0-") {
		return strings.TrimPrefix(bi.Main.Version, "v")
	}
	return "dev"
}
