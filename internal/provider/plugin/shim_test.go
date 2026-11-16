package plugin

import (
	"os"
	"strings"
	"testing"

	"github.com/Exploded-views/bitc/internal/provider/plugin/pin"
)

// doctor prints the protocol, the plugin's own version and the first 12 characters of the pin.
func TestDoctorLineNamesTheProtocolVersionAndShortPin(t *testing.T) {
	r := newRig(t)
	r.start()
	got := r.p.DoctorLine()
	if !strings.HasPrefix(got, "protocol 1, ") || !strings.HasSuffix(got, "sha256 "+pin.Short(r.core.Plugin.SHA256)) {
		t.Fatalf("doctor line %q", got)
	}
}

// The shim's pipe ends are closed by the helpers, and a spec without them (Linux has no lifeline)
// is safe to close.
func TestShimSpecClosesItsPipeEndsAndToleratesNone(t *testing.T) {
	shimSpec{}.closeChildEnds()
	shimSpec{}.closeLifeline()
	rd, wr, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	sp := shimSpec{child: rd, lifeline: wr}
	sp.closeChildEnds()
	sp.closeLifeline()
	if _, err := rd.Stat(); err == nil {
		t.Error("the child end is still open")
	}
	if _, err := wr.Stat(); err == nil {
		t.Error("the lifeline is still open")
	}
}
