package conformance

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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
	// eight assets: a tarball and a bare binary for linux and darwin on amd64 and arm64, all in
	// checksums.txt; release-check verifies every one (the agent release source still downloads
	// only the linux bare binaries)
	if !strings.Contains(y, "goos: [linux, darwin]") || !strings.Contains(y, "goarch: [amd64, arm64]") {
		t.Error(".goreleaser.yaml does not build linux and darwin on amd64 and arm64")
	}
	if !strings.Contains(mk, `test "$$n" -eq 8`) {
		t.Error("release-check does not expect 8 entries in checksums.txt")
	}
	for _, osArch := range []string{"linux_amd64", "linux_arm64", "darwin_amd64", "darwin_arm64"} {
		if !strings.Contains(mk, "tar -tzf dist/bitc_*_"+osArch+".tar.gz | grep -qx bitc") {
			t.Errorf("release-check does not check the %s tarball holds bitc", osArch)
		}
		if !strings.Contains(mk, "test -x dist/bitc_"+osArch+"_*/bitc") {
			t.Errorf("release-check does not check the %s bare binary is executable", osArch)
		}
		for _, asset := range []string{"`bitc_<version>_" + osArch + ".tar.gz`", "`bitc_<version>_" + osArch + "`"} {
			if !strings.Contains(doc, asset) {
				t.Errorf("docs/RELEASING.md does not list %s", asset)
			}
		}
	}
	if !strings.Contains(doc, "the eight assets") {
		t.Error("docs/RELEASING.md does not say release-check verifies the eight assets")
	}
	if !strings.Contains(doc, "https://github.com/Exploded-views/bitc/releases/download/v") || !strings.Contains(doc, "make release-check") {
		t.Error("docs/RELEASING.md does not name the Exploded-views/bitc release URL and make release-check")
	}
}

// relLines returns the workflow's lines with comments stripped, as the workflow lint reads them.
func relLines(text string) []string {
	var out []string
	for _, l := range strings.Split(text, "\n") {
		out = append(out, wfStripComment(l))
	}
	return out
}

// relSection returns the block under the first line at indent `ind` whose key is `key`.
func relSection(lines []string, from, to, ind int, key string) (int, []wfLine) {
	for i := from; i < to; i++ {
		if wfIndent(lines[i]) == ind && strings.HasPrefix(strings.TrimSpace(lines[i]), key+":") {
			return i, wfBlock(lines, i)
		}
	}
	return -1, nil
}

func relText(b []wfLine) string {
	var s []string
	for _, l := range b {
		s = append(s, strings.TrimSpace(l.text))
	}
	return strings.Join(s, "\n")
}

