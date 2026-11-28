package run

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/Exploded-views/bitc/internal/execgate"
	"github.com/Exploded-views/bitc/internal/exit"
)

// The command is started through an exec gate (internal/execgate): bitc re-executes itself, and
// the gate execs the command only after Exec has recorded the process in the ticket and released
// it. A supervisor killed before the record leaves a blocked gate, never a running command.

// gateResult is how the exec ended: errno 0 means the exec happened.
type gateResult struct{ errno syscall.Errno }

// gate is a started exec gate.
type gate struct {
	cmd     *exec.Cmd
	release *os.File        // the gate pipe's write end
	hello   chan bool       // true once the gate has said hello, false if it ended without
	result  chan gateResult // after the hello: the exec's outcome, sent when the status pipe ends
}

// selfExeFn is selfExe, a seam for a test that points the gate at a path that cannot run. A test
// that sets it must not use t.Parallel.
var selfExeFn = selfExe

// bypassGate, set only by a test (the -bypass-gate test flag), starts the command directly, as
// before the gate existed: no gate, no release, no hold. The gate tests must fail with it set;
// that is how they are shown to fail without the fix.
var bypassGate bool

// startDirect is startGate with bypassGate: the command is the started process, already past its
// hello and its exec.
func startDirect(lookup *exec.Cmd, s ExecSpec, setpgid bool) (*gate, error) {
	c := &exec.Cmd{Path: lookup.Path, Args: s.Argv, Env: s.Env, Dir: s.Dir, SysProcAttr: childAttr(setpgid)}
	c.Stdin, c.Stdout, c.Stderr = s.Stdin, s.Stdout, s.Stderr
	if err := c.Start(); err != nil {
		return nil, err
	}
	null, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0) // the release has nowhere to go
	if err != nil {
		_ = c.Process.Kill()
		_ = c.Wait()
		return nil, err
	}
	gt := &gate{cmd: c, release: null, hello: make(chan bool, 1), result: make(chan gateResult, 1)}
	gt.hello <- true
	gt.result <- gateResult{}
	return gt, nil
}

// setupError is a failure to set the gate up, as against a failure to start the command.
type setupError struct{ error }

func (e setupError) Unwrap() error { return e.error }

// inheritedFds lists the descriptors from 3 up that are open without FD_CLOEXEC: exactly the ones
// a plain fork/exec would pass on to the command.
func inheritedFds() ([]int, error) {
	d, err := os.Open(fdDir)
	if err != nil {
		return nil, err
	}
	names, err := d.Readdirnames(-1)
	d.Close() // before the probes: its own descriptor is then gone
	if err != nil {
		return nil, err
	}
	var out []int
	for _, n := range names {
		k, err := strconv.Atoi(n)
		if err != nil || k < 3 {
			continue
		}
		flags, err := unix.FcntlInt(uintptr(k), unix.F_GETFD, 0)
		if err != nil || flags&unix.FD_CLOEXEC != 0 { // not open (EBADF), or not inherited
			continue
		}
		out = append(out, k)
	}
	sort.Ints(out)
	return out, nil
}

