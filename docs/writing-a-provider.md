# Writing a provider plugin

This guide takes you from nothing to a pinned, tested provider plugin for bitc. The wire protocol
itself is in [plugin-protocol.md](plugin-protocol.md); this guide says how to use it, how to test
it and what you are promising to the person who installs your plugin.

Contents: 1 Before you start · 2 Build · 3 Pin and install · 4 A toy plugin in one page ·
5 Test it · 6 What a pinned plugin is trusted with · 7 Pricing rules · 8 Security requirements ·
9 Checklist

## 1. Before you start

A plugin lets bitc run a job on a platform that bitc does not know, for example a cloud's
container API. Your plugin is a small API client: it creates, lists, watches and deletes
resources. bitc core does the rest (naming, the daily cap, `max_live`, the ledger, the circuit
breaker, markers, the result MAC, artifact validation, sweep). Read section 1 of the protocol
reference for the split.

You need:

- a platform with an API that can start a Linux resource (a VM, a container) you can reach by
  network, run bitc's agent in, and read the agent's log and result files back from;
- a way to download a file from a URL on that resource, or an image that already has the agent
  (the job carries `agent_url` and `agent_sha256`; the resource must run that exact binary as
  `bitc __agent`);
- a loopback fake of your API for tests (section 5). Tests never meet the real platform and bitc
  refuses to contact a non-loopback host under its `[test]` table.

Any language works if it produces **one ELF executable**. A script with a `#!` line does not
work (the pin runs the hashed file descriptor directly; an interpreter would be unpinned). Compile
scripts into a single binary (PyInstaller, Deno compile, and so on). Go with the standard library
only is the easiest; the reference plugin is that.

## 2. Build

Start from the reference plugin, `examples/plugins/bitc-provider-fakex/`:

| File | What it shows |
| --- | --- |
| `main.go` | the protocol loop: read requests from stdin, one goroutine per request, one lock around writes, `cancel`, exit on EOF, secrets from fd 3 |
| `methods.go` | the methods: `hello`, `configure`, `preflight`, `estimate`, `launch` (create, wait, attach), `watch`, `output`, `fetch`, `owner`, `teardown`, `list`, `spend` |
| `api.go` | the HTTP client, the mapping of HTTP failures to error classes, paginated `list` |
| `faults.go` | fault modes selected by the test suites. Do not copy these |

**A plugin is its own project.** It is a Go module of its own (or a project in another language)
and needs no bitc checkout, no bitc import and no bitc Go version to build. Build it from the
plugin's own directory, statically and reproducibly:

```sh
cd myplugin     # the directory that holds your go.mod
CGO_ENABLED=0 go build -trimpath -o bitc-provider-toy .
```

