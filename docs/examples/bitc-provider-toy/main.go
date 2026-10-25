package main

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The type comes from the file name, so the same binary can be pinned as bitc-provider-toy in
// production and copied to bitc-provider-fakex by the provider suite.
var typ = strings.TrimPrefix(filepath.Base(os.Args[0]), "bitc-provider-")

// "resource" is the safe production default. A test build declares attach with
//
//	go build -ldflags "-X main.fallbackUntil=attach" ...
var fallbackUntil = "resource"

type rpcError struct {
	Class string    `json:"class"`
	Why   string    `json:"why"`
	ID    string    `json:"id,omitempty"`
	Inner *rpcError `json:"inner,omitempty"`
}

type request struct {
	ID     uint64          `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

type resource struct {
	ID, Name, Owner, SizeKey string
	Files                    map[string][]byte // "stdout", "stderr", "result", "state" (served by output) and "artifacts" (served by fetch)
}

var (
	wmu sync.Mutex // one lock around every write to stdout
	out = bufio.NewWriter(os.Stdout)

	smu     sync.Mutex // guards the maps below
	store   = map[string]*resource{}
	cancels = map[uint64]chan struct{}{} // one cancel channel per open request id
)

func send(v any) {
	wmu.Lock()
	defer wmu.Unlock()
	b, _ := json.Marshal(v)
	out.Write(append(b, '\n'))
	out.Flush()
}
func event(id uint64, kv map[string]any) { kv["id"] = id; send(kv) }
func reply(id uint64, v any)             { send(map[string]any{"id": id, "result": v}) }
func fail(id uint64, e *rpcError)        { send(map[string]any{"id": id, "error": e}) }

// find accepts a resource id or a resource name (teardown and owner get either).
func find(key string) *resource {
	smu.Lock()
	defer smu.Unlock()
	for _, r := range store {
		if r.ID == key || r.Name == key {
			return r
		}
	}
	return nil
}

func main() {
	var secrets map[string]string // fd 3: {"api_key": "...", ...}
	if fd, err := strconv.Atoi(os.Getenv("BITC_SECRETS_FD")); err == nil {
		b, _ := io.ReadAll(os.NewFile(uintptr(fd), "secrets"))
		_ = json.Unmarshal(b, &secrets)
	}
	var wg sync.WaitGroup
	in := bufio.NewReaderSize(os.Stdin, 1<<20)
	for {
		line, err := in.ReadBytes('\n')
		var r request
		if len(strings.TrimSpace(string(line))) == 0 { // skip blank lines explicitly
			if err != nil {
				wg.Wait()
				return
			}
			continue
		}
		if json.Unmarshal(line, &r) == nil && r.Method == "cancel" {
			var p struct {
				ID uint64 `json:"id"`
			}
			_ = json.Unmarshal(r.Params, &p)
			smu.Lock()
			if c, ok := cancels[p.ID]; ok { // tell the handler; cancel itself gets no answer
				close(c)
				delete(cancels, p.ID)
			}
			smu.Unlock()
		} else if r.Method != "" {
			c := make(chan struct{})
			smu.Lock()
			cancels[r.ID] = c
			smu.Unlock()
			wg.Add(1)
			go func() {
				defer wg.Done()
				handle(r, c)
				smu.Lock()
				delete(cancels, r.ID)
				smu.Unlock()
			}()
		}
		if err != nil { // stdin closed: finish what is open, then exit within 2 s
			wg.Wait()
			return
		}
	}
}

func handle(r request, cancel <-chan struct{}) {
	switch r.Method {
	case "hello":
		reply(r.ID, map[string]any{"protocol": 1, "type": typ, "plugin_version": "0.1.0",
			"caps": map[string]any{"live_logs": false, "artifacts": false, "spend": false,
				"self_delete": false, "fallback_until": fallbackUntil, "fixed": false, "multi_job": false}})
	case "configure":
		// A real plugin returns the host of its api_base here. This toy contacts nothing: [].
		reply(r.ID, map[string]any{"endpoints": []string{}, "arch": []string{"amd64"}, "max_name": 63})
	case "preflight":
		reply(r.ID, map[string]any{})
	case "estimate":
		reply(r.ID, map[string]any{"usd_micros": 0}) // bitc uses max(host price x time, this)
	case "launch":
		launch(r, cancel)
	case "watch":
		watch(r, cancel)
	case "output":
		output(r)
	case "owner":
		var p struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(r.Params, &p)
		tag := ""
		if res := find(p.ID); res != nil {
			tag = res.Owner
		}
		reply(r.ID, map[string]any{"owner": tag}) // "" for a resource that does not exist
	case "teardown":
		var p struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(r.Params, &p)
		if res := find(p.ID); res != nil {
			smu.Lock()
			delete(store, res.ID)
			smu.Unlock()
		}
		reply(r.ID, map[string]any{}) // gone is success
	case "list":
		var p struct {
			Prefixes []string `json:"prefixes"`
		}
		_ = json.Unmarshal(r.Params, &p)
		list := []any{}
		smu.Lock()
		for _, res := range store {
			for _, pre := range p.Prefixes {
				if strings.HasPrefix(res.Name, pre) {
					list = append(list, map[string]any{"id": res.ID, "name": res.Name, "size_key": res.SizeKey})
					break
				}
			}
		}
		smu.Unlock()
		reply(r.ID, map[string]any{"resources": list})
	case "fetch": // only called when caps.artifacts is true; the toy declares false but answers correctly
		fetch(r)
	case "spend": // only called when caps.spend is true; a real plugin asks its billing API here
		reply(r.ID, map[string]any{"usd_micros": 0})
	default:
		fail(r.ID, &rpcError{Class: "ineligible", Why: r.Method + " is not implemented"})
	}
}

func launch(r request, cancel <-chan struct{}) {
	var p struct {
		Job struct {
			Name    string `json:"name"`
			Owner   string `json:"owner"`
			SizeKey string `json:"size_key"`
		} `json:"job"`
	}
	_ = json.Unmarshal(r.Params, &p)
	res := &resource{ID: "toy-" + p.Job.Name, Name: p.Job.Name, Owner: p.Job.Owner, SizeKey: p.Job.SizeKey,
		Files: map[string][]byte{"stdout": []byte("hello\n"), "artifacts": []byte("not a real archive")}}
	smu.Lock()
	store[res.ID] = res // 1. create with exactly job.name; store job.owner
	smu.Unlock()
	event(r.ID, map[string]any{"event": "setup", "line": "created " + res.ID})
	select { // 2. wait until the resource is ready; no job input has been sent yet
	case <-time.After(200 * time.Millisecond):
	case <-cancel:
		// The resource exists, the job has not reached it: pre_attach, id = the assigned NAME.
		fail(r.ID, &rpcError{Class: "pre_attach", Why: "launch cancelled", ID: res.Name,
			Inner: &rpcError{Class: "transient", Why: "launch cancelled"}})
		return
	}
	// 3. attach: translate the job into the agent's spec and start the agent here
	// (protocol reference 6A); the toy runs no agent. A failure from now on is
	// {"class": "attached", "why": ..., "id": res.ID} (inner optional).
	reply(r.ID, map[string]any{"id": res.ID, "name": res.Name})
}

func watch(r request, cancel <-chan struct{}) {
	var p struct {
		Handle struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"handle"`
	}
	_ = json.Unmarshal(r.Params, &p)
	res := find(p.Handle.ID)
	if res == nil {
		// Never saw it, or it is gone: say lost. Do not invent an exit or began. bitc stops the
		// call on this event and sends cancel; whatever follows for this id is dropped.
		event(r.ID, map[string]any{"event": "phase", "phase": "lost", "why": "unknown resource"})
		fail(r.ID, &rpcError{Class: "transient", Why: "resource " + p.Handle.ID + " is gone"})
		return
	}
	beat := time.NewTicker(10 * time.Second) // at least every 30 s; bitc declares lost after 90 s
	defer beat.Stop()
	done := time.After(2 * time.Second) // stands in for "the job ended"
	for {
		select {
		case <-beat.C:
			event(r.ID, map[string]any{"event": "heartbeat"})
		case <-cancel: // answer THIS id (not 0) within 5 s
			fail(r.ID, &rpcError{Class: "transient", Why: "watch cancelled"})
			return
		case <-done:
			// A real plugin relays every log line unmodified, markers included.
			event(r.ID, map[string]any{"event": "log", "line": "toy: nothing to run"})
			reply(r.ID, map[string]any{"exit": 0, "began": true})
			return
		}
	}
}