// startGate starts the gate for the command lookup resolved. The gate's descriptors sit above
// every inherited one, so the command sees the same descriptors as it would without a gate.
// It is called from the goroutine that holds its OS thread for Pdeathsig.
func startGate(lookup *exec.Cmd, s ExecSpec, setpgid bool) (*gate, error) {
	if lookup.Err != nil { // today's answer, e.g. "executable file not found in $PATH"
		return nil, lookup.Err
	}
	if bypassGate {
		return startDirect(lookup, s, setpgid)
	}
	self, err := selfExeFn()
	if err != nil {
		return nil, setupError{fmt.Errorf("the exec gate: %w", err)}
	}
	gateR, gateW, err := os.Pipe()
	if err != nil {
		return nil, setupError{fmt.Errorf("the exec gate: %w", err)}
	}
	statR, statW, err := os.Pipe()
	if err != nil {
		gateR.Close()
		gateW.Close()
		return nil, setupError{fmt.Errorf("the exec gate: %w", err)}
	}
	closeAll := func(files ...*os.File) {
		for _, f := range files {
			f.Close()
		}
	}
	// The listing is taken as late as possible. Each inherited descriptor k is passed as a
	// CLOEXEC dup at ExtraFiles slot k-3, never as os.NewFile(k): a *File owning bitc's own
	// descriptor would close it when collected. The child's dup2 puts the same open file at k,
	// without CLOEXEC, which is what the command gets today.
	kept, err := inheritedFds()
	if err != nil {
		closeAll(gateR, gateW, statR, statW)
		return nil, setupError{fmt.Errorf("the exec gate: list descriptors: %w", err)}
	}
	top := 2
	for _, k := range kept {
		top = max(top, k)
	}
	extra := make([]*os.File, top) // slot i is descriptor 3+i; unused slots stay nil and are closed
	var dups []*os.File
	for _, k := range kept {
		d, err := unix.FcntlInt(uintptr(k), unix.F_DUPFD_CLOEXEC, top+3)
		if err != nil {
			closeAll(append(dups, gateR, gateW, statR, statW)...)
			return nil, setupError{fmt.Errorf("the exec gate: keep descriptor %d: %w", k, err)}
		}
		f := os.NewFile(uintptr(d), "inherited descriptor")
		extra[k-3] = f
		dups = append(dups, f)
	}
	extra[top-2], extra[top-1] = gateR, statW // descriptors top+1 and top+2: 3 and 4 if none are inherited
	args := append([]string{self, execgate.Verb, strconv.Itoa(top + 1), strconv.Itoa(top + 2), lookup.Path}, s.Argv...)
	g := &exec.Cmd{Path: self, Args: args, Env: s.Env, Dir: s.Dir, ExtraFiles: extra, SysProcAttr: childAttr(setpgid)}
	g.Stdin, g.Stdout, g.Stderr = s.Stdin, s.Stdout, s.Stderr
	err = g.Start()
	// os/exec closes only what it made itself: our copies of the child's ends and the dups go now,
	// or the status pipe never ends.
	closeAll(append(dups, gateR, statW)...)
	if err != nil {
		closeAll(gateW, statR)
		var pe *fs.PathError
		if errors.As(err, &pe) && pe.Op == "fork/exec" {
			// The child names the gate binary for a chdir failure too, so a bad Dir is told apart first.
			if pe.Path == self && dirUsable(s.Dir) { // the gate binary itself cannot run: bitc's failure, not the command's
				return nil, setupError{fmt.Errorf("the exec gate: %w", err)}
			}
			err = &fs.PathError{Op: "fork/exec", Path: lookup.Path, Err: pe.Err} // name the command, never bitc
		}
		return nil, err
	}
	gt := &gate{cmd: g, release: gateW, hello: make(chan bool, 1), result: make(chan gateResult, 1)}
	exit.Go(func() { // the one reader of the status pipe
		defer statR.Close()
		var h [1]byte
		if _, err := io.ReadFull(statR, h[:]); err != nil || h[0] != execgate.Hello {
			gt.hello <- false
			return
		}
		gt.hello <- true
		var e [4]byte
		if _, err := io.ReadFull(statR, e[:]); err != nil { // EOF: the exec closed the pipe
			gt.result <- gateResult{}
			return
		}
		gt.result <- gateResult{errno: syscall.Errno(binary.LittleEndian.Uint32(e[:]))}
	})
	return gt, nil
}

// dirUsable reports whether the command's working directory (empty: bitc's own) can be entered.
func dirUsable(dir string) bool {
	if dir == "" {
		return true
	}
	fi, err := os.Stat(dir)
	return err == nil && fi.IsDir() && unix.Access(dir, unix.X_OK) == nil
}

// releaseNow lets the gate exec the command; a gate that is already gone is not an error.
func (g *gate) releaseNow() {
	_, _ = g.release.Write([]byte{1})
	_ = g.release.Close()
}

// startCode is the exit code of a command that could not start.
func startCode(err error) int {
	if errors.Is(err, fs.ErrPermission) || errors.Is(err, syscall.EACCES) || errors.Is(err, syscall.ENOEXEC) {
		return 126
	}
	return 127
}
