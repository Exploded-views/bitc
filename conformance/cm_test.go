package conformance

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/Exploded-views/bitc/conformance/harness"
)

func compatHost(t *testing.T, h *harness.H, extra string) string {
	t.Helper()
	dir := filepath.Join(h.Root, "legacy")
	if err := os.MkdirAll(filepath.Join(dir, "locks"), 0o700); err != nil {
		t.Fatal(err)
	}
	h.HostConfig("groups = { bigmem = { units = 1 } }\n" + extra +
		"[compat]\nlegacy_dir = " + harness.Q(dir) + "\nhold_env = \"APP_POOL_BIGMEM\"\nname_prefix = \"legacy-pool-\"\ngroups = { bigmem = \"bigmem\" }\n" +
		"[pools.lint]\nwait = \"0s\"\n[pools.heavy]\ngroups = { bigmem = 1 }\nwait = \"0s\"\n[pools.coverage]\ngroups = { bigmem = 1 }\nwait = \"0s\"\n")
	return dir
}

func flockFile(t *testing.T, p string, content string) *os.File {
	t.Helper()
	f, err := os.OpenFile(p, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	if content != "" {
		_, _ = f.WriteString(content)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

func TestCM1_NoDoubleAdmissionEitherWay(t *testing.T) {
	h := harness.New(t)
	dir := compatHost(t, h, "")
	f := flockFile(t, filepath.Join(dir, "locks", "lint.1.lock"), "")
	if r := h.Run("run", "lint", "--", "true"); r.Code != 75 || r.Stderr != "STARVED lint (legacy pool: lint.1 busy)\n" {
		t.Fatalf("%+v", r)
	}
	f.Close()
	hold := h.Hold(nil, "lint")
	if !lockHeld(filepath.Join(dir, "locks", "lint.1.lock")) {
		t.Fatal("a bitc hold does not take the legacy slot lock")
	}
	hold.Release()
}

func TestCM2_LegacyHoldsSlotOneBitcTakesSlotTwo(t *testing.T) {
	h := harness.New(t)
	nodeRepo(t, h, "", nil)
	host := string(must(readFile(h.Config)))
	dir := filepath.Join(h.Root, "legacy")
	_ = os.MkdirAll(filepath.Join(dir, "locks"), 0o700)
	host = strings.Replace(host, "\n[test]", "\n[compat]\nlegacy_dir = "+harness.Q(dir)+"\n\n[test]", 1)
	h.Write(h.Config, host, 0o600)
	flockFile(t, filepath.Join(dir, "locks", "db.1.lock"), "")
	if got := envIn(t, h, nil, "db"); got["BITC_SLOT"] != "db.2" || got["TEST_DB_PORT"] != "47412" {
		t.Fatalf("%v", got)
	}
}

func TestCM3_NestedLegacyCallsVerifyBothWays(t *testing.T) {
	h := harness.New(t)
	dir := compatHost(t, h, "")
	// bitc inside a legacy hold: the legacy holder (this test process) holds bigmem.1 with its pid
	flockFile(t, filepath.Join(dir, "locks", "bigmem.1.lock"), strconv.Itoa(os.Getpid())+"\n")
	if r := h.RunWith([]string{"APP_POOL_BIGMEM=bigmem.1"}, "run", "coverage", "--", "true"); r.Code != 0 {
		t.Fatalf("a verified legacy claim was not treated as nested: %+v", r)
	}
	if r := h.Run("run", "coverage", "--", "true"); r.Code != 75 || !strings.Contains(r.Stderr, "legacy pool: bigmem.1 busy") {
		t.Fatalf("%+v", r)
	}
}

func TestCM3_LegacyClaimPassesDownToGrandchildren(t *testing.T) {
	h := harness.New(t)
	dir := compatHost(t, h, "")
	flockFile(t, filepath.Join(dir, "locks", "bigmem.1.lock"), strconv.Itoa(os.Getpid())+"\n")
	r := h.RunWith([]string{"APP_POOL_BIGMEM=bigmem.1"}, "run", "coverage", "--", harness.Bin, "run", "heavy", "--", "echo", "nested-ok")
	if r.Code != 0 || !strings.Contains(r.Stdout, "nested-ok") {
		t.Fatalf("a legacy claim was lost one level down: %+v", r)
	}
}

func TestCM4_BitcHoldIsVisibleToNestedLegacyCalls(t *testing.T) {
	h := harness.New(t)
	dir := compatHost(t, h, "")
	hold := h.Hold(nil, "heavy")
	if hold.Env["APP_POOL_BIGMEM"] != "bigmem.1" {
		t.Fatalf("%v", hold.Env)
	}
	pid := strings.TrimSpace(string(must(readFile(filepath.Join(dir, "locks", "bigmem.1.lock")))))
	if pid != strconv.Itoa(hold.Proc.Cmd.Process.Pid) {
		t.Fatalf("lock holds %q, the supervisor is %d", pid, hold.Proc.Cmd.Process.Pid)
	}
	hold.Release()
}

func TestCM5_PriorityMarkersBothWays(t *testing.T) {
	h := harness.New(t)
	dir := compatHost(t, h, "priority = { high_cwd_contains = [\"urgent\"] }\n")
	sleeper := startSleeper(t)
	h.Write(filepath.Join(dir, "locks", "lint.prio."+strconv.Itoa(sleeper)), "", 0o600)
	if r := h.Run("run", "--wait", "0s", "lint", "--", "true"); r.Stderr != "STARVED lint (priority lane)\n" {
		t.Fatalf("%+v", r)
	}
	// the other way: a bitc high ticket is mirrored as locks/<pool>.prio.<supervisor pid>
	old := h.Cwd
	h.Cwd = filepath.Join(h.Root, "urgent-a")
	_ = os.MkdirAll(h.Cwd, 0o755)
	hold := h.Hold([]string{"PWD=" + h.Cwd}, "heavy")
	h.Cwd = old
	marker := filepath.Join(dir, "locks", "heavy.prio."+strconv.Itoa(hold.Proc.Cmd.Process.Pid))
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("a high bitc hold has no legacy priority marker: %v", err)
	}
	hold.Release()
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the priority marker outlived the hold")
	}
}

// startSleeper starts a live process (killed at cleanup) and returns its pid.
func startSleeper(t *testing.T) int {
	t.Helper()
	c := exec.Command("sleep", "60")
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Process.Kill(); _ = c.Wait() })
	return c.Process.Pid
}

