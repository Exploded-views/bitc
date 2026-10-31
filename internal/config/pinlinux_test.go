package config

import "testing"

// pinLinux makes Load check the config against Linux for this test: the test means Linux
// semantics (remote providers, memory admission), which the darwin platform gate refuses.
func pinLinux(t *testing.T) {
	t.Helper()
	old := platformGOOS
	platformGOOS = "linux"
	t.Cleanup(func() { platformGOOS = old })
}
