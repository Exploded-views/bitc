package plugin

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/Exploded-views/bitc/conformance/harness"
	"github.com/Exploded-views/bitc/internal/agent"
)

// The protocol suite (providers delta A.10.1): a scripted bitc side drives a plugin binary over
// stdin and stdout, as bitc does, and checks what the plugin must do whatever it is a plugin for.
// The reference plugin is the default; -plugin and -plugin-config name another.

// lockedBuf is a bytes.Buffer safe for exec's copier and the test to use together.
type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}
func (l *lockedBuf) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

// peer is the scripted bitc side of one plugin process.
type peer struct {
	t      *testing.T
	cmd    *exec.Cmd
	in     io.WriteCloser
	stderr lockedBuf
	mu     sync.Mutex
	got    []map[string]json.RawMessage
	sig    chan struct{}
	done   chan struct{}
	nextID uint64
	state  string
}

// startPeer spawns bin the way bitc does (providers delta A.3): an empty environment plus the
// BITC_* variables, stdin and stdout pipes, and the secrets as one JSON object on fd 3.
func startPeer(t *testing.T, bin string, secrets map[string]string) *peer {
	t.Helper()
	dir := t.TempDir()
	state := filepath.Join(dir, "state")
	tmp := filepath.Join(dir, "tmp")
	shared := filepath.Join(dir, "shared")
	for _, d := range []string{state, tmp, shared} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	sr, sw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin)
	cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "HOME=" + dir, "LANG=C.UTF-8", "TMPDIR=" + tmp,
		"BITC_PLUGIN_STATE=" + state, "BITC_PLUGIN_SHARED=" + shared, "BITC_PLUGIN_PROTOCOL=1", "BITC_SECRETS_FD=3"}
	cmd.ExtraFiles = []*os.File{sr}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	in, _ := cmd.StdinPipe()
	out, _ := cmd.StdoutPipe()
	p := &peer{t: t, cmd: cmd, in: in, sig: make(chan struct{}, 1), done: make(chan struct{}), state: state}
	cmd.Stderr = &p.stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	sr.Close()
	b, _ := json.Marshal(secrets)
	_, _ = sw.Write(b)
	sw.Close()
	go func() {
		rd := bufio.NewReaderSize(out, 4<<20)
		for {
			line, err := rd.ReadBytes('\n')
			if len(bytes.TrimSpace(line)) > 0 {
				if len(line) > 1<<20+1 {
					t.Errorf("a frame of %d bytes: lines are at most 1 MiB (A.4.1)", len(line))
				}
				var f map[string]json.RawMessage
				if json.Unmarshal(line, &f) != nil {
					t.Errorf("stdout carries something that is not one JSON object per line: %.80q", line)
				} else {
					p.mu.Lock()
					p.got = append(p.got, f)
					p.mu.Unlock()
					select {
					case p.sig <- struct{}{}:
					default:
					}
				}
			}
			if err != nil {
				break
			}
		}
		_ = cmd.Wait()
		close(p.done)
	}()
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-p.done
	})
	return p
}

// send writes one request and returns its id.
func (p *peer) send(method string, params any) uint64 {
	p.t.Helper()
	p.nextID++
	return p.sendID(p.nextID, method, params)
}

func (p *peer) sendID(id uint64, method string, params any) uint64 {
	p.t.Helper()
	b, _ := json.Marshal(map[string]any{"id": id, "method": method, "params": params, "future_field": map[string]int{"x": 1}})
	if _, err := p.in.Write(append(b, '\n')); err != nil {
		p.t.Fatalf("write %s: %v", method, err)
	}
	return id
}

// frames returns the frames received for id, in order.
func (p *peer) frames(id uint64) []map[string]json.RawMessage {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []map[string]json.RawMessage
	for _, f := range p.got {
		var fid uint64
		if json.Unmarshal(f["id"], &fid) == nil && fid == id {
			out = append(out, f)
		}
	}
	return out
}

