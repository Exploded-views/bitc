package spritesfake

import (
	"testing"
	"time"

	"github.com/Exploded-views/bitc/internal/agent"
	"github.com/Exploded-views/bitc/internal/agent/agenttest"
)

// Job is a boot payload for a job that runs script (as `sh ../run.sh` from the work tree's sub
// directory) in a local git repository, for the exec delivery of `--boot-stdin` (spec port -1,
// no listener). The fake points the spec at the Sprite's overlay when it delivers it.
type Job struct {
	ID    string // the job id (the results directory name)
	Frame []byte // the framed boot payload, as sent on exec stdin
	Key   string // the deploy key inside it (empty unless asked for)
}

// NewJob builds a Job with the id "j1". deployKey, when non-empty, rides in the payload like a
// real one.
func NewJob(t testing.TB, script, deployKey string) Job {
	t.Helper()
	return NewJobNamed(t, "j1", script, deployKey)
}

// NewJobNamed is NewJob with a job id of your choice (two jobs on one Sprite need two).
func NewJobNamed(t testing.TB, id, script, deployKey string) Job {
	t.Helper()
	tt, ok := t.(*testing.T)
	if !ok {
		t.Fatal("spritesfake.NewJob needs a *testing.T")
	}
	fx := agenttest.NewFixture(tt, script)
	fx.Spec.Port = -1
	fx.Spec.Job = id
	enc, err := fx.Spec.Encode()
	if err != nil {
		t.Fatal(err)
	}
	p := agent.BootPayload{Spec: enc, Deadline: time.Now().Add(time.Hour).Unix(), DeployKey: deployKey, Provider: "sprites"}
	frame, err := p.Frame()
	if err != nil {
		t.Fatal(err)
	}
	return Job{ID: fx.Spec.Job, Frame: frame, Key: deployKey}
}
