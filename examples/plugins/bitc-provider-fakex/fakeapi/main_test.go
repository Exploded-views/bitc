package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newTestFake builds a fake whose agent binary does not exist, so a started job never runs a process:
// start fails at cmd.Start and marks the instance exited.
func newTestFake(t *testing.T) (*fake, string) {
	t.Helper()
	root := t.TempDir()
	f := &fake{bitc: filepath.Join(root, "no-such-bitc"), root: root, insts: map[string]*inst{}, behaviour: "ok"}
	return f, root
}

func do(f *fake, method, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	w := httptest.NewRecorder()
	f.serve(w, r)
	return w
}

func createInstance(t *testing.T, f *fake) string {
	t.Helper()
	w := do(f, "POST", "/v1/instances", `{"name":"n","size":"cpu8"}`)
	if w.Code != 201 {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	var v view
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	return v.ID
}

func jobBody(t *testing.T, id string) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{"job": map[string]any{"job": id}})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestStartJobRefusesAJobIDThatIsNotPlain(t *testing.T) {
	for _, id := range []string{"../../etc", "a/b", "..", ".hidden", "", strings.Repeat("a", 65), "a b"} {
		f, root := newTestFake(t)
		iid := createInstance(t, f)
		w := do(f, "POST", "/v1/instances/"+iid+"/job", jobBody(t, id))
		if w.Code != http.StatusBadRequest {
			t.Errorf("job id %q: status %d, want 400", id, w.Code)
		}
		f.mu.Lock()
		in := f.insts[iid]
		jobID, state, testRoot := in.jobID, in.State, in.testRoot
		f.mu.Unlock()
		if jobID != "" || state != "ready" || testRoot != "" {
			t.Errorf("job id %q: instance changed: jobID=%q state=%q testRoot=%q", id, jobID, state, testRoot)
		}
		ents, err := os.ReadDir(root)
		if err != nil {
			t.Fatal(err)
		}
		if len(ents) != 0 {
			t.Errorf("job id %q: created %d entries under the root", id, len(ents))
		}
	}
}

func TestStartJobAcceptsAPlainJobID(t *testing.T) {
	f, _ := newTestFake(t)
	iid := createInstance(t, f)
	w := do(f, "POST", "/v1/instances/"+iid+"/job", jobBody(t, "j-1.a_b"))
	if w.Code != http.StatusAccepted {
		t.Fatalf("status %d, want 202: %s", w.Code, w.Body.String())
	}
	f.mu.Lock()
	jobID := f.insts[iid].jobID
	f.mu.Unlock()
	if jobID != "j-1.a_b" {
		t.Errorf("jobID = %q", jobID)
	}
}

func TestFileServesOnlyTheDocumentedNames(t *testing.T) {
	f, root := newTestFake(t)
	iid := createInstance(t, f)
	testRoot := filepath.Join(root, "tr")
	dir := filepath.Join(testRoot, "run", "j1")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// State is set through the fake's own fields: no agent runs.
	f.mu.Lock()
	f.insts[iid].testRoot, f.insts[iid].jobID = testRoot, "j1"
	f.mu.Unlock()

	documented := []string{"out.log", "err.log", "result.json", "state.json", "artifacts.tar.gz"}
	for _, n := range documented {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("bytes of "+n), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, n := range documented {
		w := do(f, "GET", "/v1/instances/"+iid+"/files/"+n, "")
		if w.Code != 200 || w.Body.String() != "bytes of "+n {
			t.Errorf("%s: status %d body %q", n, w.Code, w.Body.String())
		}
	}
	// out.log.part is the agent's growing log; the fake serves it as out.log, never under its own name.
	others := []string{"passwd", ".part", "out.log.part", "result", "x.json"}
	for _, n := range others {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("secret "+n), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, n := range append([]string{".."}, others...) {
		w := do(f, "GET", "/v1/instances/"+iid+"/files/"+n, "")
		if w.Code != 404 {
			t.Errorf("%s: status %d, want 404 (body %q)", n, w.Code, w.Body.String())
		}
	}
}

func TestFileNeverReadsOutsideTheRunDir(t *testing.T) {
	f, root := newTestFake(t)
	iid := createInstance(t, f)
	testRoot := filepath.Join(root, "tr")
	for _, d := range []string{filepath.Join(testRoot, "run", "other"), filepath.Join(testRoot, "outside")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, n := range []string{"out.log", "result.json"} {
		if err := os.WriteFile(filepath.Join(testRoot, "outside", n), []byte("outside "+n), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// The job id is set directly, bypassing startJob and its id check.
	f.mu.Lock()
	f.insts[iid].testRoot, f.insts[iid].jobID = testRoot, "../outside"
	f.mu.Unlock()
	for _, n := range []string{"out.log", "result.json"} {
		w := do(f, "GET", "/v1/instances/"+iid+"/files/"+n, "")
		if w.Code != 404 || strings.Contains(w.Body.String(), "outside") {
			t.Errorf("%s: status %d body %q, want 404 and not the outside file", n, w.Code, w.Body.String())
		}
	}
}

func TestFileServesTheJobsOwnRunDir(t *testing.T) {
	f, root := newTestFake(t)
	iid := createInstance(t, f)
	testRoot := filepath.Join(root, "tr")
	for _, j := range []string{"j1", "j2"} {
		d := filepath.Join(testRoot, "run", j)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		for _, n := range []string{"out.log", "result.json"} {
			if err := os.WriteFile(filepath.Join(d, n), []byte(j+" "+n), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	f.mu.Lock()
	f.insts[iid].testRoot, f.insts[iid].jobID = testRoot, "j2"
	f.mu.Unlock()
	for _, n := range []string{"out.log", "result.json"} {
		w := do(f, "GET", "/v1/instances/"+iid+"/files/"+n, "")
		if w.Code != 200 || w.Body.String() != "j2 "+n {
			t.Errorf("%s: status %d body %q, want j2's bytes", n, w.Code, w.Body.String())
		}
	}
}
