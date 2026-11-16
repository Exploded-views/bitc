package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Exploded-views/bitc/internal/config"
	"github.com/Exploded-views/bitc/internal/exit"
	"github.com/Exploded-views/bitc/internal/provider"
	"github.com/Exploded-views/bitc/internal/remote"
)

func wantConfigErr(t *testing.T, err error, why string) {
	t.Helper()
	var ec *provider.ErrConfig
	if !errors.As(err, &ec) || ec.Why != why {
		t.Fatalf("got %T %v, want ErrConfig %q", err, err, why)
	}
}

func TestHelloAndConfigureFeedInfo(t *testing.T) {
	t.Setenv("STUB_FALLBACK", "attach")
	r := newRig(t, raw("endpoints", []any{"127.0.0.1"}), raw("arch", []any{"amd64", "arm64"}))
	r.start()
	in := r.p.Info()
	if in.Type != "stub" || !in.Caps.LiveLogs || !in.Caps.Artifacts || in.Caps.Spend || in.Caps.FallbackUntil != provider.UntilAttach ||
		len(in.Endpoints) != 1 || in.Endpoints[0] != "127.0.0.1" || len(in.Arch) != 2 || in.MaxName != 63 {
		t.Fatalf("Info = %+v", in)
	}
}

func TestConfigureReceivesTheNonCoreKeysAndTheSizesOnly(t *testing.T) {
	r := newRig(t)
	r.start()
	var got struct {
		Config map[string]any `json:"config"`
		Sizes  map[string]any `json:"sizes"`
	}
	if err := json.Unmarshal(r.read("configure.json"), &got); err != nil {
		t.Fatal(err)
	}
	if got.Config["region"] != "eu" || len(got.Config) != 1 {
		t.Errorf("config = %v, want only the non-core keys", got.Config)
	}
	if sz, _ := got.Sizes["cpu8"].(map[string]any); sz["usd_per_hour"] != 0.6 {
		t.Errorf("sizes = %v", got.Sizes)
	}
}

func TestHandshakeFailures(t *testing.T) {
	for _, c := range []struct{ env, val, want string }{
		{"STUB_HELLO_FAULT", "other-type", "plugin protocol error: hello: type"},
		{"STUB_HELLO_FAULT", "garbage", "plugin protocol error:"},
		{"STUB_HELLO_FAULT", "no-caps", "plugin protocol error: hello: protocol, type, plugin_version and caps are required"},
		{"STUB_HELLO_FAULT", "exit-before-hello", "plugin"},
	} {
		t.Setenv(c.env, c.val)
		r := newRig(t)
		wantUnavailableLocal(t, r.startErr(), c.want)
	}
}

func TestUnsupportedProtocolIsAConfigError(t *testing.T) {
	t.Setenv("STUB_HELLO_FAULT", "protocol2")
	r := newRig(t)
	wantConfigErr(t, r.startErr(), "providers.stub.plugin: speaks protocol 2, bitc supports [1]")
}

func TestConfigureClassConfigAndShortMaxNameAndTestGate(t *testing.T) {
	wantConfigErr(t, newRig(t, fault("config-error")).startErr(), `providers.stub: region "mars" is not served`)
	wantConfigErr(t, newRig(t, raw("max_name", int64(59))).startErr(), "providers.stub.plugin: max_name 59 is below 60, the shortest name bitc builds")
	r := newRig(t, raw("max_name", int64(60)))
	r.start()
	if r.p.Info().MaxName != 60 {
		t.Errorf("max_name 60 should be accepted: %+v", r.p.Info())
	}
	wantConfigErr(t, newRig(t, raw("endpoints", []any{"api.example.com"})).startErr(), `providers.stub: [test]: endpoint "api.example.com" is not a loopback literal`)
	newRig(t, raw("endpoints", []any{"127.0.0.1", "::1", "http://127.0.0.1:8080"})).start()
}

func TestHelloSecretsMustBeConfiguredAndNeverInline(t *testing.T) {
	t.Setenv("STUB_DECLARE", "token_secret")
	wantConfigErr(t, newRig(t).startErr(), `providers.stub.plugin.secret_files: "token_secret" required by the plugin`)
	r := newRig(t)
	sf := filepath.Join(r.dir, "tok")
	_ = os.WriteFile(sf, []byte("v"), 0o600)
	r.core.Plugin.SecretFiles["token_secret"] = sf
	r.core.Raw["token_secret"] = "inline"
	wantConfigErr(t, r.startErr(), "providers.stub.token_secret: secret; set it in plugin.secret_files")
	delete(r.core.Raw, "token_secret")
	if err := r.startErr(); err != nil {
		t.Fatalf("declared secret present as a file: %v", err)
	}
}

func TestFallbackUntilAttachNeedsThePluginToDeclareIt(t *testing.T) {
	r := newRig(t)
	r.core.FallbackUntil = "attach"
	wantConfigErr(t, r.startErr(), `providers.stub.fallback_until: "attach" is wider than stub allows ("resource")`)
	t.Setenv("STUB_FALLBACK", "attach")
	r2 := newRig(t)
	r2.core.FallbackUntil = "attach"
	r2.start()
}

func TestStartIsIdempotent(t *testing.T) {
	r := newRig(t)
	r.start()
	pid := r.diag().Pid
	r.start()
	if r.diag().Pid != pid {
		t.Error("a second Start spawned another process")
	}
}

func TestConfigCheckStartsEachPluginAndReportsConfigErrorsAs78(t *testing.T) {
	r := newRig(t, fault("config-error"))
	r.eff.HostFile = "/etc/bitc.toml"
	r.eff.Providers = map[string]*config.Provider{"stub": r.core}
	err := remote.CheckPlugins(context.Background(), r.eff, io.Discard)
	var ee *exit.Error
	if !errors.As(err, &ee) || ee.Code != 78 || ee.Line != `bitc: /etc/bitc.toml: providers.stub: region "mars" is not served` {
		t.Fatalf("got %v", err)
	}
	ok := newRig(t)
	ok.eff.Providers = map[string]*config.Provider{"stub": ok.core}
	if err := remote.CheckPlugins(context.Background(), ok.eff, io.Discard); err != nil {
		t.Fatalf("a working plugin: %v", err)
	}
	if !ok.exists("diag.json") {
		t.Error("config check did not spawn the plugin (A.3.1)")
	}
	dead := newRig(t)
	dead.eff.Providers = map[string]*config.Provider{"stub": dead.core}
	f, _ := os.OpenFile(dead.bin, os.O_WRONLY|os.O_APPEND, 0)
	_, _ = f.Write([]byte{0})
	f.Close()
	err = remote.CheckPlugins(context.Background(), dead.eff, io.Discard)
	if !errors.As(err, &ee) || ee.Code != 69 || !strings.Contains(ee.Line, "changed since load") {
		t.Fatalf("a plugin that cannot start is 69: %v", err)
	}
}
