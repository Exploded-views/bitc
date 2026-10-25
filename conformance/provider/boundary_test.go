package provider

import (
	"testing"

	"github.com/Exploded-views/bitc/conformance/harness"
)

// FB15: nothing job-specific crosses before the attach call, for every target that declares
// attach, the reference plugin included. RunPod declares resource, so the row does not apply to it.
func TestFB15_NothingCrossesBeforeAttach(t *testing.T) {
	t.Run("fake", func(t *testing.T) { RunFB15(t, &fakeTarget{}, harness.New) })
	t.Run("plugin", func(t *testing.T) { RunFB15(t, &pluginTarget{}, harness.New) })
	t.Run("runpod", func(t *testing.T) {
		if _, ok := Target(&runpodTarget{}).(CallRecorder); ok {
			t.Fatal("RunPod must not be a CallRecorder that declares attach")
		}
	})
}

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
