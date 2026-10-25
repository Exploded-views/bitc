package conformance

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Exploded-views/bitc/conformance/harness"
	"github.com/Exploded-views/bitc/internal/agent"
	"github.com/Exploded-views/bitc/internal/agent/agenttest"
)

// modes runs body once per delivery mode: the environment boot with a results listener, and exec
// delivery (no listener: spec port -1, an agent.log beside the results, the agent exits by itself
// after result.json). The RE rows hold in both (providers delta X.1).
func modes(t *testing.T, body func(t *testing.T, newFixture func(script string) *agenttest.Fixture)) {
	t.Helper()
	for _, noListener := range []bool{false, true} {
		name := "listener"
		if noListener {
			name = "exec"
		}
		t.Run(name, func(t *testing.T) {
			body(t, func(script string) *agenttest.Fixture {
				f := agenttest.NewFixture(t, script)
				f.NoListener = noListener
				return f
			})
		})
	}
}

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

func TestRE1_BootFetchCheckoutMarkers(t *testing.T) {
	modes(t, func(t *testing.T, newFixture func(string) *agenttest.Fixture) {
		f := newFixture("#!/bin/sh\ngit -C .. rev-parse HEAD\n")
		log, code := f.Run(t)
		b, g, e := strings.Index(log, "__BITC_BOOT__"), strings.Index(log, "__BITC_BEGIN__"), strings.Index(log, "__BITC_EXIT__")
		out := string(must(os.ReadFile(filepath.Join(f.Root, "j1", "out.log"))))
		if code != 0 || b < 0 || g < b || e < g || strings.TrimSpace(out) != f.Spec.Sha {
			t.Fatalf("%d %q %q", code, log, out)
		}
	})
}

func TestRE2_NonZeroCodeReachesMarkerAndResult(t *testing.T) {
	modes(t, func(t *testing.T, newFixture func(string) *agenttest.Fixture) {
		f := newFixture("#!/bin/sh\nexit 3\n")
		log, _ := f.Run(t)
		raw := must(os.ReadFile(filepath.Join(f.Root, "j1", "result.json")))
		rf, err := agent.VerifyResult(f.K, raw)
		if err != nil || rf.Exit != 3 || !strings.Contains(log, "__BITC_EXIT__ n0nce 3") {
			t.Fatal(rf, err, log)
		}
	})
}

func TestRE3_RefMismatch(t *testing.T) {
	modes(t, func(t *testing.T, newFixture func(string) *agenttest.Fixture) {
		f := newFixture("#!/bin/sh\necho RAN\n")
		f.Spec.Sha = strings.Repeat("0", 40)
		if log, _ := f.Run(t); !strings.Contains(log, "__BITC_SETUP_FAILED__ n0nce ref-mismatch infra ") || strings.Contains(log, "RAN") {
			t.Fatal(log) // not the repo's own step: infra (spec 8.4 step 5)
		}
	})
}

func TestRE4_MissingDeployKey(t *testing.T) {
	modes(t, func(t *testing.T, newFixture func(string) *agenttest.Fixture) {
		f := newFixture("#!/bin/sh\n")
		f.Spec.CloneURL = "git@example.invalid:o/r.git"
		if log, _ := f.Run(t); !strings.Contains(log, "__BITC_SETUP_FAILED__ n0nce deploy-key") {
			t.Fatal(log)
		}
	})
}

func TestRE5_JobUserGetsOnlyTheBuiltEnv(t *testing.T) {
	modes(t, func(t *testing.T, newFixture func(string) *agenttest.Fixture) {
		f := newFixture("#!/bin/sh\nenv | sort\n")
		f.Spec.Env = []string{"A=1", "B=two"}
		f.Run(t)
		out := string(must(os.ReadFile(filepath.Join(f.Root, "j1", "out.log"))))
		for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
			k, _, _ := strings.Cut(l, "=")
			switch k {
			case "A", "B", "PATH", "HOME", "PWD", "SHLVL", "_", "OLDPWD":
			default:
				t.Errorf("unexpected variable %s", k)
			}
		}
	})
}