The reference plugin is a directory inside the bitc module only because bitc ships it; yours is
not. If you build with a pinned toolchain container such as bitc's `scripts/dev.sh` (which mounts
only the bitc checkout and its caches), keep the plugin directory inside the checkout (for example
`.rem/myplugin/`; note that `.rem/` is **not** in the repo's `.gitignore`, so in a fresh clone it
shows as untracked: add `.rem/` to `$(git rev-parse --git-common-dir)/info/exclude` (the main
clone's `.git/info/exclude` when you work in a git worktree, which shares it), or use a path that is already
ignored such as `.bitc/`) and run the build from inside that directory, with the script's
path as seen from there: `cd .rem/myplugin && ../../scripts/dev.sh go build -o bitc-provider-toy .`.
Passing a directory path from the checkout root fails, because the plugin is a different module
from bitc's.

The file name must be `bitc-provider-<type>`, where `<type>` matches `^[a-z][a-z0-9]{0,15}$` and
is not `local`, `runpod`, `sprites`, `ssh` or `fake`. The same `<type>` is the section name in the
host config and the `type` your `hello` returns.

The shape of every plugin:

1. At start, read fd 3 (`BITC_SECRETS_FD`) to EOF and parse the JSON object: `api_key` and any
   `secret_files` names. Keep the secrets in memory.
2. Read stdin one line at a time. Parse `{id, method, params}`. Handle each request on its own
   goroutine or task, because `watch` stays open while `teardown` and `owner` arrive.
3. Write one final frame per id, and events before it. Serialize writes so frames never
   interleave. Never write anything but frames to stdout.
4. On `cancel` (`id` 0), stop the named request and answer it.
5. When stdin closes, finish and exit within 2 s.

What each method must do, in the order you will implement them:

| Order | Method | The essential rule |
| --- | --- | --- |
| 1 | `hello`, `configure` | declare honest caps and a `max_name` of at least 60; touch no network in `configure` |
| 2 | `list`, `owner`, `teardown` | `list` returns every live resource under the prefixes across all pages; `owner` returns the tag you stored; `teardown` treats gone as success and accepts the id or the name |
| 3 | `launch` | create with exactly `job.name`, store `job.owner`, send no job input until the final attach call, classify errors as in the protocol reference section 8 |
| 4 | `watch`, `output`, `fetch` | relay the agent's log lines unmodified; serve result files from an offset; heartbeat |
| 5 | `preflight`, `estimate`, `spend` | cheap, honest, never lower than the platform's real price |

The agent's own protocol (markers, `result.json`, MAC) is not yours to implement. You deliver the
agent and carry its output; bitc parses it. How to start the agent, what it reads and where it
writes is the agent hand-off contract, protocol reference section 6A. Note that the agent does
**not** read the wire `job` object: you translate it into the agent's own spec (6A.2). The
reference plugin's fake API (`fakeapi/main.go`) is a working example of the same contract.

## 3. Pin and install

A host pins a plugin in the host config file (not in a repository file: a `plugin` key in a repo
file is exit 78):

```toml
[providers.modal]
plugin = { path = "/usr/local/libexec/bitc/bitc-provider-modal",
           sha256 = "<64 lowercase hex>",
           env_pass = ["HTTPS_PROXY", "NO_PROXY"],       # optional; default none
           secret_files = { token_secret = "~/.config/bitc/modal.token-secret" } }  # optional
api_key_file = "~/.config/bitc/modal.key"      # credential, 0600
daily_usd = 5
max_live = 2
sizes = { cpu8 = { usd_per_hour = 0.60 } }     # required for a plugin (section 7)
region = "eu"                                  # any other key goes to your configure
```

Install it:

```sh
install -m 0755 bitc-provider-modal /usr/local/libexec/bitc/bitc-provider-modal
sha256sum /usr/local/libexec/bitc/bitc-provider-modal      # put this in the config
bitc config check                                            # validates the pin and runs hello/configure
bitc doctor                                                  # ok plugin-modal: protocol 1, 0.3.1, sha256 ...
```

The load-time rules, all enforced by bitc:

- `path` is absolute with basename `bitc-provider-<type>`. bitc never searches `PATH`.
- The file is a regular ELF file (not a symlink), executable, owned by the user or root, with no
  group or world write on it or any parent directory, no setuid or setgid bit and no file
  capabilities.
- Its sha256 equals the pinned value, checked at load and again at every spawn on the open file
  descriptor that is then executed. **Rebuilding changes the hash:** update the config each time,
  or bitc refuses the plugin (`plugin ... changed since load`).
- Credentials: `api_key` or `api_key_file` become `api_key` on fd 3. `secret_files` adds more
  named secrets (0600, owned by the user). Anything else in the section is non-secret config that
  `bitc config` and `doctor` print; do not tell users to put a secret there (bitc rejects keys
  that look like secrets, exit 78).
- `env_pass` lists environment variable names (not secret-looking ones) to copy into the plugin's
  otherwise empty environment, for example proxy settings.

## 4. A toy plugin

This is a complete plugin of 317 lines (standard library only) that implements every method
in memory: it contacts nothing and runs no job, so it cannot do real work, but it is correct on the
wire and it passes the protocol suite (section 5.1). It is meant to be copied as the skeleton of a real plugin: replace the in-memory store with calls
to your platform's API. **Do not extract it from this page** (extraction strips the indentation).
The runnable copy, identical to the listing below, is `docs/examples/bitc-provider-toy/`
(`main.go` and a `go.mod` with `module example.com/bitc-provider-toy`, `go 1.22`): copy that
directory (to a path inside the checkout if you build with `scripts/dev.sh`, section 2) and build
it as in section 2.

Points to copy, because they are the ones people get wrong:

- **The type comes from the file name**, `strings.TrimPrefix(filepath.Base(os.Args[0]),
  "bitc-provider-")`. This is the recommended pattern: `hello` must return your own type, never a
  constant that disagrees with the name the owner pinned, and the provider suite (section 5.2)
  copies your binary to `bitc-provider-fakex` and expects `fakex`.
- **Every request runs on its own goroutine with its own cancel channel.** A `cancel` frame
  (`id` 0, `params.id` = the request to stop) closes that channel; the handler of a blocking
  method (`watch`, `launch`) selects on it and answers **its own id** within 5 s. `cancel` itself
  gets no answer.
- **`watch` blocks** and sends a `heartbeat` at least every 30 s (the toy uses 10 s), relays log
  lines as `log` events, and finishes with `{exit, began}`. For a resource you never saw it sends
  `phase` `lost` and nothing invented (protocol reference 5.4).
- **`launch` creates the resource first, waits, and only then attaches.** A failure or cancel in
  between is `pre_attach` with `id` = the resource **name** and an `inner` class; after the attach
  call it is `attached` with `id` = the resource **id**.
- **`owner` and `teardown` accept an id or a name** (`find`), and teardown of a missing resource
  succeeds. `owner` of a missing one returns `""`.
- **`endpoints`**: return the exact host names you contact. A plugin whose host comes from its
  config returns the host of the configured `api_base` (so `127.0.0.1` under the host's `[test]`
  table, where anything else is refused); a plugin that contacts nothing, like the toy, returns
  `[]`. An empty list is valid; a missing member is not.
- **`fallback_until` is `"resource"` unless you can prove otherwise.** The toy reads it from a
  variable so a test build can declare `attach` without editing the source:
  `go build -trimpath -ldflags "-X main.fallbackUntil=attach" -o bitc-provider-toy .` The provider
  suite sets the host's `fallback_until = "attach"` and refuses (exit 78) a plugin that declares
  `resource`, so this is how a test build meets that. Ship the production build without the flag
  unless FB15 passes against your fake.
- The toy does not run bitc's agent, so it relays no `__BITC_*` markers; a real plugin relays every
  log line of the resource unmodified and its `exit` must equal the code in `__BITC_EXIT__`.

```go
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
```

Pin the built file as in section 3 (with `daily_usd`, `api_key` and
`sizes = { cpu8 = { usd_per_hour = 0.10 } }`) and `bitc config check` passes. The toy declares
`artifacts` and `spend` false, so bitc never calls `fetch` and `spend`; the handlers are there as
the worked example. `fetch` streams the archive from `offset` as `data` events then
`{"bytes": n}`, and refuses an archive over `max_compressed` with a `transient` error before it
streams anything. `spend` answers `{"usd_micros": n}`. Set the caps to `true` once your platform
supports them (exchanges: protocol reference 5.6 and 5.10).

## 5. Test it

There are two suites. Both live in the bitc source tree and are run from a checkout of bitc with
`go test`. Your plugin does not link bitc code, so it may be written in any language and lives
anywhere on disk (pass its absolute path). Neither suite has a README (the protocol suite skips launch, watch and cancel for a toy-class plugin, so a plugin that really launches jobs is exercised only by the provider suite, which needs your own loopback fake of the platform API as described in this section); the flags below are the
complete list (there is no other documentation to find), and each is also described in the `flag` declarations of
`conformance/plugin/main_test.go` and `conformance/provider/main_test.go`.

### 5.1 The protocol suite (`conformance/plugin`)

A scripted bitc side drives your executable over stdin and stdout, exactly as bitc does (empty
environment, fd 3 secrets, `BITC_*` variables). It checks framing and handshake (all `caps`
members, `endpoints`, `arch`, `max_name`), that every request id gets exactly one final even when
requests overlap and with unknown fields present, exit on stdin EOF, that no secret is in your or
your children's argv or environment, and that your `hello` declares a type that matches your file
name. It needs no bitc config file, no pin and no `api_key`.

Run it from the root of a bitc checkout:

```sh
go test -race -v ./conformance/plugin \
  -plugin=/abs/path/bitc-provider-toy \
  -plugin-config='{}'
```

If the checkout's toolchain is the `scripts/dev.sh` container (the host has no Go), run the same
command through it: `scripts/dev.sh go test -race -v ./conformance/plugin -plugin=...`. The
container sees only the checkout, so the `-plugin` path (and, in 5.2, `-plugin-fake`) must lie
**inside the checkout** (for example `/abs/path/to/checkout/.rem/bitc-provider-toy`); a path
elsewhere on the host is not found.

**Pass `-v`.** Without it `go test` prints a bare `ok` and nothing shows which of the 21 tests
passed or were skipped. With `-v` each row prints `PASS` or `SKIP`, including the four skipped
rows listed below.

| Flag | What it does | Default |
| --- | --- | --- |
| `-plugin` | absolute path of your plugin executable. The type is taken from its file name (`bitc-provider-<type>`) | the reference plugin |
| `-plugin-config` | a JSON object sent as `configure`'s `config`. Put only **your own** keys in it (those your section takes besides bitc's core keys, for example `{"api_base":"http://127.0.0.1:8080"}`; `{}` if you have none). The suite sends `sizes` (`cpu8` at $0.24/hour) itself | the reference fake's `api_base` |
| `-bin` | the bitc binary under test; a relative path is from the module root | builds `./cmd/bitc` into a temporary directory |

