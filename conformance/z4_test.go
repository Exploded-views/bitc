package conformance

import (
	"debug/macho"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The macOS client build (docs/spec.md 1): cmd/bitc cross-compiles for darwin on both arches
// with cgo off, so the release can ship it and CI on Linux catches a Linux-only call that
// leaked into a portable file.
func TestZ4_ClientCrossBuildsForDarwin(t *testing.T) {
	for arch, cpu := range map[string]macho.Cpu{"arm64": macho.CpuArm64, "amd64": macho.CpuAmd64} {
		t.Run(arch, func(t *testing.T) {
			bin := filepath.Join(t.TempDir(), "bitc")
			c := exec.Command("go", "build", "-trimpath", "-o", bin, "../cmd/bitc")
			c.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=darwin", "GOARCH="+arch)
			if out, err := c.CombinedOutput(); err != nil {
				t.Fatalf("darwin/%s: %v\n%s", arch, err, out)
			}
			f, err := macho.Open(bin)
			if err != nil {
				t.Fatalf("darwin/%s: not a Mach-O binary: %v", arch, err)
			}
			defer f.Close()
			if f.Cpu != cpu || f.Type != macho.TypeExec {
				t.Fatalf("darwin/%s: cpu %v type %v", arch, f.Cpu, f.Type)
			}
		})
	}
}

// CI cross-builds the darwin client on Linux and smoke-tests it on a GitHub-hosted macOS runner:
// build, version, a one-slot local run of true, status --json; read-only, no persisted
// credentials, bounded, with ci.yml's own pins and toolchain.
func TestZ5_CISmokesTheDarwinClient(t *testing.T) {
	text := string(must(os.ReadFile("../.github/workflows/ci.yml")))
	if f := checkWorkflow("ci.yml", text); len(f) != 0 {
		t.Errorf("ci.yml fails the workflow lint: %q", f)
	}
	lines := relLines(text)
	n := len(lines)
	ji, _ := relSection(lines, 0, n, 0, "jobs")
	if ji < 0 {
		t.Fatal("ci.yml has no jobs")
	}
	job := func(name string) string {
		i, body := relSection(lines, ji+1, n, 2, name)
		if i < 0 || len(body) == 0 {
			t.Fatalf("ci.yml has no %s job", name)
		}
		return strings.Join(lines[i:body[len(body)-1].n+1], "\n")
	}
	ci, mac := job("ci"), job("macos-smoke")
	for _, arch := range []string{"arm64", "amd64"} {
		if !regexp.MustCompile(`(?m)^\s*run: env CGO_ENABLED=0 GOOS=darwin GOARCH=` + arch + ` go build -trimpath -o /dev/null \./cmd/bitc$`).MatchString(ci) {
			t.Errorf("the ci job does not cross-build darwin/%s", arch)
		}
	}
	for _, re := range []string{
		`(?m)^    runs-on: macos-latest$`,
		`(?m)^    timeout-minutes: 15$`,
		`(?m)^    permissions:\n      contents: read$`,
		`(?m)^\s*persist-credentials: false$`,
		`(?m)^\s*run: go build -o bitc \./cmd/bitc$`,
		`(?m)^\s*\./bitc version$`,
		`(?m)^\s*\./bitc run smoke -- true$`,
		`(?m)^\s*\./bitc status --json`,
		`(?m)^\s*BITC_CONFIG:`,
	} {
		if !regexp.MustCompile(re).MatchString(mac) {
			t.Errorf("macos-smoke lacks %s", re)
		}
	}
	goVer := regexp.MustCompile(`(?m)^\s*go-version: "[^"]+"$`).FindString(ci)
	if goVer == "" || !strings.Contains(mac, strings.TrimSpace(goVer)) {
		t.Errorf("macos-smoke: setup-go is not given the ci job's %s", strings.TrimSpace(goVer))
	}
	for _, u := range regexp.MustCompile(`uses: (actions/[a-z-]+@[0-9a-f]+)`).FindAllStringSubmatch(mac, -1) {
		if !strings.Contains(ci, u[1]) {
			t.Errorf("%s is not the pin the ci job uses", u[1])
		}
	}
	if strings.Contains(mac, "secrets.") {
		t.Error("macos-smoke uses a secret")
	}
}
