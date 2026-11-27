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

// runStubKeys runs the download stub with fake deploy and RunPod keys in its env. fetch is the
// fetch tool's name (curl or wget) and body the shim's own script after its key log line; the
// sha256sum shim logs the same way. With onlyShims the PATH holds nothing but the shim dir (so a
// real curl cannot be found), with plain mkdir and chmod wrappers in it. The agent payload prints
// the lengths of the two keys it was given.
func runStubKeys(t *testing.T, fetch, body string, onlyShims bool) (out, log string) {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	_ = os.MkdirAll(bin, 0o755)
	shim := func(name, script string) {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+script), 0o755); err != nil { //nolint:gosec // a test shim
			t.Fatal(err)
		}
	}
	payload := "#!/bin/sh\necho agent-keys ${#BITC_DEPLOY_KEY} ${#RUNPOD_API_KEY}\n"
	keys := func(tool string) string {
		return `echo "` + tool + `-keys ${BITC_DEPLOY_KEY+set}dk ${RUNPOD_API_KEY+set}rk" >> "$LOG"` + "\n"
	}
	shim(fetch, keys(fetch)+strings.ReplaceAll(body, "PAYLOAD", payload))
	shim("sha256sum", keys("sha")+"exec /usr/bin/sha256sum \"$@\"\n")
	path := bin + ":/usr/bin:/bin"
	if onlyShims {
		shim("mkdir", "exec /bin/mkdir \"$@\"\n")
		shim("chmod", "exec /bin/chmod \"$@\"\n")
		path = bin
	}
	c := exec.Command("sh", "-c", agent.Stub("https://x.invalid/bitc", sha256Hex(payload)))
	c.Env = []string{"PATH=" + path, "BITC_STUB_DIR=" + dir, "LOG=" + filepath.Join(dir, "log"),
		"BITC_DEPLOY_KEY=fake-deploy-key-for-tests", "RUNPOD_API_KEY=fake-runpod-key"}
	b, err := c.CombinedOutput()
	l, _ := os.ReadFile(filepath.Join(dir, "log"))
	if err != nil {
		t.Fatalf("%v: %s\n%s", err, b, l)
	}
	return string(b), string(l)
}

// checkStubKeys asserts the agent got both keys whole and the fetch and checksum tools saw neither.
func checkStubKeys(t *testing.T, fetch, out, log string) {
	t.Helper()
	if !strings.Contains("\n"+out, "\nagent-keys 25 15\n") {
		t.Errorf("the agent must get both keys whole (agent-keys 25 15):\n%s\n%s", out, log)
	}
	for _, tool := range []string{fetch, "sha"} {
		n := 0
		for _, line := range strings.Split(log, "\n") {
			if strings.HasPrefix(line, tool+"-keys ") {
				n++
				if line != tool+"-keys dk rk" {
					t.Errorf("%s saw a key: %q", tool, line)
				}
			}
		}
		if n != 1 {
			t.Errorf("want one %s-keys line, got %d:\n%s", tool, n, log)
		}
	}
}

func TestRB6_StubKeepsTheKeysFromItsFetchAndChecksumTools(t *testing.T) {
	out, log := runStubKeys(t, "curl", "printf '%s' 'PAYLOAD' > \"$OUT\"\n", false)
	checkStubKeys(t, "curl", out, log)
}

func TestRB7_StubKeepsTheKeysFromWgetToo(t *testing.T) {
	out, log := runStubKeys(t, "wget", "while [ $# -gt 0 ]; do [ \"$1\" = -O ] && o=$2; shift; done\nprintf '%s' 'PAYLOAD' > \"$o\"\n", true)
	checkStubKeys(t, "wget", out, log)
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
