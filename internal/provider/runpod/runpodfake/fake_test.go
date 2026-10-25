package runpodfake

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Exploded-views/bitc/internal/agent"
)

func buildBitc(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "bitc")
	if out, err := exec.Command("go", "build", "-o", bin, "../../../../cmd/bitc").CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	return bin
}

func fakeSpec(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	for _, a := range [][]string{{"init", "-q"}, {"-c", "user.name=t", "-c", "user.email=t@x", "commit", "-q", "--allow-empty", "-m", "i"}, {"update-ref", "refs/bitc/jobs/x", "HEAD"}} {
		c := exec.Command("git", a...)
		c.Dir = repo
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("%v %s", err, out)
		}
	}
	sha, _ := exec.Command("git", "-C", repo, "rev-parse", "HEAD").Output()
	spec := agent.Spec{Job: "j1", Argv: []string{"true"}, CloneURL: repo, Ref: "refs/bitc/jobs/x", Sha: strings.TrimSpace(string(sha)),
		Timeout: agent.Duration(time.Minute), Bearer: hex.EncodeToString(bytes.Repeat([]byte{2}, 32)),
		MacKey: hex.EncodeToString(bytes.Repeat([]byte{3}, 32)), Nonce: "nn", Unprivileged: true, TestRoot: t.TempDir()}
	enc, _ := spec.Encode()
	return enc
}

