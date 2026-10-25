package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/Exploded-views/bitc/internal/provider"
)

func asAttached(t *testing.T, err error) *provider.ErrAttached {
	t.Helper()
	var att *provider.ErrAttached
	if !errors.As(err, &att) {
		t.Fatalf("got %T %v, want ErrAttached", err, err)
	}
	return att
}

func TestLaunchRequestCarriesTheJobObjectAndTheDeployKeyOnlyInSecrets(t *testing.T) {
	r := newRig(t)
	r.start()
	j := r.job()
	j.DeployKey = "-----BEGIN KEY-----\nabc\n-----END KEY-----\n"
	if _, err := r.p.Launch(context.Background(), j); err != nil {
		t.Fatal(err)
	}
	var req struct {
		Method string
		Params struct {
			Job     json.RawMessage
			Secrets map[string]string
		}
	}
	if err := json.Unmarshal(r.read("launch.json"), &req); err != nil {
		t.Fatal(err)
	}
	want, _ := marshalJob(j)
	if req.Method != "launch" || canon(t, req.Params.Job) != canon(t, want) {
		t.Errorf("launch job object differs:\n%s\nwant\n%s", req.Params.Job, want)
	}
	if req.Params.Secrets["deploy_key"] != j.DeployKey || len(req.Params.Secrets) != 1 {
		t.Errorf("secrets = %v", req.Params.Secrets)
	}
	if strings.Contains(string(req.Params.Job), "KEY-----") {
		t.Error("the deploy key is inside the job object")
	}
}

func TestLaunchWithoutADeployKeySendsNoSecretsObject(t *testing.T) {
	r := newRig(t)
	r.start()
	if _, err := r.p.Launch(context.Background(), r.job()); err != nil {
		t.Fatal(err)
	}
	var req struct{ Params map[string]json.RawMessage }
	_ = json.Unmarshal(r.read("launch.json"), &req)
	if _, has := req.Params["secrets"]; has {
		t.Error("secrets present without a deploy key")
	}
}

func TestPerLaunchSecretsJoinTheRedactorBeforeTheFrameIsWritten(t *testing.T) {
	r := newRig(t, fault("launch-echo,launch-malformed"))
	r.debug = true
	r.eff.RemoteLimits.ForwardEnvForce = []string{"MY_FORCED"}
	r.start()
	j := r.job()
	j.DeployKey = "-----BEGIN KEY-----\nabc-deploy-key-body\n-----END KEY-----\n"
	j.Env = []string{"MY_FORCED=forced-value-123", "API_TOKEN=tok-value-456", "CI=1"}
	_, err := r.p.Launch(context.Background(), j)
	att := asAttached(t, err)
	secrets := []string{"abc-deploy-key-body", "forced-value-123", "tok-value-456",
		string(j.Bearer[:]), string(j.MacKey[:]), string(j.Nonce[:])}
	for _, b := range [][32]byte{j.Bearer, j.MacKey, j.Nonce} {
		secrets = append(secrets, hexs(b))
	}
	logs := r.log.String()
	for _, s := range secrets {
		if strings.Contains(logs, s) || strings.Contains(att.Err.Error(), s) {
			t.Errorf("secret %q leaked through debug stderr or the protocol-error tail", s)
		}
	}
	if !strings.Contains(logs, "bitc: stub: plugin: ") || !strings.Contains(logs, "<redacted>") {
		t.Errorf("debug stderr lacks redacted plugin lines:\n%s", logs)
	}
	if !strings.Contains(logs, `"CI=1"`) {
		t.Error("a non-secret forwarded value was redacted")
	}
}

func TestStderrIsPrintedOnlyAtDebug(t *testing.T) {
	r := newRig(t, fault("launch-echo"))
	r.start()
	if _, err := r.p.Launch(context.Background(), r.job()); err != nil {
		t.Fatal(err)
	}
	if got := r.log.String(); got != "" {
		t.Errorf("plugin stderr printed without BITC_LOG=debug: %q", got)
	}
}

