package agent

import (
	"bytes"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

var bearer = bytes.Repeat([]byte{7}, 32)

func serve(t *testing.T) (*httptest.Server, string) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "out.log"), []byte("0123456789"), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "err.log.part"), []byte("partial"), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "secret.txt"), []byte("no"), 0o600)
	h, err := NewServer(dir, bearer)
	if err != nil {
		t.Fatal(err)
	}
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	return s, dir
}

func get(t *testing.T, method, url string, auth bool, hdr ...string) (int, string) {
	req, _ := http.NewRequest(method, url, nil)
	if auth {
		req.Header.Set("Authorization", "Bearer "+hex.EncodeToString(bearer))
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestServer(t *testing.T) {
	s, _ := serve(t)
	if c, b := get(t, "GET", s.URL+"/v1/out.log", true); c != 200 || b != "0123456789" {
		t.Fatal(c, b)
	}
	if c, b := get(t, "GET", s.URL+"/v1/out.log", true, "Range", "bytes=4-"); c != 206 || b != "456789" {
		t.Fatal(c, b)
	}
	for _, path := range []string{"/v1/out.log"} {
		if c, b := get(t, "GET", s.URL+path, false); c != 404 || b != "" {
			t.Fatal("no bearer:", c, b)
		}
	}
	for _, path := range []string{"/v1/err.log.part", "/v1/secret.txt", "/v1/", "/v1/../out.log", "/out.log", "/v1/out.log/x"} {
		if c, b := get(t, "GET", s.URL+path, true); c != 404 || b != "" {
			t.Errorf("%s: %d %q", path, c, b)
		}
	}
	if c, b := get(t, "HEAD", s.URL+"/v1/out.log", true); c != 200 || b != "" {
		t.Fatal(c, b)
	}
	if c, _ := get(t, "POST", s.URL+"/v1/out.log", true); c != 405 {
		t.Fatal(c)
	}
}

// The logs are served while they grow, by Range from an offset, and in full after the commit;
// result.json and the artifacts stay unserved until committed.
func TestServerServesGrowingLogsThenTheCommittedFile(t *testing.T) {
	root := t.TempDir()
	r, _ := NewResults(root, "j1")
	h, err := NewServer(r.Dir, bearer)
	if err != nil {
		t.Fatal(err)
	}
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	o, _ := r.Create("out.log")
	e, _ := r.Create("err.log")
	if c, b := get(t, "GET", s.URL+"/v1/out.log", true); c != 200 || b != "" {
		t.Fatal("empty running log:", c, b)
	}
	_, _ = o.WriteString("start\n")
	if c, b := get(t, "GET", s.URL+"/v1/out.log", true); c != 200 || b != "start\n" {
		t.Fatal(c, b)
	}
	_, _ = o.WriteString("second\n")
	_, _ = e.WriteString("warn\n")
	if c, b := get(t, "GET", s.URL+"/v1/out.log", true, "Range", "bytes=6-"); c != 206 || b != "second\n" {
		t.Fatal("range from the forwarded offset:", c, b)
	}
	if c, b := get(t, "GET", s.URL+"/v1/err.log", true); c != 200 || b != "warn\n" {
		t.Fatal(c, b)
	}
	for _, n := range []string{"result.json", "artifacts.tar.gz", "state.json"} {
		_ = os.WriteFile(filepath.Join(r.Dir, n+".part"), []byte("x"), 0o600)
		if c, b := get(t, "GET", s.URL+"/v1/"+n, true); c != 404 || b != "" {
			t.Fatalf("%s served before its commit: %d %q", n, c, b)
		}
	}
	o.Close()
	e.Close()
	if _, err := r.Finish(bytes.Repeat([]byte{1}, 32), 0, t.TempDir(), nil); err != nil {
		t.Fatal(err)
	}
	if c, b := get(t, "GET", s.URL+"/v1/out.log", true); c != 200 || b != "start\nsecond\n" {
		t.Fatal("after Finish:", c, b)
	}
	if c, b := get(t, "GET", s.URL+"/v1/out.log", true, "Range", "bytes=6-"); c != 206 || b != "second\n" {
		t.Fatal(c, b)
	}
	if c, _ := get(t, "GET", s.URL+"/v1/result.json", true); c != 200 {
		t.Fatal(c)
	}
}

func TestServerNeedsA256BitBearer(t *testing.T) {
	for _, b := range [][]byte{nil, make([]byte, 32), bytes.Repeat([]byte{1}, 16)} {
		if _, err := NewServer(t.TempDir(), b); err == nil {
			t.Errorf("%x accepted", b)
		}
	}
}
