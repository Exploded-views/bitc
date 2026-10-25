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
	"time"

	"github.com/Exploded-views/bitc/conformance/harness"
	"github.com/Exploded-views/bitc/internal/proc"
	"github.com/Exploded-views/bitc/internal/proctest"
)

func itoaS(i int) string { return strconv.Itoa(i) }

func TestST1_StatusShowsHoldersWaitersGroupsMemory(t *testing.T) {
	h := harness.New(t)
	h.HostConfig("groups = { g = { units = 1 } }\n[pools.lint]\ngroups = { g = 1 }\nwait = \"30s\"\n")
	hold := h.Hold(nil, "--name", "holder", "lint")
	w := h.Start("run", "--name", "waiter", "lint", "--", "true")
	h.Until(func() bool { return len(tickets(h, "lint")) == 2 }, 10*time.Second)
	if r := h.Run("run", "--wait", "0s", "lint", "--", "true"); r.Code != 75 {
		t.Fatalf("expected STARVED: %+v", r)
	}
	r := h.Run("status")
	for _, want := range []string{"lint: 1/1 slots held, 1 waiting", "slot 1  holder", "waiting  waiter  normal",
		"  last STARVED: ", "group g: 1/1 units held", "memory: "} {
		if !strings.Contains(r.Stdout, want) {
			t.Errorf("status lacks %q:\n%s", want, r.Stdout)
		}
	}
	hold.Release()
	w.Wait()
}

func TestST2_StatusJSONMatchesTheSchema(t *testing.T) {
	h := harness.New(t)
	h.HostConfig("groups = { g = { units = 2 } }\n[pools.lint]\ngroups = { g = 1 }\n")
	hold := h.Hold(nil, "lint")
	r := h.Run("--json", "status")
	hold.Release()
	var got map[string]any
	if err := json.Unmarshal([]byte(r.Stdout), &got); err != nil {
		t.Fatal(err)
	}
	var schema map[string]any
	if err := json.Unmarshal(must(os.ReadFile("../docs/status.schema.json")), &schema); err != nil {
		t.Fatal(err)
	}
	checkSchema(t, "$", schema, got)
}

// checkSchema: every required key is present and every key is declared (a small subset of
// JSON Schema: type object/array, properties, required, items).
func checkSchema(t *testing.T, at string, s map[string]any, v any) {
	t.Helper()
	switch x := v.(type) {
	case map[string]any:
		if ap, ok := s["additionalProperties"].(map[string]any); ok && s["properties"] == nil {
			for k, val := range x {
				checkSchema(t, at+"."+k, ap, val)
			}
			return
		}
		props, _ := s["properties"].(map[string]any)
		if req, ok := s["required"].([]any); ok {
			for _, k := range req {
				if _, ok := x[k.(string)]; !ok {
					t.Errorf("%s: missing %s", at, k)
				}
			}
		}
		for k, val := range x {
			sub, ok := props[k].(map[string]any)
			if !ok {
				if props != nil {
					t.Errorf("%s: %s is not in the schema", at, k)
				}
				continue
			}
			checkSchema(t, at+"."+k, sub, val)
		}
	case []any:
		if items, ok := s["items"].(map[string]any); ok {
			for i, it := range x {
				checkSchema(t, at+"["+itoaS(i)+"]", items, it)
			}
		}
	}
}