**What this suite does not check for a third-party plugin.** Four rows need bitc's real agent or
the reference plugin's provisioning behaviour, and are skipped when `-plugin` is set (they show as
`SKIP`):

- **PT3**, the life of a job: `launch` answers the assigned name, `watch` streams the agent's
  markers and output and ends with `exit` and `began`, `output` serves a result whose MAC
  verifies, `teardown` is idempotent and `list` stops showing the resource;
- **PT4**, a cancelled `watch` is answered within 5 s;
- **PT7**, a `launch` cancelled after the create names the resource (`id`, or `pre_attach` with the
  name), never a bare `transient`;
- **PT8**, the reference plugin answers `hello` with its own type when asked for another (your
  type is checked by the rest of the suite against your file name).

The suite also never calls `fetch` or `spend`, even when `caps.artifacts` or `caps.spend` is true,
so those handlers are covered only by the provider suite. On filesystems without extended-attribute
support, row PL12 may log `file capabilities cannot be set here (...): that case is skipped` inside
a PASS. That is not a failure.

So a passing protocol suite does **not** show that your `exit` equals the `__BITC_EXIT__` marker,
that markers are relayed unmodified, that `output` and `fetch` honour `offset` and the 512 KiB
chunk limit, or that cancel works in a blocking `watch` or `launch`. Only the provider suite
(5.2), run against a fake of your own API that starts bitc's real agent, covers them. Test those
behaviours there, and read the toy's `watch` and `launch` for the cancel pattern. In particular
PT3, PT4 and PT7 never exercise a third-party plugin's launch, watch or cancel, so cancel handling
is untested unless you run the provider suite.

