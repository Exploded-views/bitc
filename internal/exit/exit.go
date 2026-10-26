// Package exit is bitc's exit-code contract (spec 4). Every bitc outcome that is not the
// command's own exit code is an *Error carrying its code and the exact stderr line.
package exit

import (
	"errors"
	"fmt"
	"os"
)

const (
	OK           = 0
	Usage        = 64  // EX_USAGE: fix the call
	NoInput      = 66  // EX_NOINPUT: no such job
	Unavailable  = 69  // EX_UNAVAILABLE: do not retry blindly (also internal errors)
	TempFail     = 75  // EX_TEMPFAIL: starved; wait and retry
	Config       = 78  // EX_CONFIG: fix config
	StillRunning = 124 // wait: job still running
)

// Error is a bitc outcome. Line is printed verbatim to stderr (no trailing newline);
// Outcome and Reason go to result.json (spec 4).
type Error struct {
	Code    int
	Line    string
	Outcome string
	Reason  string
}

func (e *Error) Error() string { return e.Line }

func newf(code int, outcome, format string, a ...any) *Error {
	msg := fmt.Sprintf(format, a...)
	return &Error{Code: code, Line: "bitc: " + msg, Outcome: outcome, Reason: msg}
}

func Usagef(format string, a ...any) *Error   { return newf(Usage, "refused", format, a...) }
func NoInputf(format string, a ...any) *Error { return newf(NoInput, "unavailable", format, a...) }
func Unavailablef(format string, a ...any) *Error {
	return newf(Unavailable, "unavailable", format, a...)
}
func Configf(format string, a ...any) *Error { return newf(Config, "config", format, a...) }

// LinuxOnly is the one refusal of a Linux-only feature in a non-Linux client build: 69, like
// "not Linux" (docs/exit-codes.md). feature takes a singular verb ("the remote agent").
func LinuxOnly(feature, goos string) *Error {
	return Unavailablef("%s runs on Linux only (this is a %s build)", feature, goos)
}

// Starved is 75: "STARVED <pool>" or "STARVED <pool> (<reason>)".
func Starved(pool, reason string) *Error {
	line := "STARVED " + pool
	if reason != "" {
		line += " (" + reason + ")"
	}
	return &Error{Code: TempFail, Line: line, Outcome: "starved", Reason: reason}
}

// Refused is 64 from a refuse rule or a hook: "bitc: <pool>: refused: <reason>".
func Refused(pool, reason string) *Error {
	return &Error{Code: Usage, Line: "bitc: " + pool + ": refused: " + reason, Outcome: "refused", Reason: reason}
}

// Internal is a bitc bug: 69, never 75 (a bug that fails every time must not loop a
// retrying agent) and never Go's 2.
func Internal(v any) *Error {
	msg := fmt.Sprint(v)
	return &Error{Code: Unavailable, Line: "bitc: internal error: " + msg, Outcome: "internal", Reason: msg}
}

// Code maps an error to bitc's exit code: nil 0, *Error its code, anything else 69.
func Code(err error) int {
	if err == nil {
		return OK
	}
	return As(err).Code
}

// As returns the *Error inside err, or err wrapped as an internal error.
func As(err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return Internal(err)
}

// panicExit ends the process for a panic in a goroutine; tests replace it.
var panicExit = func(e *Error) {
	fmt.Fprintln(os.Stderr, e.Line)
	os.Exit(e.Code)
}

// Go starts fn in a goroutine whose panic ends bitc with 69 instead of Go's exit 2.
// Every goroutine in bitc is started through Go (guard_test.go enforces it).
func Go(fn func()) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				panicExit(Internal(r))
			}
		}()
		fn()
	}()
}