// A pod past D + 10m deletes itself, and the DELETE lands on the fake, never on the real API.
func TestSelfDeleteLandsOnTheFake(t *testing.T) {
	f := New(t, Options{Bin: buildBitc(t)})
	past := strconv.FormatInt(time.Now().Add(-11*time.Minute).Unix(), 10)
	body, _ := json.Marshal(v2Body(map[string]any{"name": "bitc-x", "env": map[string]string{"BITC_SPEC": fakeSpec(t),
		"BITC_DEADLINE": past, "BITC_PROVIDER_TYPE": "runpod", "BITC_API_BASE": f.URL()}}))
	resp, err := http.Post(f.URL()+"/v2/pods", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		for i, c := range f.Calls() {
			if c == "DELETE /v2/pods/pod1" {
				if a := f.Auth()[i]; a != "Bearer scoped-pod1" {
					t.Fatalf("auth %q", a)
				}
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("the fake never saw the self-delete: %v", f.Calls())
}

func TestAPodBootsBeginsAndExits(t *testing.T) {
	bin := buildBitc(t)
	enc := fakeSpec(t)
	f := New(t, Options{Bin: bin})
	body, _ := json.Marshal(v2Body(map[string]any{"name": "bitc-x", "env": map[string]string{"BITC_SPEC": enc,
		"BITC_DEADLINE": "9999999999", "BITC_PROVIDER_TYPE": "runpod"}}))
	resp, err := http.Post(f.URL()+"/v2/pods", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 201 {
		t.Fatal(resp.Status)
	}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		var lines []string
		for _, p := range f.pods {
			for _, l := range p.log {
				lines = append(lines, l.Line)
			}
		}
		f.mu.Unlock()
		joined := strings.Join(lines, "\n")
		if strings.Contains(joined, "__BITC_EXIT__ nn 0") {
			b := strings.Index(joined, "__BITC_BOOT__ nn")
			g := strings.Index(joined, "__BITC_BEGIN__ nn")
			if b < 0 || g < b {
				t.Fatal(joined)
			}
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("no exit marker")
}

// A held call is recorded, answers only after Release, and answers normally then.
func TestHoldRecordsThenAnswersOnRelease(t *testing.T) {
	f := New(t, Options{Bin: "/bin/false"})
	f.Behave("hold GET /v2/billing/pods")
	done := make(chan int, 1)
	go func() {
		resp, err := http.Get(f.URL() + "/v2/billing/pods?bucketSize=day&lastN=1")
		if err != nil {
			done <- -1
			return
		}
		_ = resp.Body.Close()
		done <- resp.StatusCode
	}()
	deadline := time.Now().Add(5 * time.Second)
	for !f.Held("GET /v2/billing/pods") {
		if time.Now().After(deadline) {
			t.Fatal("the call never reached its hold")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if c := f.Calls(); len(c) != 1 || c[0] != "GET /v2/billing/pods" {
		t.Fatal(c)
	}
	select {
	case c := <-done:
		t.Fatalf("answered %d before Release", c)
	case <-time.After(200 * time.Millisecond):
	}
	f.Release("GET /v2/billing/pods")
	if c := <-done; c != 200 {
		t.Fatalf("status %d after Release", c)
	}
}

func createWith(t *testing.T, f *Fake, spec agent.Spec, apiBase string) (int, string) {
	t.Helper()
	enc, _ := spec.Encode()
	body, _ := json.Marshal(v2Body(map[string]any{"name": "bitc-x", "env": map[string]string{"BITC_SPEC": enc,
		"BITC_DEADLINE": "9999999999", "BITC_PROVIDER_TYPE": "runpod", "BITC_API_BASE": apiBase}}))
	resp, err := http.Post(f.URL()+"/v2/pods", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct{ ID string }
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out.ID
}

// A pod always runs in test mode against the fake: a spec without TestRoot is fixed up, and
// BITC_API_BASE is always the fake's URL, whatever the request asked for, so no test can ever
// reach the real RunPod API.
func TestCreateForcesTestModeAndTheFakesAPIBase(t *testing.T) {
	f := New(t, Options{Bin: "/bin/true"})
	code, id := createWith(t, f, agent.Spec{Job: "j1", Argv: []string{"true"}}, "https://api.runpod.io")
	if code != 201 {
		t.Fatalf("status %d", code)
	}
	resp, err := http.Get(f.URL() + "/v2/pods/" + id)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var pd struct{ Env map[string]string }
	if err := json.NewDecoder(resp.Body).Decode(&pd); err != nil {
		t.Fatal(err)
	}
	spec, err := agent.DecodeSpec(pd.Env["BITC_SPEC"])
	if err != nil {
		t.Fatal(err)
	}
	if pd.Env["BITC_API_BASE"] != f.URL() {
		t.Errorf("BITC_API_BASE %q, want the fake %q", pd.Env["BITC_API_BASE"], f.URL())
	}
	if !spec.Unprivileged || spec.TestRoot == "" {
		t.Errorf("the pod's spec is not in test mode: unprivileged %v, test root %q", spec.Unprivileged, spec.TestRoot)
	}
}

// The fake refuses a pod whose spec would make the agent contact a host that is not loopback.
func TestCreateRefusesNonLoopbackHosts(t *testing.T) {
	f := New(t, Options{Bin: "/bin/true"})
	for _, spec := range []agent.Spec{
		{Job: "j1", CloneURL: "https://github.com/example/repo.git", TestRoot: t.TempDir(), Unprivileged: true},
		{Job: "j2", CloneURL: "git@github.com:example/repo.git", TestRoot: t.TempDir(), Unprivileged: true},
		{Job: "j3", Tools: []agent.Tool{{Name: "t", URL: "https://example.com/t.tar.gz"}}, TestRoot: t.TempDir(), Unprivileged: true},
	} {
		if code, _ := createWith(t, f, spec, f.URL()); code != 400 {
			t.Errorf("%s: status %d, want 400", spec.Job, code)
		}
	}
	if code, _ := createWith(t, f, agent.Spec{Job: "ok", CloneURL: "/tmp/x.git", Tools: []agent.Tool{{Name: "t", URL: "https://127.0.0.1:9/t.tar.gz"}}}, f.URL()); code != 201 {
		t.Errorf("loopback spec: status %d, want 201", code)
	}
}

// v2Body completes a create body with the required REST v2 keys the test does not care about.
func v2Body(m map[string]any) map[string]any {
	m["image"], m["cpu"], m["cmd"] = "debian", map[string]any{"id": "cpu3c", "vcpuCount": 2}, []string{"true"}
	return m
}

func postRaw(t *testing.T, f *Fake, body map[string]any) (int, string) {
	t.Helper()
	b, _ := json.Marshal(body)
	resp, err := http.Post(f.URL()+"/v2/pods", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out bytes.Buffer
	_, _ = out.ReadFrom(resp.Body)
	return resp.StatusCode, out.String()
}

// POST /v2/pods is strict like the real API: the v1-style keys, an unknown key or a missing
// required key is a 422 with a JSON error naming the field, and no pod is made.
func TestCreateIsStrictAboutTheV2Shape(t *testing.T) {
	f := New(t, Options{Bin: "/bin/true"})
	env := map[string]any{"BITC_SPEC": "x"}
	old := map[string]any{"name": "bitc-x", "imageName": "debian", "computeType": "CPU", "cpuFlavorIds": []string{"cpu3c"},
		"vcpuCount": 8, "cloudType": "SECURE", "containerDiskInGb": 40, "ports": []string{"8090/http"},
		"dockerEntrypoint": []string{"/bin/sh", "-c"}, "dockerStartCmd": []string{"true"}, "env": env}
	if code, body := postRaw(t, f, old); code != 422 || !strings.Contains(body, `"detail"`) {
		t.Errorf("the v1-style body: %d %s", code, body)
	}
	for _, drop := range []string{"name", "image", "cpu", "env", "cmd"} {
		b := v2Body(map[string]any{"name": "n", "env": env})
		delete(b, drop)
		if code, body := postRaw(t, f, b); code != 422 {
			t.Errorf("without %s: %d %s", drop, code, body)
		}
	}
	for name, mut := range map[string]func(map[string]any){
		"cpu.id":        func(b map[string]any) { b["cpu"] = map[string]any{"vcpuCount": 2} },
		"cpu.vcpuCount": func(b map[string]any) { b["cpu"] = map[string]any{"id": "cpu3c"} },
		"unknown key":   func(b map[string]any) { b["cloudType"] = "SECURE" },
	} {
		b := v2Body(map[string]any{"name": "n", "env": env})
		mut(b)
		if code, body := postRaw(t, f, b); code != 422 {
			t.Errorf("%s: %d %s", name, code, body)
		}
	}
	if n := len(f.Bodies()); n != 0 {
		t.Errorf("%d pods were made from invalid bodies", n)
	}
}