func TestREL4_ReleaseWorkflowUploadsADraftWithTheJobToken(t *testing.T) {
	b, err := os.ReadFile("../.github/workflows/release.yml")
	if err != nil {
		t.Fatal(err)
	}
	text := string(b)
	if f := checkWorkflow("release.yml", text); len(f) != 0 {
		t.Errorf("release.yml fails the workflow lint: %q", f)
	}
	lines := relLines(text)
	n := len(lines)

	// triggers: pull requests (the dry run) and pushed v* tags (the release)
	_, on := relSection(lines, 0, n, 0, "on")
	onText := relText(on)
	if !regexp.MustCompile(`(?m)^pull_request:`).MatchString(onText) {
		t.Error("release.yml does not run on pull_request")
	}
	if !regexp.MustCompile(`(?m)^tags:\s*\[\s*['"]?v\*['"]?\s*\]$`).MatchString(onText) {
		t.Errorf("release.yml has no push tags: ['v*'] trigger:\n%s", onText)
	}

	// the workflow token is read-only; contents: write sits only under the release job
	_, perms := relSection(lines, 0, n, 0, "permissions")
	if relText(perms) != "contents: read" {
		t.Errorf("top-level permissions are not exactly contents: read: %q", relText(perms))
	}
	ji, _ := relSection(lines, 0, n, 0, "jobs")
	if ji < 0 {
		t.Fatal("release.yml has no jobs")
	}
	job := func(name string) (int, int) {
		i, body := relSection(lines, ji+1, n, 2, name)
		if i < 0 {
			t.Fatalf("release.yml has no %s job", name)
		}
		if len(body) == 0 {
			t.Fatalf("the %s job is empty", name)
		}
		return i, body[len(body)-1].n + 1
	}
	ri, rend := job("release")
	di, dend := job("dry-run")
	_, rperms := relSection(lines, ri+1, rend, 4, "permissions")
	if relText(rperms) != "contents: write" {
		t.Errorf("the release job's permissions are not exactly contents: write: %q", relText(rperms))
	}
	for i, l := range lines {
		allowed := i > ri && i < rend && strings.TrimSpace(l) == "contents: write"
		if strings.Contains(l, "write") && !allowed {
			t.Errorf("release.yml:%d: a write permission outside the release job's permissions: %q", i+1, l)
		}
	}
	jobText := func(from, to int) string { return strings.Join(lines[from:to], "\n") }
	dry, rel := jobText(di, dend), jobText(ri, rend)

	// which event runs which job
	if !regexp.MustCompile(`(?m)^    if: github\.event_name == 'pull_request'$`).MatchString(dry) {
		t.Error("the dry-run job is not limited to pull_request")
	}
	if !regexp.MustCompile(`(?m)^    if: github\.event_name == 'push' && startsWith\(github\.ref, 'refs/tags/v'\)$`).MatchString(rel) {
		t.Error("the release job is not limited to a pushed v* tag")
	}

	// the dry run checks the config and builds the snapshot, publishing nothing, with no secret
	if !regexp.MustCompile(`(?m)^\s*run: goreleaser check$`).MatchString(dry) {
		t.Error("the dry-run job does not run goreleaser check")
	}
	if !regexp.MustCompile(`(?m)^\s*run: goreleaser release --snapshot --clean --skip=publish$`).MatchString(dry) {
		t.Error("the dry-run job does not run goreleaser release --snapshot --clean --skip=publish")
	}
	if strings.Contains(dry, "secrets.") || strings.Contains(dry, "permissions:") {
		t.Error("the dry-run job uses a secret or widens its permissions")
	}

	// the release: full history, the job's own token, no snapshot, no skipped publish
	if !regexp.MustCompile(`(?m)^\s*run: goreleaser release --clean$`).MatchString(rel) {
		t.Error("the release job does not run goreleaser release --clean")
	}
	if !regexp.MustCompile(`(?m)^\s*GITHUB_TOKEN: \$\{\{ secrets\.GITHUB_TOKEN \}\}$`).MatchString(rel) {
		t.Error("the release job does not pass the job's own GITHUB_TOKEN")
	}
	if len(regexp.MustCompile(`secrets\.[A-Za-z_]+`).FindAllString(text, -1)) != 1 {
		t.Error("release.yml uses a secret other than the one secrets.GITHUB_TOKEN")
	}
	if !regexp.MustCompile(`(?m)^\s*fetch-depth: 0$`).MatchString(rel) {
		t.Error("the release job does not check out the full history (fetch-depth: 0)")
	}

	// both jobs: credentials not persisted, the toolchain and tools ci.yml uses
	ci := string(must(os.ReadFile("../.github/workflows/ci.yml")))
	goVer := regexp.MustCompile(`(?m)^\s*go-version: "[^"]+"$`).FindString(ci)
	for name, j := range map[string]string{"dry-run": dry, "release": rel} {
		if c, p := strings.Count(j, "uses: actions/checkout@"), strings.Count(j, "persist-credentials: false"); c != 1 || p != 1 {
			t.Errorf("%s: %d checkouts, %d persist-credentials: false", name, c, p)
		}
		if goVer == "" || !strings.Contains(j, strings.TrimSpace(goVer)) {
			t.Errorf("%s: setup-go is not given ci.yml's %s", name, strings.TrimSpace(goVer))
		}
		if !strings.Contains(j, "make tools-ci") || !strings.Contains(j, `echo "$(go env GOPATH)/bin" >> "$GITHUB_PATH"`) {
			t.Errorf("%s: no make tools-ci with GOPATH/bin on PATH", name)
		}
		if !regexp.MustCompile(`(?m)^    timeout-minutes: [0-9]+$`).MatchString(j) {
			t.Errorf("%s: no timeout-minutes", name)
		}
		// the darwin assets are cross-compiled (cgo off) beside the linux ones: no macOS runner
		if !regexp.MustCompile(`(?m)^    runs-on: ubuntu-latest$`).MatchString(j) {
			t.Errorf("%s: does not run on ubuntu-latest", name)
		}
	}
	for _, u := range regexp.MustCompile(`uses: (actions/[a-z-]+@[0-9a-f]+)`).FindAllStringSubmatch(text, -1) {
		if !strings.Contains(ci, u[1]) {
			t.Errorf("%s is not the pin ci.yml uses", u[1])
		}
	}
	if !regexp.MustCompile(`(?m)^env:\n  GOTOOLCHAIN: local$`).MatchString(text) {
		t.Error("release.yml does not set GOTOOLCHAIN: local")
	}
	if !regexp.MustCompile(`(?m)^concurrency:$`).MatchString(text) {
		t.Error("release.yml has no concurrency group")
	}

	// the upload stays a draft for a maintainer to review and publish
	if !strings.Contains(string(must(os.ReadFile("../.goreleaser.yaml"))), "\n  draft: true\n") {
		t.Error(".goreleaser.yaml is no longer draft: true")
	}

	// RELEASING.md: the tag after the owner's OK, the workflow's draft, no personal token
	doc := strings.Join(strings.Fields(string(must(os.ReadFile("../docs/RELEASING.md")))), " ")
	for _, want := range []string{".github/workflows/release.yml", "owner's OK on the exact commit SHA", "draft release", "job's own `GITHUB_TOKEN`", "No personal token"} {
		if !strings.Contains(doc, want) {
			t.Errorf("docs/RELEASING.md lacks %q", want)
		}
	}
	for _, bad := range []string{"scripts/dev.sh goreleaser release --clean", "a `GITHUB_TOKEN` that only a maintainer holds", "nothing here runs in CI"} {
		if strings.Contains(doc, bad) {
			t.Errorf("docs/RELEASING.md still says %q", bad)
		}
	}
}
