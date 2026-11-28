package execgate

import "os/exec"

// dieWithTest is a no-op on darwin, which has no Pdeathsig: a gate that a test starts from this
// test binary may be left running if a hang ends in a -timeout panic.
func dieWithTest(c *exec.Cmd) {}
