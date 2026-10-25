package conformance

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Exploded-views/bitc/internal/agent"
)

var rsBearer = bytes.Repeat([]byte{9}, 32)

func rsServer(t *testing.T) (*httptest.Server, string) {
	dir := t.TempDir()
	for name, body := range map[string]string{"out.log": "out", "err.log": "err", "result.json": "{}", "state.json": "{}", "artifacts.tar.gz": "a", "result.json.part": "p", "x.txt": "x"} {
		_ = os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600)
	}
	h, err := agent.NewServer(dir, rsBearer)
	if err != nil {
		t.Fatal(err)
	}
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	return s, dir
}

// rsMode is one way to read a job's result files: the results server over HTTP (listener mode) or
// the exec twin, `__agent read` (no-listener mode, providers delta X.1). get takes the server path
// (/v1/<file>); the exec twin serves the same names from the same files, and anything the server
// would answer 404 is not found or a bad argument there. The bearer rule has no exec twin: the
// exec channel is the authorization.
type rsMode struct {
	name string
	get  func(t *testing.T, dir, path string) (int, string)
}

var rsModes = []rsMode{
	{"http", func(t *testing.T, dir, path string) (int, string) {
		h, err := agent.NewServer(dir, rsBearer)
		if err != nil {
			t.Fatal(err)
		}
		s := httptest.NewServer(h)
		defer s.Close()
		return rsGet(t, "GET", s.URL+path)
	}},
	{"exec", func(t *testing.T, dir, path string) (int, string) {
		var w bytes.Buffer
		file, ok := strings.CutPrefix(path, "/v1/")
		if !ok {
			return 404, ""
		}
		err := agent.ReadFile(filepath.Dir(dir), filepath.Base(dir), file, 0, &w)
		switch {
		case err == nil:
			return 200, w.String()
		case errors.Is(err, agent.ErrBadArg) || errors.Is(err, agent.ErrNotFound):
			if w.Len() != 0 {
				t.Errorf("%s: %q was written before the refusal", path, w.String())
			}
			return 404, ""
		}
		t.Fatalf("%s: %v", path, err)
		return 0, ""
	}},
}

// rsFiles writes the standard result files into a job directory named j1 under a fresh root.
func rsFiles(t *testing.T) string {
	dir := filepath.Join(t.TempDir(), "j1")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"out.log": "out", "err.log": "err", "result.json": "{}", "state.json": "{}", "artifacts.tar.gz": "a", "result.json.part": "p", "x.txt": "x"} {
		_ = os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600)
	}
	return dir
}

func rsGet(t *testing.T, method, url string) (int, string) {
	req, _ := http.NewRequest(method, url, nil)
	req.Header.Set("Authorization", "Bearer "+hex.EncodeToString(rsBearer))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestRS1_ServesOnlyTheResultFilesWithTheBearer(t *testing.T) {
	for _, m := range rsModes {
		t.Run(m.name, func(t *testing.T) {
			dir := rsFiles(t)
			for _, n := range []string{"state.json", "out.log", "err.log", "artifacts.tar.gz", "result.json"} {
				if c, _ := m.get(t, dir, "/v1/"+n); c != 200 {
					t.Errorf("%s: %d", n, c)
				}
			}
		})
	}
	s, _ := rsServer(t) // the bearer rule is the server's; exec has the channel instead
	resp, err := http.Get(s.URL + "/v1/out.log")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatal("served without the bearer")
	}
}

func TestRS2_PartFilesNeverServed(t *testing.T) {
	for _, m := range rsModes {
		t.Run(m.name, func(t *testing.T) {
			dir := rsFiles(t)
			_ = os.Remove(filepath.Join(dir, "err.log"))
			_ = os.WriteFile(filepath.Join(dir, "err.log.part"), []byte("p"), 0o600)
			for _, n := range []string{"result.json", "state.json", "artifacts.tar.gz"} {
				_ = os.Remove(filepath.Join(dir, n))
				_ = os.WriteFile(filepath.Join(dir, n+".part"), []byte("p"), 0o600)
			}
			for _, p := range []string{"/v1/err.log.part", "/v1/result.json.part", "/v1/result.json", "/v1/state.json", "/v1/artifacts.tar.gz"} {
				if c, b := m.get(t, dir, p); c != 404 || b != "" {
					t.Errorf("%s: %d %q", p, c, b)
				}
			}
			// the HTTP results server serves a running job's out.log/err.log from the .part (live
			// output, spec 8.7); the exec channel serves committed files only
			want := 404
			if m.name == "http" {
				want = 200
			}
			if c, _ := m.get(t, dir, "/v1/err.log"); c != want {
				t.Errorf("/v1/err.log with only a .part: %d, want %d", c, want)
			}
		})
	}
}

