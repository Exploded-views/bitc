// Command bitc-provider-fakex is the reference provider plugin for bitc (providers delta A.10.3).
//
// It is a small, complete example of the plugin protocol: every method of the Provider interface
// against a REST API ("fakex"), with the faults the conformance suites need. Copy it as the
// starting point of a plugin; nothing in it depends on the bitc module (standard library only).
//
// What a plugin is (the protocol is documented in docs/plugin-protocol.md):
//
//   - bitc starts the executable once per bitc process and speaks newline-delimited JSON with it:
//     requests on stdin, answers on stdout (one JSON object per line, at most 1 MiB), logs on stderr.
//   - Secrets arrive once, as one JSON object on file descriptor 3 (never argv or env).
//   - A request is {"id": n, "method": "...", "params": {...}}. The answer is zero or more events
//     {"id": n, "event": "..."} then exactly one final {"id": n, "result": ...} or {"id": n, "error": ...}.
//     Requests may overlap (bitc asks `teardown` or `owner` while a `watch` is open) and may be
//     answered in any order, so each request runs in its own goroutine.
//   - The plugin is an API client only. bitc core names the resource (job.name), computes its owner
//     tag (job.owner), enforces the money rails, parses the agent's markers and verifies results. The
//     plugin creates a resource with exactly that name, remembers the owner, runs bitc's agent in it
//     (the API's job here is to start it) and relays what the agent logs.
//
// Files: main.go is the protocol loop, methods.go the eleven methods, api.go the HTTP client of the
// fakex API, faults.go the fault modes.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	pluginVersion = "0.1.0"
	maxLine       = 1 << 20 // a frame is one line of at most 1 MiB (A.4.1)
)

// request is one line from bitc.
type request struct {
	ID     uint64          `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

// rpcError is the error object of a final frame (A.6). Class is one of ineligible, transient,
// unavailable, unavailable_local, pre_attach, attached, setup or config.
type rpcError struct {
	Class      string `json:"class"`
	Why        string `json:"why"`
	ID         string `json:"id,omitempty"`
	Step       string `json:"step,omitempty"`
	SetupClass string `json:"setup_class,omitempty"`
	Inner      *inner `json:"inner,omitempty"`
}

type inner struct {
	Class string `json:"class"`
	Why   string `json:"why"`
}

func (e *rpcError) Error() string { return e.Class + ": " + e.Why }

func transient(format string, a ...any) *rpcError {
	return &rpcError{Class: "transient", Why: fmt.Sprintf(format, a...)}
}

// call is one open request; handlers answer with their return value and send events with emit.
type call struct {
	s      *server
	id     uint64
	method string
	params json.RawMessage
	raw    []byte // the request line as received (the echo-launch fault copies it to stderr)
}

// emit sends an event of a streaming method (launch: setup; watch: phase, log, out, heartbeat;
// output and fetch: data).
func (c *call) emit(kind string, fields map[string]any) {
	m := map[string]any{"id": c.id, "event": kind}
	for k, v := range fields {
		m[k] = v
	}
	c.s.write(c.s.decorate(m))
}

// server is the protocol loop and the state methods share: the secrets, the configuration that
// `configure` supplies and the table of open calls.
type server struct {
	wmu sync.Mutex
	out *bufio.Writer

	secrets map[string]string // fd 3: api_key and the plugin.secret_files entries

	mu       sync.Mutex
	faults   map[string]bool
	cfg      *config
	api      *apiClient
	inflight map[uint64]context.CancelFunc
	wg       sync.WaitGroup
	created  atomic.Bool // this process has created an instance (the list-fails-after-create fault)
}

func main() {
	s := &server{out: bufio.NewWriter(os.Stdout), inflight: map[uint64]context.CancelFunc{}, secrets: readSecrets()}
	s.faults = parseFaults(os.Getenv("FAKEX_FAULT"))
	s.diag()
	s.serve(os.Stdin)
}

// readSecrets reads the one JSON object bitc writes to fd 3 (named by BITC_SECRETS_FD), then EOF.
// Absent when the variable is unset (the plugin is run by hand).
func readSecrets() map[string]string {
	secrets := map[string]string{}
	fd, err := strconv.Atoi(os.Getenv("BITC_SECRETS_FD"))
	if err != nil {
		return secrets
	}
	f := os.NewFile(uintptr(fd), "secrets")
	defer f.Close()
	b, err := io.ReadAll(f)
	if err == nil {
		_ = json.Unmarshal(b, &secrets)
	}
	return secrets
}

// serve reads requests until bitc closes stdin, then lets open calls finish for up to 2 s and
// returns (the process exits 0): bitc closes stdin when it is done with the plugin (A.3.5).
func (s *server) serve(in io.Reader) {
	rd := bufio.NewReaderSize(in, 1<<20)
	for {
		line, err := rd.ReadBytes('\n')
		if len(strings.TrimSpace(string(line))) > 0 {
			s.dispatch(line)
		}
		if err != nil {
			break
		}
	}
	s.mu.Lock()
	for _, cancel := range s.inflight {
		cancel()
	}
	s.mu.Unlock()
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(1500 * time.Millisecond):
	}
}

// dispatch starts the handler of one request. Unknown fields in a request are ignored (A.4.6).
func (s *server) dispatch(line []byte) {
	line = append([]byte(nil), line...)
	var r request
	if json.Unmarshal(line, &r) != nil {
		fmt.Fprintln(os.Stderr, "fakex: ignoring a line that is not a request")
		return
	}
	if r.Method == "cancel" { // {"id": 0, "method": "cancel", "params": {"id": n}}: stop request n; no answer
		var p struct {
			ID uint64 `json:"id"`
		}
		_ = json.Unmarshal(r.Params, &p)
		s.mu.Lock()
		if cancel := s.inflight[p.ID]; cancel != nil {
			cancel()
		}
		s.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.mu.Lock()
	s.inflight[r.ID] = cancel
	s.mu.Unlock()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer func() {
			s.mu.Lock()
			delete(s.inflight, r.ID)
			s.mu.Unlock()
			cancel()
		}()
		c := &call{s: s, id: r.ID, method: r.Method, params: r.Params, raw: line}
		result, err := s.handle(ctx, c)
		if ctx.Err() != nil && err != nil && err.ID == "" {
			// cancelled by bitc (or stdin closed): still answer, once, so bitc need not kill us.
			// An error that already names a resource keeps it: dropping the id would leak it.
			err = transient("%s cancelled", r.Method)
		}
		s.final(c, result, err)
	}()
}

// final writes the one final frame of a call.
func (s *server) final(c *call, result any, err *rpcError) {
	if s.fault("hang:" + c.method) {
		select {} // a fault: never answer
	}
	m := map[string]any{"id": c.id}
	if err != nil {
		m["error"] = s.decorate(err)
	} else {
		m["result"] = s.decorate(result)
	}
	s.write(m)
}

// write sends one frame; frames never interleave.
func (s *server) write(v any) {
	b, _ := json.Marshal(v)
	if len(b) >= maxLine {
		b, _ = json.Marshal(map[string]any{"id": v.(map[string]any)["id"], "error": rpcError{Class: "transient", Why: "answer too large"}})
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_, _ = s.out.Write(append(b, '\n'))
	_ = s.out.Flush()
}

// writeRaw sends a line as is (faults only).
func (s *server) writeRaw(line string) {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_, _ = s.out.WriteString(line + "\n")
	_ = s.out.Flush()
}
