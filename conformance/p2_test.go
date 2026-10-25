package conformance

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/Exploded-views/bitc/conformance/harness"
	"github.com/Exploded-views/bitc/internal/classify/classifytest"
)

type histLine struct {
	Event, Pool, Job, Cwd, Provider string
	Slot, Pid                       int
	Argv                            []string
	Exit                            *int
	TS                              string
}

func history(t *testing.T, h *harness.H) []histLine {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(h.State, "history.jsonl"))
	if os.IsNotExist(err) {
		return nil
	}
	var out []histLine
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if l == "" {
			continue
		}
		var e histLine
		if err := json.Unmarshal([]byte(l), &e); err != nil {
			t.Fatalf("%q: %v", l, err)
		}
		out = append(out, e)
	}
	return out
}

func TestP2_UsageWithoutPoolOrCommand(t *testing.T) {
	h := harness.New(t)
	h.HostConfig("[pools.lint]\n")
	for _, args := range [][]string{{"run"}, {"run", "lint"}, {"run", "lint", "--"}} {
		r := h.Run(args...)
		if r.Code != 64 || !strings.Contains(r.Stderr, "usage: bitc run") {
			t.Errorf("%q: %+v", args, r)
		}
	}
	for _, flag := range []string{"--name", "--wait", "--log", "--result", "--provider"} {
		if r := h.Run("run", flag, "", "lint", "--", "true"); r.Code != 64 {
			t.Errorf("an empty %s value: %+v", flag, r)
		}
	}
	if _, err := os.Stat(h.State); !os.IsNotExist(err) {
		t.Fatal("usage errors created the state dir")
	}
}

func TestP3_InvalidPoolNames(t *testing.T) {
	h := harness.New(t)
	h.HostConfig("[pools.lint]\n")
	for _, name := range []string{"x[$(touch pwned)]", "../x", "a/b", "a.b", "a b", "a;b", "$(id)", "`id`", "a[", "a{", "a^", `a\b`, "a:b", "a@b"} {
		r := h.Run("run", name, "--", "touch", filepath.Join(h.Root, "ran"))
		if r.Code != 64 || !strings.Contains(r.Stderr, "invalid pool name") {
			t.Errorf("%q: %+v", name, r)
		}
	}
	if _, err := os.Stat(filepath.Join(h.Root, "ran")); err == nil {
		t.Fatal("a command ran")
	}
	if _, err := os.Stat(filepath.Join(h.State, "pools")); err == nil {
		t.Fatal("pool state created")
	}
}

func TestP4_DashAndUnderscoreAreOnePool(t *testing.T) {
	h := harness.New(t)
	h.HostConfig("[pools.release_tag]\nwait = \"0s\"\n")
	hold := h.Hold(nil, "release-tag")
	if r := h.Run("run", "release_tag", "--", "true"); r.Code != 75 || r.Stderr != "STARVED release-tag\n" {
		t.Fatalf("%+v", r)
	}
	hold.Release()
	if len(history(t, h)) == 0 {
		t.Fatal("no history lines")
	}
	for _, e := range history(t, h) {
		if e.Pool != "release-tag" {
			t.Fatalf("history pool %q", e.Pool)
		}
	}
}

func TestP5_UnknownPoolsRefusedUnlessDefault(t *testing.T) {
	h := harness.New(t)
	h.HostConfig("[pools.lint]\n[pools.db]\n")
	if r := h.Run("run", "tsc", "--", "true"); r.Code != 64 || r.Stderr != "bitc: unknown pool tsc (known: db, lint)\n" {
		t.Fatalf("%+v", r)
	}
	h.HostConfig("defaults = { wait = \"0s\" }\n[host]\nunknown_pools = \"default\"\n[pools.lint]\n")
	hold := h.Hold(nil, "adhoc")
	if r := h.Run("run", "adhoc", "--", "true"); r.Code != 75 {
		t.Fatalf("an ad-hoc pool has more than one slot: %+v", r)
	}
	hold.Release()
}

func TestS7_BitcStaysTheParentAndHistoryNamesIt(t *testing.T) {
	h := harness.New(t)
	h.HostConfig("[pools.lint]\n")
	p := h.Start("run", "lint", "--", "sh", "-c", "echo $PPID")
	r := p.Wait()
	if len(history(t, h)) == 0 {
		t.Fatal("no history lines")
	}
	ppid := strings.TrimSpace(r.Stdout)
	if ppid != strconv.Itoa(p.Cmd.Process.Pid) {
		t.Fatalf("command parent %s, bitc %d", ppid, p.Cmd.Process.Pid)
	}
	for _, e := range history(t, h) {
		if e.Pid != p.Cmd.Process.Pid {
			t.Fatalf("history pid %d", e.Pid)
		}
	}
}