func TestCM6_UnderscoreSpelledGroupStillMeetsLegacyLock(t *testing.T) {
	h := harness.New(t)
	dir := filepath.Join(h.Root, "legacy")
	if err := os.MkdirAll(filepath.Join(dir, "locks"), 0o700); err != nil {
		t.Fatal(err)
	}
	h.HostConfig("groups = { big_mem = { units = 1 } }\n" +
		"[compat]\nlegacy_dir = " + harness.Q(dir) + "\nhold_env = \"APP_POOL_BIGMEM\"\ngroups = { big_mem = \"bigmem\" }\n" +
		"[pools.heavy]\ngroups = { big_mem = 1 }\nwait = \"0s\"\n")
	flockFile(t, filepath.Join(dir, "locks", "bigmem.1.lock"), "")
	if r := h.Run("run", "heavy", "--", "true"); r.Code != 75 || r.Stderr != "STARVED heavy (legacy pool: big-mem.1 busy)\n" {
		t.Fatalf("%+v", r)
	}
}

func TestCM7_CompatGroupKeysMustNameDeclaredGroups(t *testing.T) {
	h := harness.New(t)
	dir := filepath.Join(h.Root, "legacy")
	h.HostConfig("groups = { bigmem = { units = 1 } }\n" +
		"[compat]\nlegacy_dir = " + harness.Q(dir) + "\ngroups = { bigmen = \"bigmem\" }\n")
	if r := h.Run("config", "check"); r.Code != 78 || !strings.Contains(r.Stderr, "compat.groups.bigmen names no declared group") {
		t.Fatalf("%+v", r)
	}
}

func TestCM8_StatusCountsALegacyHeldSlot(t *testing.T) {
	h := harness.New(t)
	dir := filepath.Join(h.Root, "legacy")
	if err := os.MkdirAll(filepath.Join(dir, "locks"), 0o700); err != nil {
		t.Fatal(err)
	}
	h.HostConfig("[compat]\nlegacy_dir = " + harness.Q(dir) + "\n[pools.lint]\nslots = 1\nwait = \"0s\"\n")
	flockFile(t, filepath.Join(dir, "locks", "lint.1.lock"), strconv.Itoa(os.Getpid())+"\n")
	if r := h.Run("run", "lint", "--", "true"); r.Code != 75 {
		t.Fatalf("admission must see the legacy hold: %+v", r)
	}
	r := h.Run("status")
	if !strings.Contains(r.Stdout, "lint: 1/1 slots held, 0 waiting") || !strings.Contains(r.Stdout, "slot 1  legacy holder  pid "+strconv.Itoa(os.Getpid())) {
		t.Fatalf("status shows the legacy-held slot as free:\n%s", r.Stdout)
	}
	j := h.Run("--json", "status")
	var got struct {
		Pools []struct {
			Name    string
			Holders []struct {
				Slot   int
				Pid    int
				Legacy bool
			}
		}
	}
	if err := json.Unmarshal([]byte(j.Stdout), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Pools) != 1 || len(got.Pools[0].Holders) != 1 || !got.Pools[0].Holders[0].Legacy ||
		got.Pools[0].Holders[0].Slot != 1 || got.Pools[0].Holders[0].Pid != os.Getpid() {
		t.Fatalf("holders: %s", j.Stdout)
	}
	var all, schema map[string]any
	if err := json.Unmarshal([]byte(j.Stdout), &all); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(must(os.ReadFile("../docs/status.schema.json")), &schema); err != nil {
		t.Fatal(err)
	}
	checkSchema(t, "$", schema, all)
	if l, _ := all["legacy"].([]any); len(l) != 1 || !strings.HasPrefix(l[0].(string), "lint.1 held by pid ") {
		t.Fatalf("the report-level legacy list went away: %s", j.Stdout)
	}
}