func TestRE6_DeployKeyGoneBeforeRepoCode(t *testing.T) {
	modes(t, func(t *testing.T, newFixture func(string) *agenttest.Fixture) {
		f := newFixture("#!/bin/sh\n")
		f.Spec.Setup = []agent.Step{{Name: "look", RunAs: "job", Argv: []string{"sh", "-c", "ls -a " + filepath.Join(f.Root, ".ssh-j1") + " 2>&1; ls -a ~/.ssh 2>&1"}}}
		log, _ := f.Run(t)
		for _, l := range strings.Split(log, "\n") { // ls prints one name per line when piped
			if l == "key" || l == "known_hosts" {
				t.Fatal(log)
			}
		}
	})
}

func TestRE8_ToolDownloadVerifyExtract(t *testing.T) {
	modes(t, func(t *testing.T, newFixture func(string) *agenttest.Fixture) {
		tgz := makeToolTarball(t, "tool-1.0", map[string]string{"bin/hello": "#!/bin/sh\necho hello-tool\n"})
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(tgz) }))
		defer srv.Close()
		f := newFixture("#!/bin/sh\nhello\n")
		f.Spec.Tools = []agent.Tool{{Name: "hello", URL: srv.URL + "/hello.tar.gz", SHA256: sha256Hex(string(tgz))}}
		f.HTTP = &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: srv.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs}}}
		f.Run(t)
		if out := string(must(os.ReadFile(filepath.Join(f.Root, "j1", "out.log")))); out != "hello-tool\n" {
			t.Fatalf("%q", out)
		}
	})
}

func TestRE9_WrongToolShaFailsAtItsName(t *testing.T) {
	modes(t, func(t *testing.T, newFixture func(string) *agenttest.Fixture) {
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("x")) }))
		defer srv.Close()
		f := newFixture("#!/bin/sh\necho RAN\n")
		f.Spec.Tools = []agent.Tool{{Name: "node", URL: srv.URL + "/n.tar.gz", SHA256: strings.Repeat("0", 64)}}
		f.HTTP = srv.Client()
		if log, _ := f.Run(t); !strings.Contains(log, "__BITC_SETUP_FAILED__ n0nce node repo sha256 ") || strings.Contains(log, "RAN") {
			t.Fatal(log) // a sha256 mismatch is the repo's own (spec 8.4 step 5, 15.9)
		}
	})
}

func TestRE11_FailingStepPrintsItsStderrTail(t *testing.T) {
	modes(t, func(t *testing.T, newFixture func(string) *agenttest.Fixture) {
		f := newFixture("#!/bin/sh\n")
		f.Spec.Setup = []agent.Step{{Name: "deps", RunAs: "job", Argv: []string{"sh", "-c", "echo boom >&2; exit 2"}}}
		log, _ := f.Run(t)
		if i, j := strings.Index(log, "boom"), strings.Index(log, "__BITC_SETUP_FAILED__ n0nce deps repo boom\n"); i < 0 || j < i {
			t.Fatal(log)
		}
	})
}

func TestRE18_ServiceReadinessRetriesQuietly(t *testing.T) {
	modes(t, func(t *testing.T, newFixture func(string) *agenttest.Fixture) {
		f := newFixture("#!/bin/sh\n")
		marker := filepath.Join(t.TempDir(), "n")
		f.Spec.RootSteps = true
		f.Spec.Services = []agent.Service{{Name: "db", RunAs: "job", Argv: []string{"sleep", "30"},
			Ready:        []string{"sh", "-c", `n=$(cat "$0" 2>/dev/null || echo 0); n=$((n+1)); echo $n > "$0"; echo probe-failed >&2; [ $n -ge 3 ]`, marker},
			ReadyTimeout: agent.Duration(20 * time.Second)}}
		log, code := f.Run(t)
		if code != 0 || strings.Contains(log, "probe-failed") {
			t.Fatalf("%d %q", code, log)
		}
	})
}

