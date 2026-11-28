package run

import (
	"flag"
	"os"
	"os/exec"
	"testing"
)

// bypassGate is the -bypass-gate test flag: TestMain then starts every command directly, as before
// the gate existed (startFn = startDirect). It shows that the gate tests fail without the gate.
// The tests that must fail with it set are exactly these four:
// TestExecRunsNothingBeforeTheCommandIsRecorded (T1), TestAKilledSupervisorLeavesNoCommandRunning
// (T2), TestSignalToTheGroupBeforeReleaseEndsTheRun (T6) and TestFailClosedWhenTheRecordFails (T8).
// The others are not claimed: some pass under it, and two race (T9, TestCancelBeforeTheHello).
// TestUnrunnableGateBinaryIsASetupFailure also fails under it, as it must (there is no gate binary
// to be unrunnable), but it is no proof of the gate's hold, so it is not in the named set.
var bypassGate bool

func init() {
	flag.BoolVar(&bypassGate, "bypass-gate", false,
		"start commands without the exec gate; T1, T2, T6 and T8 (TestExecRunsNothingBeforeTheCommandIsRecorded, "+
			"TestAKilledSupervisorLeavesNoCommandRunning, TestSignalToTheGroupBeforeReleaseEndsTheRun, "+
			"TestFailClosedWhenTheRecordFails) must then fail")
}

// TestMain parses the flags (they are parsed after init) and sets the start seam under
// -bypass-gate. A helper process (this test binary) that gets the flag does the same.
func TestMain(m *testing.M) {
	flag.Parse()
	if bypassGate {
		startFn = startDirect
	}
	os.Exit(m.Run())
}

// startDirect is startGate without a gate: the command is the started process, already past its
// hello and its exec. It refuses a lookup error as startGate does.
func startDirect(lookup *exec.Cmd, s ExecSpec, setpgid bool) (*gate, error) {
	if lookup.Err != nil {
		return nil, lookup.Err
	}
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
