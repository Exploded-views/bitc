package version

import (
	"runtime/debug"
	"testing"
)

func TestCurrentPrefersTheStampedVersion(t *testing.T) {
	old := Version
	defer func() { Version = old }()
	Version = "1.2.3"
	if Current() != "1.2.3" {
		t.Fatalf("stamped version: got %q", Current())
	}
	Version = "dev"
	if c := Current(); c != "dev" {
		t.Fatalf("an unstamped test binary has no module version: %q", c)
	}
}

func TestCurrentFallsBackToTheModuleVersion(t *testing.T) {
	oldV, oldR := Version, readBuildInfo
	defer func() { Version, readBuildInfo = oldV, oldR }()
	Version = "dev"
	for _, c := range []struct{ mod, want string }{
		{"v1.4.0", "1.4.0"},
		{"v1.4.1-0.20261024-abcdef", "1.4.1-0.20261024-abcdef"},
		{"v0.0.0-20261024025127-c713b681bdc8", "dev"},       // a plain go build of an untagged checkout
		{"v0.0.0-20261024025127-c713b681bdc8+dirty", "dev"}, // the same, with local changes
		{"(devel)", "dev"},
		{"", "dev"},
	} {
		readBuildInfo = func() (*debug.BuildInfo, bool) {
			return &debug.BuildInfo{Main: debug.Module{Version: c.mod}}, true
		}
		if got := Current(); got != c.want {
			t.Errorf("module version %q: got %q, want %q", c.mod, got, c.want)
		}
	}
	readBuildInfo = func() (*debug.BuildInfo, bool) { return nil, false }
	if got := Current(); got != "dev" {
		t.Errorf("no build info: got %q", got)
	}
	Version = "2.0.0"
	readBuildInfo = func() (*debug.BuildInfo, bool) {
		return &debug.BuildInfo{Main: debug.Module{Version: "v9.9.9"}}, true
	}
	if got := Current(); got != "2.0.0" {
		t.Errorf("a stamped version wins: got %q", got)
	}
}
