// Package sshfake is the ssh provider's test double (providers delta C.11): a fake host gate.
// Under `[test] ssh_exec = "local"` the provider runs the sshfake gate executable instead of the
// system ssh client, with the gate's command line as arguments. The fake keeps a host in a root
// directory (slots, installed agents, jobs), records every call in order with the attach call
// marked, and fails calls as a script says, with the exit code and stderr an ssh client would
// give. Jobs run the installed agent (this module's bitc) in its unprivileged test mode.
package sshfake

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"crypto/sha256"

	"github.com/Exploded-views/bitc/internal/agent"
)

// Config is root/fake.json: the host the fake plays.
type Config struct {
	Slots int    `json:"slots"`
	Arch  string `json:"arch"`
}

// Rule scripts one failure. A call matches when its Op equals the rule's, Skip matching calls
// have passed and fewer than Times have been failed (Times < 0: every call). The failure prints
// Stdout, Stderr and exits with Exit. Read: the call first reads all of its stdin (the payload
// was written, then the connection dropped). After: the call does its work first, then fails (a
// lost acknowledgement).
type Rule struct {
	Op     string `json:"op"`
	Skip   int    `json:"skip,omitempty"`
	Times  int    `json:"times"`
	Exit   int    `json:"exit"`
	Stdout string `json:"stdout,omitempty"`
	Stderr string `json:"stderr,omitempty"`
	Read   bool   `json:"read,omitempty"`
	After  bool   `json:"after,omitempty"`
	// Mangle ("old=new", equal lengths) lets a tail through with every occurrence of old in its
	// data frames replaced by new: a log that never shows a marker (the EXIT of a lost job).
	Mangle string `json:"mangle,omitempty"`
}

// Call is one recorded call.
type Call struct {
	Op     string `json:"op"`
	Verb   string `json:"verb"`
	Attach bool   `json:"attach,omitempty"`
	Data   string `json:"data,omitempty"` // stdin; for a boot payload, the decoded spec too
}

// SlotOwner mirrors the host's slots/<i>/owner file.
type SlotOwner struct {
	Name       string `json:"name"`
	Nonce      string `json:"nonce"`
	Tag        string `json:"tag,omitempty"`
	Job        string `json:"job,omitempty"`
	Created    int64  `json:"created"`
	LeaseUntil int64  `json:"lease_until"`
}

// Init writes the fake host's config and an empty script into root.
func Init(root string, c Config) error {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return err
	}
	b, _ := json.Marshal(c)
	if err := os.WriteFile(filepath.Join(root, "fake.json"), b, 0o600); err != nil {
		return err
	}
	return SetScript(root, nil)
}

// SetScript replaces the failure script and resets its counters.
func SetScript(root string, rules []Rule) error {
	b, _ := json.Marshal(rules)
	_ = os.Remove(filepath.Join(root, "script.count"))
	return os.WriteFile(filepath.Join(root, "script.json"), b, 0o600)
}

// SetOwner plants an owner file, as another controller's lease would (tests).
func SetOwner(root string, slot int, o SlotOwner) error {
	dir := filepath.Join(root, "slots", strconv.Itoa(slot))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, _ := json.Marshal(o)
	return os.WriteFile(filepath.Join(dir, "owner"), b, 0o600)
}

// Calls reads the recorded calls in order.
func Calls(root string) []Call {
	var out []Call
	b, _ := os.ReadFile(filepath.Join(root, "calls.jsonl"))
	for _, l := range bytes.Split(b, []byte("\n")) {
		var c Call
		if json.Unmarshal(l, &c) == nil && c.Op != "" {
			out = append(out, c)
		}
	}
	return out
}

// Owners reads every slot's owner (nil for a free slot).
func Owners(root string) map[int]*SlotOwner {
	out := map[int]*SlotOwner{}
	ents, _ := os.ReadDir(filepath.Join(root, "slots"))
	for _, e := range ents {
		n, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		if o := readOwner(root, n); o != nil {
			out[n] = o
		}
	}
	return out
}

func readOwner(root string, n int) *SlotOwner {
	b, err := os.ReadFile(filepath.Join(root, "slots", strconv.Itoa(n), "owner"))
	if err != nil {
		return nil
	}
	var o SlotOwner
	if json.Unmarshal(b, &o) != nil || o.Name == "" {
		return nil
	}
	return &o
}

type gate struct {
	root   string
	conf   Config
	stdin  io.Reader
	stdout io.Writer
	stderr io.Writer
	unlock func()
}

