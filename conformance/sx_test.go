package conformance

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/Exploded-views/bitc/conformance/harness"
)

func secretHost(h *harness.H) string {
	const secret = "sk-live-0123456789abcdef"
	h.HostConfig("[pools.lint]\n[providers.runpod]\napi_key = \"" + secret + "\"\ndaily_usd = 1\nimage = \"x\"\nsizes = {}\n" +
		"api_base = \"http://127.0.0.1:9\"\nproxy_domain = \"127.0.0.1:9\"\n")
	return secret
}

func TestSX1_CredentialNeverInBitcsSinks(t *testing.T) {
	h := harness.New(t)
	secret := secretHost(h)
	res := filepath.Join(h.Root, "r.json")
	r := h.Run("run", "--result", res, "lint", "--", "sh", "-c", `echo "$1" >&2; exit 2`, "_", secret)
	if !strings.Contains(r.Stderr, secret) {
		t.Fatal("the command's own stderr must pass through untouched")
	}
	for _, f := range []string{filepath.Join(h.State, "history.jsonl"), res} {
		if strings.Contains(string(readOrFail(t, f)), secret) {
			t.Fatalf("%s holds the credential", f)
		}
	}
	r = h.Run("run", "bad/name-"+secret, "--", "true")
	if strings.Contains(r.Stderr, secret) {
		t.Fatalf("bitc's own error line printed the credential: %q", r.Stderr)
	}
}

func TestSX2_NoCredentialInTheCommandsEnvOrArgv(t *testing.T) {
	h := harness.New(t)
	secret := secretHost(h)
	r := h.Run("run", "lint", "--", "sh", "-c", `env; cat /proc/$PPID/cmdline | tr '\0' ' '`)
	if strings.Contains(r.Stdout, secret) {
		t.Fatal("credential reached the command's env or bitc's argv")
	}
}
