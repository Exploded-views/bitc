package conformance

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Exploded-views/bitc/conformance/harness"
)

func buildStamped(t *testing.T, release string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "bitc")
	c := exec.Command("go", "build", "-trimpath", "-o", bin, "-ldflags",
		"-X github.com/Exploded-views/bitc/internal/version.Version=0.9.0 -X github.com/Exploded-views/bitc/internal/version.Release="+release,
		"../cmd/bitc")
	c.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	return bin
}

func TestREL1_ReleaseStampInVersion(t *testing.T) {
	out, _ := exec.Command(buildStamped(t, "true"), "version", "--json").Output()
	if !strings.Contains(string(out), `"release":true`) || !strings.Contains(string(out), `"version":"0.9.0"`) {
		t.Fatalf("%s", out)
	}
}

func TestREL2_ReleaseSourceNeedsAReleaseBuild(t *testing.T) {
	h := harness.New(t)
	f, _ := rpRepo(t, h, "")
	// drop the [test] agent_url seam: the agent source is now "release"
	cfg := strings.Replace(string(must(os.ReadFile(h.Config))), "agent_url = \"http://127.0.0.1:9/bitc\"\n", "", 1)
	h.Write(h.Config, cfg, 0o600)
	r := h.Run("run", "heavy", "--", "echo", "x")
	if !strings.Contains(r.Stderr, `agent source "release" needs a release build`) {
		t.Fatalf("%+v", r)
	}
	if runtime.GOARCH != "amd64" {
		t.Skip("the release agent asset is linux/amd64 only")
	}
	bin := buildStamped(t, "true")
	c := exec.Command(bin, "run", "heavy", "--", "echo", "x")
	c.Env, c.Dir = h.Env, h.Cwd
	var stdout, stderr bytes.Buffer
	c.Stdout, c.Stderr = &stdout, &stderr
	_ = c.Run()
	if stdout.String() != "x\n" || strings.Contains(stderr.String(), "locally") {
		t.Fatalf("a release build did not run remotely: %q %q", stdout.String(), stderr.String())
	}
	want := "https://github.com/Exploded-views/bitc/releases/download/v0.9.0/bitc_0.9.0_linux_amd64"
	found := false
	for _, b := range f.Bodies() {
		if env, ok := b["env"].(map[string]any); ok && env["BITC_AGENT_URL"] == want {
			found = true
		}
	}
	if !found {
		t.Fatalf("no create carried the release agent URL %s", want)
	}
}

func TestREL3_ReleaseTargetsThePublicRepoAsADraft(t *testing.T) {
	y := string(must(os.ReadFile("../.goreleaser.yaml")))
	for _, want := range []string{"owner: Exploded-views\n", "name: bitc\n", "draft: true"} {
		if !strings.Contains(y, want) {
			t.Errorf(".goreleaser.yaml lacks %q", want)
		}
	}
	for _, bad := range []string{"disable: true", "name: big-ideas-tiny-computer"} {
		if strings.Contains(y, bad) {
			t.Errorf(".goreleaser.yaml still has %q", bad)
		}
	}
	mk := string(must(os.ReadFile("../Makefile")))
	if !strings.Contains(mk, "\nrelease-check:") || !strings.Contains(mk, "sha256sum -c") {
		t.Error("Makefile has no release-check target that verifies checksums.txt")
	}
	docb, err := os.ReadFile("../docs/RELEASING.md")
	if err != nil {
		t.Fatal(err)
	}
	doc := string(docb)
	if !strings.Contains(doc, "https://github.com/Exploded-views/bitc/releases/download/v") || !strings.Contains(doc, "make release-check") {
		t.Error("docs/RELEASING.md does not name the Exploded-views/bitc release URL and make release-check")
	}
}