func TestH1_HistoryLineFields(t *testing.T) {
	h := harness.New(t)
	h.HostConfig("[pools.lint]\n")
	h.Run("run", "lint", "--", "sh", "-c", "exit 0", "arg one")
	hs := history(t, h)
	if len(hs) != 2 || hs[0].Event != "start" || hs[1].Event != "end" || *hs[1].Exit != 0 {
		t.Fatalf("%+v", hs)
	}
	e := hs[1]
	if e.Pool != "lint" || e.Slot != 1 || e.Provider != "local" || !strings.HasSuffix(e.TS, "Z") ||
		strings.Join(e.Argv, "|") != "sh|-c|exit 0|arg one" {
		t.Fatalf("%+v", e)
	}
}

func TestH3_RefusalsWriteNoHistory(t *testing.T) {
	h := harness.New(t)
	nodeRepo(t, h, "", nil)
	h.Run("run", "lint", "--", "npx", "eslint", ".")
	h.Run("run", "bad/name", "--", "true")
	if len(history(t, h)) != 0 {
		t.Fatal(history(t, h))
	}
}

func TestL1_RefusedBeforeAnyLockWithTheReason(t *testing.T) {
	h := harness.New(t)
	nodeRepo(t, h, "", nil)
	for _, argv := range classifytest.RefusedFromRoot[:10] {
		r := h.Run(append([]string{"run", "lint", "--"}, argv...)...)
		if r.Code != 64 || !strings.HasPrefix(r.Stderr, "bitc: lint: refused: ") {
			t.Errorf("%q: %+v", argv, r)
		}
	}
	if _, err := os.Stat(filepath.Join(h.State, "pools", "lint", "slot.1.lock")); err == nil {
		t.Fatal("a refusal touched a slot lock")
	}
	if r := h.Run("run", "heavy", "--", "sh", "-c", "exit 0"); r.Code != 0 {
		t.Fatalf("only the lint pool refuses: %+v", r)
	}
}

func TestM5_HoldExportsAVerifiableToken(t *testing.T) {
	h := harness.New(t)
	h.HostConfig("[pools.lint]\n")
	hold := h.Hold(nil, "lint")
	parts := strings.Split(hold.Env["BITC_HOLD"], ":")
	if len(parts) != 3 || parts[0] != "lint" || len(parts[2]) != 32 {
		t.Fatalf("%q", hold.Env["BITC_HOLD"])
	}
	if _, err := os.Stat(filepath.Join(h.State, "pools", "lint", "tickets", parts[1])); err != nil {
		t.Fatal(err)
	}
	if hold.Env["BITC_SLOT"] != "lint.1" || hold.Env["BITC_POOL"] != "lint" || hold.Env["BITC_DIR"] != h.State {
		t.Fatalf("%v", hold.Env)
	}
	hold.Release()
}

func TestM9_GroupIsNotAPool(t *testing.T) {
	h := harness.New(t)
	h.HostConfig("groups = { bigmem = { units = 1 } }\n[pools.lint]\n")
	r := h.Run("run", "bigmem", "--", "touch", filepath.Join(h.Root, "ran"))
	if r.Code != 64 || r.Stderr != "bitc: bigmem is a group, not a pool\n" {
		t.Fatalf("%+v", r)
	}
}

func TestX2_SlotsExpandDifferentlyFixedTemplatesDoNot(t *testing.T) {
	h := harness.New(t)
	nodeRepo(t, h, "", nil)
	a := h.Hold(nil, "db")
	b := h.Hold(nil, "db")
	if a.Env["TEST_DB_PORT"] != "47411" || b.Env["TEST_DB_PORT"] != "47412" || b.Env["TEST_DB_CONTAINER"] != "demo-db-2" {
		t.Fatalf("%s %s", a.Env["TEST_DB_PORT"], b.Env["TEST_DB_PORT"])
	}
	c := h.Hold(nil, "vitest")
	d := h.Hold(nil, "vitest")
	if c.Env["TEST_DB_PORT"] != "47411" || d.Env["TEST_DB_PORT"] != "47411" || d.Env["BITC_SLOT"] != "vitest.2" {
		t.Fatalf("%v", d.Env)
	}
	for _, x := range []*harness.Holder{a, b, c, d} {
		x.Release()
	}
}
