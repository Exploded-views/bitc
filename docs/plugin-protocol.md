# Provider plugin protocol, version 1

This is the reference for the protocol between bitc and an external provider executable (a
plugin). It covers the wire protocol and, in section 6A, the contract with bitc's own agent. With it,
`docs/writing-a-provider.md` and the reference plugin `examples/plugins/bitc-provider-fakex/`, you
can build a plugin without reading bitc's source, with one known gap (the agent's `known_hosts`,
section 6A.2). The
design rationale is in `docs/spec.md` 8.5; where the two differ, this document describes what the
shipped code does.

Contents: 1 Model · 2 Process, environment and secrets · 3 Framing · 4 Handshake · 5 Methods ·
6 The `job` object · 6A The agent hand-off contract · 7 Errors · 8 Launch outcomes · 9 Money · 10 Limits and timeouts ·
11 Versioning · 12 Config validation messages

## 1. Model

A provider is either compiled into bitc or a plugin: an executable named `bitc-provider-<type>`,
pinned in the host config by absolute path and sha256 (author guide, section 3). bitc starts it
once per bitc process and type, and talks to it over its stdin and stdout.

A plugin is an **API client only**. It creates, lists, watches and deletes resources on one
platform. bitc core keeps everything else:

| Core does | Plugin does |
| --- | --- |
| names the resource (`job.name`) and computes its owner tag (`job.owner`) | creates the resource with exactly that name and stores the owner where `owner` can read it back |
| daily cap, `max_live`, `launch.lock`, reservations, ledger, circuit breaker | answers `list`, `estimate`, `spend` honestly |
| parses the remote agent's markers and verifies the result MAC | relays the agent's raw log lines and result files |
| validates artifacts, runs cleanup and `bitc sweep` | deletes a resource on `teardown` |
| delivers the agent: the job carries `agent_url` and `agent_sha256` | makes the agent (bitc's own `__agent` binary) run in the resource and receive the job |

The agent on the remote side is bitc's own, never the plugin's. The plugin's job is to get it
started with the `job` object and to carry its logs and files back.

## 2. Process, environment and secrets

**Spawn.** Lazily, at the first call that needs the type. One process per bitc process and type,
in its own process group, killed if bitc dies. stdin and stdout are pipes; stderr goes to a 64 KiB
ring buffer. bitc starts the plugin through its own `__plugin-exec` shim, which sets
`PR_SET_NO_NEW_PRIVS` and execs the exact file descriptor that was hashed. The plugin sees nothing
of this except that it can never gain privileges.

**Environment**, built from empty:

| Variable | Value |
| --- | --- |
| `PATH` | `/usr/sbin:/usr/bin:/sbin:/bin` |
| `HOME` | the account's real home |
| `LANG` | `C.UTF-8` |
| `TMPDIR` | `<state>/plugins/<type>/tmp/<pid>-<start>`: 0700, private to this bitc process, removed when the plugin exits. Dirs of dead bitc processes are pruned at the next spawn |
| `BITC_PLUGIN_STATE` | `<state>/plugins/<type>/`: caches only; moves with `BITC_DIR` |
| `BITC_PLUGIN_SHARED` | `<accounting dir>/remote/<type>/plugin/` (0700): state shared across bitc processes and state dirs (client-side rate limits, a creation-quota window). Does not move with `BITC_DIR`. Guard it with your own `flock`s |
| `BITC_PLUGIN_PROTOCOL` | `1` |
| `BITC_SECRETS_FD` | `3` |
| each name in the host's `env_pass` | the caller's value, if set |

**Secrets on fd 3.** bitc writes one JSON object to file descriptor 3, then closes it (EOF). Read
it to the end once, at startup. Secrets never travel in argv, env or files.

```json
{"api_key": "sk-live-...", "token_secret": "..."}
```

`api_key` is the section's `api_key` or the content of its `api_key_file` (if neither is set the
key is empty). Every other member is a `secret_files` entry of the host config, named by the host:
its value is the bytes of that file, exactly as stored. Secret files are 0600, owned by the user.

Per-job secrets travel inside the `launch` request (section 6): the deploy key in
`params.secrets.deploy_key` and B, K and M inside `job`. bitc adds all of them, raw and as hex, to
its redactor **before** it writes the request, and passes the plugin's stderr through that
redactor. Even so, never log a request or a secret.

**stderr** is for diagnostics. bitc prints lines only at `BITC_LOG=debug`, as
`bitc: <type>: plugin: <line>`, and attaches the last 20 lines to a protocol error.

**Exit.** When bitc is done it closes the plugin's stdin. Finish open work and exit within 2 s, or
get SIGTERM, then SIGKILL 2 s later. If the plugin dies mid-session bitc respawns it once for later
calls (cleanup, `teardown`, sweep); a second death in the same bitc process makes the type
unavailable (`ErrUnavailable{Local}`, no pause) for the rest of that process. A death during an
open `launch` makes that launch ambiguous (section 8).

**Non-core config keys are never secrets.** Keys of the provider section that bitc core does not
own are sent to `configure` as plain JSON and are printed by `bitc config` and `doctor`. A key whose
name looks like a secret (the 8.4 secret-name list, for example `token`, `password`, `*_key`) is
rejected at load (exit 78) with a message pointing to `plugin.secret_files`.

## 3. Framing

One JSON object per line: UTF-8, terminated by `\n`, at most **1 MiB** per line. Blank lines are
ignored. Binary data (`output`, `fetch`, and `out` events) is base64 in a `data` member, at most
**512 KiB decoded** per frame; send larger payloads as several frames.

Write only protocol frames to stdout. Anything else on stdout (a stray `print`) is a protocol
error. Write logs to stderr.

