package exit

import (
	"errors"
	"fmt"
	"testing"
)

func TestCodeAndLine(t *testing.T) {
	cases := []struct {
		err  error
		code int
		line string
	}{
		{nil, 0, ""},
		{Starved("lint", ""), 75, "STARVED lint"},
		{Starved("lint", "queue"), 75, "STARVED lint (queue)"},
		{Refused("lint", "use the heavy pool"), 64, "bitc: lint: refused: use the heavy pool"},
		{Usagef("unknown pool %s", "x"), 64, "bitc: unknown pool x"},
		{NoInputf("no job %s", "j"), 66, "bitc: no job j"},
		{Unavailablef("hook could not start"), 69, "bitc: hook could not start"},
		{Configf("%s: bad key", "f.toml"), 78, "bitc: f.toml: bad key"},
		{Internal("boom"), 69, "bitc: internal error: boom"},
		{errors.New("plain"), 69, ""},
		{fmt.Errorf("wrapped: %w", Starved("db", "")), 75, ""},
	}
	for _, c := range cases {
		if got := Code(c.err); got != c.code {
			t.Errorf("Code(%v) = %d, want %d", c.err, got, c.code)
		}
		if c.line != "" {
			if got := As(c.err).Line; got != c.line {
				t.Errorf("line %q, want %q", got, c.line)
			}
		}
	}
}

func TestPlainErrorIsInternalNeverTempFail(t *testing.T) {
	e := As(errors.New("disk on fire"))
	if e.Code != Unavailable || e.Outcome != "internal" || e.Line != "bitc: internal error: disk on fire" {
		t.Fatalf("got %+v", e)
	}
}

func TestGoTurnsAPanicIntoExit69(t *testing.T) {
	got := make(chan *Error, 1)
	old := panicExit
	panicExit = func(e *Error) { got <- e }
	defer func() { panicExit = old }()
	Go(func() { panic("in a goroutine") })
	e := <-got
	if e.Code != 69 || e.Line != "bitc: internal error: in a goroutine" {
		t.Fatalf("got %+v", e)
	}
}

// A Linux-only feature in a non-Linux client build is one line and 69 (docs/exit-codes.md:
// "not Linux"), never a silent no-op.
func TestLinuxOnlyIsOneLineAnd69(t *testing.T) {
	e := LinuxOnly("the remote agent", "darwin")
	if e.Code != Unavailable || e.Outcome != "unavailable" {
		t.Fatalf("code %d outcome %q", e.Code, e.Outcome)
	}
	if want := "bitc: the remote agent runs on Linux only (this is a darwin build)"; e.Line != want {
		t.Fatalf("line %q, want %q", e.Line, want)
	}
	if got := LinuxOnly("a plugin provider", "freebsd").Line; got != "bitc: a plugin provider runs on Linux only (this is a freebsd build)" {
		t.Fatalf("line %q", got)
	}
}