func TestST3_SweepPrunesDeadTicketsAndMarksLostJobs(t *testing.T) {
	h := harness.New(t)
	h.HostConfig("[pools.lint]\n")
	h.Write(filepath.Join(h.State, "pools", "lint", "tickets", "1-0000000000000000001-0000beef.json"), `{"pid":1,"start":1,"state":"waiting","token":"x"}`, 0o600)
	// a dead hold whose command still runs in its own process group: sweep kills the group
	orphan := exec.Command("sleep", "60")
	orphan.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := orphan.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = orphan.Process.Kill(); _ = orphan.Wait() })
	op := orphan.Process.Pid
	ost, err := proc.ReadStat(op)
	if err != nil {
		t.Fatal(err)
	}
	h.Write(filepath.Join(h.State, "pools", "lint", "tickets", "1-0000000000000000002-0000cafe.json"),
		`{"pid":1,"start":1,"state":"running","job":"j9","token":"y","leader":`+itoaS(op)+`,"pgid":`+itoaS(op)+
			`,"leader_start":`+strconv.FormatUint(ost.Start, 10)+`}`, 0o600)
	dead := exec.Command("true")
	_ = dead.Run()
	rec := filepath.Join(h.State, "jobs", "abcd1234", "j1")
	h.Write(filepath.Join(rec, "gen"), "1\n", 0o600)
	h.Write(filepath.Join(rec, "status.json"), `{"state":"running","gen":1,"pid":`+strconv.Itoa(dead.Process.Pid)+`,"start":1,"log":"x"}`, 0o600)
	_ = os.Chmod(h.State, 0o700)
	r := h.Run("sweep")
	if r.Code != 0 || !strings.Contains(r.Stdout, "swept ticket lint/1-0000000000000000001-0000beef.json (owner is dead)") ||
		!strings.Contains(r.Stdout, "swept job-group lint/j9 (supervisor died; process group killed)") ||
		!strings.Contains(r.Stdout, "swept ticket lint/1-0000000000000000002-0000cafe.json (owner is dead)") ||
		!strings.Contains(r.Stdout, "swept job abcd1234/j1 (supervisor died; marked lost)") {
		t.Fatalf("%+v", r)
	}
	h.Until(func() bool { return proctest.Gone(op) }, 10*time.Second)
	if !strings.Contains(string(must(os.ReadFile(filepath.Join(h.State, "history.jsonl")))), `"event":"reaped"`) {
		t.Fatal("no reaped history event")
	}
	if !strings.Contains(string(must(os.ReadFile(filepath.Join(rec, "result.json")))), `"lost"`) {
		t.Fatal("record not marked lost")
	}
	if !strings.Contains(string(must(os.ReadFile(filepath.Join(rec, "status.json")))), `"ended_at"`) {
		t.Fatal("lost record has no ended_at, so retention never removes it")
	}
}

func TestST4_SweepRotatesHistoryKeepingThree(t *testing.T) {
	h := harness.New(t)
	h.HostConfig("[host]\nhistory_max = \"1KiB\"\n[pools.lint]\n")
	hist := filepath.Join(h.State, "history.jsonl")
	var rotated []string // history.jsonl.1 right after each round's sweep
	for i := 0; i < 5; i++ {
		for j := 0; j < 8; j++ {
			h.Run("run", "lint", "--", "true")
		}
		h.Run("sweep")
		b, err := os.ReadFile(hist + ".1")
		if err != nil || (i > 0 && string(b) == rotated[i-1]) {
			t.Fatalf("round %d did not rotate: %v", i, err)
		}
		rotated = append(rotated, string(b))
	}
	for s, round := range map[string]int{".1": 4, ".2": 3, ".3": 2} {
		if got := string(must(os.ReadFile(hist + s))); got != rotated[round] {
			t.Errorf("history%s is not round %d's file (wrong rename order)", s, round)
		}
	}
	if _, err := os.Stat(hist + ".4"); err == nil {
		t.Fatal("kept more than three")
	}
}

func TestST5_SweepDryRunChangesNothing(t *testing.T) {
	h := harness.New(t)
	h.HostConfig("[pools.lint]\n")
	ticket := filepath.Join(h.State, "pools", "lint", "tickets", "1-0000000000000000001-0000beef.json")
	h.Write(ticket, `{"pid":1,"start":1,"state":"waiting","token":"x"}`, 0o600)
	_ = os.Chmod(h.State, 0o700)
	r := h.Run("sweep", "--dry-run")
	if !strings.Contains(r.Stdout, "swept ticket") {
		t.Fatalf("%+v", r)
	}
	if _, err := os.Stat(ticket); err != nil {
		t.Fatal("dry run removed the ticket")
	}
}

func TestST6_DoctorChecksAndExitCodes(t *testing.T) {
	h := harness.New(t)
	h.HostConfig("[pools.lint]\n")
	r := h.Run("doctor")
	if r.Code != 0 || !strings.Contains(r.Stdout, "ok linux:") || !strings.Contains(r.Stdout, "ok flock:") {
		t.Fatalf("%+v", r)
	}
	h.HostConfig("bogus = 1\n")
	if r := h.Run("doctor"); r.Code != 78 {
		t.Fatalf("%+v", r)
	}
}