// Main runs one call: args are `--root DIR [--] <the gate's command line, one argument>`. The
// command line is what the ssh client would send as the remote command, byte for byte, so the
// recorded verbs are exactly what the provider sent (the conformance suite replays them through
// the real scripts/ssh-gate.sh).
func Main(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) < 3 || args[0] != "--root" {
		fmt.Fprintln(stderr, "sshfake: usage: --root DIR -- verb...")
		return 64
	}
	root, rest := args[1], args[2:]
	if len(rest) > 0 && rest[0] == "--" {
		rest = rest[1:]
	}
	if len(rest) != 1 {
		fmt.Fprintln(stderr, "sshfake: usage: --root DIR -- 'verb ...'")
		return 64
	}
	g := &gate{root: root, stdin: stdin, stdout: stdout, stderr: stderr}
	b, err := os.ReadFile(filepath.Join(root, "fake.json"))
	if err != nil || json.Unmarshal(b, &g.conf) != nil {
		fmt.Fprintln(stderr, "sshfake: no fake.json in the root")
		return 64
	}
	unlock, err := g.lock()
	if err != nil {
		fmt.Fprintln(stderr, "sshfake:", err)
		return 69
	}
	g.unlock = unlock
	defer unlock()
	return g.dispatch(rest[0])
}