func output(r request) {
	var p struct {
		Handle struct {
			ID string `json:"id"`
		} `json:"handle"`
		Stream string `json:"stream"`
		Offset int    `json:"offset"`
	}
	_ = json.Unmarshal(r.Params, &p)
	switch p.Stream {
	case "stdout", "stderr", "result", "state":
	default: // an unknown stream is ineligible; "artifacts" is served by fetch, not here
		fail(r.ID, &rpcError{Class: "ineligible", Why: "unknown stream " + p.Stream})
		return
	}
	res := find(p.Handle.ID)
	if res == nil || res.Files[p.Stream] == nil { // nil = not there yet = transient; an empty file must be stored as a non-nil empty slice and gets bytes 0
		fail(r.ID, &rpcError{Class: "transient", Why: "no " + p.Stream + " yet"})
		return
	}
	stream(r.ID, res.Files[p.Stream], p.Offset)
}

// fetch serves the artifacts archive. Over max_compressed it refuses before streaming anything.
func fetch(r request) {
	var p struct {
		Handle struct {
			ID string `json:"id"`
		} `json:"handle"`
		MaxCompressed int `json:"max_compressed"`
		Offset        int `json:"offset"`
	}
	_ = json.Unmarshal(r.Params, &p)
	res := find(p.Handle.ID)
	if res == nil || res.Files["artifacts"] == nil {
		fail(r.ID, &rpcError{Class: "transient", Why: "no artifacts yet"})
		return
	}
	if len(res.Files["artifacts"]) > p.MaxCompressed {
		fail(r.ID, &rpcError{Class: "transient", Why: "artifacts archive is " + strconv.Itoa(len(res.Files["artifacts"])) +
			" bytes, over max_compressed " + strconv.Itoa(p.MaxCompressed)})
		return
	}
	stream(r.ID, res.Files["artifacts"], p.Offset)
}

// stream sends b from offset as data events of at most 512 KiB decoded, then the byte count.
func stream(id uint64, b []byte, offset int) {
	if offset > len(b) {
		offset = len(b)
	}
	b = b[offset:]
	n := len(b)
	for len(b) > 0 {
		c := b
		if len(c) > 512<<10 {
			c = c[:512<<10]
		}
		event(id, map[string]any{"event": "data", "data": base64.StdEncoding.EncodeToString(c)})
		b = b[len(c):]
	}
	reply(id, map[string]any{"bytes": n})
}
