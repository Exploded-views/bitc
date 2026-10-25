package runpod

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Exploded-views/bitc/internal/provider"
)

func TestHTTPMapping(t *testing.T) {
	cases := []struct {
		code int
		body string
		kind string
	}{
		{400, `{"error":"There are no longer any instances available with the requested specifications"}`, "transient"},
		{400, `{"error":"bad gpu"}`, "ineligible"},
		{422, `{}`, "ineligible"},
		{401, ``, "unavailable"}, {402, ``, "unavailable"}, {403, ``, "unavailable"},
		{413, ``, "ineligible"}, {429, ``, "transient"}, {500, ``, "transient"}, {503, ``, "transient"},
		{0, ``, "transient"}, // network failure
	}
	for _, c := range cases {
		err := mapHTTP(c.code, []byte(c.body), "create")
		var ine *provider.ErrIneligible
		var tr *provider.ErrTransient
		var un *provider.ErrUnavailable
		got := map[bool]string{errors.As(err, &ine): "ineligible", errors.As(err, &tr): "transient", errors.As(err, &un): "unavailable"}[true]
		if got != c.kind {
			t.Errorf("%d %s: %v (%T)", c.code, c.body, got, err)
		}
	}
	if mapHTTP(201, nil, "create") != nil {
		t.Fatal("2xx is ok")
	}
}

func TestDecodeSettingsIsStrict(t *testing.T) {
	ok := map[string]any{"image": "debian", "sizes": map[string]any{"cpu8": map[string]any{"cpu_type": "cpu3c", "vcpu": int64(8), "usd_per_hour": 0.24}}}
	s, err := DecodeSettings(ok)
	if err != nil || s.APIBase != "https://api.runpod.io" || s.ProxyDomain != "proxy.runpod.net" || s.Cloud != "SECURE" || s.Port != 8090 || s.Disk != 40<<30 {
		t.Fatal(s, err)
	}
	ok["bogus"] = 1
	if _, err := DecodeSettings(ok); err == nil {
		t.Fatal("unknown key accepted")
	}
	delete(ok, "bogus")
	delete(ok, "image")
	if _, err := DecodeSettings(ok); err == nil {
		t.Fatal("missing image accepted")
	}
}

func TestOutputAndFetchRejectUnknownHandle(t *testing.T) {
	p := &Provider{}
	h := provider.Handle{ID: "never-launched", ResultsURL: "http://127.0.0.1:1/v1/"}
	if err := p.Output(context.Background(), h, provider.Stdout, 0, io.Discard); err == nil {
		t.Error("Output accepted an unknown handle")
	}
	if err := p.Fetch(context.Background(), h, provider.ArtifactSpec{}, io.Discard); err == nil {
		t.Error("Fetch accepted an unknown handle")
	}
}

func TestListReturnsAPodOnceWhenTwoPrefixesMatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[{"id":"p1","name":"bitc-x","status":"RUNNING","createdAt":"2026-10-01T00:00:00Z"}]`))
	}))
	defer srv.Close()
	p := &Provider{set: Settings{Sizes: map[string]Size{}}, api: &client{base: srv.URL, key: "k", http: srv.Client()}}
	list, err := p.List(context.Background(), []string{"bitc-", "bit"})
	if err != nil || len(list) != 1 {
		t.Fatalf("%+v %v", list, err)
	}
}
