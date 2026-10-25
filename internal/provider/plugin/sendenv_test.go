package plugin

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/Exploded-views/bitc/internal/config"
	"github.com/Exploded-views/bitc/internal/exit"
	"github.com/Exploded-views/bitc/internal/provider"
	"github.com/Exploded-views/bitc/internal/remote"
)

// S.2: a plugin's Caps.SendEnv is "forwarded" and Caps.MultiJob is hello's caps.multi_job.
func TestHelloMultiJobFeedsCapsAndThePluginDefaultIsForwarded(t *testing.T) {
	r := newRig(t)
	r.start()
	if c := r.p.Info().Caps; c.MultiJob || c.SendEnv != provider.EnvForwarded {
		t.Errorf("multi_job false: Caps = %+v, want MultiJob false, SendEnv forwarded", c)
	}
	t.Setenv("STUB_MULTI_JOB", "1")
	m := newRig(t)
	m.core.SendEnv = "forwarded"
	m.start()
	if c := m.p.Info().Caps; !c.MultiJob || c.SendEnv != provider.EnvForwarded {
		t.Errorf("multi_job true: Caps = %+v, want MultiJob true, SendEnv forwarded", c)
	}
}

// S.4/S.5: send_env = "forced" on a plugin whose hello declares multi_job is 78, checked after
// hello; without multi_job it is allowed.
func TestForcedOnAMultiJobPluginIsAConfigError(t *testing.T) {
	r := newRig(t)
	r.core.SendEnv = "forced"
	r.start()
	t.Setenv("STUB_MULTI_JOB", "1")
	m := newRig(t)
	m.core.SendEnv = "forced"
	wantConfigErr(t, m.startErr(), `providers.stub.send_env: "forced" is not allowed: stub reuses resources across jobs`)

	d := newRig(t)
	d.core.SendEnv = "forced"
	d.eff.HostFile = "/etc/bitc.toml"
	d.eff.Providers = map[string]*config.Provider{"stub": d.core}
	err := remote.CheckPlugins(context.Background(), d.eff, io.Discard)
	var ee *exit.Error
	if !errors.As(err, &ee) || ee.Code != 78 ||
		ee.Line != `bitc: /etc/bitc.toml: providers.stub.send_env: "forced" is not allowed: stub reuses resources across jobs` {
		t.Fatalf("config check / doctor: got %v", err)
	}
}

// S.4: whatever the level, a forced job is ineligible on a multi-job plugin (the walk moves on),
// and a plain one is not.
func TestMultiJobPluginNeverTakesAForcedJob(t *testing.T) {
	t.Setenv("STUB_MULTI_JOB", "1")
	r := newRig(t)
	r.core.SendEnv = "forwarded"
	r.start()
	mj := r.p.Info().Caps.MultiJob
	err := remote.SendEnvCheck("stub", provider.EnvForced, mj, []string{"DEPLOY_TOKEN"}, []string{"CI"})
	var ine *provider.ErrIneligible
	if !errors.As(err, &ine) ||
		ine.Why != `forced env not sent to stub (providers.stub.send_env = "forwarded"): DEPLOY_TOKEN` {
		t.Fatalf("forced job on a multi_job plugin: %v", err)
	}
	if err := remote.SendEnvCheck("stub", provider.EnvForced, mj, nil, []string{"CI"}); err != nil {
		t.Errorf("plain job on a multi_job plugin: %v", err)
	}
}