### 5.2 The provider suite (`conformance/provider`)

The same scenarios bitc runs against its built-in providers (launch, fallback, cap, circuit
breaker, lost resource, restart, interrupt, sweep, dropped log tail, forged result, artifacts) run
against your plugin, pinned the way a host pins it, talking to **your fake** of your API:

Run it from the root of a bitc checkout (through `scripts/dev.sh` if that is your toolchain, with
`-plugin` and `-plugin-fake` inside the checkout, and `-v` to see each row's PASS or SKIP):

```sh
go test -race -v ./conformance/provider -run PX1 \
  -plugin=/abs/path/bitc-provider-toy \
  -plugin-sha256=$(sha256sum /abs/path/bitc-provider-toy | cut -d' ' -f1) \
  -plugin-fake=/abs/path/toy-fake
```

| Flag | What it does | Default |
| --- | --- | --- |
| `-plugin` | absolute path of your plugin | the reference plugin |
| `-plugin-sha256` | sha256 of `-plugin`, checked against the file before it is pinned | none. Optional check: if set it must equal the file's sha256 (the suite computes the pin itself) |
| `-plugin-fake` | your fake API server binary (below) | the reference fake |
| `-bin` | the bitc binary under test; relative to the module root | builds `./cmd/bitc` |

The fake is a separate process you write, a loopback imitation of your platform's API. The suite
starts it as

```text
toy-fake -bitc <path of the bitc binary> -root <scratch dir> -listen 127.0.0.1:0
```

and waits for the line `listening http://127.0.0.1:<port>` on its stdout. It then gives the
plugin the fake's URL as the config key `api_base`. Each started job must run bitc's own agent
(`<bitc> __agent`) as a local process in a private directory, with the agent's output as the
resource's log and its result files served as the resource's files. The exact contract (the
`BITC_SPEC` the agent reads, how your fake builds it from the wire job, the results directory
layout and the log) is protocol reference section 6A; `examples/plugins/bitc-provider-fakex/fakeapi/main.go`
shows how in a few hundred lines. Besides your API, the fake serves the control contract:

| Request | Meaning |
| --- | --- |
| `POST /__bitc/behave {"behaviour": "<name>"}` | how resources created from now on behave. Names: `ok`, `create-fails`, `setup-fails-after-create`, `setup-fails-repo-after-create` (the last two need nothing from the fake), `lost-after-begin`, `restart-after-begin`, `drop-log-tail`, `forge-result`, `paginate-2` (your list pages hold 2), and `preattach-fails` (an instance that fails before it gets the job) |
| `GET /__bitc/calls` | JSON array of every API call in order: `{seq, method, path, query, headers, body, attach}` (exact shapes below). The call that first carries job input is marked `"attach": true` |
| `GET /__bitc/live` | JSON array of the names of the resources the fake holds as live |
| `GET /__bitc/torn` | JSON array of the ids of the resources deleted so far |
| `POST /__bitc/spend {"usd": n}` | optional: the spend your billing call should report; enables the cap scenarios |
| `POST /__bitc/seed {name, owner, created, expires}` | optional: a resource with no process, for the sweep scenarios |

The exact shapes, so you can write the control endpoints without the reference fake:

- Each `/__bitc/calls` entry is `{"seq": int (counting from 1), "method": string, "path": string,
  "query": string (the raw query, without the `?`), "headers": object (header name to its first
  value), "body": string (the raw request body), "attach": bool}`. Requests to `/__bitc/*`
  themselves are not recorded.
- `behave` and `spend` answer `204` with no body. A `behave` with an unknown behaviour answers
  `400` with `{"error": "unknown behaviour <name>"}`. `paginate-2` is a switch that stays on
  alongside the current behaviour; the other names replace the current behaviour.
- The `seed` body members are all strings: `name`, `owner` (the 64-hex owner tag) and `created` and `expires` as RFC 3339 UTC (for example `2026-01-02T03:04:05Z`).
- Any non-2xx control answer makes the suite fatal, so `behave` and `spend` may answer any 2xx (the reference fake sends `204`); only `seed`, `calls`, `live` and `torn` bodies are read.
- `seed` answers `200` with `{"id": "<resource id>"}`; the seeded resource then shows in your API's
  list and in `/__bitc/live`.
- `live` answers a JSON array of names (sorted, `[]` when empty) and `torn` a JSON array of ids
  (`[]` when empty).
- FB15 searches, for every entry before the first `attach` entry, the text `METHOD path?query`,
  the body, and one `Name: value` line per header.

A sample `GET /__bitc/calls` answer:

```json
[
 {"seq": 1, "method": "POST", "path": "/v1/instances", "query": "", "headers": {"Authorization": "Bearer k", "Content-Type": "application/json"}, "body": "{\"name\":\"bitc-ab12\",\"size\":\"cpu8\"}", "attach": false},
 {"seq": 2, "method": "GET", "path": "/v1/instances/inst-1", "query": "", "headers": {"Authorization": "Bearer k"}, "body": "", "attach": false},
 {"seq": 3, "method": "POST", "path": "/v1/instances/inst-1/jobs", "query": "wait=1", "headers": {"Authorization": "Bearer k", "Content-Type": "application/json"}, "body": "{\"spec\":\"...\"}", "attach": true}
]
```

Two rows apply to every provider, plugin or built-in:

- **PS1, `list` completeness.** With `paginate-2`, five jobs with overlapping lifetimes run; after
  each launch, end and teardown, your `list` must equal `GET /__bitc/live` exactly: no omission,
  no duplicate, no foreign name. This is the only defence against a plugin that hides a live,
  paid resource from the cap and the sweep, so write your fake with a paginated list that
  behaves like the real one.
- **FB15, nothing crosses before attach.** Required if you declare `fallback_until: "attach"`. The
  suite runs three behaviours (`ok`, `preattach-fails`, `setup-fails-after-create`), each on a
  fresh sandbox, running `bitc run heavy -- echo ARGV-CANARY-7f3a9c` with the environment value
  `ENV-CANARY-51d2e8` forwarded through `forward_env` (not `forward_env_force`). The canaries are
  B, K and M (fixed under the test seam `fixed_secrets` to 32 bytes of 0x0b, 0x0c and 0x0d, as 64
  hex characters), the argv sentinel and the env value. A deploy-key canary is not implemented
  yet. For each call before the first one marked `attach`, the suite searches the text
  `METHOD path?query`, the body, and one `Name: value` line per header, for each canary in these
  forms: raw, hex, and the four base64 alphabets (standard and URL-safe, padded and unpadded);
  for B, K and M it also searches the raw bytes the hex stands for and their encodings. Any hit
  fails.
  - A call marked `attach` is required under `ok` and `setup-fails-after-create`; a `Target`
    with none fails there. Under `preattach-fails` it is not required (the instance fails before
    it gets the job), and then every call is searched.
  - The check is not vacuous: under `ok`, the `attach` call's recorded text must contain the argv
    sentinel and the env value as raw text (plain substring). An attach body that base64-encodes
    or encrypts the job therefore fails FB15, so send the job as readable JSON or text in that
    call, and put nothing of it in any earlier one.
  - Mark the call in your fake that first carries job input. If you cannot say which call that
    is, declare `resource`.

**What the suite harness fixes today.** The provider suite is written around the reference
plugin's shape. It pins your plugin under the type name `fakex` (the file is copied to
`bitc-provider-fakex`), passes your fake's URL as the key `api_base`, offers one size `cpu8` at
$0.24/hour, and sets the host's `fallback_until = "attach"`. Your plugin therefore (a) must answer
`hello` with whatever type bitc asks for in this run, which a plugin gets for free by taking its type
from its own file name (`filepath.Base(os.Args[0])` minus `bitc-provider-`) in test builds, (b)
must read `api_base` from its config, (c) must offer a `cpu8` size, and (d) must declare
`attach` (else the host's setting is refused as wider than the plugin allows, exit 78). Adjust
your test build, not your production plugin, to meet these. For (a) and (d) the toy in section 4
already does it: build the test binary with `-ldflags "-X main.fallbackUntil=attach"` and take
the type from the file name. A plugin that hardcodes its type or declares `resource` fails here.
The toy itself cannot pass this suite, since it runs no job; the suite needs a plugin that talks
to a fake of a real API, which in turn runs bitc's agent.

### 5.3 Conformance claim

A plugin that passes both suites, with PS1 and FB15, may call itself "bitc conformant (protocol
1)". CI runs both suites against the reference plugin with `-race`; run yours the same way.