func TestAmbiguousLaunchesAreAttachedAndReapedByName(t *testing.T) {
	for _, f := range []string{"launch-hang", "launch-die", "launch-malformed", "launch-unknown-class", "launch-unknown-class-id", "launch-phase-begun"} {
		t.Run(f, func(t *testing.T) {
			r := newRig(t, fault(f))
			r.start()
			j := r.job()
			_, err := r.p.Launch(context.Background(), j)
			att := asAttached(t, err)
			var tr *provider.ErrTransient
			if !att.Ambiguous || att.Unconfirmed || att.ID != j.Name || !errors.As(att.Err, &tr) || !strings.HasPrefix(tr.Why, "launch ambiguous: ") {
				t.Fatalf("got %+v (%v)", att, att.Err)
			}
			if len(r.resources()) != 0 || !strings.Contains(r.torn(), "r-"+j.Name) {
				t.Errorf("the resource was not reaped by name: %v torn %q", r.resources(), r.torn())
			}
		})
	}
}

func TestAmbiguousLaunchTimeoutNamesTheWait(t *testing.T) {
	r := newRig(t, fault("launch-hang"))
	r.start()
	_, err := r.p.Launch(context.Background(), r.job())
	att := asAttached(t, err)
	if !strings.Contains(att.Err.Error(), "launch ambiguous: launch timed out after ") {
		t.Errorf("why = %q", att.Err.Error())
	}
}

func TestAmbiguousLaunchWithAFailingListLeavesTheNameForTheSweep(t *testing.T) {
	r := newRig(t, fault("launch-transient-noid-created,list-fails"))
	r.start()
	j := r.job()
	_, err := r.p.Launch(context.Background(), j)
	att := asAttached(t, err)
	if !att.Ambiguous || !att.Unconfirmed || !strings.HasSuffix(att.Err.Error(), "; "+j.Name+" left for sweep") {
		t.Fatalf("got %+v (%v)", att, att.Err)
	}
}

func TestReapChecksTheOwnerTagBeforeTearingDown(t *testing.T) {
	r := newRig(t, fault("owner-wrong,launch-hang")) // the resource the hung launch made answers with another owner
	r.start()
	j := r.job()
	_, err := r.p.Launch(context.Background(), j)
	att := asAttached(t, err)
	if !att.Unconfirmed || r.torn() != "" {
		t.Fatalf("a resource with a foreign owner was torn down: %+v torn %q", att, r.torn())
	}
}

func TestIdLessErrorWithNothingCreatedIsAPreResourceFailure(t *testing.T) {
	for f, want := range map[string]string{"launch-transient-noid": "*provider.ErrTransient", "launch-ineligible-noid": "*provider.ErrIneligible"} {
		r := newRig(t, fault(f))
		r.start()
		_, err := r.p.Launch(context.Background(), r.job())
		var att *provider.ErrAttached
		var pre *provider.ErrPreAttach
		if err == nil || errors.As(err, &att) || errors.As(err, &pre) || typeName(err) != want {
			t.Errorf("%s: got %T %v, want a plain %s", f, err, err, want)
		}
	}
}

func TestIdLessErrorAfterWhichTheResourceExistsIsAttached(t *testing.T) {
	r := newRig(t, fault("launch-transient-noid-created"))
	r.start()
	j := r.job()
	_, err := r.p.Launch(context.Background(), j)
	att := asAttached(t, err)
	want := "launch said transient but " + j.Name + " exists"
	if !att.Ambiguous || att.Unconfirmed || att.Err.Error() != want || att.ID != "r-"+j.Name {
		t.Fatalf("got %+v (%v), want %q", att, att.Err, want)
	}
	if !strings.Contains(r.torn(), "r-"+j.Name) {
		t.Errorf("the resource was not torn down: %q", r.torn())
	}
}