**Request** (bitc to plugin):

```json
{"id": 7, "method": "teardown", "params": {"id": "inst-42"}}
```

`id` is an unsigned integer chosen by bitc, unique among open requests (it counts up from 1). `id`
0 is reserved for `cancel` (section 5.12).

**Event** (plugin to bitc, streaming methods only), zero or more, before the final:

```json
{"id": 7, "event": "log", "line": "__BITC_BOOT__ 4f2a..."}
```

**Final** (plugin to bitc): exactly one per request, after its events. Either

```json
{"id": 7, "result": {}}
```

or

```json
{"id": 7, "error": {"class": "transient", "why": "teardown refused: HTTP 503"}}
```

**Oversize lines.** bitc never sends a line over 1 MiB (it refuses the request first), so a plugin
need not enforce the limit on its input and may read with an unbounded line reader. A plugin must
never **write** a line over 1 MiB: bitc treats it as a protocol error and kills the plugin. Keep
`data` frames at 512 KiB decoded (about 700 KiB encoded) to stay far below it.

**Concurrency.** bitc may have several requests open at once, for example `teardown` or `owner`
while a `watch` streams. Answer each id exactly once, in any order, and accept requests in any
order. The simplest design is one goroutine or task per request, with a single lock around writes
so frames never interleave.

**Unknown fields are ignored**, by bitc and by the plugin, in every request, result, event and
error, `hello` included. Only a missing required field, a wrong JSON type or an out-of-range
value is a protocol error.

A frame for an unknown or already-finished id, a frame without an `id`, a second final for one id,
or a frame with both `result` and `error` is a protocol error (section 7). One exception: after
bitc has **cancelled** a call (section 5.12, or because a `watch` reported `lost`), events and the
final for that id are silently dropped, not protocol errors, since the plugin cannot know in
advance that bitc has stopped listening.

## 4. Handshake

### 4.1 hello

The first request is always `hello`:

```json
{"id": 1, "method": "hello", "params": {"protocols": [1], "bitc_version": "1.4.0", "type": "modal"}}
```

Answer:

```json
{"id": 1, "result": {
  "protocol": 1, "type": "modal", "plugin_version": "0.3.1",
  "caps": {"live_logs": true, "artifacts": true, "spend": false, "self_delete": false,
           "fallback_until": "resource", "fixed": false, "multi_job": false},
  "secrets": ["token_secret"]}}
```

| Field | Required | Meaning |
| --- | --- | --- |
| `protocol` | yes | integer; must be in `protocols` (today only 1) |
| `type` | yes | **your own** type name. If it differs from the `type` bitc asked for, it is a protocol error, which is how a binary pinned under the wrong name is caught. Do not echo the request's value |
| `plugin_version` | yes | free-form string, shown by `doctor` |
| `caps` | yes | all seven members below are required |
| `secrets` | no | names (from `secret_files`) the plugin needs; each must be set in the host's `plugin.secret_files`, else exit 78 |

Capabilities:

| `caps` member | Type | Meaning |
| --- | --- | --- |
| `live_logs` | bool | `watch` streams `out` events of the job's live stdout and stderr. With `false`, send no `out` events: they are not required. Either way bitc reads the final stdout and stderr with `output` after the job and verifies them against `result.json` (byte count and sha256), so `output` must serve the complete streams whatever this cap says |
| `artifacts` | bool | `fetch` can return the job's artifacts archive |
| `spend` | bool | the `spend` method exists; `false` means bitc never calls it |
| `self_delete` | bool | the platform itself enforces a time limit from the job (for example a max-runtime set at create). Set true only if so: the agent's own self-delete hook is not used for plugins, and `doctor --remote` warns when it is missing |
| `fallback_until` | `"resource"` or `"attach"` | how late a failed launch may still be handed to the next provider (section 8). Declare `attach` only if you can prove it (author guide, section 5) |
| `fixed` | bool | required. Declares that the capacity is a fixed host rather than priced hourly; plugins today are always priced by the host's `sizes`, so declare `false` |
| `multi_job` | bool | one resource serves more than one job, so its disk, files and processes can outlive a job. bitc then never sends this plugin env names the host forced, and a host setting `send_env = "forced"` is refused with exit 78 |

A plugin's caller-env level (`send_env`) defaults to `forwarded`; the host may set `none` or
`forwarded`, and `forced` only when `multi_job` is false.

`bitc_version` is informational. `hello` has a 5 s timeout.

### 4.2 configure

`configure` follows `hello`. It carries the section's non-core keys and the host's prices:

```json
{"id": 2, "method": "configure", "params": {
  "config": {"region": "eu"},
  "sizes": {"cpu8": {"usd_per_hour": 0.60}}}}
```

`config` has the keys of `[providers.<type>]` that core does not decode, as decoded TOML (strings,
numbers, booleans, arrays, objects). `sizes` is the host's core `sizes` table. **`configure` must
not touch the network.** Everything that depends on configuration comes back in its result:

```json
{"id": 2, "result": {"endpoints": ["api.eu.modal.example"], "arch": ["amd64"], "max_name": 63}}
```

