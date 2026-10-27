// Command stubplugin is a minimal provider plugin for bitc's own tests (providers delta P.1): a
// protocol-1 peer with fault modes, driven by the `fault` key of its [providers.<t>] section
// (or STUB_HELLO_FAULT for faults before configure). It keeps its resources in
// $BITC_PLUGIN_STATE/res.json so a respawned process still lists what the dead one created.
// The reference plugin of P.2 supersedes it; nothing ships it.
package main

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
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Exploded-views/bitc/internal/exit"
)

type res struct {
	ID         string  `json:"id"`
	Name       string  `json:"name"`
	Owner      string  `json:"owner"`
	SizeKey    string  `json:"size_key"`
	USDPerHour float64 `json:"usd_per_hour"`
	Created    string  `json:"created"`
	Expires    string  `json:"expires"`
	State      string  `json:"state"`
	Nonce      string  `json:"nonce"`
}

type req struct {
	ID     uint64          `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

var (
	outMu  sync.Mutex
	out    = bufio.NewWriter(os.Stdout)
	cfg    = map[string]any{}
	state  = os.Getenv("BITC_PLUGIN_STATE")
	tmpDir = os.Getenv("TMPDIR")
	resMu  sync.Mutex
)

func send(v any) {
	b, _ := json.Marshal(v)
	raw(string(b))
}

func raw(s string) {
	outMu.Lock()
	defer outMu.Unlock()
	_, _ = out.WriteString(s + "\n")
	out.Flush()
}

func final(id uint64, result any) { send(map[string]any{"id": id, "result": result}) }
func fail(id uint64, class, why string, extra map[string]any) {
	e := map[string]any{"class": class, "why": why}
	for k, v := range extra {
		e[k] = v
	}
	send(map[string]any{"id": id, "error": e})
}
func event(id uint64, kind string, kv map[string]any) {
	m := map[string]any{"id": id, "event": kind}
	for k, v := range kv {
		m[k] = v
	}
	send(m)
}

// has reports whether the comma-separated `fault` key lists name; mode returns the first listed
// fault with the prefix (launch-echo aside, which only adds an echo).
func has(name string) bool {
	for _, f := range strings.Split(str("fault"), ",") {
		if f == name {
			return true
		}
	}
	return false
}

func mode(prefix string) string {
	for _, f := range strings.Split(str("fault"), ",") {
		if strings.HasPrefix(f, prefix) && f != "launch-echo" {
			return f
		}
	}
	return ""
}

func str(k string) string { s, _ := cfg[k].(string); return s }
func num(k string, def float64) float64 {
	switch n := cfg[k].(type) {
	case float64:
		return n
	}
	return def
}

func load() (rs []res) {
	b, err := os.ReadFile(filepath.Join(state, "res.json"))
	if err == nil {
		_ = json.Unmarshal(b, &rs)
	}
	return rs
}
func save(rs []res) {
	b, _ := json.Marshal(rs)
	_ = os.WriteFile(filepath.Join(state, "res.json"), b, 0o600)
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "sleeper" {
		sleeper()
		return
	}
	if f := os.Getenv("STUB_HELLO_FAULT"); f == "exit-before-hello" {
		os.Exit(3)
	}
	diag()
	in := bufio.NewReaderSize(os.Stdin, 4<<20)
	for {
		line, err := in.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			var r req
			if json.Unmarshal(line, &r) == nil {
				r := r
				line := append([]byte(nil), line...)
				if r.Method == "cancel" {
					_ = appendTo("cancel.log", strings.TrimSpace(string(r.Params)))
					continue
				}
				exit.Go(func() { handle(r, line) })
			}
		}
		if err != nil {
			if err != io.EOF {
				fmt.Fprintln(os.Stderr, "stub: read:", err)
			}
			os.Exit(0)
		}
	}
}

// diag records what the process was given: no-new-privs, env, argv, open fds and the fd 3 secrets.
func diag() {
	d := map[string]any{"env": os.Environ(), "args": os.Args, "tmpdir": os.Getenv("TMPDIR"), "pid": os.Getpid(), "pgid": syscall.Getpgrp()}
	st, _ := os.ReadFile("/proc/self/status")
	for _, l := range strings.Split(string(st), "\n") {
		if v, ok := strings.CutPrefix(l, "NoNewPrivs:"); ok {
			d["nonewprivs"] = strings.TrimSpace(v)
		}
	}
	ents, _ := os.ReadDir("/proc/self/fd")
	var fds []string
	for _, e := range ents {
		l, _ := os.Readlink("/proc/self/fd/" + e.Name())
		fds = append(fds, e.Name()+" "+l)
	}
	sort.Strings(fds)
	d["fds"] = fds
	if os.Getenv("BITC_SECRETS_FD") == "3" {
		if b, err := io.ReadAll(os.NewFile(3, "secrets")); err == nil {
			d["secrets"] = string(b)
		}
	}
	d["exe"], _ = os.Readlink("/proc/self/exe")
	d["cmdline"], _ = os.ReadFile("/proc/self/cmdline")
	d["cmdline"] = strings.ReplaceAll(fmt.Sprint(string(d["cmdline"].([]byte))), "\x00", " ")
	b, _ := json.Marshal(d)
	_ = os.WriteFile(filepath.Join(state, "diag.json"), b, 0o600)
}

func hang() { select {} }

// holdStderr leaves a session-leading helper that keeps this process's stderr pipe open until
// 150 ms after its parent is gone, so the adapter's Wait returns late (tmp-check).
func holdStderr() {
	c := exec.Command("/proc/self/exe", "sleeper")
	c.Stderr = os.Stderr
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	_ = c.Start()
}

func sleeper() {
	parent := os.Getppid()
	for os.Getppid() == parent {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(150 * time.Millisecond)
}

func handle(r req, line []byte) {
	if has("hang:" + r.Method) {
		_ = appendTo("hanging.log", r.Method) // the handshake a test waits on: this call is in flight
		hang()
	}
	for _, f := range strings.Split(str("fault"), ",") {
		if ms, ok := strings.CutPrefix(f, "slow:"+r.Method+"="); ok {
			var n int
			_, _ = fmt.Sscan(ms, &n)
			time.Sleep(time.Duration(n) * time.Millisecond)
		}
	}
	var p map[string]any
	_ = json.Unmarshal(r.Params, &p)
	switch r.Method {
	case "hello":
		hf := os.Getenv("STUB_HELLO_FAULT")
		caps := map[string]any{"live_logs": true, "artifacts": true, "spend": os.Getenv("STUB_SPEND") != "",
			"self_delete": false, "fallback_until": envOr("STUB_FALLBACK", "resource"), "fixed": false, "multi_job": os.Getenv("STUB_MULTI_JOB") != ""}
		h := map[string]any{"protocol": 1, "type": r.typ(p), "plugin_version": "0.0.1-stub", "caps": caps, "future": map[string]any{"x": 1}}
		if s := os.Getenv("STUB_DECLARE"); s != "" {
			h["secrets"] = strings.Split(s, ",")
		}
		switch hf {
		case "protocol2":
			h["protocol"] = 2
		case "other-type":
			h["type"] = "other"
		case "garbage":
			raw("not json at all")
			return
		case "no-caps":
			delete(h, "caps")
		}
		final(r.ID, h)
	case "configure":
		var c struct {
			Config map[string]any `json:"config"`
		}
		_ = json.Unmarshal(r.Params, &c)
		_ = os.WriteFile(filepath.Join(state, "configure.json"), r.Params, 0o600)
		cfg = c.Config
		if cfg == nil {
			cfg = map[string]any{}
		}
		if has("config-error") {
			fail(r.ID, "config", "region \"mars\" is not served", nil)
			return
		}
		eps := []any{}
		if l, ok := cfg["endpoints"].([]any); ok {
			eps = l
		}
		arch := []any{"amd64"}
		if l, ok := cfg["arch"].([]any); ok {
			arch = l
		}
		final(r.ID, map[string]any{"endpoints": eps, "arch": arch, "max_name": int(num("max_name", 63)), "extra": 1})
	case "preflight":
		if has("preflight-ineligible") {
			fail(r.ID, "ineligible", "request too large", nil)
			return
		}
		final(r.ID, map[string]any{})
	case "estimate":
		switch str("fault") {
		case "estimate-negative":
			final(r.ID, map[string]any{"usd_micros": -5})
		case "estimate-float":
			raw(fmt.Sprintf(`{"id":%d,"result":{"usd_micros":1.5}}`, r.ID))
		default:
			final(r.ID, map[string]any{"usd_micros": int(num("estimate_micros", 0))})
		}
	case "launch":
		launch(r, line, p)
	case "watch":
		watch(r, p)
	case "output":
		event(r.ID, "data", map[string]any{"data": base64.StdEncoding.EncodeToString([]byte(str("output")))})
		final(r.ID, map[string]any{"bytes": len(str("output"))})
	case "fetch":
		event(r.ID, "data", map[string]any{"data": base64.StdEncoding.EncodeToString([]byte("tarball"))})
		final(r.ID, map[string]any{"bytes": 7})
	case "owner":
		if has("tmp-check") { // the respawned process must still own its TMPDIR after the old one is reaped
			time.Sleep(300 * time.Millisecond)
			_, err := os.Stat(filepath.Join(tmpDir, "probe"))
			_ = appendTo("tmpcheck.log", fmt.Sprint(err == nil))
		}
		if has("owner-oversize") {
			raw(strings.Repeat("x", 2<<20))
			return
		}
		if has("owner-fails") {
			fail(r.ID, "transient", "owner lookup failed", nil)
			return
		}
		resMu.Lock()
		defer resMu.Unlock()
		for _, x := range load() {
			if x.ID == p["id"] {
				if has("owner-wrong") {
					final(r.ID, map[string]any{"owner": "forged"})
					return
				}
				final(r.ID, map[string]any{"owner": x.Owner})
				return
			}
		}
		final(r.ID, map[string]any{"owner": ""})
	case "teardown":
		if has("teardown-fails") {
			fail(r.ID, "transient", "teardown refused", nil)
			return
		}
		resMu.Lock()
		defer resMu.Unlock()
		var keep []res
		for _, x := range load() {
			if x.ID != p["id"] && x.Name != p["id"] {
				keep = append(keep, x)
			}
		}
		save(keep)
		_ = appendTo("torn.log", fmt.Sprint(p["id"]))
		final(r.ID, map[string]any{})
	case "list":
		list(r, p)
	case "spend":
		if has("spend-negative") {
			final(r.ID, map[string]any{"usd_micros": -1})
			return
		}
		final(r.ID, map[string]any{"usd_micros": int(num("spend_micros", 0))})
	default:
		fail(r.ID, "transient", "unknown method "+r.Method, nil)
	}
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func (r req) typ(p map[string]any) any { return p["type"] }

func appendTo(name, s string) error {
	f, err := os.OpenFile(filepath.Join(state, name), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(s + "\n")
	return err
}

func list(r req, p map[string]any) {
	f := mode("list-")
	if has("list-fails") {
		fail(r.ID, "transient", "list failed", nil)
		return
	}
	var prefixes []string
	if l, ok := p["prefixes"].([]any); ok {
		for _, x := range l {
			prefixes = append(prefixes, fmt.Sprint(x))
		}
	}
	if has("tmp-check") {
		_ = os.WriteFile(filepath.Join(tmpDir, "probe"), []byte("x"), 0o600)
	}
	resMu.Lock()
	defer resMu.Unlock()
	rs := []res{}
	for _, x := range load() {
		for _, pre := range prefixes {
			if strings.HasPrefix(x.Name, pre) {
				rs = append(rs, x)
				break
			}
		}
	}
	switch f {
	case "list-omits":
		rs = []res{}
	case "list-foreign-name":
		rs = append(rs, res{ID: "foreign", Name: "elsewhere-1", Owner: "x", USDPerHour: 0.1})
	case "list-negative-price":
		for i := range rs {
			rs[i].USDPerHour = -1
		}
	case "list-no-id":
		for i := range rs {
			rs[i].ID = ""
		}
	}
	final(r.ID, map[string]any{"resources": rs})
}

func launch(r req, line []byte, p map[string]any) {
	f := mode("launch-")
	job, _ := p["job"].(map[string]any)
	name, _ := job["name"].(string)
	owner, _ := job["owner"].(string)
	nonce, _ := job["nonce"].(string)
	size, _ := job["size_key"].(string)
	_ = os.WriteFile(filepath.Join(state, "launch.json"), line, 0o600)
	_ = appendTo("launches.log", name)
	if has("launch-echo") || has("launch-echo-malformed") {
		fmt.Fprintln(os.Stderr, string(line))
	}
	if f == "launch-transient-noid" {
		fail(r.ID, "transient", "capacity", nil)
		return
	}
	if f == "launch-ineligible-noid" {
		fail(r.ID, "ineligible", "too big", nil)
		return
	}
	resMu.Lock()
	save(append(load(), res{ID: "r-" + name, Name: name, Owner: owner, SizeKey: size, USDPerHour: num("price", 0.1),
		Created: time.Now().UTC().Format(time.RFC3339), Expires: time.Now().Add(time.Hour).UTC().Format(time.RFC3339), State: "running", Nonce: nonce}))
	resMu.Unlock()
	id := "r-" + name
	if has("tmp-check") {
		holdStderr()
	}
	switch f {
	case "launch-hang":
		hang()
	case "launch-die":
		_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
	case "launch-malformed":
		raw("{this is not json")
		hang()
	case "launch-unknown-class":
		fail(r.ID, "bogus", "who knows", nil)
	case "launch-unknown-class-id":
		fail(r.ID, "bogus", "who knows", map[string]any{"id": id})
	case "launch-transient-id":
		fail(r.ID, "transient", "capacity after create", map[string]any{"id": id})
	case "launch-transient-noid-created":
		fail(r.ID, "transient", "capacity", nil)
	case "launch-preattach":
		fail(r.ID, "pre_attach", "apt timeout", map[string]any{"id": name, "inner": map[string]any{"class": "transient", "why": "apt timeout"}})
	case "launch-preattach-wrong-id":
		fail(r.ID, "pre_attach", "apt timeout", map[string]any{"id": "something-else", "inner": map[string]any{"class": "transient", "why": "apt timeout"}})
	case "launch-attached":
		fail(r.ID, "attached", "stream dropped", map[string]any{"id": id, "inner": map[string]any{"class": "transient", "why": "stream dropped"}})
	case "launch-second-final":
		final(r.ID, map[string]any{"id": id, "name": name})
		final(r.ID, map[string]any{"id": id, "name": name})
	case "launch-phase-begun":
		event(r.ID, "phase", map[string]any{"phase": "begun"})
		hang()
	default:
		event(r.ID, "setup", map[string]any{"line": "creating " + name})
		final(r.ID, map[string]any{"id": id, "name": name, "results_url": "http://127.0.0.1:1/v1/"})
	}
}

func watch(r req, p map[string]any) {
	f := mode("watch-")
	h, _ := p["handle"].(map[string]any)
	id, _ := h["id"].(string)
	nonce := ""
	for _, x := range load() {
		if x.ID == id {
			nonce = x.Nonce
		}
	}
	log := func(l string) { event(r.ID, "log", map[string]any{"line": l}) }
	switch f {
	case "watch-silent":
		hang()
	case "watch-phase-begun":
		event(r.ID, "phase", map[string]any{"phase": "begun"})
		hang()
	case "watch-phase-lost":
		event(r.ID, "phase", map[string]any{"phase": "lost", "why": "resource restarted"})
		final(r.ID, map[string]any{"exit": 0, "began": false})
		return
	case "watch-setup-failed":
		log("__BITC_SETUP_FAILED__ " + nonce + " deps repo npm ERR missing script")
		final(r.ID, map[string]any{"exit": 0, "began": false})
		return
	case "watch-bad-nonce":
		log("__BITC_BEGIN__ " + strings.Repeat("f", 64))
		log("__BITC_EXIT__ " + strings.Repeat("f", 64) + " 7")
		final(r.ID, map[string]any{"exit": 0, "began": false})
		return
	}
	event(r.ID, "heartbeat", nil)
	log("__BITC_BOOT__ " + nonce)
	log("installing packages")
	log("__BITC_BEGIN__ " + nonce)
	event(r.ID, "out", map[string]any{"stream": "stdout", "data": base64.StdEncoding.EncodeToString([]byte("hello\n"))})
	event(r.ID, "out", map[string]any{"stream": "stderr", "data": base64.StdEncoding.EncodeToString([]byte("warn\n"))})
	code := 0
	if f == "watch-exit-mismatch" {
		code = 3
	}
	log(fmt.Sprintf("__BITC_EXIT__ %s %d", nonce, code))
	final(r.ID, map[string]any{"exit": 0, "began": true})
}
