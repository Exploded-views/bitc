package conformance

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Exploded-views/bitc/conformance/harness"
)

// resultOf runs `true` under lint in h.Cwd with --result and returns result.json and the
// history `start` and `end` lines, decoded loosely so absent keys stay absent.
func resultOf(t *testing.T, h *harness.H) (res map[string]any, hist []map[string]any) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "r.json")
	if r := h.Run("run", "--result", path, "lint", "--", "true"); r.Code != 0 {
		t.Fatalf("%+v", r)
	}
	if err := json.Unmarshal(must(os.ReadFile(path)), &res); err != nil {
		t.Fatal(err)
	}
	for _, l := range strings.Split(strings.TrimSpace(string(must(os.ReadFile(filepath.Join(h.State, "history.jsonl"))))), "\n") {
		var e map[string]any
		if err := json.Unmarshal([]byte(l), &e); err != nil {
			t.Fatal(err)
		}
		if e["event"] == "start" || e["event"] == "end" {
			hist = append(hist, e)
		}
	}
	if len(hist) < 2 { // the history accumulates over the test's runs: this run's are the last two
		t.Fatalf("history: %v", hist)
	}
	hist = hist[len(hist)-2:]
	return res, hist
}

func TestTL6_ResultNamesTheCommitTreeStateAndCwd(t *testing.T) {
	h := harness.New(t)
	h.HostConfig("[pools.lint]\n")
	wt := h.Repo("app", map[string]string{"a.txt": "a\n"})
	sub := filepath.Join(wt, "sub")
	h.Write(filepath.Join(sub, "keep"), "", 0o644)
	if out, err := exec.Command("git", "-C", wt, "-c", "user.name=t", "-c", "user.email=t@example.invalid", "add", "-A").CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	if out, err := exec.Command("git", "-C", wt, "-c", "user.name=t", "-c", "user.email=t@example.invalid", "-c", "commit.gpgsign=false", "commit", "-q", "-m", "sub").CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	head := strings.TrimSpace(string(must(exec.Command("git", "-C", wt, "rev-parse", "HEAD").Output())))
	h.Cwd = sub

	// clean: head is the full sha, tree_clean true, cwd is where bitc ran; the history says the same
	res, hist := resultOf(t, h)
	canon, _ := filepath.EvalSymlinks(sub)
	if res["head"] != head || res["tree_clean"] != true || res["cwd"] != canon || hist[0]["cwd"] != canon {
		t.Fatalf("clean: %v", res)
	}
	for _, e := range hist {
		if e["head"] != head || e["tree_clean"] != true {
			t.Fatalf("clean history: %v", e)
		}
	}

	// dirty: an untracked file counts, an ignored one does not
	h.Write(filepath.Join(wt, "new.txt"), "x", 0o644)
	if res, hist = resultOf(t, h); res["head"] != head || res["tree_clean"] != false || hist[0]["tree_clean"] != false {
		t.Fatalf("dirty: %v %v", res, hist[0])
	}
	if err := os.Remove(filepath.Join(wt, "new.txt")); err != nil {
		t.Fatal(err)
	}
	h.Write(filepath.Join(wt, ".gitignore"), "ign\n", 0o644)
	for _, args := range [][]string{{"add", ".gitignore"}, {"-c", "user.name=t", "-c", "user.email=t@example.invalid", "-c", "commit.gpgsign=false", "commit", "-q", "-m", "ignore"}} {
		if out, err := exec.Command("git", append([]string{"-C", wt}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("%v %s", err, out)
		}
	}
	head = strings.TrimSpace(string(must(exec.Command("git", "-C", wt, "rev-parse", "HEAD").Output())))
	h.Write(filepath.Join(wt, "ign"), "x", 0o644)
	if res, _ = resultOf(t, h); res["head"] != head || res["tree_clean"] != true {
		t.Fatalf("ignored file made the tree dirty: %v", res)
	}
}

func TestTL7_ResultOutsideAGitTreeHasCwdOnly(t *testing.T) {
	h := harness.New(t)
	h.HostConfig("[pools.lint]\n")
	dir := filepath.Join(h.Root, "plain")
	h.Write(filepath.Join(dir, "f"), "", 0o644)
	h.Cwd = dir
	res, hist := resultOf(t, h)
	if _, ok := res["tree_clean"]; ok {
		t.Fatalf("tree_clean outside git: %v", res)
	}
	canon, _ := filepath.EvalSymlinks(dir)
	if v, _ := res["head"].(string); v != "" || res["cwd"] != canon || hist[0]["cwd"] != canon {
		t.Fatalf("%v", res)
	}
	for _, e := range hist {
		if _, ok := e["tree_clean"]; ok || e["head"] != nil && e["head"] != "" {
			t.Fatalf("history outside git: %v", e)
		}
	}
}

func str(v any) string { s, _ := v.(string); return s }
