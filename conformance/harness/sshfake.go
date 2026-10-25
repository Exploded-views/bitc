package harness

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// SSHFakeGate is the fake ssh host gate (internal/provider/ssh/sshfake/gate) the conformance
// TestMain built; a host file names it with `[test] ssh_gate` and `ssh_exec = "local"`.
var SSHFakeGate string

// BuildSSHFake builds the fake gate into a new temp dir (the caller removes it).
func BuildSSHFake() (dir string, err error) {
	if dir, err = os.MkdirTemp("", "bitc-sshfake-"); err != nil {
		return "", err
	}
	SSHFakeGate = filepath.Join(dir, "sshfakegate")
	build := exec.Command("go", "build", "-trimpath", "-o", SSHFakeGate, "github.com/Exploded-views/bitc/internal/provider/ssh/sshfake/gate")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		return dir, fmt.Errorf("build sshfakegate: %w\n%s", err, out)
	}
	return dir, nil
}
