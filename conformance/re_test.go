package conformance

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Exploded-views/bitc/conformance/harness"
	"github.com/Exploded-views/bitc/internal/agent"
)

func TestRB1_StubSmallAndDeterministic(t *testing.T) {
	a := agent.Stub("https://x.invalid/bitc", strings.Repeat("b", 64))
	if a != agent.Stub("https://x.invalid/bitc", strings.Repeat("b", 64)) || len(a) >= 2048 {
		t.Fatal(len(a))
	}
}

func TestRB2_StubVerifiesTheAgent(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	_ = os.MkdirAll(bin, 0o755)
	payload := "#!/bin/sh\necho agent-ran\n"
	_ = os.WriteFile(filepath.Join(bin, "curl"), []byte("#!/bin/sh\nprintf '%s' '"+payload+"' > \"$OUT\"\n"), 0o755)
	run := func(sum string) string {
		c := exec.Command("sh", "-c", agent.Stub("https://x.invalid/bitc", sum))
		c.Env = []string{"PATH=" + bin + ":/usr/bin:/bin", "BITC_STUB_DIR=" + dir}
		out, _ := c.CombinedOutput()
		return string(out)
	}
	if out := run(sha256Hex(payload)); !strings.Contains(out, "agent-ran") {
		t.Fatal(out)
	}
	if out := run(strings.Repeat("0", 64)); strings.Contains(out, "agent-ran") {
		t.Fatal("ran an unverified agent")
	}
}

func TestRB4_ToolsArePinned(t *testing.T) {
	h := harness.New(t)
	h.HostConfig("[pools.heavy]\n")
	h.Cwd = h.Repo("app", map[string]string{".bitc.toml": "version = 1\n[remote]\ntools = [{ name = \"node\", url = \"https://x.invalid/n.tgz\", sha256 = \"" + strings.Repeat("a", 63) + "\" }]\n"})
	if r := h.Run("config", "check"); r.Code != 78 || !strings.Contains(r.Stderr, "64-hex sha256") {
		t.Fatalf("%+v", r)
	}
}

func TestAG1_PrivilegedSuiteCompilesWithItsSixChecks(t *testing.T) {
	out, err := exec.Command("go", "test", "-tags", "privileged", "-list", "^TestPrivilegedAgent$", "../internal/agent/").CombinedOutput()
	if err != nil || !strings.Contains(string(out), "TestPrivilegedAgent") {
		t.Fatalf("%v %s", err, out)
	}
	src := string(must(os.ReadFile("../internal/agent/privileged_test.go")))
	for _, name := range []string{"stage1_listener_bound_before_setup", "stage2_env_empty_same_pid", "boot_marker_once_per_boot",
		"results_dir_root_owned_0700", "job_runs_as_uid_10001_with_only_remote_env", "deploy_key_gone_before_first_repo_step"} {
		if !strings.Contains(src, `t.Run("`+name+`"`) {
			t.Errorf("no subtest %s", name)
		}
	}
}

func sha256Hex(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }

// makeToolTarball is an in-memory .tar.gz with one top dir; regular files are 0755.
func makeToolTarball(t *testing.T, top string, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: top + "/" + name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
