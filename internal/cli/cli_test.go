package cli

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/Exploded-views/bitc/internal/config"
)

func std(t *testing.T) (Std, func() string) {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "err")
	if err != nil {
		t.Fatal(err)
	}
	return Std{In: os.Stdin, Out: os.Stdout, Err: f}, func() string {
		b, _ := os.ReadFile(f.Name())
		return string(b)
	}
}

func TestPanicIsInternalError69(t *testing.T) {
	Register("__test_panic", func(context.Context, config.Flags, []string, Std) int { panic("kaboom") })
	defer delete(commands, "__test_panic")
	s, stderr := std(t)
	if code := Main([]string{"__test_panic"}, s); code != 69 {
		t.Fatalf("code %d", code)
	}
	if got := stderr(); got != "bitc: internal error: kaboom\n" {
		t.Fatalf("stderr %q", got)
	}
}

func TestUnknownCommandAndNoCommandAre64(t *testing.T) {
	s, stderr := std(t)
	if code := Main([]string{"frobnicate"}, s); code != 64 {
		t.Fatalf("code %d", code)
	}
	if code := Main(nil, s); code != 64 {
		t.Fatalf("code %d", code)
	}
	if !strings.Contains(stderr(), "unknown command") {
		t.Fatal(stderr())
	}
}

func TestGlobalFlags(t *testing.T) {
	var got config.Flags
	Register("__test_flags", func(_ context.Context, g config.Flags, args []string, _ Std) int {
		got = g
		return len(args)
	})
	defer delete(commands, "__test_flags")
	s, _ := std(t)
	code := Main([]string{"--config", "/x.toml", "--state-dir", "/s", "-v", "-v", "--json", "__test_flags", "a", "b"}, s)
	if code != 2 || got.ConfigFile != "/x.toml" || got.StateDir != "/s" || got.Verbose != 2 || !got.JSON {
		t.Fatalf("code %d flags %+v", code, got)
	}
	if code := Main([]string{"--config"}, s); code != 64 {
		t.Fatalf("missing value: %d", code)
	}
}
