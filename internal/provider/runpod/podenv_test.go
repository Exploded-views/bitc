package runpod

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Exploded-views/bitc/internal/agent"
	"github.com/Exploded-views/bitc/internal/config"
	"github.com/Exploded-views/bitc/internal/exit"
	"github.com/Exploded-views/bitc/internal/provider"
)

// podEnvSecret is a value no error, log or explain line may ever carry.
const podEnvSecret = "s3cr3t-pod-env-value"

func podEnvRaw(podEnv map[string]any) map[string]any {
	return map[string]any{"image": "debian", "pod_env": podEnv,
		"sizes": map[string]any{"cpu8": map[string]any{"cpu_type": "cpu3c", "vcpu": int64(8), "usd_per_hour": 0.24}}}
}

// wantPodEnvRefusal fails unless DecodeSettings refuses raw with an error that carries want, is
// no "unknown key" (the key must be known and refused on its merits) and never shows the value.
func wantPodEnvRefusal(t *testing.T, raw map[string]any, want string) {
	t.Helper()
	_, err := DecodeSettings(raw)
	if err == nil {
		t.Fatalf("accepted: want an error naming %q", want)
	}
	if !strings.Contains(err.Error(), want) || strings.Contains(err.Error(), "unknown key") {
		t.Fatalf("error %q does not refuse %q on its merits", err, want)
	}
	if strings.Contains(err.Error(), podEnvSecret) {
		t.Fatalf("error shows the value: %v", err)
	}
}

// One test per refused entry of the fixed list: the exact names, then one name per prefix and
// the suffix.
func TestPodEnvRefusedNames(t *testing.T) {
	for _, name := range []string{
		"PATH", "HOME", "SHELL", "USER", "PUBLIC_KEY", "JUPYTER_PASSWORD", "WGETRC", // exact
		"BITC_OWNER", "BITC_X", "RUNPOD_API_KEY", "RUNPOD_POD_ID", // owner tag, agent keys, scoped key
		"LD_PRELOAD", "SSH_AUTH_SOCK", "SSL_CERT_FILE", "GIT_SSH_COMMAND", // prefixes
		"GOFLAGS", "GOPATH", "GOOGLE_X", "GO", "CGO_ENABLED", "CURL_CA_BUNDLE",
		"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "ALL_PROXY", "X_PROXY", // suffix
	} {
		t.Run(name, func(t *testing.T) {
			wantPodEnvRefusal(t, podEnvRaw(map[string]any{name: podEnvSecret}), "pod_env."+name)
		})
	}
}

func TestPodEnvNameShape(t *testing.T) {
	for _, name := range []string{"", "hf_home", "Hf", "1A", "_A", "A-B", "A.B", "A B", "É", strings.Repeat("A", 65)} {
		t.Run(name, func(t *testing.T) {
			wantPodEnvRefusal(t, podEnvRaw(map[string]any{name: podEnvSecret}), "pod_env key "+quote(name))
		})
	}
}

func quote(s string) string { return strconv.Quote(s) }

// bodyEnv is the env of the create body a provider built from raw would send.
func bodyEnv(t *testing.T, raw map[string]any) map[string]string {
	t.Helper()
	p, err := New(&config.Provider{Type: "runpod", NamePrefix: "bitc", Raw: raw}, &config.Effective{})
	if err != nil {
		t.Fatal(err)
	}
	body, err := p.createBody(podEnvJob(), agent.Spec{Job: "j1"})
	if err != nil {
		t.Fatal(err)
	}
	return body["env"].(map[string]string)
}

func TestPodEnvAcceptedNames(t *testing.T) {
	for _, name := range []string{"A", "HF_HOME", "MY_GOAL", "PROXY", "USERNAME", "HOMEDIR", "PATHS", "GIT", "GITHUB_X", "LD", "SSH", "X9_", strings.Repeat("A", 64)} {
		raw := podEnvRaw(map[string]any{name: "v"})
		if _, err := DecodeSettings(raw); err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if env := bodyEnv(t, raw); env[name] != "v" {
			t.Errorf("%s is not in the create env: %v", name, env)
		}
	}
}

func TestPodEnvValues(t *testing.T) {
	for name, v := range map[string]any{
		"TOO_LONG":    strings.Repeat("x", 257),
		"NEWLINE":     "a\nb",
		"RETURN":      "a\rb",
		"TEMPLATE":    "{{ RUNPOD_SECRET_app_key }}",
		"HALF":        "x{{",
		"NOT_STRING":  int64(7),
		"NOT_STRING2": true,
	} {
		wantPodEnvRefusal(t, podEnvRaw(map[string]any{name: v}), "pod_env."+name)
	}
	if _, err := DecodeSettings(podEnvRaw(map[string]any{"EDGE": strings.Repeat("x", 256), "BRACE": "{ok}"})); err != nil {
		t.Fatalf("256 bytes and a single brace are fine: %v", err)
	}
	if _, err := DecodeSettings(podEnvRaw(map[string]any{"MULTI": strings.Repeat("é", 129)})); err == nil { // 258 bytes, 129 runes
		t.Fatal("the limit is bytes, not characters")
	}
	raw := podEnvRaw(nil)
	raw["pod_env"] = "A=1"
	if _, err := DecodeSettings(raw); err == nil || !strings.Contains(err.Error(), "pod_env") || strings.Contains(err.Error(), "unknown key") {
		t.Fatalf("a non-table pod_env: %v", err)
	}
}

