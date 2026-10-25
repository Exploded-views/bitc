package conformance

import (
	"os"
	"strings"
	"testing"

	"github.com/Exploded-views/bitc/conformance/harness"
)

func TestD3_StateDirMustBePrivate(t *testing.T) {
	h := harness.New(t)
	h.HostConfig("[pools.lint]\nslots = 1\n")
	if err := os.Mkdir(h.State, 0o755); err != nil {
		t.Fatal(err)
	}
	r := h.Run("run", "lint", "--", "true")
	if r.Code != 78 || !strings.Contains(r.Stderr, "group- or world-accessible") {
		t.Fatalf("code %d stderr %q", r.Code, r.Stderr)
	}
}
