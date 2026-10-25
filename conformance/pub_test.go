package conformance

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestPUB1_ContributorFilesExist(t *testing.T) {
	want := map[string][]string{
		"../CONTRIBUTING.md":                    {"make tools-ci", "scripts/dev.sh make ci", "behaviours.toml", "Test<ID>_", "make secrets", "provider-author-guide.md", "Releases are cut by the maintainers"},
		"../SECURITY.md":                        {"security advisor", "secret", "cap bypass", "sandbox", "forgery"},
		"../.github/ISSUE_TEMPLATE/bug.yml":     {"bitc version --json", "Exit code", "bitc doctor", "credentials"},
		"../.github/ISSUE_TEMPLATE/feature.yml": {"bitc version --json", "name:", "body:"},
		"../.github/pull_request_template.md":   {"make ci", "behaviours.toml"},
	}
	for f, subs := range want {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Errorf("%s: %v", f, err)
			continue
		}
		for _, s := range subs {
			if !strings.Contains(string(b), s) {
				t.Errorf("%s lacks %q", f, s)
			}
		}
	}
}

// pubSecretsRun runs secrets.sh with a fake gitleaks first on PATH. The fake writes the report
// named by the mode into --report-path (every pass) and exits with fakeExit.
func pubSecretsRun(t *testing.T, mode, extra string) (stdout, stderr string, code int) {
	t.Helper()
	bin := t.TempDir()
	fake := `#!/bin/sh
rp=
while [ $# -gt 0 ]; do
  case "$1" in --report-path) rp=$2; shift ;; --config) echo "$2" >>"$FAKE_CONFIGS" ;; esac
  shift
done
echo "leaks found: Secret: FAKE-SECRET-VALUE-0000 Match: FAKE-SECRET-VALUE-0000" >&2
echo "Finding: FAKE-SECRET-VALUE-0000"
case "$FAKE_MODE" in
  clean) printf '[]' >"$rp" ;;
  leak) cat >"$rp" <<'JSON'
[
 {
  "RuleID": "fake-rule",
  "Description": "FAKE-SECRET-VALUE-0000",
  "StartLine": 3,
  "Match": "key = FAKE-SECRET-VALUE-0000",
  "Secret": "FAKE-SECRET-VALUE-0000",
  "File": "dir/file.txt",
  "Commit": "0123456789abcdef0123456789abcdef01234567",
  "Author": "FAKE-SECRET-VALUE-0000"
 },
 {
  "RuleID": "second-rule",
  "Match": "FAKE-SECRET-VALUE-0000",
  "Secret": "FAKE-SECRET-VALUE-0000",
  "File": "other.txt",
  "Commit": "89abcdef0123456789abcdef0123456789abcdef"
 }
]
JSON
  ;;
  broken) echo 'not json FAKE-SECRET-VALUE-0000' >"$rp" ;;
  noreport) ;;
  crash) exit 1 ;;
esac
exit 0
`
	if err := os.WriteFile(filepath.Join(bin, "gitleaks"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	script, err := filepath.Abs("../scripts/secrets.sh")
	if err != nil {
		t.Fatal(err)
	}
	configs := filepath.Join(t.TempDir(), "configs")
	c := exec.Command("sh", script)
	c.Dir = ".."
	c.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "FAKE_MODE="+mode, "FAKE_CONFIGS="+configs)
	if extra != "" {
		c.Env = append(c.Env, "GITLEAKS_EXTRA="+extra)
	}
	var so, se strings.Builder
	c.Stdout, c.Stderr = &so, &se
	err = c.Run()
	code = 0
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("run secrets.sh: %v", err)
	}
	b, _ := os.ReadFile(configs)
	return so.String(), se.String(), code + 1000*strings.Count(string(b), "\n")
}