| Field | Required | Meaning |
| --- | --- | --- |
| `endpoints` | yes | the exact host names or IP literals the plugin will contact (no wildcards, no URLs, no ports). A plugin whose host comes from its config declares the host of that config value (for example the host of `api_base`; under `[test]` that is `127.0.0.1`). A plugin that contacts nothing declares `[]`: an empty list is valid, a missing member is a protocol error. bitc uses them for the `[test]` loopback gate: under `[test]` any endpoint that is not `127.0.0.1` or `::1` is refused, exit 78 |
| `arch` | yes | CPU architectures the resources run: `"amd64"`, `"arm64"` (the job's `tools` and the agent are chosen per arch) |
| `max_name` | yes | the longest resource name the platform accepts. **At least 60**, else exit 78. bitc builds names of at most `min(max_name, 63)` characters and shortens only the pool part, so you must accept any name up to your declared `max_name` (a name has the shape `<prefix>-<install16>-<pool>-<size>-<job8>-<expiry>`) |

If the configuration is unusable, answer an error of class `config` (for example
`{"class": "config", "why": "region \"mars\" is not offered"}`). bitc reports it as exit 78:
`bitc: <file>: providers.<type>: <why>`. `configure` has a 5 s timeout.

## 5. Methods

Every method maps onto bitc's internal `Provider` interface. Handles are objects
`{"id": "<resource id>", "name": "<resource name>", "results_url": "<optional>"}` exactly as your
`launch` returned them (`results_url` is `""` if you gave none).

| Method | params | Events | Result | bitc timeout |
| --- | --- | --- | --- | --- |
| `hello` | section 4.1 | | section 4.1 | 5 s |
| `configure` | section 4.2 | | section 4.2 | 5 s |
| `preflight` | `{job}` | | `{}` | 5 s |
| `estimate` | `{job}` | | `{"usd_micros": int}` | 10 s |
| `launch` | `{job, secrets?}` | `setup`, `phase` | `{id, name, results_url?}` | `start_timeout` |
| `watch` | `{handle}` | `phase`, `log`, `out`, `heartbeat` | `{exit, began, why?}` | none; 90 s of silence = lost; for an unknown resource: one `lost` phase, then any `transient` final (5.4) |
| `output` | `{handle, stream, offset}` | `data` | `{bytes}` | 60 s of silence |
| `fetch` | `{handle, max_compressed, offset}` | `data` | `{bytes}` | 60 s of silence |
| `owner` | `{id}` | | `{"owner": "<tag>"}` | 30 s |
| `teardown` | `{id}` | | `{}` | 10 s |
| `list` | `{prefixes}` | | `{"resources": [...]}` | 30 s |
| `spend` | `{}` | | `{"usd_micros": int}` | 30 s |
| `cancel` | `{id}` | | none (no answer) | |

### 5.1 preflight

```json
{"id": 3, "method": "preflight", "params": {"job": { "...": "see section 6" }}}
```

Network-free eligibility: could this job ever run here? Answer `{}`, or an error of class
`ineligible` (for example a size you do not offer or a request too large) so bitc moves on to the
next provider without creating anything. bitc itself already rejects a `size_key` that is not in
the host's `sizes`.

### 5.2 estimate

```json
{"id": 4, "result": {"usd_micros": 1800000}}
```

Your own estimate of the job's worst-case cost in micro-dollars (millionths of a dollar) up to its
expiry `E`. bitc uses the larger of this and the host's price times the time to `E` (section 9).
It must be a non-negative integer.

### 5.3 launch

```json
{"id": 5, "method": "launch", "params": {
  "job": { "...": "section 6" },
  "secrets": {"deploy_key": "-----BEGIN OPENSSH PRIVATE KEY-----\n..."}}}
```

`secrets` is present only when the job has a deploy key. Create the resource with **exactly**
`job.name` and store `job.owner` on it, wait until it is ready, then hand it the job so that bitc's
agent starts. Result:

```json
{"id": 5, "result": {"id": "inst-42", "name": "bitc-0123456789abcdef-heavy-cpu8-deadbeef-1793000100"}}
```

`name` must equal the assigned `job.name`, and `id` must be non-empty; anything else makes the
launch ambiguous. Optional events while you work (progress text for the user):

```json
{"id": 5, "event": "setup", "line": "created inst-42"}
```

`launch` runs until `start_timeout`. Its failure handling is the delicate part of the protocol:
read section 8 before writing it.

**Job input boundary.** Do not send the job object, argv, env, B, K, M, or the deploy key to any
API call until the moment you hand the job to the resource. Everything before that call is the
"pre-attach" part, where bitc can still fall back to another provider. A plugin that declares
`fallback_until: "attach"` is tested for this (author guide, section 5).

### 5.4 watch

```json
{"id": 6, "method": "watch", "params": {"handle": {"id": "inst-42", "name": "bitc-...", "results_url": ""}}}
```

Stream until the job has ended. Events:

```json
{"id": 6, "event": "log",   "line": "__BITC_BEGIN__ 9a8b..."}
{"id": 6, "event": "out",   "stream": "stdout", "data": "aGVsbG8K"}
{"id": 6, "event": "phase", "phase": "booting", "why": "instance restarting"}
{"id": 6, "event": "heartbeat"}
```

| Event | Fields | Meaning |
| --- | --- | --- |
| `log` | `line` | one raw line of the resource's log, **markers included**. Relay every line unmodified; bitc parses the agent's markers (`__BITC_BOOT__`, `__BITC_BEGIN__`, `__BITC_EXIT__`, `__BITC_LOST__`, `__BITC_SETUP_FAILED__`, each carrying the job's nonce M) and turns them into phases. Lines before the job begins are shown as setup output |
| `out` | `stream` (`"stdout"` or `"stderr"`), `data` (base64) | live output of the command. Send it only when `caps.live_logs` is true; with `false` send none (bitc does not rely on `out` for the final stdout and stderr, it reads them with `output`) |
| `phase` | `phase`, `why?` | only `"booting"` (the resource is starting or restarted) or `"lost"` (it is gone). `begun` and `ended` come **only** from markers: any other phase value is a protocol error |
| `heartbeat` | | send at least every 30 s when nothing else is happening. bitc treats 90 s without any event as `lost` |

Result when the job's resource has stopped:

```json
{"id": 6, "result": {"exit": 0, "began": true, "why": ""}}
```

`exit` (0 to 255, required) must equal the code in the `__BITC_EXIT__` marker if one was relayed,
else bitc treats the result as failed verification. `began` (bool, required) says whether the
command started. **A resource that is gone or that the plugin never saw** (a restarted plugin, a deleted instance,
a handle it does not recognise): send one `phase` event with `phase: "lost"` and a `why`, and no
invented `exit` or `began` value (for example `exit 255, began false`): bitc takes `lost` as the
outcome. Then send a final (any `transient` error), which bitc discards: on a `lost` event bitc
stops the call, sends `cancel` for it and drops any further event or final for that id (section 3). The same applies to a `lost` you report mid-job. A setup failure that the agent did not mark can be
returned as an error of class `setup`.

### 5.5 output

```json
{"id": 8, "method": "output", "params": {"handle": {"id": "inst-42", "name": "bitc-..."}, "stream": "stdout", "offset": 4096}}
```

`stream` is `"stdout"`, `"stderr"`, `"result"` (the job's `result.json`) or `"state"` (its
`state.json`). Send the bytes from `offset` to the end as `data` events, then finish:

```json
{"id": 8, "event": "data", "data": "<base64, at most 512 KiB decoded>"}
{"id": 8, "result": {"bytes": 1024}}
```

An unknown stream is an `ineligible` error. A file that is not there yet is `transient`. A file that exists but is empty, or an `offset` equal to its length, is answered with no `data` events and `{"bytes": 0}`, never `transient`. Distinguish "absent" from "empty" in your store (store an empty file as a non-nil empty slice, or test with a `_, ok :=` map lookup).

### 5.6 fetch

```json
{"id": 9, "method": "fetch", "params": {"handle": {"id": "inst-42", "name": "bitc-..."}, "max_compressed": 268435456, "offset": 0}}
```

Send the job's artifacts archive (`artifacts.tar.gz`) from `offset` as `data` events. If the
archive is larger than `max_compressed` bytes, refuse with a `transient` error before streaming.
bitc validates the archive itself. A complete exchange (a 40 000 byte archive, sent from offset 0
in two frames, then the count of bytes sent from `offset`):

```json
{"id": 9, "event": "data", "data": "<base64 of the first 32768 bytes>"}
{"id": 9, "event": "data", "data": "<base64 of the remaining 7232 bytes>"}
{"id": 9, "result": {"bytes": 40000}}
```

The same call against an archive of 300 000 000 bytes with `max_compressed` 268 435 456 is
refused with one frame and no `data` events:

```json
{"id": 9, "error": {"class": "transient", "why": "artifacts archive is 300000000 bytes, over max_compressed 268435456"}}
```

An archive that is not there yet is also `transient`. Only called when `caps.artifacts` is true.

### 5.7 owner

```json
{"id": 10, "method": "owner", "params": {"id": "inst-42"}}
{"id": 10, "result": {"owner": "efefefef...ef"}}
```

Return the owner tag you stored at create, byte for byte (it is an opaque 64-character hex string; keep it with the resource in whatever form your platform offers, and never regenerate it). For a resource that does not exist return
`{"owner": ""}`. bitc tears down only resources whose tag it can verify, so this call guards every
sweep.

### 5.8 teardown

```json
{"id": 11, "method": "teardown", "params": {"id": "inst-42"}}
{"id": 11, "result": {}}
```

Delete the resource. It must be idempotent: a resource that is already gone is **success**.
`id` is the resource id, **or the assigned resource name**: after a `pre_attach` error, and when it reaps an ambiguous launch, bitc tears down by name, so accept both. 10 s timeout.

### 5.9 list

```json
{"id": 12, "method": "list", "params": {"prefixes": ["bitc-0123456789abcdef-heavy-"]}}
{"id": 12, "result": {"resources": [
  {"id": "inst-42", "name": "bitc-0123456789abcdef-heavy-cpu8-deadbeef-1793000100",
   "size_key": "cpu8", "usd_per_hour": 0.60,
   "created": "2026-10-23T05:00:00Z", "expires": "2026-10-23T07:15:00Z", "state": "running"}]}}
```

Return **every** live resource whose name starts with any of `prefixes`: follow all pages of the
platform's API. bitc counts what you return in the daily cap and `max_live` and tears it down in
a sweep, so an omitted resource is invisible to all three. Resource members:

| Member | Required | Notes |
| --- | --- | --- |
| `id`, `name` | yes | non-empty; the name must start with one of `prefixes` |
| `size_key` | no | the host's size key; an unknown or empty one is priced at the highest host price |
| `usd_per_hour` | no | number, not negative; priced at max(host price, this) |
| `created`, `expires` | no | RFC 3339. Leaving `expires` out is allowed and the suites accept it; it only helps the sweep when the platform knows it |
| `state` | no | free-form string |
| `install`, `pool`, `job` | no | parsed from the name when absent |

A foreign name, a negative price, or a missing `id` or `name` is an out-of-range value: bitc treats
it as over the cap and refuses to launch.

### 5.10 spend

```json
{"id": 13, "method": "spend", "params": {}}
{"id": 13, "result": {"usd_micros": 12500000}}
```

Today's account spend in micro-dollars. Only called when `caps.spend` is true. A value below
bitc's own ledger changes nothing (bitc uses the larger).

### 5.11 heartbeat and event rules

Events are only legal for the streaming methods above. Events for a finished or unknown id are a
protocol error, except for an id bitc has cancelled (including after a `lost` phase): its events
and final are dropped silently (section 3). Unknown event kinds on a known id are ignored.

### 5.12 cancel

When a call outlives its timeout, or bitc is interrupted, it sends

```json
{"id": 0, "method": "cancel", "params": {"id": 5}}
```

There is no answer to the `cancel` itself. Stop the work for request 5 and answer **request 5**
once, with whatever you have, within 5 s. A plugin that has not answered 5 s after a `cancel` is
killed and respawned. For every method except `launch` and `watch`, bitc reports the call as a transient
`<method> timed out after <d>`. For `launch`, see section 8. For `watch`, 90 s without an event is
not a transient: bitc reports the resource as lost (`<id> was lost: no event from the plugin for <d>`,
with a `lost` phase). A bitc interrupt (the job is cancelled by the user or a signal) also sends
`cancel`, but is not reported as a `timed out` transient: the call ends as an interrupt.

If a `launch` is cancelled after the resource was created, answer with an error that names it
(`pre_attach` or `attached` with `id`), never a bare `transient`.

## 6. The `job` object

`preflight`, `estimate` and `launch` carry `params.job`: the protocol-1 wire form of bitc's job
spec. All keys are snake_case. Every key except `test` is always present.

| Key | JSON type and encoding |
| --- | --- |
| `job`, `pool`, `cwd`, `clone_url`, `ref`, `sha`, `size_key`, `repo_id`, `install`, `name`, `owner`, `agent_url`, `agent_sha256` | string. `sha`, `agent_sha256` and `owner` are lowercase hex |
| `argv` | array of strings: the command |
| `env` | array of `"KEY=VALUE"` strings, in order: only the names the host's `send_env` lets cross |
| `timeout_s` | integer seconds |
| `created`, `deadline`, `expiry` | string, RFC 3339 UTC with `Z`, whole seconds. `expiry` (E) is the latest time the resource may exist |
| `bearer`, `mac_key`, `nonce` | string, 64 lowercase hex characters (B, K, M). Secret |
| `artifacts` | array of `{"path": string, "replace": bool}` |
| `packages` | array of strings |
| `tools` | array of `{"name", "url", "sha256", "arch"}` strings |
| `setup` | array of `{"name": string, "run_as": "root" \| "job", "argv": [string]}` |
| `services` | array of `{"name", "run_as", "argv", "ready": [string], "ready_timeout_s": int}` |
| `root_steps` | bool |
| `test` | `{"unprivileged": bool, "test_root": string}`, present **only** under the host's `[test]` table |

Empty lists are `[]`, never `null`. The deploy key is **not** in `job`; it is only in
`params.secrets.deploy_key`. A RunPod-style secret name is not sent either. A new field is added
to this table only as an optional one, without a protocol bump; ignore fields you do not know.

An example (values shortened):

```json
{"job": "j1", "pool": "heavy", "cwd": "sub",
 "clone_url": "git@example.com:o/r.git", "ref": "refs/bitc/jobs/j1", "sha": "abab...ab",
 "size_key": "cpu8", "repo_id": "repo1", "install": "0123456789abcdef",
 "name": "bitc-0123456789abcdef-heavy-cpu8-deadbeef-1793000100", "owner": "efef...ef",
 "agent_url": "https://example.com/agent", "agent_sha256": "1212...12",
 "argv": ["npm", "test"], "env": ["CI=1"], "timeout_s": 5400,
 "created": "2026-10-23T05:00:00Z", "deadline": "2026-10-23T06:00:00Z", "expiry": "2026-10-23T07:15:00Z",
 "bearer": "0b0b...0b", "mac_key": "0c0c...0c", "nonce": "0d0d...0d",
 "artifacts": [{"path": "out", "replace": true}], "packages": ["git"],
 "tools": [{"name": "go", "url": "https://example.com/go.tgz", "sha256": "cdcd...cd", "arch": "amd64"}],
 "setup": [{"name": "deps", "run_as": "job", "argv": ["npm", "ci"]}],
 "services": [{"name": "pg", "run_as": "root", "argv": ["pg"], "ready": ["pg_isready"], "ready_timeout_s": 30}],
 "root_steps": true}
```

The complete golden example is `conformance/plugin/testdata/job-v1.json`.

The agent does **not** read this object. The plugin translates it into the agent's own spec,
section 6A.

## 6A. The agent hand-off contract

The agent is the executable `<bitc> __agent`: bitc's own binary, fetched from `job.agent_url` and
verified against `job.agent_sha256`. The plugin has it run on the resource and later reads what it
wrote. The agent is configured by its own spec, **not** by the wire `job`.

### 6A.1 Starting the agent

There are two ways to start it. Pick one per platform.

**Environment boot** (`bitc __agent`, no arguments). The agent reads these variables from its
environment, moves the secrets out of it, and re-executes itself:

| Variable | Value |
| --- | --- |
| `BITC_SPEC` | required. The agent spec of 6A.2 as JSON, then base64 (standard alphabet, padded) |
| `BITC_DEADLINE` | the job deadline `job.deadline` as unix seconds (decimal). Without it the deadline is the epoch |
| `BITC_DEPLOY_KEY` | `params.secrets.deploy_key` when the job has one, else unset |
| `BITC_PROVIDER_TYPE` | a built-in provider's type; the agent uses it only to look up that provider's self-delete hook. A plugin leaves it unset |

The agent binds `spec.port` before it touches any package or repository and serves its results
over HTTP there (authorised by the bearer token `B`). If you read the results from the
filesystem instead, `port` is still bound, so choose one that is free on the resource (for
example 8080). Under the `[test]` table (`job.test` present) with `port` 0 the agent binds a free
loopback port, as the reference fake does.

**`--boot-stdin`** (`bitc __agent --boot-stdin`, for platforms with an exec channel). No
environment variables, no listener. Write one frame to the agent's stdin: an 8-byte big-endian
length `n` (1 to 8 388 608), then `n` bytes of JSON:

```json
{"spec": "<the BITC_SPEC value>", "deadline": 1793000000, "deploy_key": "-----BEGIN ...",
 "provider": "", "resource_id": "", "scoped_key": "", "api_base": "", "results_root": ""}
```

`spec.port` must be `-1`. `results_root` is optional (default `/run/bitc`); if present it must be
a clean absolute path. A refused payload prints `refused: <why>` on the agent's stdout and exits
65 with nothing started. Results are then read with the agent's own verbs, run through the same
exec channel: `bitc __agent [--root DIR] read <job> <file> <offset>` (the file's bytes from the
offset to its end), `tail <job> <off_agent.log> <off_out.log> <off_err.log>`, `list` and
`teardown <job>`. Their exit codes: 64 bad argument, 65 offset beyond the end, 66 not found, 69
agent error. `<file>` is one of `agent.log`, `out.log`, `err.log`, `result.json`, `state.json`,
`artifacts.tar.gz`.

### 6A.2 The agent spec (`BITC_SPEC`)

A JSON object. Build it from the wire job (section 6) as follows; the fields differ in name and
in encoding, so copying the wire job does not work.

| Spec field | Type | From the wire job |
| --- | --- | --- |
| `job`, `pool`, `cwd`, `clone_url`, `ref`, `sha` | string | the same keys |
| `argv`, `env` | array of strings | the same keys |
| `timeout` | **Go duration string**, for example `"5400s"` or `"1h30m0s"` | `timeout_s` seconds, formatted as a duration |
| `packages` | array of strings | `packages` |
| `tools` | array of `{"Name","URL","SHA256"}` (capitalised keys, no `arch`) | `tools[]` `name`, `url`, `sha256` |
| `setup` | array of `{"name","run_as","argv"}` | `setup` |
| `services` | array of `{"name","run_as","argv","ready","ready_timeout"}` | `services`; `ready_timeout` is a **duration string** made from `ready_timeout_s` |
| `root_steps` | bool | `root_steps` |
| `artifacts` | array of **path strings** | the `path` of each `artifacts[]` member; `replace` is not passed to the agent |
| `bearer`, `mac_key`, `nonce` | string, 64 lowercase hex | `bearer`, `mac_key`, `nonce` |
| `port` | integer | yours: the results port (environment boot) or `-1` (`--boot-stdin`) |
| `known_hosts` | string, OpenSSH `known_hosts` text | **not in the wire job**, see below |
| `unprivileged`, `test_root` | bool, string | from `job.test` when present, else `false` and `""`. Set only for a local test process: the agent then runs as the current user and keeps everything under `test_root` |

Omit `packages`, `tools`, `setup`, `services` and `artifacts` when empty (the agent accepts either
absent or `[]`). Unknown spec fields are ignored by the agent. Example (values shortened):

```json
{"job": "j1", "pool": "heavy", "argv": ["npm", "test"], "cwd": "sub", "env": ["CI=1"],
 "clone_url": "git@example.com:o/r.git", "ref": "refs/bitc/jobs/j1", "sha": "abab...ab",
 "known_hosts": "example.com ssh-ed25519 AAAA...", "timeout": "5400s",
 "packages": ["git"], "tools": [{"Name": "go", "URL": "https://example.com/go.tgz", "SHA256": "cdcd...cd"}],
 "setup": [{"name": "deps", "run_as": "job", "argv": ["npm", "ci"]}],
 "services": [{"name": "pg", "run_as": "root", "argv": ["pg"], "ready": ["pg_isready"], "ready_timeout": "30s"}],
 "root_steps": true, "artifacts": ["out"], "port": 8080,
 "bearer": "0b0b...0b", "mac_key": "0c0c...0c", "nonce": "0d0d...0d"}
```

The spec carries B and K, so it is a secret: never put it in argv, in a log, or in anything the
platform shows to other users; the agent moves it out of its own environment at boot.

**`known_hosts` (known gap).** The agent verifies the clone host's SSH key against this text
(`StrictHostKeyChecking=yes`) and never falls back to a default: an empty value makes a clone
over SSH fail. The wire job does not carry it. bitc's own providers take it from the host's
`[transport] known_hosts`, which defaults to GitHub's published keys (spec 8.4). The reference
plugin takes it from bitc's internal `config.BuiltinKnownHosts`, which a third-party plugin cannot
import. Until a later protocol revision adds an optional `known_hosts` job member (it can be
added without a version bump, section 11), a plugin whose jobs clone over SSH must embed the host
keys of the git hosts it serves (verify them out of band, for example `ssh-keygen -lf`). A plugin
whose jobs clone over HTTPS or from a local path may send an empty string.

### 6A.3 What the agent writes, and where

The agent writes the job's results to `<results_root>/<job>/`, mode 0700, where `<results_root>` is
`/run/bitc` (or the `results_root` of a boot-stdin payload, or `<test_root>/run` when
`unprivileged` and `test_root` are set) and `<job>` is `job.job`.

| File | Content | While the job runs |
| --- | --- | --- |
| `out.log`, `err.log` | the command's stdout and stderr | written as `out.log.part`, `err.log.part`; renamed when complete |
| `state.json` | the signed phase record | replaced atomically |
| `result.json` | the MAC-signed result | appears once, last |
| `artifacts.tar.gz` | the packed artifacts | appears before `result.json` |
| `agent.log` | the agent's own log (boot-stdin) | grows |

The plugin's methods map straight onto these files (a file may not exist yet, then the answer is
`transient`): `output` with `stream` `stdout`, `stderr`, `result`, `state` serves `out.log`,
`err.log`, `result.json`, `state.json` from the byte `offset`; while a log is still growing serve
its `.part` file, and the finished file once the `.part` is gone. `fetch` serves
`artifacts.tar.gz`. Serve the bytes unmodified: bitc verifies the MAC.

### 6A.4 The log

The agent's **standard output, with standard error merged into it, is the resource's log**. It
contains the `__BITC_BOOT__`, `__BITC_BEGIN__`, `__BITC_EXIT__`, `__BITC_SETUP_FAILED__` and
`__BITC_LOST__` marker lines among other text. `watch` relays it line by line, unmodified and in
order, as `log` events (section 5.4); bitc parses the markers and `exit` in the final must equal
the code in `__BITC_EXIT__`. The agent keeps serving its results after the job ends; the
`__BITC_EXIT__` line is printed once `result.json` is committed, so it is the end of the job. After
that an environment-boot agent keeps running and serving its results (so the platform does not
restart the resource); the plugin ends it by `teardown`. A `--boot-stdin` agent exits instead. In
`--boot-stdin` mode the same lines are also appended to `agent.log`, which is the durable record
to read if the exec channel dropped.

The reference fake (`fakeapi/main.go`) is a working example of 6A.1 (environment boot), 6A.2, 6A.3
and 6A.4 with the agent as a local process.

## 7. Errors

An error final is `{"id": n, "error": {...}}`:

```json
{"class": "transient", "why": "one line", "id": "inst-42", "step": "deps",
 "setup_class": "repo", "inner": {"class": "transient", "why": "..."}}
```

`class` and `why` are required; the others depend on the class. Never put a secret in `why`.

| `class` | Use | Where |
| --- | --- | --- |
| `ineligible` | this job can never run here (size, request too large, unsupported feature) | any method |
| `transient` | try again later (a 5xx, a rate limit, no capacity) | any method |
| `unavailable` | the provider is down or refusing us (auth failed, quota exhausted): bitc records one pause so no job tries it for a while | any method |
| `unavailable_local` | the problem is on this machine (cannot reach the network): no pause | any method |
| `pre_attach` | `launch` only: a resource exists but no job input has reached it. Needs `id` = the assigned name and `inner` `{class, why}` with class `transient`, `unavailable` or `ineligible` | `launch` |
| `attached` | `launch` only: a resource exists and job input may have reached it. Needs `id` (the resource id). `inner` optional (default transient) | `launch` |
| `setup` | the repo's own setup step failed. Needs `step` and `setup_class` (`repo`: the repo's fault, exit 69; `infra`: ours, exit 75) | `watch`; `launch` after attach |
| `config` | the configuration is unusable | `configure` |