// await waits for the final frame (result or error) of id and returns it with the events before it.
func (p *peer) await(id uint64, d time.Duration) (final map[string]json.RawMessage, events []map[string]json.RawMessage) {
	p.t.Helper()
	deadline := time.After(d)
	for {
		for _, f := range p.frames(id) {
			if _, ok := f["result"]; ok {
				return f, p.events(id)
			}
			if _, ok := f["error"]; ok {
				return f, p.events(id)
			}
		}
		select {
		case <-p.sig:
		case <-time.After(20 * time.Millisecond):
		case <-deadline:
			p.t.Fatalf("no final frame for request %d within %s; stderr: %s", id, d, p.stderr.String())
		case <-p.done:
			p.t.Fatalf("the plugin exited before answering request %d; stderr: %s", id, p.stderr.String())
		}
	}
}

func (p *peer) events(id uint64) []map[string]json.RawMessage {
	var out []map[string]json.RawMessage
	for _, f := range p.frames(id) {
		if _, ok := f["event"]; ok {
			out = append(out, f)
		}
	}
	return out
}

// call is send + await, 30 s.
func (p *peer) call(method string, params any) (final map[string]json.RawMessage, events []map[string]json.RawMessage) {
	p.t.Helper()
	return p.await(p.send(method, params), 30*time.Second)
}

// result decodes the result of a call that must have succeeded.
func (p *peer) result(method string, params any, into any) []map[string]json.RawMessage {
	p.t.Helper()
	f, ev := p.call(method, params)
	if e, bad := f["error"]; bad {
		p.t.Fatalf("%s: error %s", method, e)
	}
	if into != nil {
		if err := json.Unmarshal(f["result"], into); err != nil {
			p.t.Fatalf("%s: result %s: %v", method, f["result"], err)
		}
	}
	return ev
}

func str(raw json.RawMessage) string { var s string; _ = json.Unmarshal(raw, &s); return s }

// refPeer starts the plugin under test, with a fake API for the reference one, and runs hello and
// configure; sizes are the host prices.
func refPeer(t *testing.T, secrets map[string]string) (*peer, *harness.H, *harness.FakeAPI) {
	t.Helper()
	h := harness.New(t)
	bin, cfg := harness.RefPlugin, map[string]any{}
	var api *harness.FakeAPI
	if *pluginFlag != "" {
		bin = *pluginFlag
		if *configFlag != "" {
			if err := json.Unmarshal([]byte(*configFlag), &cfg); err != nil {
				t.Fatal(err)
			}
		}
	} else {
		api = h.StartFakeAPI(harness.RefFakeAPI)
		cfg["api_base"] = api.URL
	}
	p := startPeer(t, bin, secrets)
	var hello struct {
		Type string `json:"type"`
	}
	p.result("hello", map[string]any{"protocols": []int{1}, "bitc_version": "1.4.0", "type": strings.TrimPrefix(filepath.Base(bin), "bitc-provider-")}, &hello)
	p.result("configure", map[string]any{"config": cfg, "sizes": map[string]any{"cpu8": map[string]any{"usd_per_hour": 0.24}}}, nil)
	return p, h, api
}