// lock serializes the fake's bookkeeping across the concurrent processes of a test (a tail runs
// for the whole job, so it releases the lock before it streams).
func (g *gate) lock() (func(), error) {
	f, err := os.OpenFile(filepath.Join(g.root, "lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	var once bool
	return func() {
		if !once {
			once = true
			f.Close()
		}
	}, nil
}

func (g *gate) record(c Call) {
	b, _ := json.Marshal(c)
	f, err := os.OpenFile(filepath.Join(g.root, "calls.jsonl"), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(b, '\n'))
}

// match returns the rule that fires for this call, and counts it.
func (g *gate) match(op string) *Rule {
	var rules []Rule
	b, _ := os.ReadFile(filepath.Join(g.root, "script.json"))
	_ = json.Unmarshal(b, &rules)
	counts := map[string]int{}
	cb, _ := os.ReadFile(filepath.Join(g.root, "script.count"))
	_ = json.Unmarshal(cb, &counts)
	var hit *Rule
	for i := range rules {
		r := rules[i]
		if r.Op != op {
			continue
		}
		key := strconv.Itoa(i)
		seen := counts[key]
		counts[key] = seen + 1
		if seen < r.Skip {
			continue
		}
		if r.Times >= 0 && seen-r.Skip >= r.Times {
			continue
		}
		hit = &r
		break
	}
	nb, _ := json.Marshal(counts)
	_ = os.WriteFile(filepath.Join(g.root, "script.count"), nb, 0o600)
	return hit
}

func (g *gate) fail(r *Rule) int {
	fmt.Fprint(g.stdout, r.Stdout)
	fmt.Fprint(g.stderr, r.Stderr)
	return r.Exit
}

// framed mirrors the gate's framing of the command line: single spaces between words, none at
// the ends, and the gate's character set (the replay through the real gate is the authority).
func framed(verb string) bool {
	if verb == "" || slices.Contains(strings.Split(verb, " "), "") {
		return false
	}
	return !strings.ContainsFunc(verb, func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && !strings.ContainsRune("._=+- ", r)
	})
}

func (g *gate) dispatch(verb string) int {
	invalid := func() int { // the real gate answers 64 with nothing on stdout; the call is kept for the replay
		g.record(Call{Op: "invalid", Verb: verb})
		return 64
	}
	if !framed(verb) {
		return invalid()
	}
	words := strings.Split(verb, " ")
	op, sha, args := "", "", []string(nil)
	switch words[0] {
	case "probe":
		op = "probe"
	case "install":
		op = "install"
		if len(words) == 2 {
			sha = words[1]
		}
	case "agent":
		if len(words) < 3 {
			return invalid()
		}
		sha, op, args = words[1], words[2], words[3:]
		if op == "slot-boot" {
			if len(args) < 1 {
				return invalid()
			}
			op, args = map[string]string{"--prepare": "prepare", "--run": "run"}[args[0]], args[1:]
		}
	}
	if op == "" {
		return invalid()
	}
	call := Call{Op: op, Verb: verb, Attach: op == "run"}
	rule := g.match(op)
	var in []byte
	if op == "install" {
		in, _ = io.ReadAll(io.LimitReader(g.stdin, 256<<20))
		call.Data = fmt.Sprintf("%d bytes", len(in))
		g.record(call)
		if rule != nil {
			return g.fail(rule)
		}
		return g.install(sha, in)
	}
	if op == "run" {
		in = g.readRun(&call)
	} else if (op == "prepare" && contains(args, "payload")) || (rule != nil && rule.Read) {
		in, _ = io.ReadAll(io.LimitReader(g.stdin, agent.MaxBootPayload+16))
		call.Data = string(in)
	}
	g.record(call)
	if rule != nil && rule.Mangle != "" {
		if old, repl, ok := strings.Cut(rule.Mangle, "="); ok && op == "tail" && len(old) == len(repl) {
			g.stdout = &mangleWriter{w: g.stdout, old: []byte(old), repl: []byte(repl)}
		}
		rule = nil
	}
	if rule != nil && !rule.After {
		return g.fail(rule)
	}
	var code int
	switch op {
	case "probe":
		fmt.Fprintf(g.stdout, `{"arch":%q,"kernel":"fake","cgroup2":true,"slots":%d,"users_ok":true,"gate_version":1}`+"\n", g.conf.Arch, g.conf.Slots)
	case "check-host":
		code = g.checkHost(sha)
	case "prepare":
		code = g.prepare(args, in)
	case "run":
		code = g.run(sha, args, in)
	case "tail", "read":
		return g.stream(op, args) // releases the bookkeeping lock itself
	case "list":
		code = g.list()
	case "teardown":
		code = g.teardown(args)
	default:
		return 64
	}
	if rule != nil && rule.After {
		return g.fail(rule)
	}
	return code
}

// readRun reads the frame of a `slot-boot --run` call and, when its payload announces a code
// upload, exactly len bytes after it into <root>/uploads/<job>.bundle; whatever follows is
// discarded. The recorded data is the frame, the decoded spec and an `upload N bytes` line (or
// `upload short` when the stream ended early).
func (g *gate) readRun(call *Call) []byte {
	var head [8]byte
	n, _ := io.ReadFull(g.stdin, head[:])
	in := append([]byte(nil), head[:n]...)
	if n == len(head) {
		if size := binary.BigEndian.Uint64(head[:]); size > 0 && size <= agent.MaxBootPayload {
			body := make([]byte, size)
			m, _ := io.ReadFull(g.stdin, body)
			in = append(in, body[:m]...)
		}
	}
	call.Data = string(in)
	p, err := agent.ReadBootPayload(bytes.NewReader(in))
	if err != nil {
		return in
	}
	spec, err := agent.DecodeSpec(p.Spec)
	if err != nil {
		return in
	}
	sb, _ := json.Marshal(spec)
	call.Data += "\n" + string(sb)
	if p.Code != nil && p.Code.Len > 0 && jobFile(spec.Job) {
		dir := filepath.Join(g.root, "uploads")
		_ = os.MkdirAll(dir, 0o700)
		f, err := os.OpenFile(filepath.Join(dir, spec.Job+".bundle"), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
		if err == nil {
			got, _ := io.CopyN(f, g.stdin, p.Code.Len)
			_ = f.Close()
			if got == p.Code.Len {
				call.Data += fmt.Sprintf("\nupload %d bytes", got)
			} else {
				call.Data += fmt.Sprintf("\nupload short: %d of %d bytes", got, p.Code.Len)
			}
		}
	}
	_, _ = io.Copy(io.Discard, g.stdin) // bytes after the bundle are not used
	return in
}

// handOver checks the bytes the fake saved for an upload against the payload's sha256 and len and
// puts them where the agent's code step reads them: <test root>/work/.bitc-upload/code.bundle.
// It returns the refusal text, or "" when the bundle is in place.
func (g *gate) handOver(spec agent.Spec, c *agent.CodeUpload) string {
	if !jobFile(spec.Job) {
		return "bad job id"
	}
	b, err := os.ReadFile(filepath.Join(g.root, "uploads", spec.Job+".bundle"))
	if err != nil || int64(len(b)) != c.Len {
		return "truncated payload"
	}
	if sum := sha256.Sum256(b); hex.EncodeToString(sum[:]) != c.SHA256 {
		return "bundle hash mismatch"
	}
	dir := filepath.Join(spec.TestRoot, "work", ".bitc-upload")
	_ = os.RemoveAll(dir)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "cannot stage the bundle"
	}
	if err := os.WriteFile(filepath.Join(dir, "code.bundle"), b, 0o600); err != nil { //nolint:gosec // the job id passed jobFile; the dir is the fake host's test root
		return "cannot stage the bundle"
	}
	return ""
}

// jobFile reports whether a job id is safe as a file name.
func jobFile(job string) bool {
	return job != "" && !strings.ContainsAny(job, "/\\") && job != "." && job != ".."
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

func (g *gate) install(sha string, b []byte) int {
	sum := sha256.Sum256(b)
	if hex.EncodeToString(sum[:]) != sha {
		fmt.Fprintln(g.stderr, "install: sha256 mismatch")
		return 65
	}
	dir := filepath.Join(g.root, "agents", sha)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return 69
	}
	if err := os.WriteFile(filepath.Join(dir, "bitc.part"), b, 0o700); err != nil { //nolint:gosec // the fake host's agent binary must be executable
		return 69
	}
	if err := os.Rename(filepath.Join(dir, "bitc.part"), filepath.Join(dir, "bitc")); err != nil {
		return 69
	}
	fmt.Fprintln(g.stdout, "installed "+sha)
	return 0
}

func (g *gate) installed(sha string) bool {
	st, err := os.Stat(filepath.Join(g.root, "agents", sha, "bitc"))
	return err == nil && st.Mode().IsRegular()
}

func (g *gate) checkHost(sha string) int {
	if !g.installed(sha) {
		fmt.Fprintf(g.stderr, "agent %s is not installed\n", sha)
		return 66
	}
	fmt.Fprintln(g.stdout, "ok ssh-arch: "+g.conf.Arch)
	return 0
}

type bootArgs struct {
	slots       []int
	name, nonce string
	tag         string
	lease       time.Duration
}

func parseBoot(args []string) (bootArgs, bool) {
	b := bootArgs{lease: 10 * time.Minute}
	if len(args) < 3 {
		return b, false
	}
	slots, ok := agent.ParseSlots(args[0])
	if !ok {
		return b, false
	}
	b.slots, b.name, b.nonce = slots, args[1], args[2]
	for _, a := range args[3:] {
		switch {
		case a == "payload":
		case strings.HasPrefix(a, "tag="):
			b.tag = a[4:]
		case strings.HasPrefix(a, "lease="):
			n, err := strconv.Atoi(a[6:])
			if err != nil {
				return b, false
			}
			b.lease = time.Duration(n) * time.Second
		default:
			return b, false
		}
	}
	return b, true
}

func (g *gate) writeOwner(slot int, o SlotOwner) error {
	return SetOwner(g.root, slot, o)
}

func (g *gate) prepare(args []string, in []byte) int {
	b, ok := parseBoot(args)
	if !ok {
		return 64
	}
	now := time.Now()
	for _, i := range b.slots {
		if o := readOwner(g.root, i); o != nil && o.LeaseUntil > now.Unix() {
			fmt.Fprintf(g.stdout, "refused: slot %d busy on host\n", i)
			return 65
		}
	}
	for _, i := range b.slots {
		if err := g.writeOwner(i, SlotOwner{Name: b.name, Nonce: b.nonce, Tag: b.tag, Created: now.Unix(), LeaseUntil: now.Add(b.lease).Unix()}); err != nil {
			fmt.Fprintln(g.stderr, err)
			return 69
		}
	}
	if len(in) > 0 {
		if _, err := agent.ReadProvisionSpec(bytes.NewReader(in)); err != nil {
			fmt.Fprintln(g.stdout, "refused: "+err.Error())
			return 65
		}
	}
	fmt.Fprintln(g.stdout, "prepared "+b.name)
	return 0
}

func (g *gate) run(sha string, args []string, in []byte) int {
	b, ok := parseBoot(args)
	if !ok {
		return 64
	}
	now := time.Now()
	for _, i := range b.slots {
		o := readOwner(g.root, i)
		if o == nil || o.Name != b.name || o.Nonce != b.nonce || o.LeaseUntil <= now.Unix() {
			fmt.Fprintf(g.stdout, "refused: slot %d not leased to %s\n", i, b.name)
			return 65
		}
	}
	p, err := agent.ReadBootPayload(bytes.NewReader(in))
	if err != nil {
		fmt.Fprintln(g.stdout, "refused: "+err.Error())
		return 65
	}
	spec, err := agent.DecodeSpec(p.Spec)
	if err != nil || spec.Port != -1 || !spec.Unprivileged || spec.TestRoot == "" {
		fmt.Fprintln(g.stdout, "refused: unusable spec")
		return 65
	}
	results := filepath.Join(spec.TestRoot, "run")
	if err := os.MkdirAll(filepath.Join(g.root, "jobs"), 0o700); err != nil {
		return 69
	}
	if err := os.WriteFile(filepath.Join(g.root, "jobs", spec.Job), []byte(results), 0o600); err != nil {
		return 69
	}
	for _, i := range b.slots {
		o := *readOwner(g.root, i)
		o.Job, o.LeaseUntil = spec.Job, time.Unix(p.Deadline, 0).Unix()
		_ = g.writeOwner(i, o)
	}
	frame, err := p.Frame()
	if err != nil {
		return 69
	}
	if p.Code != nil { // the root side's hand-over: verified bytes at work/.bitc-upload/code.bundle
		if why := g.handOver(spec, p.Code); why != "" {
			fmt.Fprintln(g.stdout, "refused: "+why)
			return 65
		}
	}
	c := exec.Command(filepath.Join(g.root, "agents", sha, "bitc"), "__agent", "--boot-stdin")
	// the gate exits right after the start, so the payload must be a file the agent reads itself
	// (a reader would be copied by this process's goroutine, which dies with it)
	pf, err := os.Create(filepath.Join(g.root, "jobs", spec.Job+".boot"))
	if err != nil {
		return 69
	}
	if _, err := pf.Write(frame); err != nil {
		return 69
	}
	_, _ = pf.Seek(0, 0)
	c.Stdin = pf
	c.Env = []string{}
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := c.Start(); err != nil {
		fmt.Fprintln(g.stderr, err)
		return 69
	}
	_ = c.Process.Release()
	fmt.Fprintln(g.stdout, "started "+b.name)
	return 0
}

// stream is `tail` and `read`: the agent's own exec verbs over the job's results root. The
// bookkeeping lock is dropped first, because a tail lasts as long as the job.
func (g *gate) stream(op string, args []string) int {
	g.unlock()
	if len(args) == 0 {
		return 64
	}
	b, err := os.ReadFile(filepath.Join(g.root, "jobs", args[0]))
	if err != nil {
		fmt.Fprintln(g.stderr, "not found")
		return 66
	}
	return agent.ExecVerb(append([]string{"--root", string(b), op}, args...), g.stdout, g.stderr)
}

func (g *gate) list() int {
	now := time.Now().Unix()
	enc := json.NewEncoder(g.stdout)
	for i := 1; i <= g.conf.Slots; i++ {
		o := readOwner(g.root, i)
		line := struct {
			Slot int `json:"slot"`
			*SlotOwner
			Busy bool `json:"busy"`
		}{Slot: i, SlotOwner: o}
		line.Busy = o != nil && o.LeaseUntil > now
		if enc.Encode(line) != nil {
			return 69
		}
	}
	return 0
}

func (g *gate) teardown(args []string) int {
	if len(args) != 2 {
		return 64
	}
	slots, ok := agent.ParseSlots(args[0])
	if !ok {
		return 64
	}
	torn := false
	for _, i := range slots {
		o := readOwner(g.root, i)
		if o == nil || o.Name != args[1] {
			continue
		}
		torn = true
		if o.Job != "" {
			if b, err := os.ReadFile(filepath.Join(g.root, "jobs", o.Job)); err == nil {
				if err := agent.Teardown(string(b), o.Job, io.Discard); err != nil && !errors.Is(err, os.ErrNotExist) {
					fmt.Fprintln(g.stderr, err)
					return 69
				}
			}
		}
		_ = os.Remove(filepath.Join(g.root, "slots", strconv.Itoa(i), "owner"))
	}
	if torn {
		fmt.Fprintln(g.stdout, "torn")
	} else {
		fmt.Fprintln(g.stdout, "gone")
	}
	return 0
}

// mangleWriter rewrites the data of NDJSON tail frames (same-length replacements keep offsets).
type mangleWriter struct {
	w         io.Writer
	buf       []byte
	old, repl []byte
}

func (m *mangleWriter) Write(b []byte) (int, error) {
	m.buf = append(m.buf, b...)
	for {
		i := bytes.IndexByte(m.buf, '\n')
		if i < 0 {
			return len(b), nil
		}
		line := m.buf[:i]
		m.buf = m.buf[i+1:]
		var f agent.Frame
		if json.Unmarshal(line, &f) == nil && len(f.Data) > 0 {
			f.Data = bytes.ReplaceAll(f.Data, m.old, m.repl)
			line, _ = json.Marshal(f)
		}
		if _, err := m.w.Write(append(append([]byte(nil), line...), '\n')); err != nil {
			return 0, err
		}
	}
}