func TestST7_VersionTextAndJSON(t *testing.T) {
	h := harness.New(t)
	if r := h.Run("version"); r.Code != 0 || !strings.HasPrefix(r.Stdout, "bitc ") || !strings.Contains(r.Stdout, "release=false") {
		t.Fatalf("%+v", r)
	}
	var v map[string]any
	if err := json.Unmarshal([]byte(h.Run("version", "--json").Stdout), &v); err != nil || v["release"] != false || v["state_schema"] != 1.0 {
		t.Fatalf("%v %v", v, err)
	}
}

func TestST8_AdminArgumentsAreChecked(t *testing.T) {
	h := harness.New(t)
	h.HostConfig("[pools.lint]\n")
	var got map[string]any
	if r := h.Run("status", "--json"); r.Code != 0 || json.Unmarshal([]byte(r.Stdout), &got) != nil {
		t.Fatalf("status --json: %+v", r)
	}
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"status", "nosuch"}, `unknown pool "nosuch"`},
		{[]string{"status", "--bogus"}, "usage: bitc status [--json] [POOL...]"},
		{[]string{"doctor", "--bogus"}, "usage: bitc doctor [--remote]"},
		{[]string{"sweep", "--provider"}, "usage: bitc sweep "},
		{[]string{"sweep", "--bogus"}, "usage: bitc sweep "},
	} {
		if r := h.Run(c.args...); r.Code != 64 || !strings.Contains(r.Stderr, c.want) {
			t.Errorf("%v: want 64 and %q, got %+v", c.args, c.want, r)
		}
	}
}

func TestST9_StatusRedactsSecretsInArgvCwdAndReason(t *testing.T) {
	h := harness.New(t)
	const secret = "rpa_SECRETVALUE123"
	h.HostConfig("[pools.lint]\nslots = 2\nwait = \"30s\"\n[providers.runpod]\napi_key = \"" + secret + "\"\ndaily_usd = 1\nimage = \"x\"\nsizes = {}\n" +
		"api_base = \"http://127.0.0.1:9\"\nproxy_domain = \"127.0.0.1:9\"\n")
	hold := h.Hold(nil, "--name", "holder", "lint")
	// a second holder whose argv carries the secret (the hold script's own args come after "--")
	args := []string{"run", "--name", "leaky", "lint", "--", "sh", "-c", "sleep 30", secret}
	w := h.Start(args...)
	h.Until(func() bool { return len(tickets(h, "lint")) == 2 }, 10*time.Second)
	for _, args := range [][]string{{"status"}, {"status", "--json"}, {"--json", "status"}} {
		r := h.Run(args...)
		if r.Code != 0 {
			t.Fatalf("%v: %+v", args, r)
		}
		if strings.Contains(r.Stdout+r.Stderr, secret) {
			t.Errorf("%v printed the credential:\n%s", args, r.Stdout)
		}
		if !strings.Contains(r.Stdout, "redacted>") && !strings.Contains(r.Stdout, `redacted\u003e`) {
			t.Errorf("%v shows no redaction marker:\n%s", args, r.Stdout)
		}
	}
	if r := h.Run("doctor"); strings.Contains(r.Stdout+r.Stderr, secret) {
		t.Errorf("doctor printed the credential:\n%s", r.Stdout)
	}
	w.Signal(syscall.SIGTERM)
	w.Wait()
	hold.Release()
}

func TestST10_SweepRemovesStaleStagingDirs(t *testing.T) {
	h := harness.New(t)
	h.HostConfig("[pools.lint]\n")
	wt := h.Repo("app", map[string]string{".bitc.toml": "version = 1\n"})
	h.Cwd = wt
	stale, fresh := filepath.Join(wt, ".bitc", "staging", "j1"), filepath.Join(wt, ".bitc", "staging", "j2")
	for _, d := range []string{stale, fresh} {
		h.Write(filepath.Join(d, "out", "f"), "x", 0o600)
	}
	old := time.Now().Add(-7 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	r := h.Run("sweep", "--dry-run")
	if r.Code != 0 || !strings.Contains(r.Stdout, "swept staging "+stale+" (older than 6h)") || strings.Contains(r.Stdout, fresh) {
		t.Fatalf("%+v", r)
	}
	if _, err := os.Stat(stale); err != nil {
		t.Fatal("the dry run removed the stale dir")
	}
	if r := h.Run("sweep"); r.Code != 0 || !strings.Contains(r.Stdout, "swept staging "+stale) {
		t.Fatalf("%+v", r)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("the stale staging dir was kept")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatal("a fresh staging dir was removed")
	}
}
