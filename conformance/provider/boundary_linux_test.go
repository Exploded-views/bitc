// Linux-only: runs the provider suite on the ssh fake (sshTarget), which does not build elsewhere.

package provider

import (
	"testing"

	"github.com/Exploded-views/bitc/conformance/harness"
)

// PS1: list completeness with a paginated list and five overlapping jobs.
func TestPS1_ListCompleteness(t *testing.T) {
	for _, tg := range []Target{&fakeTarget{}, &runpodTarget{}, &pluginTarget{}, &sshTarget{slots: 5}} {
		t.Run(tg.Name(), func(t *testing.T) {
			h := harness.New(t)
			tg.Configure(t, h)
			RunPS1(t, tg, h)
		})
	}
}