func TestRS3_NothingElseServedOrListed(t *testing.T) {
	for _, m := range rsModes {
		t.Run(m.name, func(t *testing.T) {
			dir := rsFiles(t)
			for _, p := range []string{"/v1/", "/v1/x.txt", "/", "/out.log", "/v2/out.log", "/v1/./out.log", "/v1/sub/out.log"} {
				if c, b := m.get(t, dir, p); c != 404 || b != "" {
					t.Errorf("%s: %d %q", p, c, b)
				}
			}
			// a link in the job directory is no result file either
			_ = os.Remove(filepath.Join(dir, "out.log"))
			_ = os.Symlink("/etc/hostname", filepath.Join(dir, "out.log"))
			if c, b := m.get(t, dir, "/v1/out.log"); c != 404 || b != "" {
				t.Errorf("a symlink was served: %d %q", c, b)
			}
		})
	}
}

func TestRS4_BadTargetsDoNotStopTheServer(t *testing.T) {
	s, _ := rsServer(t)
	conn, err := net.Dial("tcp", strings.TrimPrefix(s.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = conn.Write([]byte("GET /v1/%zz HTTP/1.1\r\nHost: x\r\n\r\n"))
	line, _ := bufio.NewReader(conn).ReadString('\n')
	conn.Close()
	if !strings.Contains(line, " 400 ") && !strings.Contains(line, " 404 ") {
		t.Fatalf("%q", line)
	}
	if c, _ := rsGet(t, "GET", s.URL+"/v1/out.log"); c != 200 {
		t.Fatal("server stopped serving")
	}
	// the exec twin: a malformed name is refused, and the next read still works
	dir := rsFiles(t)
	m := rsModes[1]
	if c, b := m.get(t, dir, "/v1/%zz"); c != 404 || b != "" {
		t.Fatalf("%d %q", c, b)
	}
	if c, b := m.get(t, dir, "/v1/out.log"); c != 200 || b != "out" {
		t.Fatalf("read stopped working: %d %q", c, b)
	}
}

func TestRS5_HeadAndOtherMethods(t *testing.T) {
	s, _ := rsServer(t)
	if c, b := rsGet(t, "HEAD", s.URL+"/v1/out.log"); c != 200 || b != "" {
		t.Fatal(c, b)
	}
	for _, m := range []string{"POST", "PUT", "DELETE", "PATCH"} {
		if c, _ := rsGet(t, m, s.URL+"/v1/out.log"); c != 405 {
			t.Errorf("%s: %d", m, c)
		}
	}
}

func TestRS6_RefusesWithoutA256BitBearer(t *testing.T) {
	if _, err := agent.NewServer(t.TempDir(), make([]byte, 32)); err == nil {
		t.Fatal("an all-zero bearer was accepted")
	}
}

func TestRE22_ArtifactsArchiveOnlyDeclaredExisting(t *testing.T) {
	wt := t.TempDir()
	_ = os.MkdirAll(filepath.Join(wt, "coverage"), 0o755)
	_ = os.WriteFile(filepath.Join(wt, "coverage", "c.txt"), []byte("c"), 0o644)
	var buf bytes.Buffer
	if err := agent.PackArtifacts(wt, []string{"coverage", "nope"}, &buf); err != nil || buf.Len() == 0 {
		t.Fatal(err)
	}
}

func TestRV1_ForgedResultRejected(t *testing.T) {
	k := bytes.Repeat([]byte{3}, 32)
	r := agent.Sign(k, agent.ResultFile{Exit: 1})
	r.Exit = 0
	raw := []byte(`{"exit":0,"out_bytes":0,"err_bytes":0,"out_sha256":"","err_sha256":"","artifacts_sha256":"","mac":"` + r.MAC + `"}`)
	if _, err := agent.VerifyResult(k, raw); err == nil {
		t.Fatal("forged result accepted")
	}
}

func TestRV2_ForeignNonceIsPlainOutput(t *testing.T) {
	if _, ok := agent.ParseMarker("__BITC_EXIT__ other 0", "mine"); ok {
		t.Fatal("foreign marker accepted")
	}
}