func TestPUB5_SecretsSummaryNeverPrintsTheSecret(t *testing.T) {
	for _, mode := range []string{"clean", "leak", "broken", "noreport"} {
		so, se, _ := pubSecretsRun(t, mode, "")
		if strings.Contains(so+se, "FAKE-SECRET-VALUE") || strings.Contains(so+se, "key =") {
			t.Errorf("mode %s leaked a secret or a match:\nstdout: %s\nstderr: %s", mode, so, se)
		}
	}
	so, _, code := pubSecretsRun(t, "leak", "")
	if code%1000 != 1 {
		t.Errorf("leak: exit %d, want 1", code%1000)
	}
	for _, want := range []string{"secrets: 2 findings", "fake-rule dir/file.txt 0123456789ab", "second-rule other.txt 89abcdef0123"} {
		if !strings.Contains(so, want) {
			t.Errorf("leak summary lacks %q:\n%s", want, so)
		}
	}
}

func TestPUB6_SecretsExitCodesFailClosed(t *testing.T) {
	cases := []struct {
		mode string
		want int
	}{{"clean", 0}, {"leak", 1}, {"broken", 2}, {"noreport", 2}, {"crash", 2}}
	for _, c := range cases {
		so, _, code := pubSecretsRun(t, c.mode, "")
		if code%1000 != c.want {
			t.Errorf("%s: exit %d, want %d (stdout %q)", c.mode, code%1000, c.want, so)
		}
	}
	if so, _, _ := pubSecretsRun(t, "clean", ""); !strings.Contains(so, "secrets: 0 findings") {
		t.Errorf("clean summary: %q", so)
	}
}

func TestPUB7_SecretsExtraPassUsesTheExtraConfig(t *testing.T) {
	extra := filepath.Join(t.TempDir(), "extra.toml")
	if err := os.WriteFile(extra, []byte("title = \"x\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, code := pubSecretsRun(t, "clean", "")
	if code != 1000 { // one pass: the repository config only
		t.Errorf("without GITLEAKS_EXTRA: %d passes/exit, want 1 pass and exit 0", code)
	}
	_, _, code = pubSecretsRun(t, "clean", extra)
	if code != 2000 {
		t.Errorf("with GITLEAKS_EXTRA: %d, want 2 passes and exit 0", code)
	}
	_, _, code = pubSecretsRun(t, "leak", extra)
	if code%1000 != 1 {
		t.Errorf("findings in the extra pass: exit %d, want 1", code%1000)
	}
	if _, _, code = pubSecretsRun(t, "clean", filepath.Join(t.TempDir(), "missing.toml")); code%1000 != 2 {
		t.Errorf("unreadable GITLEAKS_EXTRA: exit %d, want 2", code%1000)
	}
}

func TestPUB8_GitleaksWiring(t *testing.T) {
	read := func(f string) string {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Errorf("%s: %v", f, err)
		}
		return string(b)
	}
	mk := read("../Makefile")
	for _, s := range []string{"github.com/zricethezav/gitleaks/v8@v8.30.1", "\nsecrets:", "scripts/secrets.sh"} {
		if !strings.Contains(mk, s) {
			t.Errorf("Makefile lacks %q", s)
		}
	}
	if ci := regexpLine(mk, "ci:"); strings.Contains(ci, "secrets") {
		t.Errorf("make ci runs secrets: %q", ci)
	}
	cfg := read("../.gitleaks.toml")
	if !strings.Contains(cfg, "useDefault = true") || !strings.Contains(cfg, "[extend]") || !strings.Contains(cfg, "[[allowlists]]") {
		t.Errorf(".gitleaks.toml lacks extend/useDefault/allowlists:\n%s", cfg)
	}
	dev := read("../scripts/dev.sh")
	for _, s := range []string{"-e GITLEAKS_EXTRA", `-v "$GITLEAKS_EXTRA:$GITLEAKS_EXTRA:ro"`} {
		if !strings.Contains(dev, s) {
			t.Errorf("dev.sh lacks %q", s)
		}
	}
	sc := read("../scripts/secrets.sh")
	for _, s := range []string{"--redact", "--no-banner", "--log-opts=--all", "--config"} {
		if !strings.Contains(sc, s) {
			t.Errorf("secrets.sh lacks %q", s)
		}
	}
}

func regexpLine(s, prefix string) string {
	for _, l := range strings.Split(s, "\n") {
		if strings.HasPrefix(l, prefix) {
			return l
		}
	}
	return ""
}
