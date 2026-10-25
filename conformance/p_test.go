package conformance

import (
	"testing"

	"github.com/Exploded-views/bitc/conformance/harness"
)

func TestP1_ExitCodePassThrough(t *testing.T) {
	h := harness.New(t)
	h.HostConfig("[pools.lint]\nslots = 1\n")
	for _, c := range []struct {
		script string
		code   int
	}{{"exit 0", 0}, {"exit 3", 3}, {"exit 75", 75}, {"kill -TERM $$", 143}} {
		r := h.Run("run", "lint", "--", "sh", "-c", c.script)
		if r.Code != c.code {
			t.Errorf("%q: code %d, want %d (stderr %q)", c.script, r.Code, c.code, r.Stderr)
		}
	}
}
