package conformance

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Exploded-views/bitc/conformance/harness"
)

// With no local entry after the provider no failure claims a local run (spec 8.2, D3): an
// ineligible job prints the not-eligible line alone, the others end by the no-provider-left rule.
func TestPV1_NoProviderLeft69Or75(t *testing.T) {
	for mode, want := range map[string]struct {
		code int
		line string
	}{
		"ineligible":  {69, "bitc: fake: heavy: not eligible for remote: fake: request too large\n"},
		"unavailable": {69, "bitc: heavy: no provider left: fake: account disabled\n"},
		"transient":   {75, "STARVED heavy (no provider left: fake: no capacity)\n"},
	} {
		h := harness.New(t)
		fakeRepo(t, h, mode, "")
		h.HostConfig(strings.Replace(readCfg(t, h), `providers = ["fake", "local"]`, `providers = ["fake"]`, 1))
		if r := h.Run("run", "heavy", "--", "echo", "x"); r.Code != want.code || r.Stdout != "" || r.Stderr != want.line {
			t.Errorf("%s: %+v", mode, r)
		}
	}
}

// A remote-only pool's ineligible job never runs here; with local listed after the provider the
// same job falls back with the running-locally note, as before (spec 8.2, D3).
func TestPV1_RemoteOnlyIneligibleNeverRunsLocally(t *testing.T) {
	h := harness.New(t)
	wt := fakeRepo(t, h, "ineligible", "")
	h.HostConfig(strings.Replace(readCfg(t, h), `providers = ["fake", "local"]`, `providers = ["fake"]`, 1))
	r := h.Run("run", "heavy", "--", "sh", "-c", "echo ran > ran-here")
	if r.Code != 69 || r.Stdout != "" || r.Stderr != "bitc: fake: heavy: not eligible for remote: fake: request too large\n" {
		t.Fatalf("%+v", r)
	}
	if _, err := os.Stat(filepath.Join(wt, "ran-here")); err == nil {
		t.Fatal("a remote-only pool ran the job locally")
	}
	h.HostConfig(strings.Replace(readCfg(t, h), `providers = ["fake"]`, `providers = ["fake", "local"]`, 1))
	if r := h.Run("run", "heavy", "--", "echo", "x"); r.Code != 0 || r.Stdout != "x\n" ||
		r.Stderr != "bitc: fake: running heavy locally: fake: request too large\n" {
		t.Fatalf("with local listed: %+v", r)
	}
}

func TestPV2_OnFailureStopDoesNotFallBack(t *testing.T) {
	h := harness.New(t)
	fakeRepo(t, h, "transient", "")
	cfg := strings.Replace(readCfg(t, h), `worktrees = ["**/app"]`, "worktrees = [\"**/app\"]\non_failure = \"stop\"", 1)
	h.HostConfig(strings.Replace(cfg, `providers = ["fake"]`, `providers = ["fake", "local"]`, 1))
	// a stopped walk never announces a local run that does not happen (spec 4, 8.2)
	if r := h.Run("run", "heavy", "--", "echo", "x"); r.Code != 75 || r.Stdout != "" ||
		r.Stderr != "STARVED heavy (no provider left: fake: no capacity)\n" {
		t.Fatalf("%+v", r)
	}
}

func TestPV3_FailureAfterBeginIs75NeverFallback(t *testing.T) {
	h := harness.New(t)
	fakeRepo(t, h, "lost", "")
	r := h.Run("run", "heavy", "--", "echo", "x")
	if r.Code != 75 || r.Stdout != "" || !strings.Contains(r.Stderr, "STARVED heavy (fake: fake-1 was lost)") {
		t.Fatalf("%+v", r)
	}
}

// A setup step of the repo's own that failed by itself (spec 8.2, 8.4 "Host side", 15.9): torn
// down, 69 with the setup-step line, never STARVED, never a local run.
func TestPV7_RepoSetupFailureIs69TornDownNoFallback(t *testing.T) {
	h := harness.New(t)
	wt := fakeRepo(t, h, "setup-fails-repo", "")
	st := filepath.Join(h.Root, "fakestate")
	if err := os.MkdirAll(st, 0o700); err != nil {
		t.Fatal(err)
	}
	h.HostConfig(strings.Replace(readCfg(t, h), `mode = "setup-fails-repo"`, "mode = \"setup-fails-repo\"\nstate_dir = "+harness.Q(st), 1))
	r := h.Run("run", "heavy", "--", "sh", "-c", "echo ran > ran-locally")
	if r.Code != 69 || r.Stdout != "" || !strings.Contains(r.Stderr, "bitc: fake: heavy: setup step deps failed: npm ERR! missing script: ci\n") ||
		strings.Contains(r.Stderr, "STARVED") || strings.Contains(r.Stderr, "running heavy locally") {
		t.Fatalf("%+v", r)
	}
	if _, err := os.Stat(filepath.Join(wt, "ran-locally")); err == nil {
		t.Fatal("the job ran locally after its resource was created")
	}
	if torn := lines(t, filepath.Join(st, "torn.log")); len(torn) != 1 || torn[0] != "fake-1" {
		t.Fatalf("the resource was not torn down: %v", torn)
	}
}

func TestPV6_QuietHidesLaunchAndPodLines(t *testing.T) {
	h := harness.New(t)
	fakeRepo(t, h, "run", "")
	if r := h.Run("run", "heavy", "--", "echo", "x"); !strings.Contains(r.Stderr, "pod: fake setup step") || !strings.Contains(r.Stderr, "bitc: fake: heavy job ") {
		t.Fatalf("info: %+v", r)
	}
	if r := h.RunWith([]string{"BITC_LOG=quiet"}, "run", "heavy", "--", "echo", "x"); r.Code != 0 || r.Stdout != "x\n" || r.Stderr != "" {
		t.Fatalf("quiet: %+v", r)
	}
	h2 := harness.New(t)
	fakeRepo(t, h2, "lost", "")
	if r := h2.RunWith([]string{"BITC_LOG=quiet"}, "run", "heavy", "--", "echo", "x"); r.Code != 75 || !strings.Contains(r.Stderr, "STARVED heavy (fake: fake-1 was lost)") {
		t.Fatalf("quiet hid STARVED: %+v", r)
	}
}

func TestPV4_FakeProviderOnlyWithATestTable(t *testing.T) {
	h := harness.New(t)
	cfg := h.Root + "/plain.toml"
	h.Write(cfg, "version = 1\n[providers.fake]\ndaily_usd = 1\n", 0o600)
	if r := h.RunWith([]string{"BITC_CONFIG=" + cfg}, "config", "check"); r.Code != 78 || !strings.Contains(r.Stderr, "for tests only") {
		t.Fatalf("%+v", r)
	}
}