Anything else is a protocol error: an unknown `class`, a malformed frame, a second final for one
id, a frame for an unknown id, a `begun` or `ended` phase event, or a missing or mistyped required
field. bitc reports it as `bitc: <type>: plugin protocol error: <why>` (the type becomes
unavailable locally, no pause; exit 69 if it was the last provider). **During an open `launch` a
protocol error is an ambiguous launch instead** (section 8).

Use `unavailable` sparingly: it pauses the provider for every job.

## 8. Launch outcomes

Once bitc has written a `launch` request, the outcome is **definite** only if it ends in a
well-formed final frame that is

- a `result` (success), or
- an `error` of class `ineligible`, `transient`, `unavailable` or `unavailable_local` **without**
  `id`, followed by bitc's name check (below), or
- an `error` of class `pre_attach` (definite only if the host's effective boundary is `attach`) or
  `attached` or `setup` (after attach), or any of the first four **with** `id`.

Every other outcome is an **ambiguous launch**: a timeout, a cancel, the plugin dying or exiting, a
malformed frame, a second final, a forbidden phase event, an error of an unknown class, a result
whose `name` is not the assigned one. bitc then:

1. restarts the plugin and **reaps by name**: `list` with the job's full name as the only prefix,
   `owner` on each match, `teardown` on a match whose owner tag it computed itself. If that cannot
   be done the name is left for the sweep (`; <name> left for sweep`);
2. exits **75** with `STARVED <pool> (<type>: launch ambiguous: <why>)`. There is no retry and no
   next provider, `local` included, because only the plugin knows whether job input was sent;
3. keeps the job's reservation until `E` if the name was not confirmed gone.

**Rules for the plugin's `launch`:**

| Situation | Answer |
| --- | --- |
| nothing was created, and you are sure | error without `id`: `transient`, `unavailable`, `unavailable_local` or `ineligible` |
| a resource exists, no job input sent yet | `pre_attach` with `id` = the assigned name |
| a resource exists and the job may have been sent | `attached` with `id` = the resource id |
| you do not know | do not guess: let it time out or exit; bitc reaps by name |

An error without `id` is trusted only after bitc's own **name check**: before any retry or next
provider it calls `list` with the job's full name and, for each match, `owner`. If nothing is
found the error stands as a failure before any resource existed (retry once if `transient`). If a
resource with the right owner exists, bitc tears it down, records a breaker failure, and exits 75
with `STARVED <pool> (<type>: launch said <class> but <name> exists)`. If `list` or `owner` fails,
the launch is treated as ambiguous. So a `list` that works is part of a correct `launch`.

**The two boundaries.** `fallback_until` says how late a failed job may still move on to the next
provider: `resource` (until a resource exists) or `attach` (until job input first reaches it). The
plugin declares what it can honour in `hello`; the host may set the key lower, never higher, and a
plugin defaults to `resource` until the host opts in. Under `resource`, a `pre_attach` is read as
`attached`. After the boundary a job never moves to another provider, `local` included.

## 9. Money

A plugin cannot lower any limit the host configured.

- The host **must** price each size: `sizes.<key>.usd_per_hour` (> 0) in the provider section. A
  pool's size must name one of them.
- **Estimate** = max(host price x time to `E`, your `estimate`).
- **Each listed resource** is priced at max(host price of its `size_key`, its `usd_per_hour`).
- **Spend** = max(your `spend`, bitc's ledger of today).
- **Out of range** values (a negative or non-integer `usd_micros`, a negative or non-numeric
  `usd_per_hour`, a resource without `id` or `name`, a `list` name outside the prefixes) count as
  over the cap: `daily cap $<d>: plugin <type> reported <field> out of range`; no job launches.
- A resource you leave out of `list` is still counted while this host holds a reservation for it,
  and is invisible to the cap, `max_live` and the sweep afterwards. Core cannot detect that; the
  provider suite row PS1 does (author guide, section 5).

## 10. Limits and timeouts

| Limit | Value |
| --- | --- |
| line (frame) | 1 MiB, including the request line bitc sends. A `launch` request larger than that is refused as ineligible |
| decoded payload per `data` or `out` frame | 512 KiB |
| stderr kept | 64 KiB ring; last 20 lines attached to a protocol error |
| `hello`, `configure`, `preflight` | 5 s |
| `estimate` | 10 s |
| `teardown` | 10 s |
| `owner`, `list`, `spend` | 30 s |
| `launch` | the host's `start_timeout` |
| `watch` | none, but 90 s without any event = resource lost; send `heartbeat` at least every 30 s |
| `output`, `fetch` | 60 s without an event |
| answer to a `cancel` | 5 s, then killed and respawned |
| exit after stdin closes | 2 s, then SIGTERM, then SIGKILL after 2 more |
| resource name | at most `min(max_name, 63)`, and `max_name` at least 60 |

On expiry of any bound other than `launch` and `watch`, bitc sends `cancel` and reports a transient
`<method> timed out after <d>`. On expiry of the `watch` bound (90 s without an event) bitc sends
`cancel` and treats the resource as lost (`<id> was lost: no event from the plugin for <d>`), not as
a transient. An interrupt of bitc itself is not reported as a timeout either.

## 11. Versioning

`protocol` is an integer, now 1. Changes that a version-1 peer could misread bump it: a new
required field, a changed meaning or type, or a new method bitc depends on. New optional fields,
new optional events and new error `inner` details do not bump it, which is why every peer ignores
unknown fields. bitc 1.x speaks protocol 1 for its whole major version. A plugin that answers a
`protocol` bitc does not support is refused at `config check` and `doctor`:
`providers.<type>.plugin: speaks protocol <n>, bitc supports [1]`.

## 12. Config validation messages

These are the exit 78 messages related to plugins (prefix `bitc: <file>: `). Authors meet them
while pinning.

| Case | Message |
| --- | --- |
| `plugin` key in a repo file | `providers.<t>.plugin: host-only key` |
| `<t>` is a built-in type | `providers.<t>.plugin: "<t>" is a built-in provider` |
| `<t>` not `^[a-z][a-z0-9]{0,15}$` | `providers.<t>: invalid provider type` |
| `path` not absolute, or basename not `bitc-provider-<t>` | `providers.<t>.plugin.path: must be an absolute path ending in bitc-provider-<t>: "<v>"` |
| missing, symlink, not regular, not executable, not ELF | `providers.<t>.plugin.path: <reason>: "<v>"` |
| file or a parent group- or world-writable, or owned by another user | `providers.<t>.plugin.path: insecure: <which path>` |
| setuid or setgid bit, or a `security.capability` xattr | `providers.<t>.plugin.path: insecure: setuid/setgid/capabilities: "<v>"` |
| `sha256` not 64 lowercase hex | `providers.<t>.plugin.sha256: invalid: "<v>"` |
| sha256 mismatch | `providers.<t>.plugin: sha256 mismatch (want <12>, file <12>)` |
| `env_pass` entry invalid or secret-named | `providers.<t>.plugin.env_pass: "<v>" not allowed` |
| `secret_files` name invalid or `api_key`; file insecure | `providers.<t>.plugin.secret_files.<n>: <reason>` |
| a `hello` `secrets` name missing from `secret_files` | `providers.<t>.plugin.secret_files: "<n>" required by the plugin` |
| inline non-core key named as a declared secret, or secret-looking | `providers.<t>.<n>: secret; set it in plugin.secret_files` |
| no `sizes`, a size without `usd_per_hour` > 0, or a pool size not in `sizes` | `providers.<t>.sizes: a usd_per_hour per size is required for a plugin` |
| `[test]` and a non-loopback endpoint | `providers.<t>: [test]: endpoint "<e>" is not a loopback literal` |
| `configure` error | `providers.<t>: <why>` |
| unsupported protocol | `providers.<t>.plugin: speaks protocol <n>, bitc supports [1]` |
| `max_name` below 60 | `providers.<t>.plugin: max_name <n> is below 60, the shortest name bitc builds` |
| `fallback_until = "attach"` wider than the plugin declares | `providers.<t>.fallback_until: "attach" is wider than <t> allows ("resource")` |
| `send_env = "forced"` on a `multi_job` plugin | `providers.<t>.send_env: "forced" is not allowed: <t> reuses resources across jobs` |

Run-time lines: `bitc: <type>: plugin protocol error: <why>`; `bitc: <type>: plugin <path> changed
since load (sha256 <want12>, now <got12>)`; `bitc: <type>: plugin <path>: insecure:
setuid/setgid/capabilities`; `STARVED <pool> (<type>: launch ambiguous: <why>)`; `STARVED <pool>
(<type>: launch said <class> but <name> exists)`; `bitc: <type>: plugin: <line>` (debug only).
`doctor` adds `ok|fail plugin-<type>: protocol 1, <plugin_version>, sha256 <12>`.