func TestPodEnvAtMostSixteenEntries(t *testing.T) {
	m := map[string]any{}
	for i := 0; i < 16; i++ {
		m["K"+string(rune('A'+i))] = "v"
	}
	if _, err := DecodeSettings(podEnvRaw(m)); err != nil {
		t.Fatalf("16 entries: %v", err)
	}
	if env := bodyEnv(t, podEnvRaw(m)); len(env) != 7+16 {
		t.Fatalf("16 entries: %d env keys", len(env))
	}
	m["KQ"] = podEnvSecret
	wantPodEnvRefusal(t, podEnvRaw(m), "pod_env has 17 entries (at most 16)")
}

func podEnvProvider(t *testing.T, podEnv map[string]any) *Provider {
	t.Helper()
	core := &config.Provider{Type: "runpod", NamePrefix: "bitc", APIKey: "sk-test-key-0123", Raw: podEnvRaw(podEnv)}
	p, err := New(core, &config.Effective{})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func podEnvJob() *provider.JobSpec {
	return &provider.JobSpec{Job: "j1", SizeKey: "cpu8", Name: "bitc-x", Owner: "owner-tag", AgentURL: "https://example.invalid/bitc",
		AgentSHA256: strings.Repeat("a", 64), Deadline: time.Now().Add(time.Hour)}
}

func TestCreateBodyCarriesPodEnvAndNeverOverridesOwnership(t *testing.T) {
	p := podEnvProvider(t, map[string]any{"HF_HOME": "/cache/hf", "TOKEN_X": podEnvSecret})
	body, err := p.createBody(podEnvJob(), agent.Spec{Job: "j1"})
	if err != nil {
		t.Fatal(err)
	}
	env := body["env"].(map[string]string)
	if env["HF_HOME"] != "/cache/hf" || env["TOKEN_X"] != podEnvSecret {
		t.Fatalf("pod_env is not in the create env: %v", env)
	}
	if env["BITC_OWNER"] != "owner-tag" || env["BITC_PROVIDER_TYPE"] != "runpod" || env["BITC_SPEC"] == "" {
		t.Fatalf("ownership keys changed: %v", env)
	}
	if len(env) != 9 { // the seven fixed keys (no build, no deploy secret) plus the two entries
		t.Fatalf("env keys %d: %v", len(env), env)
	}
}

func TestCreateBodyWithoutPodEnvIsUnchanged(t *testing.T) {
	core := &config.Provider{Type: "runpod", NamePrefix: "bitc", Raw: podEnvRaw(nil)}
	delete(core.Raw, "pod_env")
	p, err := New(core, &config.Effective{})
	if err != nil {
		t.Fatal(err)
	}
	body, err := p.createBody(podEnvJob(), agent.Spec{Job: "j1"})
	if err != nil {
		t.Fatal(err)
	}
	if n := len(body["env"].(map[string]string)); n != 7 {
		t.Fatalf("%d env keys: %v", n, body["env"])
	}
}

// The existing 100 KiB check counts the entries: a spec that just fits alone does not with 16
// entries of 256 bytes added.
func TestPodEnvCountsAgainstTheCreateBodyLimit(t *testing.T) {
	plain := podEnvProvider(t, nil)
	full := map[string]any{}
	for i := 0; i < 16; i++ {
		full["K"+string(rune('A'+i))] = strings.Repeat("v", 256)
	}
	big := podEnvProvider(t, full)
	base, err := plain.createBody(podEnvJob(), agent.Spec{Job: "j1"})
	if err != nil {
		t.Fatal(err)
	}
	baseLen, _ := json.Marshal(base)
	pad := (100<<10 - 2000 - len(baseLen)) * 3 / 4 // BITC_SPEC is base64: 4 bytes per 3
	as := agent.Spec{Job: "j1", Env: []string{"X=" + strings.Repeat("y", pad)}}
	if _, err := plain.createBody(podEnvJob(), as); err != nil {
		t.Fatalf("the spec alone should fit: %v", err)
	}
	_, err = big.createBody(podEnvJob(), as)
	var ine *provider.ErrIneligible
	if !errors.As(err, &ine) {
		t.Fatalf("16 full entries on top should pass the limit: %v", err)
	}
}

// A pod_env refusal at load is a config error (exit 78) that names the key and never the value.
func TestPodEnvRefusalAtLoadIsExit78(t *testing.T) {
	defer config.SetPlatformForTest("linux")()
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	wt := filepath.Join(home, "wt")
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, podEnv := range map[string]string{
		"LD_PRELOAD":   `{ LD_PRELOAD = "` + podEnvSecret + `" }`,
		"BITC_OWNER":   `{ BITC_OWNER = "` + podEnvSecret + `" }`,
		"TEMPLATE_VAR": `{ TEMPLATE_VAR = "{{ RUNPOD_SECRET_` + podEnvSecret + ` }}" }`,
	} {
		host := filepath.Join(home, "bitc-"+name+".toml")
		body := "version = 1\n[host]\nexperimental_remote = true\n[providers.runpod]\ndaily_usd = 1\nimage = \"x\"\n" +
			"sizes = { cpu8 = { cpu_type = \"cpu3c\", vcpu = 8, usd_per_hour = 0.24 } }\npod_env = " + podEnv + "\n"
		if err := os.WriteFile(host, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := config.Load(config.LoadOptions{Env: []string{"HOME=" + home, "BITC_CONFIG=" + host}, Cwd: wt})
		if exit.Code(err) != exit.Config || !strings.Contains(err.Error(), "providers.runpod: pod_env."+name) || strings.Contains(err.Error(), podEnvSecret) {
			t.Errorf("%s: %v", name, err)
		}
	}
}