func TestRE21_RestartIsLostNeverRunsTwice(t *testing.T) {
	modes(t, func(t *testing.T, newFixture func(string) *agenttest.Fixture) {
		f := newFixture("#!/bin/sh\necho RAN\n")
		f.Run(t)
		log, _ := f.Run(t)
		if !strings.Contains(log, "__BITC_LOST__ n0nce") || strings.Contains(log, "__BITC_BEGIN__") {
			t.Fatal(log)
		}
	})
}

// A restart keeps the original deadline: the LOST boot still schedules self-delete at D + 10m.
// The timer is asserted only after the self-delete request is seen.
func TestRE21_RestartKeepsTheOriginalDeadline(t *testing.T) {
	f := agenttest.NewFixture(t, "#!/bin/sh\n")
	f.Run(t)
	deadline := time.Now().Add(-10*time.Minute + 300*time.Millisecond)
	called := make(chan time.Time, 1)
	f.Deadline = deadline
	f.SelfDelete = func() { called <- time.Now() }
	log, _ := f.Run(t)
	if !strings.Contains(log, "__BITC_LOST__ n0nce") {
		t.Fatal(log)
	}
	select {
	case at := <-called:
		if at.Before(deadline.Add(10 * time.Minute)) {
			t.Fatal("self-delete ran before the original deadline + 10m")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a restarted pod never self-deleted")
	}
}

func TestRE23_CommandRunsInTheCallersSubdir(t *testing.T) {
	modes(t, func(t *testing.T, newFixture func(string) *agenttest.Fixture) {
		f := newFixture("#!/bin/sh\npwd\n")
		f.Run(t)
		if out := string(must(os.ReadFile(filepath.Join(f.Root, "j1", "out.log")))); !strings.HasSuffix(strings.TrimSpace(out), "/sub") {
			t.Fatal(out)
		}
	})
}

func TestRE24_SelfDeleteAtDeadlinePlus10m(t *testing.T) {
	f := agenttest.NewFixture(t, "#!/bin/sh\n")
	called := make(chan time.Time, 1)
	f.Deadline = time.Now().Add(-10*time.Minute + 300*time.Millisecond)
	f.SelfDelete = func() { called <- time.Now() }
	start := time.Now()
	f.Run(t)
	select {
	case at := <-called:
		if at.Sub(start) < 250*time.Millisecond {
			t.Fatal("self-delete ran before deadline + 10m")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("self-delete never ran")
	}
}

func TestRE36_MacKeyNeverReachesTheJob(t *testing.T) {
	modes(t, func(t *testing.T, newFixture func(string) *agenttest.Fixture) {
		f := newFixture("#!/bin/sh\nenv\n")
		f.Run(t)
		out := string(must(os.ReadFile(filepath.Join(f.Root, "j1", "out.log"))))
		if strings.Contains(out, hex.EncodeToString(f.K)) || strings.Contains(out, f.Spec.Bearer) {
			t.Fatal("K or B reached the job")
		}
	})
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

// The setup-failure marker carries the class the host maps to 69 or 75 (spec 8.4 step 5, 15.9).
func TestAG2_SetupFailureMarkerCarriesItsClass(t *testing.T) {
	modes(t, func(t *testing.T, newFixture func(string) *agenttest.Fixture) {
		for argv, want := range map[string]string{
			"echo 'npm ERR! missing script: ci' >&2; exit 1": "__BITC_SETUP_FAILED__ n0nce deps repo npm ERR! missing script: ci\n",
			"kill -9 $$": "__BITC_SETUP_FAILED__ n0nce deps infra ",
		} {
			f := newFixture("#!/bin/sh\necho RAN\n")
			f.Spec.Setup = []agent.Step{{Name: "deps", RunAs: "job", Argv: []string{"sh", "-c", argv}}}
			if log, _ := f.Run(t); !strings.Contains(log, want) || strings.Contains(log, "RAN") {
				t.Errorf("%s: %q", argv, log)
			}
		}
	})
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
