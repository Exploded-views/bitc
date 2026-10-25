package remote

import (
	"errors"
	"fmt"
	"syscall"
	"testing"

	"github.com/Exploded-views/bitc/internal/exit"
	"github.com/Exploded-views/bitc/internal/provider"
)

func TestNoProviderLeftRule(t *testing.T) {
	cases := []struct {
		last error
		code int
	}{
		{&provider.ErrIneligible{Why: "x"}, 69},
		{&provider.ErrUnavailable{Why: "x"}, 69},
		{&provider.ErrTransient{Why: "x"}, 75},
		{errors.New("network"), 75},
	}
	for _, c := range cases {
		if got := exit.Code(NoProviderLeft("lint", c.last)); got != c.code {
			t.Errorf("%T: %d, want %d", c.last, got, c.code)
		}
	}
}

// The running-locally note needs local as the next provider, "trying" a remote next one (spec 8.2).
func TestNextProviderAndLastProvider(t *testing.T) {
	cases := []struct {
		list []string
		next string
		last bool
	}{
		{[]string{"fake", "local"}, "local", false},
		{[]string{"fake"}, "", true},
		{[]string{"local", "fake"}, "", true},
		{[]string{"fake", "runpod"}, "runpod", false},
		{[]string{"fake", "runpod", "local"}, "runpod", false},
	}
	for _, c := range cases {
		if got := nextProvider(c.list, "fake"); got != c.next {
			t.Errorf("nextProvider(%q): %q, want %q", c.list, got, c.next)
		}
		if got := lastProvider(c.list, "fake"); got != c.last {
			t.Errorf("lastProvider(%q): %v", c.list, got)
		}
	}
}

func TestPauseWorthy(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{&provider.ErrUnavailable{Why: "HTTP 401"}, true},
		{fmt.Errorf("create: %w", &provider.ErrUnavailable{Why: "HTTP 402"}), true},
		{&provider.ErrUnavailable{Why: "no credentials (set api_key or api_key_file)", Local: true}, false},
		{&provider.ErrTransient{Why: "no capacity"}, false},
		{&provider.ErrIneligible{Why: "dirty"}, false},
		{Interrupted{Sig: syscall.SIGTERM}, false},
	}
	for _, c := range cases {
		if _, got := pauseWorthy(c.err); got != c.want {
			t.Errorf("%v: %v, want %v", c.err, got, c.want)
		}
	}
}

// The ledger outcome: "fallback" only when nothing was created (spec 8.2, 8.3); a repo-class
// setup failure is 69, any other setup failure 75 (spec 8.4 "Host side", 15.9).
func TestEndOutcome(t *testing.T) {
	fail := errors.New("x")
	setup := func(class string) error {
		return fmt.Errorf("watch: %w", &provider.ErrSetup{Step: "deps", Class: class, Line: "npm ERR! missing script"})
	}
	cases := []struct {
		code                        int
		began, created, interrupted bool
		err                         error
		want                        string
	}{
		{3, true, true, false, nil, "3"},
		{0, false, false, false, fail, "fallback"},      // a create that left nothing
		{0, false, true, false, setup("repo"), "69"},    // the repo's own setup step failed by itself
		{0, false, true, false, setup("infra"), "75"},   // an infrastructure setup failure
		{0, false, true, false, setup(""), "75"},        // a missing class counts as infrastructure
		{0, false, true, false, setup("garbage"), "75"}, // so does an unparsable one
		{0, false, true, false, fail, "75"},             // restart or start timeout after create
		{0, true, true, false, fail, "75"},              // lost after BEGIN
		{0, false, true, true, Interrupted{Sig: syscall.SIGINT}, "interrupted"},
	}
	for _, c := range cases {
		if got := endOutcome(c.code, c.began, c.created, c.interrupted, c.err); got != c.want {
			t.Errorf("%+v: %q, want %q", c, got, c.want)
		}
	}
}
