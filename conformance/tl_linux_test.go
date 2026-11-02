// Linux-only: a remote provider runs on Linux only (the RunPod fake is driven by the Linux agent).

package conformance

import (
	"strings"
	"testing"

	"github.com/Exploded-views/bitc/conformance/harness"
)

func TestTL5_ARemoteRunIgnoresTheTimeoutFlag(t *testing.T) {
	h := harness.New(t)
	f, _ := rpRepo(t, h, "")
	r := h.Run("run", "--timeout", "1s", "heavy", "--", "echo", "x")
	if r.Code != 0 || r.Stdout != "x\n" || calls(f, "POST /v2/pods") != 1 || strings.Contains(r.Stderr, "time limit") {
		t.Fatalf("%+v %v", r, f.Calls())
	}
}
