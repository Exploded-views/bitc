// Linux-only: these run the real agent (buildBitc), which is Linux-only by design.

package runpodfake

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

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
