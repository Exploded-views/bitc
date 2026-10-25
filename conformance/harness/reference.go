package harness

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The reference plugin of providers delta P.2 and its fake API, built from this module by the
// test binary's TestMain (BuildReference) and installed per test with PluginFrom / StartFakeAPI.
const (
	refPluginPkg  = "github.com/Exploded-views/bitc/examples/plugins/bitc-provider-fakex"
	refFakeAPIPkg = refPluginPkg + "/fakeapi"
)

// RefPlugin and RefFakeAPI are the built reference plugin and fake API (BuildReference).
var RefPlugin, RefFakeAPI string

// BuildReference builds the reference plugin and its fake API into a new temp dir, reproducibly
// (-trimpath, CGO off), and sets RefPlugin and RefFakeAPI. The caller removes dir.
func BuildReference() (dir string, err error) {
	if dir, err = os.MkdirTemp("", "bitc-refplugin-"); err != nil {
		return "", err
	}
	for _, b := range []struct {
		out *string
		pkg string
	}{{&RefPlugin, refPluginPkg}, {&RefFakeAPI, refFakeAPIPkg}} {
		*b.out = filepath.Join(dir, filepath.Base(b.pkg))
		build := exec.Command("go", "build", "-trimpath", "-o", *b.out, b.pkg)
		build.Env = append(os.Environ(), "CGO_ENABLED=0")
		if out, err := build.CombinedOutput(); err != nil {
			return dir, fmt.Errorf("build %s: %w\n%s", b.pkg, err, out)
		}
	}
	return dir, nil
}

// APICall is one call the fake API recorded (the /__bitc/calls contract, design A.10.2).
type APICall struct {
	Seq     int               `json:"seq"`
	Method  string            `json:"method"`
	Path    string            `json:"path"`
	Query   string            `json:"query"`
	Headers map[string]string `json:"headers"`
	Body    string            `json:"body"`
	Attach  bool              `json:"attach"`
}

// FakeAPI is a running fake API process (a plugin author's loopback fake of their API) and the
// client of its /__bitc/ control contract.
type FakeAPI struct {
	URL string
	t   *testing.T
	cmd *exec.Cmd
}

// StartFakeAPI starts the fake API binary on a loopback port, with this bitc as every instance's
// agent, and stops it with the test. The binary prints "listening <url>" once it serves.
func (h *H) StartFakeAPI(bin string) *FakeAPI {
	h.T.Helper()
	cmd := exec.Command(bin, "-bitc", Bin, "-root", filepath.Join(h.Root, "fakeapi"), "-listen", "127.0.0.1:0")
	cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	out, err := cmd.StdoutPipe()
	if err != nil {
		h.T.Fatal(err)
	}
	var errb bytes.Buffer
	cmd.Stderr = &errb
	if err := cmd.Start(); err != nil {
		h.T.Fatal(err)
	}
	f := &FakeAPI{t: h.T, cmd: cmd}
	h.T.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		done := make(chan struct{})
		go func() { _ = cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			<-done
		}
	})
	line := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			if u, ok := strings.CutPrefix(sc.Text(), "listening "); ok {
				line <- u
				break
			}
		}
		_, _ = io.Copy(io.Discard, out)
	}()
	select {
	case f.URL = <-line:
	case <-time.After(20 * time.Second):
		h.T.Fatalf("the fake API did not start: %s", errb.String())
	}
	return f
}

func (f *FakeAPI) do(method, path string, body any, into any) {
	f.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, f.URL+path, rd)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		f.t.Fatalf("fake API %s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(resp.Body)
		f.t.Fatalf("fake API %s %s: %d %s", method, path, resp.StatusCode, b)
	}
	if into != nil {
		if err := json.NewDecoder(resp.Body).Decode(into); err != nil {
			f.t.Fatalf("fake API %s %s: %v", method, path, err)
		}
	}
}

// Behave is POST /__bitc/behave: the behaviour the fake shows to instances created from now on.
func (f *FakeAPI) Behave(b string) {
	f.t.Helper()
	f.do("POST", "/__bitc/behave", map[string]string{"behaviour": b}, nil)
}

// Calls is GET /__bitc/calls: every API call in order, the attach call marked.
func (f *FakeAPI) Calls() []APICall {
	f.t.Helper()
	var cs []APICall
	f.do("GET", "/__bitc/calls", nil, &cs)
	return cs
}

// Live is GET /__bitc/live: the names of the instances the fake holds.
func (f *FakeAPI) Live() []string {
	f.t.Helper()
	var l []string
	f.do("GET", "/__bitc/live", nil, &l)
	return l
}

// Torn is GET /__bitc/torn: the ids of the instances deleted so far.
func (f *FakeAPI) Torn() []string {
	f.t.Helper()
	var l []string
	f.do("GET", "/__bitc/torn", nil, &l)
	return l
}

// SetSpend sets the spend the fake's billing endpoint reports (optional part of the contract).
func (f *FakeAPI) SetSpend(usd float64) {
	f.t.Helper()
	f.do("POST", "/__bitc/spend", map[string]float64{"usd": usd}, nil)
}

// Seed lists an instance with no process (optional part of the contract); it returns its id.
func (f *FakeAPI) Seed(name, owner string, created, expires time.Time) string {
	f.t.Helper()
	var r struct{ ID string }
	f.do("POST", "/__bitc/seed", map[string]string{"name": name, "owner": owner,
		"created": created.UTC().Format(time.RFC3339), "expires": expires.UTC().Format(time.RFC3339)}, &r)
	return r.ID
}

// Host is the host part of the fake's URL.
func (f *FakeAPI) Host() string { u, _ := url.Parse(f.URL); return u.Host }

var _ = fmt.Sprint
