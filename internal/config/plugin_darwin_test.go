//go:build darwin

// darwin: a plugin carrying com.apple.quarantine is a config error (78) that says how to clear it.

package config

import (
	"testing"

	"golang.org/x/sys/unix"
)

func TestQuarantinedPluginIsRefusedAtLoadWith78(t *testing.T) {
	pl := newPlug(t, "modal")
	if err := unix.Setxattr(pl.path, "com.apple.quarantine", []byte("0083;00000000;Safari;"), 0); err != nil {
		t.Fatalf("cannot set com.apple.quarantine: %v", err)
	}
	_, err := newFixture(t, pluginHost("modal", pl, "", ""), "").load()
	expect78(t, err, "providers.modal.plugin.path: ", "com.apple.quarantine", "xattr -d com.apple.quarantine")
}
