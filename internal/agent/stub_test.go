package agent

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestStubIsSmallAndDeterministic(t *testing.T) {
	a := Stub("https://example.invalid/bitc", strings.Repeat("a", 64))
	if a == "" || a != Stub("https://example.invalid/bitc", strings.Repeat("a", 64)) || len(a) >= 2048 {
		t.Fatalf("len %d", len(a))
	}
}

func TestStubVerifiesTheShaBeforeExec(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	_ = os.MkdirAll(bin, 0o755)
	payload := "#!/bin/sh\necho agent-ran\n"
	// a fake curl that "downloads" the payload
	_ = os.WriteFile(filepath.Join(bin, "curl"), []byte("#!/bin/sh\nwhile [ $# -gt 1 ]; do shift; done\nprintf '%s' '"+payload+"' > \"$OUT\"\n"), 0o755)
	sum := sha256hex([]byte(payload))
	run := func(sha string) string {
		cmd := exec.Command("sh", "-c", Stub("https://x.invalid/bitc", sha))
		cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin", "BITC_STUB_DIR=" + dir}
		out, _ := cmd.CombinedOutput()
		return string(out)
	}
	if out := run(sum); !strings.Contains(out, "agent-ran") {
		t.Fatalf("%q", out)
	}
	if out := run(strings.Repeat("0", 64)); strings.Contains(out, "agent-ran") {
		t.Fatalf("ran an agent with the wrong sha: %q", out)
	}
}

func installRun(t *testing.T, sha string) (out string, installed []byte, err error) {
	t.Helper()
	dir := t.TempDir()
	bin, dest := filepath.Join(dir, "bin"), filepath.Join(dir, "dest")
	_ = os.MkdirAll(bin, 0o755)
	payload := "#!/bin/sh\necho agent-ran\n"
	_ = os.WriteFile(filepath.Join(bin, "curl"), []byte("#!/bin/sh\nout=\nwhile [ $# -gt 1 ]; do if [ \"$1\" = -o ]; then out=$2; fi; shift; done\nprintf '%s' '"+payload+"' > \"$out\"\n"), 0o755)
	if sha == "" {
		sha = sha256hex([]byte(payload))
	}
	cmd := exec.Command("sh", "-c", StubInstall("https://x.invalid/bitc", sha))
	cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin", "BITC_INSTALL_DIR=" + dest}
	o, err := cmd.CombinedOutput()
	b, _ := os.ReadFile(filepath.Join(dest, "bitc"))
	if st, serr := os.Stat(filepath.Join(dest, "bitc")); serr == nil && st.Mode().Perm() != 0o755 {
		t.Errorf("installed mode %v", st.Mode().Perm())
	}
	return string(o), b, err
}

func TestStubInstallPlacesTheVerifiedAgentAndRunsNothing(t *testing.T) {
	out, b, err := installRun(t, "")
	if err != nil || !strings.Contains(string(b), "agent-ran") || strings.Contains(out, "agent-ran") {
		t.Fatalf("err %v, out %q, installed %q", err, out, b)
	}
}

func TestStubInstallRefusesAWrongShaAndInstallsNothing(t *testing.T) {
	out, b, err := installRun(t, strings.Repeat("0", 64))
	if err == nil || len(b) != 0 || !strings.Contains(out, "sha256 mismatch") {
		t.Fatalf("err %v, out %q, installed %q", err, out, b)
	}
}

func TestStubInstallIsSmallAndDeterministic(t *testing.T) {
	a := StubInstall("https://example.invalid/bitc", strings.Repeat("a", 64))
	if a == "" || a != StubInstall("https://example.invalid/bitc", strings.Repeat("a", 64)) || len(a) >= 2048 {
		t.Fatalf("len %d", len(a))
	}
}
