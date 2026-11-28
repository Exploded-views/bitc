package execgate

import (
	"os/exec"
	"syscall"
)

// dieWithTest makes a gate that a test starts from this test binary die with the test process
// (Pdeathsig SIGKILL), so a hang that ends in a -timeout panic does not leave it running.
func dieWithTest(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
}