// PT1: hello declares the protocol, type, version and all seven caps; configure answers endpoints,
// arch and a max_name of at least 60; requests with fields the plugin does not know are answered.
func TestPT1_HelloAndConfigureAnswerTheRequiredFields(t *testing.T) {
	h := harness.New(t)
	bin, cfg := harness.RefPlugin, map[string]any{}
	if *pluginFlag != "" {
		bin = *pluginFlag
		_ = json.Unmarshal([]byte(*configFlag), &cfg)
	} else {
		cfg["api_base"] = h.StartFakeAPI(harness.RefFakeAPI).URL
	}
	typ := strings.TrimPrefix(filepath.Base(bin), "bitc-provider-")
	p := startPeer(t, bin, map[string]string{"api_key": "sk-pt-0123456789"})
	var hello struct {
		Protocol      *int            `json:"protocol"`
		Type          *string         `json:"type"`
		PluginVersion *string         `json:"plugin_version"`
		Caps          map[string]any  `json:"caps"`
		Secrets       json.RawMessage `json:"secrets"`
	}
	p.result("hello", map[string]any{"protocols": []int{1}, "bitc_version": "1.4.0", "type": typ}, &hello)
	if hello.Protocol == nil || *hello.Protocol != 1 || hello.Type == nil || *hello.Type != typ || hello.PluginVersion == nil || *hello.PluginVersion == "" {
		t.Errorf("hello = %+v, want protocol 1, type %q and a plugin_version", hello, typ)
	}
	for _, k := range []string{"live_logs", "artifacts", "spend", "self_delete", "fixed", "multi_job"} {
		if _, ok := hello.Caps[k].(bool); !ok {
			t.Errorf("caps.%s = %v, want a boolean", k, hello.Caps[k])
		}
	}
	if fb, _ := hello.Caps["fallback_until"].(string); fb != "resource" && fb != "attach" {
		t.Errorf("caps.fallback_until = %v, want resource or attach", hello.Caps["fallback_until"])
	}
	var conf struct {
		Endpoints *[]string `json:"endpoints"`
		Arch      *[]string `json:"arch"`
		MaxName   *int      `json:"max_name"`
	}
	p.result("configure", map[string]any{"config": cfg, "sizes": map[string]any{"cpu8": map[string]any{"usd_per_hour": 0.24}}}, &conf)
	if conf.Endpoints == nil || conf.Arch == nil || conf.MaxName == nil || *conf.MaxName < 60 || len(*conf.Arch) == 0 {
		t.Errorf("configure = %+v, want endpoints, a non-empty arch and max_name >= 60", conf)
	}
}

// PT2: one final frame per request id, however many are open at once and in whatever order; every
// frame is one JSON object per line; an unknown method is answered with an error, not a crash.
func TestPT2_ConcurrentRequestsGetOneFinalEachAndUnknownMethodsAnError(t *testing.T) {
	p, _, _ := refPeer(t, map[string]string{"api_key": "sk-pt-0123456789"})
	ids := []uint64{p.sendID(900, "list", map[string]any{"prefixes": []string{"bitc-"}}),
		p.sendID(7, "owner", map[string]any{"id": "no-such-resource"}),
		p.sendID(31, "no_such_method", map[string]any{}),
		p.sendID(5, "list", map[string]any{"prefixes": []string{"other-"}})}
	for _, id := range ids {
		p.await(id, 30*time.Second)
	}
	time.Sleep(200 * time.Millisecond)
	for _, id := range ids {
		finals := 0
		for _, f := range p.frames(id) {
			if _, ok := f["result"]; ok {
				finals++
			}
			if _, ok := f["error"]; ok {
				finals++
			}
		}
		if finals != 1 {
			t.Errorf("request %d got %d final frames, want exactly one", id, finals)
		}
	}
	f := p.frames(31)[0]
	var eo struct{ Class, Why string }
	_ = json.Unmarshal(f["error"], &eo)
	switch eo.Class {
	case "ineligible", "transient", "unavailable", "unavailable_local":
	default:
		t.Errorf("an unknown method answered %s, want an error of a class in the A.6 table", f["error"])
	}
	var o struct{ Owner *string }
	_ = json.Unmarshal(p.frames(7)[0]["result"], &o)
	if o.Owner == nil || *o.Owner != "" {
		t.Errorf("owner of an unknown resource = %s, want {\"owner\": \"\"}", p.frames(7)[0]["result"])
	}
	var l struct{ Resources *[]json.RawMessage }
	_ = json.Unmarshal(p.frames(5)[0]["result"], &l)
	if l.Resources == nil || len(*l.Resources) != 0 {
		t.Errorf("list of an unused prefix = %s, want {\"resources\": []}", p.frames(5)[0]["result"])
	}
	select {
	case <-p.done:
		t.Error("the plugin exited")
	default:
	}
}

