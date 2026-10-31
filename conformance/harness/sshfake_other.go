//go:build !linux

package harness

// SSHFakeGate is empty off Linux: the fake ssh host gate (internal/provider/ssh/sshfake/gate) is
// Linux-only, and every test that uses it lives in a _linux_test.go file.
var SSHFakeGate string

// BuildSSHFake builds nothing off Linux and returns an empty dir, so a TestMain that calls it
// still starts (os.RemoveAll("") is a no-op). It takes no *testing.T, so it cannot skip; the
// tests that need the gate are compiled only on Linux.
func BuildSSHFake() (dir string, err error) { return "", nil }