func TestClassifiedErrorWithAnIdIsAttached(t *testing.T) {
	r := newRig(t, fault("launch-transient-id"))
	r.start()
	j := r.job()
	_, err := r.p.Launch(context.Background(), j)
	att := asAttached(t, err)
	var tr *provider.ErrTransient
	if att.Ambiguous || att.ID != "r-"+j.Name || !errors.As(att.Err, &tr) || tr.Why != "capacity after create" {
		t.Fatalf("got %+v (%v)", att, att.Err)
	}
}

func TestAttachedClassIsAttachedWithItsId(t *testing.T) {
	r := newRig(t, fault("launch-attached"))
	r.start()
	_, err := r.p.Launch(context.Background(), r.job())
	if att := asAttached(t, err); att.Ambiguous || att.Err.Error() != "stream dropped" {
		t.Fatalf("got %+v", att)
	}
}

func TestPreAttachIsDefiniteOnlyUnderAnEffectiveAttachBoundary(t *testing.T) {
	t.Setenv("STUB_FALLBACK", "attach")
	r := newRig(t, fault("launch-preattach"))
	r.core.FallbackUntil = "attach"
	r.start()
	j := r.job()
	_, err := r.p.Launch(context.Background(), j)
	var pre *provider.ErrPreAttach
	var tr *provider.ErrTransient
	if !errors.As(err, &pre) || pre.ID != j.Name || !errors.As(pre.Err, &tr) || tr.Why != "apt timeout" {
		t.Fatalf("under attach: got %T %v", err, err)
	}
	for _, setup := range []func(*rig){func(r *rig) {}, func(r *rig) { r.core.FallbackUntil = "resource" }} {
		r := newRig(t, fault("launch-preattach"))
		setup(r)
		r.start()
		_, err := r.p.Launch(context.Background(), r.job())
		if att := asAttached(t, err); att.Ambiguous {
			t.Errorf("under resource a pre_attach is read as attached, not ambiguous: %+v", att)
		}
	}
}

func TestPreAttachNeedsTheAssignedNameAsItsId(t *testing.T) {
	t.Setenv("STUB_FALLBACK", "attach")
	r := newRig(t, fault("launch-preattach-wrong-id"))
	r.core.FallbackUntil = "attach"
	r.start()
	_, err := r.p.Launch(context.Background(), r.job())
	if att := asAttached(t, err); !att.Ambiguous {
		t.Errorf("a pre_attach whose id is not the assigned name is a protocol error, hence ambiguous: %+v", att)
	}
}

func typeName(err error) string {
	var tr *provider.ErrTransient
	var ine *provider.ErrIneligible
	switch {
	case errors.As(err, &tr):
		return "*provider.ErrTransient"
	case errors.As(err, &ine):
		return "*provider.ErrIneligible"
	}
	return "other"
}

func hexs(b [32]byte) string {
	const d = "0123456789abcdef"
	out := make([]byte, 64)
	for i, c := range b {
		out[2*i], out[2*i+1] = d[c>>4], d[c&15]
	}
	return string(out)
}

// The old process's waiter must not remove the TMPDIR of the plugin that replaced it: the stub
// holds its stderr pipe open past its death, so the old Wait returns after the respawn is up.
func TestRespawnAfterAnAmbiguousLaunchKeepsTheNewPluginsTmpdir(t *testing.T) {
	r := newRig(t, fault("launch-hang,tmp-check"))
	r.start()
	j := r.job()
	_, err := r.p.Launch(context.Background(), j)
	att := asAttached(t, err)
	if !att.Ambiguous || att.Unconfirmed {
		t.Fatalf("got %+v (%v)", att, att.Err)
	}
	if got := strings.TrimSpace(string(r.read("tmpcheck.log"))); got != "true" {
		t.Errorf("the respawned plugin's TMPDIR probe survived the reap = %q, want true", got)
	}
}