// job is a wire-schema job object (A.5.1) for a plugin that runs bitc's own agent, from the repo r.
func scripted(t *testing.T, h *harness.H, name, script string, nonce string) map[string]any {
	t.Helper()
	wt := h.Repo("app", map[string]string{"README": "x\n"})
	sha, err := exec.Command("git", "-C", wt, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	hex32 := func(b byte) string { return strings.Repeat(fmt.Sprintf("%02x", b), 32) }
	return map[string]any{"job": "pt1", "pool": "heavy", "cwd": ".", "clone_url": h.Origin("app"), "ref": "refs/heads/main",
		"sha": strings.TrimSpace(string(sha)), "size_key": "cpu8", "repo_id": "r", "install": "0123456789abcdef", "name": name,
		"owner": strings.Repeat("ab", 32), "agent_url": "http://127.0.0.1:9/bitc", "agent_sha256": strings.Repeat("00", 32),
		"argv": []string{"sh", "-c", script}, "env": []string{}, "timeout_s": 120,
		"created": now.Format(time.RFC3339), "deadline": now.Add(3 * time.Minute).Format(time.RFC3339), "expiry": now.Add(20 * time.Minute).Format(time.RFC3339),
		"bearer": hex32(0x0b), "mac_key": hex32(0x0c), "nonce": nonce, "artifacts": []any{}, "packages": []string{}, "tools": []any{},
		"setup": []any{}, "services": []any{}, "root_steps": false, "test": map[string]any{"unprivileged": true, "test_root": ""}}
}

// PT3: the life of a job over the protocol: launch answers the assigned name, watch streams log
// lines with the agent's markers and out events with the output and ends with exit and began,
// output serves a result whose MAC verifies under K, teardown is idempotent and list stops showing
// the resource. A plugin only ever reports booting or lost as a phase.
func TestPT3_ALaunchedJobIsWatchedReadAndTornDown(t *testing.T) {
	if *pluginFlag != "" {
		t.Skip("the scripted job needs the reference fake's agent")
	}
	p, h, _ := refPeer(t, map[string]string{"api_key": "sk-pt-0123456789"})
	nonce := strings.Repeat("0d", 32)
	name := "bitc-0123456789abcdef-heavy-cpu8-" + "0badf00d" + "-" + strconv.FormatInt(time.Now().Add(20*time.Minute).Unix(), 10)
	job := scripted(t, h, name, "echo pt-ok; echo pt-err >&2", nonce)
	var launched struct{ ID, Name string }
	setup := p.result("launch", map[string]any{"job": job}, &launched)
	if launched.ID == "" || launched.Name != name {
		t.Fatalf("launch result = %+v, want an id and the assigned name %q", launched, name)
	}
	for _, ev := range setup {
		if str(ev["event"]) != "setup" {
			t.Errorf("launch event %s: only setup lines are allowed", ev["event"])
		}
	}
	handle := map[string]any{"id": launched.ID, "name": launched.Name}
	var res struct {
		Exit  *int  `json:"exit"`
		Began *bool `json:"began"`
	}
	events := p.result("watch", map[string]any{"handle": handle}, &res)
	if res.Exit == nil || *res.Exit != 0 || res.Began == nil || !*res.Began {
		t.Errorf("watch result = %+v, want exit 0 and began", res)
	}
	var sawBegin, sawExit bool
	var stdout, stderrOut string
	for _, ev := range events {
		switch str(ev["event"]) {
		case "log":
			l := str(ev["line"])
			sawBegin = sawBegin || l == "__BITC_BEGIN__ "+nonce
			sawExit = sawExit || l == "__BITC_EXIT__ "+nonce+" 0"
		case "phase":
			if ph := str(ev["phase"]); ph != "booting" && ph != "lost" {
				t.Errorf("a phase event %q: a plugin may only report booting or lost", ph)
			}
		case "out":
			b, err := base64.StdEncoding.DecodeString(str(ev["data"]))
			if err != nil {
				t.Errorf("out data is not base64: %v", err)
			}
			if str(ev["stream"]) == "stdout" {
				stdout += string(b)
			} else if str(ev["stream"]) == "stderr" {
				stderrOut += string(b)
			}
		}
	}
	if !sawBegin || !sawExit {
		t.Errorf("the log events lack the agent's markers (begin %v, exit %v)", sawBegin, sawExit)
	}
	read := func(stream string) []byte {
		var buf bytes.Buffer
		for _, ev := range p.result("output", map[string]any{"handle": handle, "stream": stream, "offset": 0}, nil) {
			if str(ev["event"]) == "data" {
				b, _ := base64.StdEncoding.DecodeString(str(ev["data"]))
				buf.Write(b)
			}
		}
		return buf.Bytes()
	}
	K, _ := hexDecode(strings.Repeat("0c", 32))
	rf, err := agent.VerifyResult(K, read("result"))
	if err != nil || rf.Exit != 0 {
		t.Errorf("result.json does not verify under K or has the wrong exit: %+v %v", rf, err)
	}
	if got := string(read("stdout")); got != "pt-ok\n" {
		t.Errorf("output stdout = %q", got)
	}
	if got := string(read("stderr")); got != "pt-err\n" {
		t.Errorf("output stderr = %q", got)
	}
	var lst struct{ Resources []struct{ ID, Name string } }
	p.result("list", map[string]any{"prefixes": []string{name}}, &lst)
	if len(lst.Resources) != 1 || lst.Resources[0].Name != name {
		t.Errorf("list = %+v, want the launched resource", lst)
	}
	var owner struct{ Owner string }
	p.result("owner", map[string]any{"id": launched.ID}, &owner)
	if owner.Owner != strings.Repeat("ab", 32) {
		t.Errorf("owner = %q, want the tag the job carried", owner.Owner)
	}
	for i := 0; i < 2; i++ { // idempotent: "gone" is success
		p.result("teardown", map[string]any{"id": launched.ID}, nil)
	}
	p.result("list", map[string]any{"prefixes": []string{name}}, &lst)
	if len(lst.Resources) != 0 {
		t.Errorf("list after teardown = %+v", lst)
	}
}

func hexDecode(s string) ([]byte, error) {
	b := make([]byte, len(s)/2)
	for i := range b {
		v, err := strconv.ParseUint(s[2*i:2*i+2], 16, 8)
		if err != nil {
			return nil, err
		}
		b[i] = byte(v)
	}
	return b, nil
}

// PT4: after a cancel request (id 0) the open call is answered within 5 s, so bitc need not kill
// the plugin.
func TestPT4_ACancelledWatchIsAnsweredPromptly(t *testing.T) {
	if *pluginFlag != "" {
		t.Skip("the scripted job needs the reference fake's agent")
	}
	p, h, _ := refPeer(t, map[string]string{"api_key": "sk-pt-0123456789"})
	nonce := strings.Repeat("0d", 32)
	name := "bitc-0123456789abcdef-heavy-cpu8-0badf00d-" + strconv.FormatInt(time.Now().Add(20*time.Minute).Unix(), 10)
	var launched struct{ ID, Name string }
	p.result("launch", map[string]any{"job": scripted(t, h, name, "sleep 60", nonce)}, &launched)
	id := p.send("watch", map[string]any{"handle": map[string]any{"id": launched.ID, "name": launched.Name}})
	h.Until(func() bool {
		for _, ev := range p.events(id) {
			if str(ev["event"]) == "log" && strings.HasPrefix(str(ev["line"]), "__BITC_BEGIN__") {
				return true
			}
		}
		return false
	}, 30*time.Second)
	p.sendID(0, "cancel", map[string]any{"id": id})
	p.await(id, 5*time.Second)
	p.result("teardown", map[string]any{"id": launched.ID}, nil)
}

// PT5: when bitc closes stdin the plugin exits within 2 s.
func TestPT5_ThePluginExitsWhenStdinCloses(t *testing.T) {
	p, _, _ := refPeer(t, map[string]string{"api_key": "sk-pt-0123456789"})
	_ = p.in.Close()
	select {
	case <-p.done:
	case <-time.After(2500 * time.Millisecond):
		t.Fatal("the plugin was still running 2.5 s after stdin closed")
	}
}

// PT6: no secret is in the plugin's argv or environment, or in those of its children.
func TestPT6_SecretsAreInNoArgvOrEnvironment(t *testing.T) {
	secrets := map[string]string{"api_key": "sk-pt-secret-AAAA1111", "token_secret": "tok-pt-secret-BBBB2222"}
	p, _, _ := refPeer(t, secrets)
	pids := []int{p.cmd.Process.Pid}
	ents, _ := filepath.Glob("/proc/[0-9]*/stat")
	for _, s := range ents {
		b, err := os.ReadFile(s)
		if err != nil {
			continue
		}
		f := strings.Fields(string(b[strings.LastIndexByte(string(b), ')')+2:]))
		if len(f) > 1 && f[1] == strconv.Itoa(p.cmd.Process.Pid) {
			pid, _ := strconv.Atoi(strings.Split(strings.TrimPrefix(s, "/proc/"), "/")[0])
			pids = append(pids, pid)
		}
	}
	for _, pid := range pids {
		for _, f := range []string{"cmdline", "environ"} {
			b, err := os.ReadFile(fmt.Sprintf("/proc/%d/%s", pid, f))
			if err != nil {
				t.Errorf("read /proc/%d/%s: %v", pid, f, err)
				continue
			}
			for n, v := range secrets {
				if bytes.Contains(b, []byte(v)) {
					t.Errorf("/proc/%d/%s holds the %s secret", pid, f, n)
				}
			}
		}
	}
	b, _ := os.ReadFile(fmt.Sprintf("/proc/%d/environ", p.cmd.Process.Pid))
	if !bytes.Contains(b, []byte("BITC_SECRETS_FD=3")) {
		t.Error("the plugin's environment lacks BITC_SECRETS_FD=3")
	}
}

// PT7: a launch cancelled while the created instance is still provisioning is answered with an
// error that names the resource (an id, or pre_attach with the assigned name), never a bare
// transient: bitc must be able to tear the instance down.
func TestPT7_ACancelledLaunchAfterCreateStillNamesTheResource(t *testing.T) {
	if *pluginFlag != "" {
		t.Skip("needs the reference fake's provisioning behaviour")
	}
	p, h, api := refPeer(t, map[string]string{"api_key": "sk-pt-0123456789"})
	api.Behave("provisioning-forever")
	name := "bitc-0123456789abcdef-heavy-cpu8-0badf00d-" + strconv.FormatInt(time.Now().Add(20*time.Minute).Unix(), 10)
	id := p.send("launch", map[string]any{"job": scripted(t, h, name, "true", strings.Repeat("0d", 32))})
	h.Until(func() bool { return len(api.Live()) == 1 }, 20*time.Second)
	p.sendID(0, "cancel", map[string]any{"id": id})
	final, _ := p.await(id, 5*time.Second)
	var eo struct{ Class, Why, ID string }
	if _, ok := final["error"]; !ok {
		t.Fatalf("a cancelled launch answered %v, want an error", final)
	}
	_ = json.Unmarshal(final["error"], &eo)
	if eo.ID == "" {
		t.Errorf("a launch cancelled after create answered %s, want an error that carries the id (class pre_attach with the assigned name)", final["error"])
	}
}

// PT8: hello declares the plugin's own type, whatever type bitc asked for, so core's check that
// a differing type is a protocol error (A.4.4) has something to catch.
func TestPT8_HelloDeclaresThePluginsOwnType(t *testing.T) {
	if *pluginFlag != "" {
		t.Skip("the type of the reference plugin")
	}
	p := startPeer(t, harness.RefPlugin, map[string]string{"api_key": "sk-pt-0123456789"})
	var hello struct {
		Type string `json:"type"`
	}
	p.result("hello", map[string]any{"protocols": []int{1}, "bitc_version": "1.4.0", "type": "somethingelse"}, &hello)
	if hello.Type != "fakex" {
		t.Errorf("hello answered type %q when asked for somethingelse, want fakex", hello.Type)
	}
}