## 6. What a pinned plugin is trusted with

**A pinned plugin is fully trusted host code.** Once the owner pins it, it runs as the owner's
account, with the account's real home directory. Read that sentence as the whole trust model;
the rest of this section is detail.

What the owner hands your plugin:

- the provider credential (`api_key`) and any `secret_files`, on a pipe;
- every job's spec, including the bearer token B, the MAC key K, the nonce M, the forwarded env
  values the host allows and the deploy key when the job has one;
- network access to anything the account can reach, and the account's files.

What bitc's rails do **protect** (against a **buggy** plugin):

- a plugin that mislabels a phase, drops the log tail, mixes up results, forgets to delete a
  resource, under-reports cost or loses a name is caught or bounded: markers and the result MAC
  are checked in core, the cap and `max_live` use the host's prices, an ambiguous launch is reaped
  by name, out-of-range values count as over the cap, a protocol error never reaches a job as
  success;
- nothing runs above the account: setuid, setgid and file capabilities are refused, and the plugin
  runs under no-new-privs;
- a different binary under the same name (a changed file, a `PATH` substitution) is refused by
  the sha256 pin.

What they do **not** protect (against a **hostile** plugin the owner installed):

- the plugin holds B, K and M, so it can print a begin marker and an exit code 0 and write a
  `result.json` whose MAC verifies. A hostile plugin can fake a passing job;
