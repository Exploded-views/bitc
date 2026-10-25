package conformance

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Exploded-views/bitc/conformance/harness"
)

// fakeRepo: a git work tree whose heavy pool may go to the fake provider.
func fakeRepo(t *testing.T, h *harness.H, mode, extraHost string) string {
	t.Helper()
	h.HostConfig("[remote_limits]\nworktrees = [\"**/app\"]\n" + extraHost +
		"[pools.heavy]\nproviders = [\"fake\", \"local\"]\nremote_limits = { size = { fake = \"small\" } }\nwait = \"5s\"\n" +
		"[providers.fake]\ndaily_usd = 1\nmode = " + harness.Q(mode) + "\n")
	wt := h.Repo("app", map[string]string{".bitc.toml": "version = 1\n[pools.heavy.remote]\nallow = [[\"sh\", \"*\"], [\"echo\", \"*\"]]\n"})
	h.Cwd = wt
	return wt
}

func TestQ1_LocalByDefaultEnvCannotEnable(t *testing.T) {
	h := harness.New(t)
	h.HostConfig("[pools.heavy]\n")
	if r := h.Run("run", "heavy", "--", "echo", "ran"); r.Code != 0 || r.Stderr != "" {
		t.Fatalf("%+v", r)
	}
	if r := h.RunWith([]string{"BITC_PROVIDER=fake"}, "run", "heavy", "--", "true"); r.Code != 64 {
		t.Fatalf("%+v", r)
	}
}

func TestQ2_RemoteProviderRunsBeforeLocalAdmission(t *testing.T) {
	h := harness.New(t)
	fakeRepo(t, h, "run", "")
	hold := h.Hold([]string{"BITC_PROVIDER=local"}, "heavy") // the only local slot is busy
	r := h.Run("run", "heavy", "--", "echo", "remote-ok")
	hold.Release()
	if r.Code != 0 || r.Stdout != "remote-ok\n" || !strings.Contains(r.Stderr, "bitc: fake: heavy job ") {
		t.Fatalf("%+v", r)
	}
}

func TestQ3_ForcedLocalNestedOrUnmatchedStaySilent(t *testing.T) {
	h := harness.New(t)
	fakeRepo(t, h, "transient", "")
	for _, env := range [][]string{{"BITC_PROVIDER=local"}, nil} {
		args := []string{"run", "heavy", "--", "echo", "x"}
		if env == nil {
			args = []string{"run", "--local", "heavy", "--", "echo", "x"}
		}
		if r := h.RunWith(env, args...); r.Code != 0 || r.Stderr != "" {
			t.Fatalf("%v: %+v", env, r)
		}
	}
	r := h.Run("run", "heavy", "--", "sh", "-c", `"$1" run heavy -- echo nested`, "_", harness.Bin)
	if strings.Count(r.Stderr, "running heavy locally") > 1 {
		t.Fatalf("the nested call noted a fallback: %+v", r)
	}
	h.Cwd = h.Repo("elsewhere", map[string]string{".bitc.toml": "version = 1\n[pools.heavy.remote]\nallow = [[\"echo\", \"*\"]]\n"})
	if r := h.Run("run", "heavy", "--", "echo", "x"); r.Stderr != "" {
		t.Fatalf("unmatched work tree: %+v", r)
	}
}

func TestRR1_FallbackNoteThenLocalWithAFreshWait(t *testing.T) {
	h := harness.New(t)
	fakeRepo(t, h, "transient", "")
	r := h.Run("run", "heavy", "--", "echo", "ran-local")
	if r.Code != 0 || r.Stdout != "ran-local\n" || r.Stderr != "bitc: fake: running heavy locally: fake: no capacity\n" {
		t.Fatalf("%+v", r)
	}
	if hs := history(t, h); hs[len(hs)-1].Provider != "local" {
		t.Fatal("did not run locally")
	}
}

func TestRR2_NoSizeStaysLocalSilently(t *testing.T) {
	h := harness.New(t)
	fakeRepo(t, h, "transient", "")
	h.HostConfig(strings.Replace(readCfg(t, h), `remote_limits = { size = { fake = "small" } }`, "", 1))
	if r := h.Run("run", "heavy", "--", "echo", "x"); r.Code != 0 || r.Stderr != "" {
		t.Fatalf("%+v", r)
	}
}

// An infra-class setup failure after the create (spec 8.2, 15.8): torn down, 75, never a local run, although
// "local" is next in the pool's providers and on_failure is "next".
func TestRR14_CreatedResourceNeverFallsBack(t *testing.T) {
	h := harness.New(t)
	wt := fakeRepo(t, h, "setup-fails", "")
	st := filepath.Join(h.Root, "fakestate")
	if err := os.MkdirAll(st, 0o700); err != nil {
		t.Fatal(err)
	}
	h.HostConfig(strings.Replace(readCfg(t, h), `mode = "setup-fails"`, "mode = \"setup-fails\"\nstate_dir = "+harness.Q(st), 1))
	r := h.Run("run", "heavy", "--", "sh", "-c", "echo ran > ran-locally")
	if r.Code != 75 || r.Stdout != "" || !strings.Contains(r.Stderr, "STARVED heavy (fake: fake-1's setup failed at 'tools')") ||
		strings.Contains(r.Stderr, "running heavy locally") {
		t.Fatalf("%+v", r)
	}
	if _, err := os.Stat(filepath.Join(wt, "ran-locally")); err == nil {
		t.Fatal("the job ran locally after its resource was created")
	}
	if torn := lines(t, filepath.Join(st, "torn.log")); len(torn) != 1 || torn[0] != "fake-1" {
		t.Fatalf("the resource was not torn down: %v", torn)
	}
}

// readCfg returns the host file's body as HostConfig takes it: without the version line and
// the harness's [test] table.
func readCfg(t *testing.T, h *harness.H) string {
	t.Helper()
	b, err := os.ReadFile(h.Config)
	if err != nil {
		t.Fatal(err)
	}
	s := strings.TrimPrefix(string(b), "version = 1\n")
	if i := strings.Index(s, "\n\n[test]\n"); i >= 0 {
		s = s[:i]
	}
	return s
}
