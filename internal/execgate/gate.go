// Package execgate is the exec gate: bitc re-executes itself as "__exec-gate-1" so the command's
// process exists, in its own group, before the command runs. The supervisor records that process
// (pid, start time, group) in the ticket and only then releases the gate, which execs the
// command. A supervisor killed before the record leaves a gate, never a running command.
//
// The gate runs in init() when os.Args[1] is Verb and never returns. It writes nothing to stdout
// or stderr on any path it controls.
//
// Protocol, over two descriptors the supervisor passes as ExtraFiles:
//   - gatefd, the read end of a pipe: one byte releases the gate; EOF means the supervisor is
//     gone, and the gate exits 125 without running the command.
//   - statusfd, the write end of a pipe: the gate writes the Hello byte first. After the release,
//     a successful exec closes statusfd (it is close-on-exec) with nothing more written; a failed
//     exec writes the errno as 4 little-endian bytes and the gate exits 127.
//
// Signals: the gate catches QUIT only (exit 131). TERM, INT and HUP keep the Go runtime's
// default, a synchronous death by that signal, which is what the command would have done. The
// gate inherits SIG_DFL for all four, because the supervisor catches them, and the command gets
// SIG_DFL, as without a gate. Go's default for a signal nobody asked for (USR1, ALRM, WINCH and
// the like) is to drop it, so such a signal sent to the group from outside before the exec is
// swallowed; that is accepted.
//
// Known side effects: LD_PRELOAD and DYLD_* in the job's environment also load into the gate, and
// GOTRACEBACK, GODEBUG and GOMAXPROCS apply to it. A QUIT to the group from outside in the
// instant between the gate's runtime start and its handler gives a Go traceback on stderr.
package execgate

import (
	"encoding/binary"
	"errors"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/Exploded-views/bitc/internal/exit"
)

// Verb is the first argument of a gate process:
// <self> __exec-gate-1 <gatefd> <statusfd> <path> <argv...>.
const Verb = "__exec-gate-1"

// Hello is the first byte the gate writes to statusfd.
const Hello byte = 'H'

// HoldEnv and HoldAtEnv are a test-only hold. HoldEnv names a FIFO; HoldAtEnv names the stage at
// which the gate blocks reading one byte from it: HoldHello (before it writes the hello byte) or
// HoldExec (after the release byte, before the exec). A test writes a byte to let the gate go on.
// Both are honoured, and stripped from the command's environment, only in a test binary.
const (
	HoldEnv   = "BITC_EXECGATE_TEST_HOLD"
	HoldAtEnv = "BITC_EXECGATE_TEST_HOLD_AT"
)

// The stages HoldAtEnv names.
const (
	HoldHello = "hello"
	HoldExec  = "exec"
)

// Exit codes of the gate itself; none of them is the command's.
const (
	exitNotReleased = 125 // bad arguments, or EOF on gatefd: the supervisor never released the gate
	exitExecFailed  = 127 // the exec failed; the errno is on statusfd
	exitQuit        = 131 // QUIT before the exec, 128+SIGQUIT as the command would have died
)

func init() {
	if len(os.Args) > 1 && os.Args[1] == Verb {
		run(os.Args[2:])
	}
}

// run never returns.
func run(args []string) {
	if len(args) < 4 {
		syscall.Exit(exitNotReleased)
	}
	gatefd, err1 := strconv.Atoi(args[0])
	statusfd, err2 := strconv.Atoi(args[1])
	path, argv := args[2], args[3:]
	if err1 != nil || err2 != nil || gatefd < 3 || statusfd < 3 || gatefd == statusfd {
		syscall.Exit(exitNotReleased)
	}
	var st unix.Stat_t
	if unix.Fstat(gatefd, &st) != nil || st.Mode&unix.S_IFMT != unix.S_IFIFO {
		syscall.Exit(exitNotReleased)
	}
	unix.CloseOnExec(gatefd)
	unix.CloseOnExec(statusfd)
	_ = unix.SetNonblock(gatefd, false) // the read end must block until the release

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGQUIT)
	exit.Go(func() {
		<-quit
		syscall.Exit(exitQuit)
	})

	var hold, holdAt string
	if testing.Testing() {
		hold, holdAt = os.Getenv(HoldEnv), os.Getenv(HoldAtEnv)
	}
	holdStage(hold, holdAt, HoldHello)
	if !writeAll(statusfd, []byte{Hello}) {
		syscall.Exit(exitNotReleased)
	}
	var b [1]byte
	for {
		n, err := unix.Read(gatefd, b[:])
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if n != 1 || err != nil {
			syscall.Exit(exitNotReleased)
		}
		break
	}

	env := os.Environ()
	if testing.Testing() {
		env = withoutHold(env)
	}
	holdStage(hold, holdAt, HoldExec)
	err := syscall.Exec(path, argv, env) //nolint:gosec // G702: the gate execs the command the user asked bitc to run, resolved by the parent's exec.LookPath
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		errno = syscall.EINVAL
	}
	var e [4]byte
	binary.LittleEndian.PutUint32(e[:], uint32(errno))
	writeAll(statusfd, e[:])
	syscall.Exit(exitExecFailed)
}

// holdStage blocks reading one byte from the FIFO hold when at names stage; a hold that cannot be
// read ends the gate as not released.
func holdStage(hold, at, stage string) {
	if hold == "" || at != stage {
		return
	}
	var fd int
	var err error
	for { // blocks until a writer has the FIFO open
		fd, err = unix.Open(hold, unix.O_RDONLY|unix.O_CLOEXEC, 0)
		if !errors.Is(err, syscall.EINTR) {
			break
		}
	}
	if err != nil {
		syscall.Exit(exitNotReleased)
	}
	var b [1]byte
	for {
		n, err := unix.Read(fd, b[:])
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if n != 1 || err != nil {
			syscall.Exit(exitNotReleased)
		}
		break
	}
	_ = unix.Close(fd)
}

func withoutHold(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if !strings.HasPrefix(kv, HoldEnv+"=") && !strings.HasPrefix(kv, HoldAtEnv+"=") {
			out = append(out, kv)
		}
	}
	return out
}

func writeAll(fd int, b []byte) bool {
	for len(b) > 0 {
		n, err := unix.Write(fd, b)
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if err != nil || n <= 0 {
			return false
		}
		b = b[n:]
	}
	return true
}
