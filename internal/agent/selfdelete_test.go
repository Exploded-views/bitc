package agent

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Exploded-views/bitc/internal/provider"
)

// A hook that makes a real request through the default transport, as the runpod hook does.
func requesting(hit *[]string) func(context.Context, provider.SelfDeleteEnv) error {
	return func(ctx context.Context, env provider.SelfDeleteEnv) error {
		req, _ := http.NewRequestWithContext(ctx, "DELETE", env.APIBase+"/v2/pods/"+env.ResourceID, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		_ = resp.Body.Close()
		*hit = append(*hit, env.APIBase)
		return nil
	}
}

func TestSelfDeleteTestModeRefusesNonLoopbackBase(t *testing.T) {
	var recorded []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { recorded = append(recorded, r.URL.Path) }))
	defer srv.Close()
	spec := Spec{Unprivileged: true, TestRoot: t.TempDir()}
	for _, base := range []string{"https://api.example.invalid", "", "http://192.0.2.5:1", "http://localhost.invalid", "ftp://127.0.0.1"} {
		var hit []string
		var log bytes.Buffer
		// Never touches the network: if the guard regresses the hook only records the call.
		recording := func(_ context.Context, env provider.SelfDeleteEnv) error {
			hit = append(hit, env.APIBase)
			return nil
		}
		err := selfDeleteHook(spec, BootPayload{ResourceID: "p", APIBase: base}, recording, &log)(context.Background())
		if err == nil || len(hit) != 0 || !strings.Contains(log.String(), "refusing") {
			t.Errorf("base %q: err=%v hit=%v log=%q", base, err, hit, log.String())
		}
	}
	if len(recorded) != 0 {
		t.Fatalf("a request was made: %v", recorded)
	}
	var hit []string
	if err := selfDeleteHook(spec, BootPayload{ResourceID: "p", APIBase: srv.URL}, requesting(&hit), &bytes.Buffer{})(context.Background()); err != nil || len(recorded) != 1 || recorded[0] != "/v2/pods/p" {
		t.Fatalf("loopback base not used: %v %v", err, recorded)
	}
}

func TestSelfDeleteProductionPassesTheBaseThrough(t *testing.T) {
	var got provider.SelfDeleteEnv
	del := func(_ context.Context, e provider.SelfDeleteEnv) error { got = e; return nil }
	if err := selfDeleteHook(Spec{}, BootPayload{ResourceID: "p", ScopedKey: "k", APIBase: "https://api.example"}, del, &bytes.Buffer{})(context.Background()); err != nil || got.APIBase != "https://api.example" || got.ResourceID != "p" {
		t.Fatalf("%v %+v", err, got)
	}
}