- it runs as the same account, so it can read the install secret (and forge owner tags), the
  ledger and reservations, other providers' key files, and anything else in the account's home;
- the pin proves the file is the file the host owner approved. It says nothing about what that file
  does.

So: install only plugins you would run as yourself. A plugin sandbox (a separate uid or Landlock)
is not part of protocol 1.

## 7. Pricing rules

bitc never trusts a plugin to price its own resources.

- The host configures `sizes.<key>.usd_per_hour` (money, > 0) for each size the plugin offers;
  the host's `remote_limits.size` for the pool names one. This table is also sent to `configure`.
- **Estimate** = max(host price x time to expiry, your `estimate`). A low or zero estimate just
  yields the host price. A negative or non-integer one counts as over the cap and no job launches.
- **Every listed resource** is priced at max(host price of its `size_key`, its `usd_per_hour`).
  An unknown or empty `size_key` takes the **highest** host size price. Return the `size_key` you
  were sent in `job.size_key` (store it on the resource) so resources are priced correctly.
- **Spend** is max(your `spend`, bitc's ledger of today). Reporting less changes nothing.
- A negative price, a non-integer micro-dollar value, a resource without `id` or `name`, a
  `list` entry whose name is outside the requested prefixes: all count as over the cap, with the
  note `daily cap $<d>: plugin <type> reported <field> out of range`. Test your plugin so it never
  does this by accident.
- The cap and `max_live` are checked under `launch.lock` in core. You are never asked about
  locks, reservations or the ledger.

## 8. Security requirements

Treat these as requirements of a conformant plugin:

1. **Never put a secret in argv, a log line, stderr, an error `why`, a file under `TMPDIR`, or a
   URL.** Secrets arrive on fd 3 and in `launch`; send them to the platform over TLS in a request
   body or header only. bitc redacts what it knows, but a transformed secret (an encoding of the
   key) is not recognized.
2. **No job input before attach.** The job object, argv, env, B, K, M and the deploy key may reach
   an API call only when you hand the job to the resource. Before that point every call must be
   independent of the job's content (name, size and owner tag aside). This is what
   `fallback_until = "attach"` promises and FB15 checks.
3. **Create with exactly `job.name`; store `job.owner`.** Return it in `owner`. Never accept a name
   longer than your declared `max_name`, and never create two resources for one name.
4. **Never lie about outcomes.** Answer `launch` with an error that names the resource (`id`)
   whenever something exists. A `transient` without `id` after a create is a bug bitc catches with
   a name check, but only if your `list` works.
5. **`list` is the truth.** Follow every page. Return every live resource under the prefixes,
   created by any bitc process using your plugin on this account.
6. **`teardown` is idempotent** and works by id or by name. Gone is success.
7. **Share state safely.** Several bitc processes (and hosts) run your plugin at once. Be
   stateless, or keep shared state under `BITC_PLUGIN_SHARED` with your own `flock`s. Do not write
   outside `TMPDIR`, `BITC_PLUGIN_STATE` and `BITC_PLUGIN_SHARED`.
8. **Do not shell out.** Talk to the platform with a library or a direct HTTP client, never a
   shell string built from job fields. Job fields come from a repository.
9. **Declare honestly.** `fallback_until: "attach"`, `self_delete` and `multi_job` change
   what core allows. `multi_job` must be true if a resource ever serves more than one job.
10. **Contact only your declared `endpoints`.** `configure` returns the host names you will
    reach; they are shown to the owner and gate the `[test]` mode.
11. **Ship a single static ELF,** build it reproducibly (`-trimpath`, `CGO_ENABLED=0`), publish its
    sha256 and keep its dependencies minimal: the owner trusts all of it.

## 9. Checklist

- [ ] Binary is one ELF named `bitc-provider-<type>`; type regexp met; not a built-in type.
- [ ] fd 3 read to EOF; secrets never logged; stdout carries only frames.
- [ ] `hello`: own type, all seven caps, honest `fallback_until`, `multi_job`, `secrets` if used.
- [ ] `configure`: no network; `endpoints`, `arch`, `max_name` >= 60; `config` error for bad input.
- [ ] One final per id; concurrent requests; `cancel` answered within 5 s; exit within 2 s of EOF.
- [ ] `launch`: exact name, owner stored, no job input before attach, errors named by `id`.
- [ ] `list` follows all pages; `owner` and `teardown` work by id and name; teardown idempotent.
- [ ] `watch` relays raw log lines, heartbeats, `exit` equals the `__BITC_EXIT__` code.
- [ ] Protocol suite green with `-race`. Provider suite green with `-race`, PS1 and FB15 included.
- [ ] Release notes give the sha256 and say: "a pinned plugin is fully trusted host code".
