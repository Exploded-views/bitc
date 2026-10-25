# Design: providers delta (plugins, fallback boundary, fixed-capacity hosts)

Status: **draft r4, 2026-10-23, for review** (r4 applies the caller-env decision,
r3 review round 2, r2 round 1; see Review notes).
A delta against `docs/spec.md` v3.3; it does
not edit the spec. Once approved it folds in as spec v3.4 together with
`docs/design/sprites-provider.md` ("Sprites phase 1"), which builds on it. Edits to the spec are
marked **[n edit]** with the spec section they replace or extend.

Direction (2026-10-23): three additions. **A.** The exec/JSON provider protocol of
8.5 becomes a designed, first-class seam. **B.** A per-provider fallback boundary replaces any
Sprites-only amendment of 15.8. **C.** Fixed-capacity backends ("a host you already have", over
ssh), first target a Mac running a Linux VM.

Decision (2026-10-23): "generalized, host setting". Which caller env may reach a provider
is **one host-only key on every provider section**, `providers.<t>.send_env` (section S), in
place of any provider-specific rule (the r3 ssh `forward_env` boolean and the Sprites
`forward_env_force` decision).

The spec's model and vocabulary stay: pools with an ordered `providers` list, `on_failure`, the
`Provider` interface and `Caps`, `daily_usd` and `max_live`, host / repo key classes,
`ErrIneligible` / `ErrTransient` / `ErrUnavailable`, and the 15.8 "no fallback once committed"
principle. Items marked **(unverified)** are probes listed in C.9.

## 0. Summary

- **A. Plugins.** A provider may be an external executable `bitc-provider-<type>`, pinned in the
  host file by absolute path and sha256, speaking a versioned NDJSON protocol on stdin/stdout in
  one long-lived session per bitc process. Its requests map 1:1 onto the eleven `Provider` methods
  and `Caps`; `Watch`, `Output` and `Fetch` stream. Errors carry the 8.1 classes plus the two
  boundary classes of B. A `launch` that ends without a definite result or classified error
  (timeout, crash, malformed frame) is an **ambiguous launch**: reaped by name, 75, never retried
  or passed on. A classified error without `id` falls back only after core's own `list` by the
  job's full name finds nothing. Secrets reach the plugin on fd 3 (per-job ones inside `launch`),
  never argv or env, and join the redactor before they are written. The `job` object has an exact
  wire schema (A.5.1). A plugin's `attach` claim is proven by row FB15, and a plugin defaults to
  `resource` until the host opts in. Every rail (cap, `max_live`,
  launch lock, reservations, ledger, breaker, sweep, naming and owner tag) stays in bitc core and
  prices from host config, never below it. Markers and the result MAC are checked in core too.
  These core checks catch a **buggy** plugin, not a hostile one: a pinned plugin is fully trusted
  host code that runs as the account. Built-ins (`runpod`, later `sprites` and `ssh`) stay
  compiled in and implement the same Go interface. One conformance suite runs against both,
  through a reference plugin that wraps the existing fake.
- **B. Fallback boundary.** A new `Caps.FallbackUntil` declares how late a provider can still
  hand a failed job to the next provider: `resource` (today's 15.8: until a resource exists) or
  `attach` (until job input first reaches the resource). The host key
  `providers.<t>.fallback_until` may never exceed it. Built-ins default to their declaration;
  plugins default to `resource`. RunPod declares `resource`, so nothing changes for it. Sprites
  and ssh declare `attach`. Post-attach never falls back. `on_failure`
  still decides whether a pre-boundary failure moves on at all. A create that left nothing is
  retried once (unchanged); a pre-attach discard is retried once, only after its resource is
  confirmed gone. Each discarded resource is a breaker `failure`. A reservation left behind by an
  unconfirmed discard is an `orphan` and never shields its resource from the sweep.
- **C. Fixed capacity.** A built-in `ssh` provider runs jobs on one host the owner already has.
  Capacity is slots, not price × time; cost is $0 unless `usd_per_hour` is set for accounting.
  Each slot has its own unprivileged user. Each job gets a wiped workdir, private `/tmp`,
  `/var/tmp` and `/dev/shm`, its own IPC and network namespaces, and cgroup/uid process cleanup.
  No cron, `at` or linger is allowed for slot users. A seccomp filter denies `AF_VSOCK`, user
  namespaces and kernel keyrings. The host key and the controller key are pinned. The only
  long-lived secret on the host is the reachability credential (root-only). Per job, the
  read-only deploy key (shredded after fetch), the MAC key K and the nonce M reach agent memory;
  forwarded env values reach the host only as `providers.ssh.send_env` allows (default
  `"forwarded"`, so no forced name; C.5.2, S). The
  agent is delivered by sha256 over the ssh session. It needs a `linux/arm64` agent and per-arch eligibility, and leaves reachability as an
  decision. `doctor` **fails**, and the agent refuses at every job start, when the VM shows
  any host (Mac) file share (mounted in any mount namespace, or merely exported as a virtio-fs or
  9p device), Mac integration, Rosetta handler or bridged Docker socket, or when the LAN block
  (all IPv6 included) is not loaded. A dedicated full VM with no shares is the recommended target. OrbStack
  is acceptable only if the owner accepts its shared-kernel risk (C.7).
- **S. Caller env per provider.** One host-only key on every provider section,
  `providers.<t>.send_env = "none" | "forwarded" | "forced"`, says how much of the caller's
  forwarded env may reach that provider. Built-ins declare a default in `Caps` (RunPod and Sprites
  phase 1 `"forced"`, ssh `"forwarded"`); plugins default to `"forwarded"`; the host may set any
  value. A job that needs more than the provider allows is `ErrIneligible` there and moves to the
  next provider with an exact note. One invariant holds whatever the setting: forced names never
  reach a resource that serves more than one job (`Caps.MultiJob`, S.4).

## 1. Decisions (each with a recommendation)

1. **Fallback boundary shape.** (a) `Caps.FallbackUntil` enum plus a narrowing host key
   `fallback_until = "resource" | "attach"`; (b) a boolean `Caps.CheapRelaunch`.
   **Rec: (a).** It names *the event* that ends fallback, which the core can check and the
   ledger can record. "Cheap" is a cost judgment that differs by host and day. An enum leaves room
   for a third boundary without breaking configs, and it maps onto the error types the core
   already needs (`ErrPreAttach`, `ErrAttached`, B.3).
2. **Default of `fallback_until`.** (a) The provider's declared maximum everywhere; (b)
   `resource` everywhere, `attach` opt-in; (c) built-ins default to their declaration (RunPod
   `resource`, Sprites and ssh `attach`), plugins default to `resource` and the host opts in with
   `fallback_until = "attach"`. **Rec: (c).** A declaration of `attach` is the claim that no job
   input crosses before attach. Row FB15 (B.6) checks that claim for every `Target` that declares
   `attach`: canaries for B, K, M, an argv sentinel, the deploy key and a forwarded value must not
   appear in the fake's recorded calls before the attach call. For built-ins it runs in CI
   (next to SP9 for Sprites and SH9 for ssh) and gates the merge. For a plugin, bitc cannot see
   that it passed, so `attach` stays an explicit host choice made after running the suite. A
   plugin that declares `attach` and fails FB15 is not conformant. (FB3–FB5 only test how core
   walks a provider; they prove nothing about what crosses.)
3. **15.8 rewording** (B.5). **Rec: approve** the wording in B.5. RunPod behaviour is unchanged.
4. **Plugin session model.** (a) One long-lived plugin process per bitc process, requests
   multiplexed by id; (b) one process per call. **Rec: (a).** `Watch` is long-lived anyway, a
   per-call process would re-read credentials and re-open connections dozens of times per job,
   and (a) still lets a plugin be written as a simple loop (bitc never pipelines requests for one
   job except `Teardown` or `Owner` during `Watch`).
5. **Plugin trust.** A plugin receives the provider API key and each job's spec, including the
   results MAC key K, the bearer B and the nonce M. It runs as the account, so it can also read
   the install secret, the ledger and every other credential file. **Rec: treat a pinned plugin
   as fully trusted host code** (same trust as a built-in). Pinning (absolute path + sha256)
   guards against an accidental swap and a PATH hijack, not against a hostile plugin. The core's
   marker, MAC and rail checks catch a buggy plugin only. Spec 10 says so plainly (A.9).
6. **Unpinned plugins.** **Rec: none**, not even with `[test]`. The protocol suite pins its
   reference plugin like any other.
7. **Scope change** (C.1). **Rec: approve** the new spec 1 sentence: fixed-capacity providers
   are in scope as providers in a pool's list; cross-host scheduling stays out.
8. **ssh hosts per provider.** (a) One host per `[providers.ssh]` in phase 1; (b) a host list
   now. **Rec: (a).** A second fixed host later comes with named instances
   (`[providers.<name>] type = "ssh"`), the same change plugins would want. That change touches the
   "one instance per type" rule (spec 2), so it gets its own delta.
9. **Reachability of the Mac VM** (C.6). **Rec: (a) a reverse ssh tunnel started by the VM**,
   unless the owner already runs a mesh VPN. (b) a mesh VPN is the alternative. Never (c), a
   router port-forward. Both (a) and (b) put one long-lived credential on the VM (a tunnel key or
   a mesh node key), which must be root-only (C.6.2). Under (a) the controller's tunnel account is
   locked to one remote listen port and nothing else (C.6.3).
10. **VM product** (C.7). OrbStack Linux machines are believed to be system containers on one
    shared Linux VM (and kernel) that also runs OrbStack's Docker engine, which mounts the Mac's
    `/Users` (unverified, O8). If so, a kernel exploit in a job reaches the Mac's home directory even
    with per-machine sharing off. **Rec: a dedicated full VM with no shares and Rosetta off**
    (Lima `vmType: vz` with `mounts: []` and `rosetta.enabled: false`, Tart or UTM) as the
    default. Use OrbStack only if O8 shows a separate VM boundary, or if the owner accepts the
    risk stated in C.7 in writing.
    **DECIDED, 2026-10-23: keep the OrbStack machine and accept the risk** (O8 confirmed
    a shared kernel). Mitigations stay mandatory: per-job users, the job seccomp filter (D21), the
    mount check in doctor and at job start, and no forced env (`send_env` default "forwarded").
11. **Agent install on the ssh host** (C.5). (a) The gate installs any binary whose sha256
    matches what the controller sends; (b) the gate also requires the sha in a root-owned
    allowlist on the VM, so the owner edits it per upgrade. **Rec: (a)**, with the VM isolated by
    C.7 and C.8. Whoever holds the controller key already controls every job that runs there.
12. **Arch default for pools.** **Rec: `amd64` only; `arm64` opt-in per pool** in the repo's
    remote profile, once that pool's native deps are checked on arm64 (C.4).
13. **LAN block on the ssh host.** **Rec: mandatory.** Jobs get no route to RFC 1918, CGNAT /
    tailnet (100.64.0.0/10), 198.18.0.0/15, loopback, link-local, multicast, any IPv6 at all
    (home devices hold global IPv6 addresses), or the VM's gateway, except DNS to the configured
    resolver (C.8.7 ruleset).
    `check-host` verifies that the ruleset is loaded at every job start, and `doctor` proves it
    with a connect test. A home network holds the owner's devices.
14. **Code to the ssh host.** (a) `git-push` with the read-only deploy key, as RunPod (phase 1);
    (b) a `git bundle` over the ssh session, so no key reaches the host. **Rec: (a) now, (b)
    later.** (b) is the `upload` transport already listed in spec 14 "Later".
15. **Root steps and services on the ssh host.** **Rec: never, in phase 1.** A repo granted
    `root_steps`, or whose profile lists `services`, is ineligible for `ssh` (spec 8.4 runs
    services as root and only with `root_steps`). `packages` must already be installed (checked,
    never installed). Jobs that need a database service (pgvector) stay on Sprites or RunPod.
    User-mode services are a later delta, once per-job network namespaces are proven (C.5).
16. **Breaker and pre-attach discards** (B.3.3). (a) Each resource created and then discarded
    before attach writes one breaker `failure`; (b) a separate `preattach` counter with its own
    threshold. **Rec: (a).** A broken mirror then pauses the provider after `failures` discards
    (3 by default, that is about two jobs) instead of burning two resources per job until
    `daily_usd` runs out. An ambiguous plugin launch also writes one. Pre-resource capacity
    errors stay uncounted.
17. **Plugin sandbox.** Run plugins under a separate uid, or Landlock-restricted to their own
    state dir. **Rec: Later.** In phase 1 a plugin is trusted host code (decision 5). A sandbox
    makes "a plugin cannot read the install secret or the ledger" true, and it needs its own
    delta (it also has to cover a plugin's network and its TLS roots).
18. **Network isolation between ssh jobs.** (a) Each job gets its own network namespace with a
    veth pair and NAT, filtered by the C.8.7 ruleset; (b) a shared network namespace, with
    `slots = 1` forced. **Rec: (a)**, with `slots = 1` as the default for the Mac target until
    probe O9 passes. Under (b), two jobs bind the same ports and reach each other's listeners.
19. **Plugin pricing.** Core requires a host-config price per size for a plugin type and uses
    max(host price, plugin figure) everywhere (A.7.1). **Rec: approve.** This is RunPod's rule
    (max of size price and API price), so a buggy plugin cannot under-report its way past
    `daily_usd`.
20. **Caller env per provider** (S). **DECIDED, 2026-10-23** ("generalized, host
    setting"). One host-only key on every provider section, `providers.<t>.send_env = "none" |
    "forwarded" | "forced"`, default from `Caps.SendEnv` for a built-in (RunPod `"forced"`,
    Sprites phase 1 `"forced"`, ssh `"forwarded"`) and `"forwarded"` for a plugin; the host may
    set any value, except `"forced"` on a provider whose resources serve more than one job (S.4).
    This replaces r3's ssh-only `providers.ssh.forward_env` boolean (former decision 20) and the
    Sprites `forward_env_force` decision (sprites-provider.md decision 10), which are merged here.
21. **Job seccomp filter on ssh hosts** (C.5.3). It denies `AF_VSOCK`, user namespaces
    (`CLONE_NEWUSER`) and the kernel keyring calls. **Rec: approve**, mandatory. vsock is a path to
    the Mac that no nftables rule or network namespace covers; most kernel privilege escalations
    need a user namespace; and denying the keyring calls removes a cross-job carry-over instead of
    listing it as a limit. Cost: Chromium's own sandbox needs user namespaces, so Playwright must
    run with its default `chromiumSandbox: false`.

## A. Plugin protocol [8.5 edit, new 8.5.1–8.5.9]

### A.1 Model

1. A provider type is **built in** (compiled Go package, registered in the 8.5 table) or a
   **plugin**: a `[providers.<type>]` section with a `plugin` table. Built-in types: `local` (not
   an implementation, 8.6), `runpod`, `sprites`, `ssh`, and `fake` (test only). A plugin may not
   use a built-in type name (78).
2. The core wraps a plugin in an adapter that implements the Go `Provider` interface, so the walk,
   preflight (8.4), rails (8.3), orchestration, artifact validation and sweep code are the same
   for both kinds. Nothing in 8.2–8.4 branches on "is a plugin".
3. A plugin is an **API client only**. bitc core still names resources, computes the owner tag,
   holds `launch.lock`, reservations, ledger, breaker and cap, parses markers with M, verifies the
   result MAC under K, validates artifacts, and runs cleanup and sweep. The remote side is still
   bitc's own agent (8.4), which the plugin delivers by the stub with `JobSpec.AgentURL` and
   `AgentSHA256`. Keeping these in core makes a **buggy** plugin fail safe. It does not contain
   a hostile one (A.9).

### A.2 Discovery and pinning (host-only)

```toml
[providers.modal]                                  # <type> = "modal"
plugin = { path = "/usr/local/libexec/bitc/bitc-provider-modal",
           sha256 = "<64 lowercase hex>",
           env_pass = ["HTTPS_PROXY", "NO_PROXY"],   # optional; default []
           secret_files = { token_secret = "~/.config/bitc/modal.token-secret" } }  # optional
api_key_file = "~/.config/bitc/modal.key"           # core keys as 8.5
daily_usd = 5
max_live = 2
sizes = { cpu8 = { usd_per_hour = 0.60 } }          # core for plugin types: required (A.7.1)
# every other key goes to the plugin's configure request, as JSON; never a secret (A.3.3)
region = "eu"
```

1. `plugin` is a **host** key. In a repo file it is 78, like every `[providers.*]` key (5.3).
2. bitc never looks the plugin up on `PATH`. The basename of `path` must be
   `bitc-provider-<type>`.
3. **At load:** open `path` with `O_NOFOLLOW`. It must be a regular ELF file, executable, owned by
   the user or root and not group- or world-writable, and so must every parent directory. It must
   have no setuid or setgid bit and no `security.capability` extended attribute (file
   capabilities), because either would run it above the account. Its sha256 must equal `sha256`.
   Any failure is 78 (A.8).
4. **At spawn:** open again with `O_NOFOLLOW|O_CLOEXEC`, re-check the mode bits and the xattr on
   the open fd, hash it, compare, then exec that same fd, so the bytes hashed are the bytes run.
   Go's `SysProcAttr` can set `Pdeathsig` and `Setpgid` but has no `PR_SET_NO_NEW_PRIVS` field
   and no `execveat`, so the spawn goes through a shim: bitc starts its own binary as
   `/proc/self/exe __plugin-exec` with the plugin fd as fd 4 (fd 3 is the secrets pipe, A.3.3).
   The shim locks its OS thread, calls `prctl(PR_SET_NO_NEW_PRIVS, 1)`, then sets `FD_CLOEXEC` on fd 4 again, then calls
   `execveat(4, "", argv, envp, AT_EMPTY_PATH)` by raw syscall. No-new-privs is inherited across
   the exec, so even a bit set after the check (a race with root) cannot raise privileges. A
   mismatch here (the file changed since load) is `ErrUnavailable{Local}`:
   `bitc: <type>: plugin <path> changed since load (sha256 <want12>, now <got12>)`. A mode or
   xattr change is `bitc: <type>: plugin <path>: insecure: setuid/setgid/capabilities`.
5. Scripts are not plugins, because a `#!` interpreter breaks the fd-exec pin. A plugin written
   in an interpreted language ships as a single ELF (for example a PyInstaller or Deno compile).

### A.3 Process, environment and secrets

1. **Spawn** lazily, at the first call the walk, `sweep`, `status`, `doctor` or `config check`
   makes for that type. One process per bitc process and type, in its own process group, with
   `Pdeathsig = SIGKILL` (6.4), stdin and stdout pipes, and stderr to a 64 KiB ring buffer.
2. **Environment** built from empty: `PATH=/usr/sbin:/usr/bin:/sbin:/bin`, `HOME`, `LANG=C.UTF-8`,
   `TMPDIR`, `BITC_PLUGIN_STATE`, `BITC_PLUGIN_SHARED`, `BITC_PLUGIN_PROTOCOL=1`,
   `BITC_SECRETS_FD=3`, plus each `env_pass` name the caller has set. `env_pass` entries follow the
   5.2 env-name rule, and a name the 8.4 secret-name list matches is 78. The three directories:
   - `TMPDIR = <state>/plugins/<type>/tmp/<pid>-<start>`: per bitc process (pid and its `/proc`
     start time), 0700, created at spawn and removed when the plugin exits. At each spawn bitc
     removes sibling dirs whose pid is dead or whose start time differs. Several bitc processes
     therefore never wipe each other's files.
   - `BITC_PLUGIN_STATE = <state>/plugins/<type>/`: caches only. It moves with `BITC_DIR`, so two
     state dirs see two copies.
   - `BITC_PLUGIN_SHARED = ACCOUNT/remote/<type>/plugin/` (0700, fixed accounting dir, 9): any
     state that must be shared across processes and state dirs, such as client-side rate limits
     or a creation-quota window. Like the rails, it does not move with `BITC_DIR`. The plugin
     guards it with its own `flock`s.
3. **Secrets on fd 3**, never argv, env or a file: one JSON object, then EOF:
   `{"api_key": "<from api_key or api_key_file>", "<name>": "<bytes of secret_files.<name>>", ...}`.
   - `plugin.secret_files = { <name> = "<path>" }` is host-only. `<name>` matches
     `^[a-z][a-z0-9_]{0,31}$` and is not `api_key`. Each file follows the credential-file rules
     (0600, owned by the user, else 78), read at spawn.
   - **Non-core keys are never secrets.** They go to `configure` as plain JSON and `bitc config`
     and `doctor` print them. `hello` may declare `secrets: ["<name>", ...]`. Each declared name
     must be present in `secret_files` (else 78), and the same name as an inline non-core key is
     78 (`providers.<t>.<name>: secret; set it in plugin.secret_files`). An inline non-core key
     whose name matches the 8.4 secret-name list is 78 too.
   - Per-job secrets travel inside the `launch` request on stdin: `secrets.deploy_key` (the bytes
     of the grant's deploy key file, when one names this type), and B, K and M inside `job` (as
     lowercase hex, A.5.1). Forwarded env values sit in `job.env`, and only the names that
     `providers.<t>.send_env` lets cross (S); a job that needs more never reaches `launch`.
   - **Redaction before write.** The spawn-time secrets (`api_key`, each `secret_files` value)
     join the redacting writer before the spawn. Each `launch`'s per-job secrets join it **before
     the `launch` frame is written**: the deploy key bytes, B, K and M in raw and lowercase-hex
     form, and every forwarded value whose name is in `forward_env_force` or matches the 8.4
     secret-name list. The plugin's stderr (A.3.4, debug lines and the 20-line protocol-error
     tail) passes through that writer, so a plugin that echoes its request on stderr leaks none
     of them. They leave the redactor when the bitc process exits.
4. **stderr** lines are redacted and printed only at `BITC_LOG=debug`, as
   `bitc: <type>: plugin: <line>`. The last 20 lines are attached to a protocol error.
5. **Exit:** bitc closes stdin when done. The plugin exits within 2 s, or gets SIGTERM, then
   SIGKILL after 2 s. A plugin that dies mid-session is respawned once for later calls (cleanup,
   `Teardown`, sweep); a second death in the same bitc process makes that type
   `ErrUnavailable{Local}` for the rest of the process. A death while a `launch` is open makes
   that launch **ambiguous** (A.5.4), never `ErrUnavailable{Local}`.

### A.4 Framing and handshake

1. **Framing:** one JSON object per line (NDJSON, UTF-8, `\n`), each line ≤ 1 MiB. Binary data
   (`Output`, `Fetch`) is base64 in `data`, ≤ 512 KiB decoded per frame.
2. **Request:** `{"id": <uint>, "method": "<name>", "params": {...}}`.
   **Event** (streaming methods only): `{"id": <same>, "event": "<kind>", ...}`.
   **Final:** `{"id": <same>, "result": {...}}` or `{"id": <same>, "error": {...}}`; exactly one
   per request, after its events.
3. **Concurrency:** bitc may have several requests open, for example `teardown` or `owner` while
   `watch` streams. The plugin answers each id once and must accept requests in any order.
4. **Handshake:** the first request is always
   `hello {"protocols": [1], "bitc_version": "1.4.0", "type": "modal"}`. The plugin answers with
   ```json
   {"protocol": 1, "type": "modal", "plugin_version": "0.3.1",
    "caps": {"live_logs": true, "artifacts": true, "spend": false, "self_delete": false,
             "fallback_until": "resource", "fixed": false, "multi_job": false},
    "secrets": ["token_secret"]}
   ```
   Required: `protocol`, `type`, `plugin_version`, `caps` (all seven fields; `multi_job` is
   S.4's `Caps.MultiJob`). Optional: `secrets`
   (default `[]`). A `type` that differs, or a `protocol` not in `protocols`, is a protocol error.
   `caps` come before `configure` because the 78 checks of `fallback_until` (B.6) and
   `secret_files` need them.
5. **configure** follows `hello`: `{"config": {<the section's non-core keys, as decoded TOML>},
   "sizes": {<the core sizes table>}}`. It must be network-free. The result carries everything
   that depends on config:
   ```json
   {"endpoints": ["api.eu.modal.example"], "arch": ["amd64"], "max_name": 63}
   ```
   All three are required. `max_name` must be at least **60**, else it is a protocol error that
   `config check`, `doctor` and the walk report as 78 (A.8). 60 is the shortest name core can
   build without cutting anything but the pool: head `<prefix ≤ 10>-<install16>-` (28) plus tail
   `-<size ≤ 10>-<job8>-<E unix, 10 digits>` (31) plus one pool character. Core builds names with
   `ResourceName` at min(`max_name`, 63). It truncates only the pool and never below one character,
   so `job8` and `E` always survive and two live jobs never share a name, nor therefore an owner
   tag. A plugin must accept every name up to its declared `max_name`. `endpoints` are exact host
   names or IP literals (no wildcards), and they become `Info().Endpoints`. The `[test]` loopback gate (11) checks these **post-configure**
   endpoints. Under `[test]`, any endpoint that is not a loopback literal (`127.0.0.1`, `::1`) is
   78: `providers.<t>: [test]: endpoint "<e>" is not a loopback literal`. `error.class = "config"`
   with `why` makes `config check` and `doctor` exit 78 with `bitc: <file>: providers.<type>:
   <why>`. During `run`, the same error is 78 when the walk first reaches the type.
6. **Fields:** in every request, result, event and error, **unknown fields are ignored**, by bitc
   and by plugins, `hello` included. Only a missing required field, a wrong JSON type or an
   out-of-range value (A.7.1) is a protocol error.
7. **Versioning:** `protocol` is an integer. A change that a v1 peer could misread bumps it: a new
   required field, a changed meaning or type, or a new method that bitc depends on. New optional
   fields do not (A.4.6). bitc 1.x supports protocol 1 for its whole major version.

### A.5 Methods (1:1 with `Provider`, 8.1)

| Method | params | result / events | bitc timeout |
| --- | --- | --- | --- |
| `hello`, `configure` | A.4 | A.4 | 5 s each |
| `preflight` | `{job}` | `{}` | 5 s (network-free) |
| `estimate` | `{job}` | `{"usd_micros": <int>}` | 10 s |
| `launch` | `{job, secrets?: {"deploy_key": "..."}}` | `{"id", "name", "results_url"?}`; events `setup {line}` (pre-attach output) | `start_timeout`; expiry → ambiguous (A.5.4) |
| `watch` | `{handle}` | events `phase {phase, why}`, `log {line}` (raw provider log, markers included), `out {stream, data}`, `heartbeat {}` at least every 30 s; result `{exit, began, why}` | none; 90 s with no event → `lost` |
| `output` | `{handle, stream, offset}` | events `data {data}`; result `{bytes}` | 60 s with no event |
| `fetch` | `{handle, max_compressed, offset}` | events `data {data}`; result `{bytes}` | 60 s with no event |
| `owner` | `{id}` | `{"owner": "<tag>"}` | 30 s |
| `teardown` | `{id}` | `{}` (idempotent; "gone" is success) | 10 s (8.3) |
| `list` | `{prefixes}` | `{"resources": [Resource...]}` (8.1 fields, snake_case) | 30 s |
| `spend` | `{}` | `{"usd_micros": <int>}` (only with `caps.spend`) | 30 s |

1. `job` is the 8.1 `JobSpec` in the **protocol-1 wire schema** below, with `name` and `owner`
   already set by core. The plugin must create the resource with exactly that `name` (A.4.5) and
   store `owner` where `owner` can read it back. Go's default JSON encoding of `JobSpec` is not
   the wire format: core marshals a separate wire struct, and the field names, encodings and
   omissions are fixed here. All keys are snake_case, like `list`'s resources.

   | Key | JSON type and encoding | From `JobSpec` |
   | --- | --- | --- |
   | `job`, `pool`, `cwd`, `clone_url`, `ref`, `sha`, `size_key`, `repo_id`, `install`, `name`, `owner`, `agent_url`, `agent_sha256` | string (`sha`, `agent_sha256` and `owner` lowercase hex) | same-named field |
   | `argv` | array of strings | `Argv` |
   | `env` | array of `"KEY=VALUE"` strings, in order | `Env` |
   | `timeout_s` | integer seconds | `Timeout` |
   | `created`, `deadline`, `expiry` | string, RFC 3339 UTC with `Z`, whole seconds | `Created`, `Deadline` (D), `Expiry` (E) |
   | `bearer`, `mac_key`, `nonce` | string, 64 lowercase hex characters | `Bearer` (B), `MacKey` (K), `Nonce` (M) |
   | `artifacts` | array of `{"path": string, "replace": bool}` | `Artifacts` |
   | `packages` | array of strings | `Packages` |
   | `tools` | array of `{"name", "url", "sha256", "arch"}` strings | `Tools` (C.2.3) |
   | `setup` | array of `{"name": string, "run_as": "root" \| "job", "argv": [string]}` | `Setup` |
   | `services` | array of `{"name", "run_as", "argv", "ready": [string], "ready_timeout_s": int}` | `Services` |
   | `root_steps` | bool | `RootSteps` |
   | `test` | `{"unprivileged": bool, "test_root": string}`, **present only under `[test]`** | `Unprivileged`, `TestRoot` |

   Every key except `test` is always present (empty arrays as `[]`, an unused B as 64 zeros).
   **Omitted:** `DeploySecret` (a RunPod secret name, meaningless to a plugin); the deploy key
   bytes travel only in `secrets.deploy_key`, never in `job`. A field added to `JobSpec` later
   reaches plugins only by an edit of this table (optional, so no protocol bump, A.4.7). The
   protocol suite holds the golden file `conformance/plugin/testdata/job-v1.json` (row PL10).
2. **Markers stay in core.** The plugin forwards raw provider log lines as `log` events. The
   adapter parses `__BITC_BOOT__`, `__BITC_BEGIN__`, `__BITC_EXIT__`, `__BITC_LOST__` and
   `__BITC_SETUP_FAILED__` with M, and turns them into `Sink.Phase` and `Sink.Setup` exactly as a
   built-in does. A plugin's own `phase` event can only report `booting` or `lost` (a resource gone
   or restarted). `begun` and `ended` come only from markers. `result.exit` must equal the
   `__BITC_EXIT__` code, or the core treats the result as failed verification (8.4). Core then
   completes output by `output` and checks `result.json`'s MAC under K. **What this protects:** a
   buggy plugin that mislabels phases, drops a log tail or mixes up results is caught. **What it
   does not:** the plugin holds K, B and M (decision 5), so a hostile plugin can emit
   `__BITC_BEGIN__ <M>` and `__BITC_EXIT__ <M> 0` and write a `result.json` whose MAC verifies.
   Only trust in the pinned binary covers that (A.9).
3. **Timeouts:** on expiry bitc sends `{"id": 0, "method": "cancel", "params": {"id": <n>}}`. For
   every method except `launch`, the call is then an `ErrTransient` with `why = "<method> timed
   out after <d>"`. A `launch` that times out or is cancelled is ambiguous (A.5.4). If the plugin
   has not answered 5 s after the `cancel`, bitc kills and respawns it (A.3.5).
4. **Ambiguous launch.** Once bitc has written a `launch` request, its outcome is **definite**
   only if it ends in a well-formed final frame that is a `result`, or an `error` of a class in
   A.6 whose rules below say "definite". Every other outcome is ambiguous: a timeout, a bitc-side
   cancel (signal), the plugin's death or exit, a malformed frame, a second final, a phase event
   that A.6 forbids, or an `error` with an unknown class (with or without `id`). On an ambiguous
   launch, the adapter returns `ErrAttached{ID: <the name core assigned>, Err: ErrTransient{why}}`
   and core then:
   1. reaps by name: `list` with the job's full name as the only prefix (on the respawned plugin,
      A.3.5), checks `owner` against the tag core computed, and calls `teardown` on the match. A
      dead or failing plugin leaves the name to the sweep (`; <name> left for sweep`);
   2. exits 75 with `STARVED <pool> (<type>: launch ambiguous: <why>)` under **both** boundaries.
      There is no retry and no next provider, `local` included. Only the plugin knows whether job
      input was sent, so core reads every ambiguous launch as attached. This is stricter than the
      built-in rule of spec 8.2 (a lookup that finds nothing may fall back): a plugin's `list` is
      plugin-reported, so "not found" proves nothing here;
   3. keeps the job's reservation until `E` if the reap did not confirm the name gone (B.3.3).

   The only way a plugin's launch failure can reach the next provider after a request was written
   is a definite error: a classified error **without** `id` (the plugin states that nothing was
   created) that core's own name check then confirms (A.5.5), or `pre_attach` from a plugin whose
   effective boundary is `attach` (A.6).
5. **Name check after an id-less error.** A plugin bug can create a resource and still answer
   `transient` without an `id`. Before any retry or next provider after an id-less classified
   `launch` error, core runs `list` with the job's full name as the only prefix and, for each
   returned resource, `owner`:
   1. nothing returned: the error stands as pre-resource (8.2 path, retry once if `transient`);
   2. a resource whose `owner` equals the tag core computed: read as attached. Core tears it down,
      writes one breaker `failure`, and exits 75 with
      `STARVED <pool> (<type>: launch said <class> but <name> exists)`;
   3. the `list` or `owner` call fails or times out: read as ambiguous (A.5.4: reap by name, 75).

   This closes "two live resources for one job" for a buggy plugin whose `list` works. A plugin
   whose `list` also omits the resource is not caught here; provider-suite row PS1 (A.10.2) is the
   defence for that.

### A.6 Error classes (A.5 `error` object)

`{"class": "<class>", "why": "<one line>", "id"?: "<resource id>", "step"?: "...",
"setup_class"?: "repo" | "infra"}`

| class | Maps to | Notes |
| --- | --- | --- |
| `ineligible` | `ErrIneligible{why}` | |
| `transient` | `ErrTransient{why}` | |
| `unavailable` | `ErrUnavailable{why}` | one `pause` record (8.3) |
| `unavailable_local` | `ErrUnavailable{why, Local: true}` | no pause |
| `pre_attach` | `ErrPreAttach{ID: id, Err: <inner>}` (B.3) | `launch` only; needs `id` = the assigned name and `inner: {class, why}` with inner class `transient`, `unavailable` or `ineligible`; definite only when the effective boundary is `attach`, else read as attached |
| `attached` | `ErrAttached{ID: id, Err: <inner>}` (B.3) | `launch` only; needs `id`; definite |
| `setup` | `ErrSetup{step, setup_class, why}` (8.4) | `watch`, or `launch` after attach (then definite, attached) |
| `config` | 78 (A.4.5) | `configure` only |
| anything else, a malformed frame, a second final for one id, an unknown id, a `begun`/`ended` `phase` event | protocol error: `ErrUnavailable{Local}` | `bitc: <type>: plugin protocol error: <why>`; no pause; 69 if it was the last provider. **During an open `launch` it is an ambiguous launch instead (A.5.4): 75** |

**`launch` errors, in full.** `ineligible`, `transient`, `unavailable` and `unavailable_local`
**without** `id` are pre-resource failures (the 8.2 pre-resource path) once the name check of
A.5.5 finds nothing. The same classes
**with** an `id` are read as attached under both boundaries (teardown by that id and by name, 75).
`pre_attach` is definite only under an effective `attach`; under `resource` (declared or narrowed)
it is read as attached. Any other `launch` outcome is ambiguous (A.5.4), which is also read as
attached. A plugin that can fall back after creating a resource must say so with `pre_attach`.

### A.7 Rails stay in core

The rails run in core so that a buggy plugin cannot weaken them. They are not a boundary against
a hostile plugin, which runs as the account and can read and write the accounting dir itself
(A.9).

1. **Cap and `max_live`:** core calls `list`, `spend` and `estimate` under `launch.lock` and runs
   the 8.3 arithmetic. The plugin is never asked about the lock, the ledger or reservations.
   Money never rests on the plugin's figures alone:
   1. **Host price per size is required** for a plugin type: `sizes.<k>.usd_per_hour` (money,
      > 0), a core key for plugin types, also passed to `configure`. A pool's
      `remote_limits.size.<t>` must name one of them (78).
   2. **Estimate** = max(host price × (E − now), the plugin's `estimate`).
   3. **Each listed resource** is priced at max(host price of its `size_key`, its `usd_per_hour`).
      An unknown or empty `size_key` takes the highest host size price.
   4. **Spend:** `committed` already uses max(`spend`, `ledger_today`) (8.3), so a `spend` below
      the ledger changes nothing.
   5. **Out-of-range values are protocol errors that count as over the cap** (8.3: "error counts
      as over the cap"): a negative or non-integer `usd_micros`, a negative or non-numeric
      `usd_per_hour`, a resource without `id` or `name`, and a `list` entry whose name does not
      start with one of the requested prefixes. The note is `daily cap $<d>: plugin <type>
      reported <field> out of range`.
   6. **A `list` that omits a live resource** is still counted while this host holds a
      reservation for it (8.3: a reservation counts while its name is unlisted). Once that job
      ended, an omitted resource is invisible to the cap, `max_live` and the sweep. Core cannot
      detect this at run time. The defence is provider-suite row **PS1** (A.10.2), run with
      `pluginTarget` against the author's fake: it compares `list` with the fake's own live set
      across pages and after each job's end. The protocol suite (A.10.1) cannot see provider state
      and does not cover it. PS1 proves only what the fake models: a fake that paginates or
      filters unlike the real API leaves the omission uncovered.
2. **Concurrency:** several bitc processes on one host (and several hosts) may run the same plugin
   at once. A plugin must be stateless across processes, or keep shared state in
   `BITC_PLUGIN_SHARED` (A.3.2) with its own `flock`s.
3. **Sweep:** `bitc sweep` calls `list`, `owner` and `teardown` as for a built-in. Only resources
   whose owner tag verifies are torn down (10).
4. **Self-delete:** the agent's `D + 10m` self-delete hook (8.4 step 3) exists only for built-in
   types. For a plugin type the agent makes no call. `caps.self_delete = true` is allowed only when
   the platform itself enforces a TTL from the job (for example a max-runtime field set at create);
   else `doctor --remote` warns as for any provider without it.

### A.8 Validation (78) and stderr lines

| Case | Message (`bitc: <file>: ` prefix) |
| --- | --- |
| `plugin` key in a repo file | `providers.<t>.plugin: host-only key` |
| `<t>` is a built-in type | `providers.<t>.plugin: "<t>" is a built-in provider` |
| `<t>` not `^[a-z][a-z0-9]{0,15}$` | `providers.<t>: invalid provider type` |
| `path` not absolute, or basename ≠ `bitc-provider-<t>` | `providers.<t>.plugin.path: must be an absolute path ending in bitc-provider-<t>: "<v>"` |
| missing, symlink, not regular, not executable, not ELF | `providers.<t>.plugin.path: <reason>: "<v>"` |
| file or a parent group/world-writable, or owned by another user | `providers.<t>.plugin.path: insecure: <which path>` |
| setuid or setgid bit, or a `security.capability` xattr | `providers.<t>.plugin.path: insecure: setuid/setgid/capabilities: "<v>"` |
| `sha256` not 64 lowercase hex | `providers.<t>.plugin.sha256: invalid: "<v>"` |
| sha256 mismatch | `providers.<t>.plugin: sha256 mismatch (want <12>, file <12>)` |
| `env_pass` entry invalid or secret-named | `providers.<t>.plugin.env_pass: "<v>" not allowed` |
| `secret_files` name invalid or `api_key`; file insecure | `providers.<t>.plugin.secret_files.<n>: <reason>` |
| a `hello` `secrets` name missing from `secret_files` | `providers.<t>.plugin.secret_files: "<n>" required by the plugin` |
| inline non-core key named as a declared secret, or matching the 8.4 secret-name list | `providers.<t>.<n>: secret; set it in plugin.secret_files` |
| plugin type without `sizes`, a size without `usd_per_hour` > 0, or a pool size key not in `sizes` | `providers.<t>.sizes: a usd_per_hour per size is required for a plugin` |
| `[test]` and a post-configure endpoint that is not a loopback literal | `providers.<t>: [test]: endpoint "<e>" is not a loopback literal` |
| `configure` error (config check, doctor, first use in run) | `providers.<t>: <why>` |
| `hello` protocol unsupported (config check, doctor) | `providers.<t>.plugin: speaks protocol <n>, bitc supports [1]` |
| `configure` `max_name` < 60 (config check, doctor, first use in run) | `providers.<t>.plugin: max_name <n> is below 60, the shortest name bitc builds` |

Run-time lines: `bitc: <type>: plugin protocol error: <why>`; `bitc: <type>: plugin <path> changed
since load (...)`; `bitc: <type>: plugin <path>: insecure: setuid/setgid/capabilities`;
`STARVED <pool> (<type>: launch said <class> but <name> exists)` (A.5.5); `bitc: <type>: plugin:
<line>` (debug only, redacted). `doctor` adds
`ok|fail plugin-<type>: protocol 1, <plugin_version>, sha256 <12>`.

### A.9 Security [10 edit]

Add to spec 10: "A plugin is fully trusted host code. It runs as the account with the account's
real home (never above it: setuid, setgid and file capabilities are refused and it runs under
`PR_SET_NO_NEW_PRIVS`), receives the provider credential on a pipe and each job's spec (K, B, M
included), and is trusted like a built-in provider. bitc hands it no other secret, but nothing stops it from
reading the install secret (and so forging owner tags), the ledger, reservations or another
provider's key file, because they are files of the same account. Core's marker parsing, result
MAC check and rails protect against a buggy plugin, not a hostile one, which can defeat them all.
Pinning by absolute path and sha256 stops an accidental or `PATH`-based substitution, not a hostile
plugin the owner installed. Install only plugins you would run as yourself." A plugin sandbox
(separate uid or Landlock) is decision 17, Later.

### A.10 Conformance for plugins [11 edit]

1. **Protocol suite** (`conformance/plugin/`, new): runs a plugin binary against a scripted bitc
   side and checks framing, handshake and version refusal, one final per id, concurrent ids,
   `cancel`, timeouts, crash and respawn, the error-class table (A.6), that `begun`/`ended` phase
   events are refused, and that no secret appears in `/proc/<pid>/cmdline` or `environ` of the
   plugin or its children. The adapter side of the suite (family `PL`, run with the reference
   plugin in fault modes) pins core's reading of plugin output:

   | ID | Behaviour |
   | --- | --- |
   | PL1 | unknown fields in `hello`, `configure`, results, events and errors are ignored; a missing required field or a wrong type is a protocol error |
   | PL2 | `endpoints`, `arch`, `max_name` come from `configure`; under `[test]` a non-loopback endpoint is 78 |
   | PL3 | `estimate` of 0 → host price × (E − now) is used; a negative or non-integer `usd_micros` → over the cap, no launch |
   | PL4 | `list` omits a live reserved resource → still counted by its reservation in cap and `max_live` |
   | PL5 | `list` returns a name outside the requested prefixes, or a negative `usd_per_hour` → protocol error, over the cap |
   | PL6 | `spend` < `ledger_today` → `committed` uses the ledger |
   | PL7 | two bitc processes with one plugin: neither removes the other's `TMPDIR`; a dead process's dir is pruned at the next spawn |
   | PL8 | `secret_files` values arrive on fd 3, are redacted in `config`, `doctor` and debug stderr, and appear in no argv or env; a declared secret given inline is 78. Fault mode `echo-launch`: the plugin writes each `launch` request to stderr; with `BITC_LOG=debug` and in a forced protocol-error tail, none of the deploy key, B, K, M (raw or hex) or a `forward_env_force` value appears |
   | PL9 | `configure` with `max_name` 59 → 78 with the A.8 message; 60 → names of at most 60 characters with `job8` and `E` intact, for a 10-character prefix and size key |
   | PL10 | the `job` object of a scripted `launch` equals the golden file `testdata/job-v1.json` byte for byte after key sorting: snake_case keys, hex B/K/M, RFC 3339 times, `timeout_s`, no `deploy_secret`, no `test` without `[test]`; the deploy key only in `secrets` |
   | PL11 | fault mode `create-then-transient-no-id`: the plugin creates the resource and answers `transient` without `id` → the A.5.5 name check finds it, teardown, one breaker `failure`, 75 (`launch said transient but <name> exists`), no retry, no next provider; the same with `list` failing → reaped as ambiguous, 75; a true id-less `transient` (nothing created) → one retry, then `trying <next>` |
   | PL12 | a plugin file with the setuid bit, the setgid bit, or a `security.capability` xattr → 78 at load; the bit set between load and spawn → `insecure` at spawn, no exec; the plugin process shows `NoNewPrivs: 1` in `/proc/<pid>/status` |
2. **Provider suite** (`conformance/provider/`, unchanged scenario set): a new `pluginTarget`
   implements `Target` for any plugin, given `-plugin=<path>`, `-plugin-sha256=<hex>` and
   `-plugin-fake=<path>`. The fake is the plugin author's own loopback fake of their API, run as a
   process. It must accept `POST /__bitc/behave {"behaviour": "<name>"}` for the `Behave` names the
   suite uses (`ok`, `create-fails`, `setup-fails-after-create`, `setup-fails-repo-after-create`,
   `lost-after-begin`, `restart-after-begin`, `drop-log-tail`, `forge-result`, `paginate-2`, plus
   `preattach-fails` when `fallback_until = "attach"`), and serve `GET /__bitc/calls` (every API
   call in order with method, path, query, headers and body, the call the fake treats as the
   attach marked `"attach": true`), `GET /__bitc/live` (the names the fake holds as live) and
   `GET /__bitc/torn`. Two suite rows exist for plugins and built-ins alike:

   | ID | Behaviour |
   | --- | --- |
   | PS1 | **`list` completeness.** With `paginate-2` (the fake's page size forced to 2), the suite runs five jobs with overlapping lifetimes. After each launch, each job's end and each teardown, `list` with the provider's prefix returns exactly `GET /__bitc/live` (no omission, no duplicate, no foreign name). Required of every `Target` |
   | FB15 | **Nothing crosses before attach.** Required of every `Target` that declares `attach`, `pluginTarget` included. The suite seeds canaries: B, K and M set to fixed values, an argv sentinel, a deploy key file with a sentinel body, and a forwarded `forward_env_force` value. It runs `ok`, `preattach-fails` and `setup-fails-after-create`, and asserts that no canary, in raw, hex or base64 form, appears in any `GET /__bitc/calls` entry before the first one marked `attach`. A `Target` with no marked call fails. A plugin that declares `attach` and fails FB15 is not conformant |
3. **Reference plugin** `examples/plugins/bitc-provider-fakex/` (Go): the existing `fake`
   provider's logic behind the protocol. CI runs both suites against it, so the adapter is proven
   to behave exactly like the compiled-in `fake`. The two must give the same scenario results.
4. **Author guide** (the provider-author guide): build, pin, write the fake, run both
   suites. A plugin that passes both may call itself "bitc conformant (protocol 1)".

### A.11 The 8.5 text after this delta

"A provider is either a compiled-in Go package `internal/provider/<type>`, registered in one table
`type → {decoder, constructor, agent self-delete hook}`, or a plugin executable
`bitc-provider-<type>` pinned in the host file (8.5.1–8.5.9). Core decodes the common keys of
every `[providers.<type>]` section (`api_key` / `api_key_file`, `name_prefix`, `daily_usd`,
`max_live`, `launch_wait`, `start_timeout`, `fallback_until`, `breaker`, `transport`, `agent`,
`artifacts`; `plugin` and `sizes` for plugins). The provider's strict decoder, or the plugin's
`configure`, gets only the remaining keys (a plugin also gets `sizes`). Non-core keys are never
secrets. Either kind must pass the shared provider conformance suite (11)
against a fake of its API, and a plugin also the protocol suite. The rails stay shared."
Spec 14 "Later" drops "an exec/JSON provider protocol".

## B. Per-provider fallback boundary [8.1, 8.2, 15.8 edit]

### B.1 Definitions (generic)

- **Job input:** anything specific to this job: the spec (argv, cwd, remote env, B, K, M,
  deadline), the code ref or any repo content, the deploy key, and forwarded env values. The
  remote profile's `packages` and `tools` (names, URLs and pins) are **not** job input. They are
  provisioning, the same for every job of the repo.
- **Resource boundary** (`resource`): the first moment a provider-side resource for this job
  exists or may exist. That is `Launch` returning a handle, or a create request whose outcome is
  not a definite "nothing was created" (spec 8.2's name-lookup rule).
- **Attach boundary** (`attach`): the first moment job input has been sent to the resource, or a
  request has been sent that may have started the agent with job input and has not been
  definitely refused. "Definitely refused" means an HTTP or protocol error status received before
  the provider accepted the request.
- `attach` is never earlier than `resource`. For a provider whose create request carries job input
  (RunPod: `BITC_SPEC` in the create env), the two coincide and the provider declares `resource`.

### B.2 Declaration and setting

```go
type Boundary uint8
const (
    UntilResource Boundary = iota // fall back only before a resource exists (15.8 as of 2026-10-09)
    UntilAttach                   // also after a resource exists, until job input reaches it
)
type Caps struct {
    LiveLogs, Artifacts, Spend, SelfDelete bool
    FallbackUntil Boundary // the latest boundary this provider supports
    Fixed         bool     // fixed capacity: slots, configured price (C.3)
    SendEnv       EnvLevel // default of providers.<t>.send_env (S.2)
    MultiJob      bool     // a resource may serve more than one job (S.4)
}
```

| Provider | `Caps.FallbackUntil` | Why |
| --- | --- | --- |
| `runpod` | `resource` | the create body carries the spec, so create is attach |
| `sprites` (phase 1) | `attach` | create, policies and provisioning carry no job input; the agent exec carries it (sprites-provider.md 1.3) |
| `ssh` | `attach` | resource = the `slot-boot --prepare` exec request written; attach = the `--run` exec request written on an authenticated session (C.5.1). Lease, wipe and host checks carry no job input |
| `fake` | test-settable | `[providers.fake] fallback_until_cap` (test only) |
| plugin | `caps.fallback_until` from `hello` | |

Host key `providers.<t>.fallback_until = "resource" | "attach"`: class **host, narrow=boundary**
(5.3 table row added). Default: the provider's `Caps.FallbackUntil` for a built-in, `resource`
for a plugin (decision 2). A value later than the provider's declaration is 78. A repo file cannot
set it (78), and no env or flag touches it. The effective boundary is the configured value (or
the default), never later than the declaration. A host that sets `attach` for a plugin states
that it has run FB15 (A.10.2) against that plugin version; `doctor` prints
`ok plugin-<type>: fallback_until = attach (host opt-in; FB15 is the author's claim)`.

### B.3 Error types and the Walk rules [8.1 table, 8.2 edit]

New error types (`internal/provider`):

```go
// ErrPreAttach: Launch made a resource but failed before any job input reached it.
type ErrPreAttach struct{ ID string; Err error } // Err is ErrTransient, ErrUnavailable or ErrIneligible
// ErrAttached: Launch failed after attach; the job never falls back.
type ErrAttached struct{ ID string; Err error }
```

A `Launch` that returns a `Handle` has attached. A `Launch` error carrying no resource id is a
pre-resource failure, exactly as today. A built-in provider returns such an error only when it
knows nothing was created, or after its own 8.2 name lookup found nothing. The plugin adapter
returns one only for a definite classified error without `id` whose name check (A.5.5) found
nothing. Every ambiguous plugin launch comes back as `ErrAttached` (A.5.4).

Rules, in order:

1. **Post-attach never falls back.** `ErrAttached`, or any error once a `Handle` was returned,
   tears the resource down and exits 75 with `STARVED <pool> (<provider>: <why>)`. A `repo`-class
   `ErrSetup` exits 69 instead (15.9). `on_failure` and `fallback_until` play no part.
2. **`ErrPreAttach` with effective boundary `resource`** is treated as rule 1: teardown, 75. This
   is what "the host narrowed to `resource`" means.
3. **`ErrPreAttach` with effective boundary `attach`:**
   1. Core tears down `ID` (10 s bound). The resource is **confirmed gone** only if the teardown
      succeeded or returned "gone" (404), or, after an ambiguous create, a successful lookup found
      nothing. Otherwise (teardown failed, or the lookup failed) it is **unconfirmed**: left for
      the sweep.
   2. **Ledger per attempt.** Each attempt has its own resource name and writes its own `launch
      {name}` before the create and its own `end {name, exit: "fallback", created: true,
      cost_usd: price × minutes alive}`. `LedgerToday` already sums `created` ends, so a
      discarded resource's minutes count.
   3. **Reservation per attempt.** A confirmed-gone attempt's reservation is dropped. An
      unconfirmed attempt's reservation is **kept** with its estimate and marked `orphan:
      true`. An orphan is not tied to the pid, because the job's process usually lives on (it
      moved to the next provider). It counts in cap and `max_live` while its name is unlisted, so
      money and slots stay accounted for. **An orphan never protects its name from teardown:**
      the sweep (8.3 "Leak control") and the cap check of any later launch for that provider
      (under `launch.lock`, beside K8's past-`E` teardown) tear down at once every listed
      resource whose owner tag verifies and whose only reservation on this host is an orphan.
      When that teardown is confirmed (success or "gone"), the orphan is dropped; otherwise it is
      pruned at its `E`. Lines: `swept <type> <name> (orphaned by a pre-attach discard)` from the
      sweep, `bitc: <type>: tore down orphan <name>` (debug) from the cap check. **[8.3 edit]**:
      "kept until the job's ledger `end`" becomes "kept until the job's ledger `end`; for an
      attempt whose resource was not confirmed gone, kept as an orphan until a later teardown
      confirms it gone or its `E` passes. An orphan never shields a resource from the sweep".
   4. **Retries.** The 8.3 sentence "A create is retried once (... no retry follows a create
      that left a resource)" becomes three clauses: "**A create that left nothing is retried
      once (unchanged).** Under an effective `attach`, **a pre-attach discard is retried once,
      only after the discarded resource is confirmed gone.** Under `resource`, **no retry follows
      a create that left a resource.**" The first clause is RunPod's (and every provider's) 429
      or capacity retry, untouched; the job has at most one retry in total. An
      `infra` provisioning failure is wrapped as `ErrPreAttach{ID, ErrTransient{"setup step <name>
      failed: <line>"}}`. If the inner error is `ErrTransient`, attempt 1 is confirmed gone, and no
      retry has happened, `Launch` runs again with a new reservation and a new name, taken under
      `launch.lock` with cap and `max_live` re-checked. `ErrUnavailable` and `ErrIneligible` are
      never retried. An unconfirmed attempt is never retried: its `ErrPreAttach` goes straight to
      step 6. One job therefore never holds two possibly-live resources under one estimate.
   5. **Breaker** (decision 16): each attempt whose resource was created and then discarded before
      attach writes one `{kind: "failure"}` record. Pre-resource errors (capacity, quota, cap,
      `max_live`, a create that left nothing) still write none. **[8.1, 8.3 edit]**: the breaker
      counts "each 75 after BEGIN, and each resource discarded before attach".
   6. Then the **inner error** goes through the existing pre-resource path: `on_failure = "stop"`
      ends the walk; `"next"` notes it and tries the next provider. `ErrUnavailable` writes its
      `pause` record.
4. **A `repo`-class `ErrSetup` before attach** (for example a tool pin mismatch during
   provisioning) exits 69 with the 8.2 setup line, whatever the boundary, because the next
   provider would fail the same way.
5. **Ambiguous create** (a built-in's create request with an unknown outcome): with boundary
   `resource`, spec 8.2 applies unchanged (lookup, teardown, 75). With boundary `attach` and a
   create that carries no job input, it is `ErrPreAttach` with `ID` = the name (teardown by name).
   A failed lookup leaves it unconfirmed (rule 3.1): no retry, its reservation kept until `E`,
   and the sweep proves ownership from the owner tag. A plugin's ambiguous `launch` is always
   attached (A.5.4).
6. **Fallback notes** (one per provider passed over), replacing "when `local` follows":
   - next provider in the walk is `local`: `bitc: <provider>: running <pool> locally: <why>`
     (unchanged text);
   - next provider is remote: `bitc: <provider>: <pool>: trying <next>: <why>` (**new, [4
     edit]**);
   - after a discarded resource, `<why>` ends with `; <id> torn down before attach`, or
     `; <id> left for sweep` when the teardown failed.
   The `not eligible for remote` line and the no-provider-left rule (69 for `ErrUnavailable` or
   `ErrIneligible` last, else 75) are unchanged. "No `local` follows" becomes "the next provider
   is not `local`".

8.1 table after the edit:

| Error | Before the boundary | At or after it | Breaker |
| --- | --- | --- | --- |
| `ErrIneligible` | next provider | — | no |
| `ErrTransient` | no resource: one create retry (8.3 clause 1), then next provider. `ErrPreAttach{ErrTransient}` under `attach`: one retry only after the discard is confirmed gone (clause 2), then next provider | 75 | after BEGIN; and each resource discarded before attach |
| `ErrUnavailable` | next provider; one `pause` record | 75 | pause |
| `ErrSetup` `repo` | 69 | 69 | no |
| `ErrSetup` `infra` | as `ErrTransient` (provisioning before attach, wrapped in `ErrPreAttach`) | 75 | after BEGIN; and each resource discarded before attach |
| ambiguous plugin `launch` (A.5.4) | — (read as attached) | 75 | one `failure` (a resource may have been made) |
| command failure | — | its own code | no |

### B.4 Worked examples

Pool `heavy`, `providers = ["sprites", "runpod", "local"]`, `on_failure = "next"`.

1. Sprites create returns 429 → no resource: `bitc: sprites: heavy: trying runpod: 429 sprite
   creation rate limited`; RunPod runs the job.
2. Sprites create succeeds, the apt mirror times out while packages install (infra, pre-attach),
   effective `attach`: teardown (204, confirmed gone), one retry on a fresh Sprite, which fails
   the same way → `bitc: sprites: heavy: trying runpod: setup step packages failed: apt timeout;
   bitc-...-3f2a torn down before attach`. Two ledger `end fallback created=true` lines and two
   breaker `failure` records; the next job that fails the same way pauses Sprites for `window`.
2a. As 2, but the first teardown times out: no retry, attempt 1's reservation kept until its `E`
   → `bitc: sprites: heavy: trying runpod: setup step packages failed: apt timeout;
   bitc-...-3f2a left for sweep`. Attempt 1's reservation becomes an `orphan`. Ten minutes later
   the sweep timer lists the Sprite while the job still runs on RunPod in the same process: the
   orphan does not shield it, so `swept sprites bitc-...-3f2a (orphaned by a pre-attach discard)`,
   and the orphan is dropped on the 204.
3. Same as 2 with `[providers.sprites] fallback_until = "resource"` →
   `STARVED heavy (sprites: setup step packages failed: apt timeout)`, 75. RunPod and `local` are
   not tried.
4. Sprites attached (agent exec sent), then the WSS stream drops and the Sprite is gone →
   `STARVED heavy (sprites: <id> gone)`, 75, whatever `fallback_until` says.
5. RunPod create returns 500 and a name lookup finds the pod → spec 8.2 unchanged: teardown, 75.
6. ssh host unreachable (`connect timed out`) → no resource: `bitc: ssh: heavy: trying sprites:
   ssh <host>: connect timed out after 5s`.
7. Pool `["sprites", "runpod"]` (remote-only), `on_failure = "stop"`, Sprites pre-attach fails
   transiently twice → the stop rule: `STARVED heavy (no provider left: ...)`, 75.
8. ssh: the `--run` payload was written, then the connection drops (ssh exit 255) →
   `STARVED heavy (ssh: connection lost after attach)`, 75; the slot is torn down by name (or by
   the agent at its deadline). Sprites, RunPod and `local` are not tried.
9. A plugin `modal` first in the list: its `launch` times out after `start_timeout` →
   `STARVED heavy (modal: launch ambiguous: launch timed out after 20m0s)`, 75, after reaping
   by name. Nothing else is tried, whatever `fallback_until` says.
10. The plugin `modal` declares `caps.fallback_until = "attach"`, and the host file sets no
    `fallback_until`: the effective boundary is `resource` (plugin default), so its `pre_attach`
    is read as attached → teardown, 75. After a maintainer runs FB15 against that plugin version and
    adds `fallback_until = "attach"`, the same error goes the B.3.3 path.
11. ssh first in the list, the Mac asleep: `--prepare` fails with `ssh: connect to host 127.0.0.1
    port 22022: Connection refused` (tunnel down) → pre-resource, one retry, then `bitc: ssh:
    heavy: trying sprites: ssh 127.0.0.1: Connection refused`. No orphan, no breaker record, and
    the slot lock is free for the next job at once.

### B.5 15.8 rewording [15 edit]

"8. **No fallback once job input reaches a resource** (2026-10-09: "Yes a go";
amended <date of approval>). A remote job whose provider's fallback boundary (8.2: `resource` or
`attach`; a built-in's declaration or a plugin's `resource`, either narrowed or, up to the
declaration, set by the host) has passed never runs on another provider, `local` included.
Failures tear down and exit 75 (8.2), as the shell pool it replaces does. A request whose outcome
is unknown counts as past the boundary if it may have carried job input (a create under
`resource`, unless a name lookup succeeded and found nothing, 8.2; any attach request; any plugin
`launch` without a definite outcome, and any plugin `launch` error after which bitc finds the
job's resource). Before the boundary, failures tear down what exists and follow `on_failure`;
at most one retry happens, and never while a discarded resource may still be live. RunPod's
boundary is `resource`, so for RunPod this is the 2026-10-09 rule unchanged: fallback only before
any resource exists."

Spec 2 adds the row: **fallback boundary**: the latest point at which a failed remote job may move
on to the next provider (`resource` or `attach`, 8.2).

### B.6 Validation (78) and tests

| Case | Message |
| --- | --- |
| `fallback_until` not `resource`/`attach` | `providers.<t>.fallback_until: must be "resource" or "attach": "<v>"` |
| `attach` on a provider that declares `resource` | `providers.<t>.fallback_until: "attach" is wider than <t> allows ("resource")` |
| `fallback_until` in a repo file | `providers.<t>.fallback_until: host-only key` |

Conformance rows (new family `FB`, fake provider unless stated):

| ID | Behaviour |
| --- | --- |
| FB1 | `fallback_until` default equals `Caps.FallbackUntil` for a built-in and `resource` for a plugin declaring `attach`; a plugin opt-in to `attach` works; exceeding the declaration is 78; narrowing works; repo file 78 |
| FB2 | `ErrPreAttach` under `attach`, teardown ok: one retry with a new name and reservation (cap and `max_live` re-checked), then `trying <next>` with the `torn down before attach` suffix; one ledger `launch` and `end fallback created=true` per attempt name, counted by `LedgerToday` |
| FB3 | `ErrPreAttach` under `resource` (narrowed): teardown, 75, no other provider tried |
| FB4 | `ErrAttached` and post-handle failures: 75 under both boundaries; `repo` setup 69 |
| FB5 | pre-attach `repo` setup failure: 69, no fallback, no retry |
| FB6 | `trying <next>` only when the next provider is remote; `running locally` only when `local` is next |
| FB7 | teardown failure, or a failed lookup after an ambiguous create, before attach: **no retry**, `left for sweep`, then `on_failure`; the attempt's reservation is kept as an `orphan` (also past the process's exit) and counts in cap and `max_live` while unlisted; a resource that appears in `List` later is counted once. **Orphan vs sweep:** with the job's pid still alive (it moved on to the next provider), `bitc sweep` tears the listed resource down at once (`orphaned by a pre-attach discard`) and drops the orphan on a confirmed teardown; the next launch's cap check does the same under `launch.lock`; a failed teardown keeps the orphan until `E`; a live, non-orphan reservation still protects its name |
| FB8 | `on_failure = "stop"` after a pre-attach failure ends the walk (no retry beyond the one) |
| FB9 | RunPod (fake API): declared `resource`; every existing 15.8 row is unchanged; a 429 on create is retried once (clause 1), then `trying <next>`; a create that left a pod is never retried |
| FB10 | plugin (reference, fault mode): `launch` times out after the resource was created → reaped by name, 75, `launch ambiguous`, no retry, no next provider, under `resource` and `attach` |
| FB11 | plugin killed (SIGKILL) mid-`launch` after create → respawn, reap by name, 75, no next provider |
| FB12 | plugin sends a malformed frame, or a final error with an unknown class (with and without `id`), mid-`launch` after create → reap by name, 75, no next provider |
| FB13 | plugin `launch` error `transient` with an `id` → 75 under both boundaries; `pre_attach` under narrowed `resource` → 75; `pre_attach` under `attach` → FB2 path |
| FB14 | breaker: each discarded pre-attach resource writes one `failure`; after `failures` of them the provider is paused (`<provider> is paused: ...`) and the next job goes to the next provider without a create; pre-resource 429s write none |

## C. Fixed-capacity backends: the `ssh` provider [1, 8.9 new]

### C.1 Scope [1 edit]

Replace "multi-host scheduling" in the v1 non-goals with: "scheduling across hosts beyond a pool's
ordered provider list (no load balancing, job migration or cluster view)". Add to "What bitc is":
"**A provider may also be a fixed-capacity host the owner already has, reached over ssh, with a
number of slots instead of a price.**" macOS stays a non-goal for the controller. A Mac takes part
only through a Linux VM, which is an ordinary ssh host.

### C.2 Config

```toml
[providers.ssh]
host = "127.0.0.1"                         # IP or DNS name; see C.6 (here: reverse tunnel)
port = 22022
user = "bitc"                              # login user on the VM (gate only, C.5)
host_key = "ssh-ed25519 AAAAC3Nza...rest"  # required; the only key accepted
identity_file = "~/.config/bitc/ssh/vm_ed25519"  # required; 0600, unencrypted, credential rules
arch = "arm64"                             # required; amd64 | arm64; checked at every attach
slots = 1                                  # fixed capacity; 1 on the Mac target until probe O9
max_live = 1                               # core; ≤ slots
usd_per_hour = 0                           # accounting only (default 0)
connect_timeout = "5s"
memory_per_slot = "16GiB"                  # optional cgroup memory.max per slot
resolver = "9.9.9.9"                       # required; the only DNS server jobs may reach (C.8.7)
send_env = "forwarded"                     # default (Caps); "none" | "forwarded" | "forced" (S, decision 20)
netns = true                               # per-job network namespace (decision 18); false forces slots = 1
agent = { source = "release" }             # or "url" / "build" (C.4)
sizes = { s1 = { slots = 1 } }

[pools.heavy]
providers = ["ssh", "sprites", "runpod", "local"]
remote_limits = { size = { ssh = "s1", sprites = "cpu8", runpod = "cpu8" }, timeout = "30m" }
```

Repo side (remote profile, repo class):

```toml
[pools.heavy.remote]
allow = [["npm", "run", "typecheck"]]
arch = ["amd64", "arm64"]                  # default ["amd64"] (decision 12)

[remote]
tools = [
  { name = "node", url = "https://nodejs.org/dist/v22.11.0/node-v22.11.0-linux-x64.tar.xz", sha256 = "<hex>" },
  { name = "node", arch = "arm64", url = "https://nodejs.org/dist/v22.11.0/node-v22.11.0-linux-arm64.tar.xz", sha256 = "<hex>" },
]
```

1. **Core keys** apply as 8.5, except: `api_key`/`api_key_file` are unknown for `ssh` (the
   credential is `identity_file`); `daily_usd` is required **iff** `usd_per_hour > 0` (C.3).
   **[5.4 edit]**: the `daily_usd` default "required" becomes "required unless `Caps.Fixed` and
   `usd_per_hour` = 0".
2. `ssh` uses the system OpenSSH client, always as
   `ssh -F none -o BatchMode=yes -o IdentitiesOnly=yes -o IdentityAgent=none -o IdentityFile=<f>
   -o UserKnownHostsFile=<state>/ssh/known_hosts -o GlobalKnownHostsFile=/dev/null
   -o HostKeyAlias=bitc-ssh -o CheckHostIP=no -o UpdateHostKeys=no
   -o StrictHostKeyChecking=yes -o HostKeyAlgorithms=<type of host_key>
   -o ConnectTimeout=<s> -o ServerAliveInterval=15 -o ServerAliveCountMax=4 -o LogLevel=ERROR
   -o ForwardAgent=no -o ForwardX11=no -o ClearAllForwardings=yes -o ControlMaster=no
   -o ControlPath=none -o ProxyCommand=none -o ProxyJump=none -p <port> -l <user> -- <host> <verb...>`.
   The `known_hosts` file holds exactly one line, `bitc-ssh <host_key>`, written by bitc at each
   spawn. `HostKeyAlias` makes the line independent of host and port, so a tunnel endpoint such
   as `127.0.0.1:22022` needs no `[127.0.0.1]:22022` form and cannot collide with another
   `127.0.0.1` host. bitc never trusts a key on first use. No secret is in argv. `identity_file`
   is a path, and its content stays in the file. **No jump host in phase 1:** OpenSSH does not
   pass `-o` options to a `ProxyJump` hop, so its key could not be pinned this way. A relay
   (C.6 option (e)) comes later as an explicit `ProxyCommand` argv with its own pinned
   `known_hosts` line.
3. **Tools per arch:** a tool entry may carry `arch` (default `amd64`). `(name, arch)` is unique
   (else 78). A job is ineligible for a provider whose arch lacks an entry for one of its tools:
   `tool node has no arm64 entry`.

### C.3 Capacity, cost and rails

1. **Slots.** The provider owns `slots` numbered 1..N. A job of size `sK` takes `slots` units,
   the lowest free ones, all or none. **Controller side:** `ACCOUNT/remote/ssh/slot.<i>.lock`
   (`flock`), taken under `launch.lock` and held by the job's process for **the ssh attempt
   only**: it is released when that attempt ends (its cleanup after a `Handle`, or its teardown,
   confirmed or not, when the walk moves on), never at the exit of a process that went on to
   Sprites or RunPod. An unconfirmed attempt's slots stay accounted for by its `orphan`
   reservation (B.3.3.3) and by the host-side lease below, not by the lock.
   **Host side: the lease is the `owner` file**, `/var/lib/bitc/slots/<i>/owner` (root 0600):
   `{name, nonce, tag, created, lease_until}`. `/var/lib/bitc/slots/<i>/lock` is only a short
   `flock` mutex that every slot verb (`--prepare`, `--run`, `teardown`, the agent's own release,
   sweep's `list`) holds while it reads or changes `owner`. No verb holds it across ssh calls, so
   nothing depends on a process outliving its ssh session. A slot is **busy** while its `owner`
   names a job and `lease_until` is ahead. That guards against a second controller and a dead one:
   - `--prepare` writes `lease_until = now + start_timeout` and a fresh 128-bit `nonce` from the
     controller (an attempt id, not a secret);
   - `--run` proceeds only if `owner.name` and `owner.nonce` match its own, then sets
     `lease_until = E`;
   - `--prepare` on a busy slot is refused (`ErrTransient{"slot <i> busy on host"}`), and
     `teardown`, the agent's release and the sweep act only when `owner.name` equals the name they
     were given (else "gone", a no-op).
2. **`max_live`** counts live ssh jobs as for any provider and must be ≤ `slots` (else 78). All
   slots taken → `ErrTransient{"<n> of <slots> slots busy"}`, a fallback note, never a wait. The
   pool's own `wait` budget is for local admission only.
3. **Cost.** `Caps.Fixed = true`, `Caps.Spend = false`. `Estimate` = `usd_per_hour × (E − now)`,
   which is $0 by default. Every 8.3 rail runs for an unpriced fixed provider: the breaker,
   `launch.lock`, `List` (an error still counts as a refusal: `ssh <host>: list failed: <why>`,
   `ErrTransient`), the reservation, `max_live`, the slot accounting and the ledger. **Only the
   comparison `committed ≤ daily_usd` is skipped** when `usd_per_hour = 0` (there is no
   `daily_usd`). `committed` is still computed for `status`. Ledger lines carry `cost_usd =
   usd_per_hour × minutes`, so the owner can see utilisation.
4. **Breaker** applies unchanged. A host that keeps losing jobs (sleeping Mac) is paused for
   `window` after `failures` 75s.
5. **Resource id and name:** `<host>/s<i>[+s<j>...]/<name>`; the name is the 8.7 rule (core
   `ResourceName`), written to each slot's `owner` file with the owner tag. `List` returns one
   `Resource` per occupied slot group, with `Created` and `Expires` = `E` from `owner`. `Owner`
   reads the tag from `owner`.
6. **Leak control** (8.3 three layers):
   - **Agent:** at `D` it kills the job's cgroup and writes the result. At `D + 10m`, under the
     slot mutex and only if `owner.name` is still its own, it runs the teardown steps (C.5 step 8)
     and removes `owner` (`Caps.SelfDelete = true`; the "self-delete" of a fixed host is freeing
     the slot).
   - **Host cleanup:** `Teardown(id)` = the gate's `teardown <slots> <name>`. Under the mutex it
     compares `owner.name` with `<name>`. If they differ, or `owner` is missing, it prints `gone`
     and changes nothing, so a stale cleanup can never kill a later job that leased the same slot.
     Otherwise it kills the cgroup and uid, runs the teardown steps and removes `owner`.
   - **Sweep:** `bitc sweep --provider ssh` lists slots and tears down a slot group only if its
     **own owner tag verifies** AND (it is past `E`, OR its only reservation on this host is an
     `orphan` (B.3.3.3; at once), OR (no reservation of that name exists on this host AND
     `created` is more than `start_timeout` ago)). A non-orphan reservation whose pid is alive
     protects the name. This is the Sprites rule (sprites-provider.md 2.4). The next ssh
     launch's cap check applies the orphan clause too, so on the Mac (`slots = 1`) a leaked
     pre-attach slot is freed by the next job that can reach the host, not after `E`. A slot whose tag does not verify is another controller's or
     install's job: it is never touched before its `E`, and after `E` only with `--foreign`.

### C.4 arm64 and agent delivery [8.4 step 1, 13 edit]

1. `Info().Arch`, new field: the arch list a provider can run (`runpod`: `["amd64"]`; `sprites`:
   `["amd64"]` (unverified, probe 9.24 of the Sprites doc); `ssh`: `[arch]`). A job's arch set
   is the repo's `[pools.<p>.remote] arch` (default `["amd64"]`). If the provider's list and the
   job's set do not intersect, the job is `ErrIneligible`:
   `arch arm64 not allowed by pool (arch = ["amd64"])`.
2. The agent ships for **linux/amd64 and linux/arm64**. GoReleaser already builds both tarballs
   (13). The `release` source picks `bitc_<ver>_linux_<arch>` and its sha from `checksums.txt`.
   `url` gains per-arch forms: `url = "https://host/bitc-{arch}"` (template `{arch}`) and
   `paths = { amd64 = "...", arm64 = "..." }` (the local files whose shas are sent), replacing
   `agent_path` (kept as an alias for `paths.amd64`).
3. **New source `build`:** `agent = { source = "build", module_dir = "~/src/bitc" }`. bitc runs
   `go build -trimpath -ldflags=-buildid= ./cmd/bitc` there with `CGO_ENABLED=0 GOOS=linux
   GOARCH=<arch>` into `ACCOUNT/agents/<arch>/<sha256>`, once per source tree state
   (`git rev-parse HEAD` plus a clean tree, else 78 at `doctor --remote`, ineligible at run). It is
   for development hosts. A `release=true` binary never needs it.
4. **Delivery on ssh:** the bytes go over the ssh session (`gate install <sha>`, C.5), whatever
   the source. The VM fetches nothing to get the agent.
5. **Native deps of jobs likely routed to the Mac VM** all publish linux/arm64 builds, but each
   pool must be checked before it lists `arm64`: Prisma engines (`linux-arm64-openssl-3.0.x`
   target in `binaryTargets`), better-sqlite3 (prebuilt or built from source, which needs
   `python3`, `make` and `g++` installed), and Playwright Chromium (Playwright's Ubuntu arm64
   build; no branded Chrome on linux/arm64). pgvector also exists for arm64 (built per Postgres
   major, or the distro package), but a job that needs Postgres needs `services`, which `ssh`
   does not run in phase 1 (decision 15), so such jobs stay on Sprites or RunPod. The runbook
   gives one smoke command per dep. A mismatch there is a command failure, not a fallback, which
   is why arm64 is opt-in (decision 12). **Rosetta must be off** in the VM (C.8.8): with it, an
   amd64 tool entry would run silently on arm64 and defeat per-arch eligibility.

### C.5 Host layout, gate and job isolation

**On the host (set up once by the owner's runbook, verified by `doctor`):**

```
/opt/bitc/sbin/bitc-login       root:root 0755, the forced command (below)
/opt/bitc/sbin/bitc-gate        root:root 0755, POSIX sh, ≤ 200 lines, in the repo (scripts/ssh-gate.sh)
/opt/bitc/agent/<sha256>/bitc   root:root 0755, installed only by the gate
/etc/bitc/host.conf             root:root 0644: slots, resolver, gateway, secret_paths (C.6.2)
/var/lib/bitc/                  root 0700
  slots/<i>/{lock, owner}       root 0600 (C.3.1)
  slots/<i>/{work, home, tmp, vartmp, runlock}/   bitc-s<i> 0700, wiped per job
  results/<job>/                root 0700 (as 8.4 step 6), removed at teardown
/etc/sudoers.d/bitc             root:root 0440 (below); `visudo -c` clean
/etc/ssh/sshd_config.d/bitc.conf  Match User bitc: AuthenticationMethods publickey,
                                DisableForwarding yes, AllowAgentForwarding no, PermitTTY no
~bitc/.ssh/authorized_keys      root:root 0644, ~bitc/.ssh root:root 0755
/etc/cron.allow, /etc/at.allow  root:root 0644, listing root only, so every bitc-s<i> is denied
/etc/nftables.d/bitc.nft        the C.8.7 ruleset, loaded at boot
user bitc                       login user: shell /bin/sh (a real shell: sshd runs the forced
                                command through it), password locked, no other groups
users bitc-s1..bitc-sN          no password, shell /usr/sbin/nologin, no sudo, own group each,
                                no linger
```

`authorized_keys`, one line:
`restrict,command="/opt/bitc/sbin/bitc-login" ssh-ed25519 <controller key> bitc-controller`.

`/opt/bitc/sbin/bitc-login`:

```sh
#!/bin/sh
# sshd runs this as `/bin/sh -c /opt/bitc/sbin/bitc-login` with SSH_ORIGINAL_COMMAND set
[ -z "${SSH_AUTH_SOCK-}" ] || exit 64          # a forwarded agent never reaches the gate
exec /usr/bin/sudo -n /opt/bitc/sbin/bitc-gate "${SSH_ORIGINAL_COMMAND-}"
```

`/etc/sudoers.d/bitc`:

```
Defaults!/opt/bitc/sbin/bitc-gate !use_pty, !requiretty, !log_output, env_reset
bitc ALL=(root) NOPASSWD: /opt/bitc/sbin/bitc-gate
```

sudo's `env_reset` strips `SSH_ORIGINAL_COMMAND`, so the wrapper passes it as the gate's only
argument, and the gate never reads its environment. `!use_pty` keeps stdin a pipe, so the
binary payload of `--run` and `install` reaches the gate unchanged. The agent-socket check
therefore lives in `bitc-login` (before sudo) and in `doctor` (C.8.4), not in the gate.

**Gate verbs** (`$1` split on single spaces, no quoting, each field checked against a fixed
grammar; any other input exits 64 with nothing on stdout):

| Verb | Does |
| --- | --- |
| `probe` | prints `{arch, kernel, cgroup2, slots, users_ok, gate_version}` as JSON |
| `install <sha>` | reads stdin into a root-only temp file, checks sha256 = `<sha>`, moves it into `/opt/bitc/agent/<sha>/bitc`; keeps the newest 3 |
| `agent <sha> <verb> <args>` | re-hashes `/opt/bitc/agent/<sha>/bitc`, then execs it with `__agent <verb>`, `<verb>` ∈ {`check-host`, `slot-boot`, `tail`, `read`, `list`, `teardown`} |

For `slot-boot --run` the gate sends the agent's stderr to the slot's `agent.log`, never to the
ssh channel, so everything on the ssh client's stderr comes from the client itself (C.5.1).

**Eligibility** (preflight, network-free): a repo granted `root_steps`, or whose profile lists
`services`, is `ErrIneligible` for `ssh` (decision 15), with the notes
`bitc: ssh: <pool>: trying <next>: services are not run on ssh hosts` and
`... root steps are not run on ssh hosts`.

**Job flow:**

1. **Lease** (controller, under `launch.lock`): pick slots (C.3.1), reservation keyed by the
   resource name, ledger `launch {name}`.
2. **Connect and check:** `gate probe`. `arch` must equal the configured `arch`, else
   `ErrUnavailable` (`host arch amd64, configured arm64`). Then `install` if the sha is missing,
   then `agent <sha> check-host` (C.8). Nothing for this job exists on the host yet.
3. *(resource point, C.5.1)* **Prepare slot** (`agent <sha> slot-boot --prepare <slots> <name>
   <nonce>`). For each slot, under its mutex: a busy slot (C.3.1) →
   `ErrTransient{"slot <i> busy on host"}`; else write `owner` with `lease_until = now +
   start_timeout`, kill every process of `bitc-s<i>`, run the teardown steps of step 8 against
   any residue, and recreate `work`, `home`, `tmp`, `vartmp` and `runlock` (0700, owned by
   `bitc-s<i>`). Then, outside the mutex: check that the `packages` are installed
   (`dpkg-query`), failing with `ErrIneligible{"package <p> not installed on <host>"}` if not;
   download and sha-check tools into `/var/lib/bitc/tools/<sha>/` (root-owned, read-only,
   shared by sha). A failure here is `ErrPreAttach` (C.3.5 id) for infra, 69 for a `repo` tool
   pin.
4. *(attach point, C.5.1)*
5. **Attach:** `agent <sha> slot-boot --run <slots> <name> <nonce>` with the boot payload on
   stdin: an 8-byte big-endian length, then exactly that many bytes of JSON (spec, deadline,
   deploy key), the `--boot-stdin` format of the Sprites design (shared code,
   sprites-provider.md 4). Under each slot's mutex, the agent checks that `owner.name` and
   `owner.nonce` match and that `lease_until` is ahead, else it exits 65 with `refused: slot <i>
   not leased to <name>`. It sets `lease_until = E`, then reads the payload. A short read
   (EOF before the declared length) or unparsable JSON exits 65 with `refused: truncated
   payload` and starts nothing. Otherwise it prints **`started <name>`** on stdout, closes
   stdout, `setsid`s and detaches, so an ssh drop does not end the job. `Launch` returns the
   `Handle` only on that ack line.
6. **Run (agent, root):** re-check the host (C.8, every item but 5 and 12; a failure is `infra`, 75 and
   a `pause` record). Fetch code with the deploy key on a per-job root tmpfs, then shred it (8.4
   step 4). Create cgroup `/sys/fs/cgroup/bitc/slot-<i>` (`memory.max` from `memory_per_slot`,
   `pids.max` 4096). Enter new mount, PID, IPC, UTS and (with `netns = true`) network
   namespaces:
   - a fresh `/proc` for the PID namespace, and a fresh `/dev/mqueue` for the IPC namespace (so
     SysV and POSIX IPC are per job);
   - `/tmp` and `/dev/shm` bound from the slot's `tmp`, `/var/tmp` from `vartmp` and
     `/run/lock` from `runlock`;
   - network: a veth pair `bv<i>` (host side, `10.203.<i>.1/30`) and `eth0` (job side,
     `10.203.<i>.2/30`, default route via `.1`), NAT in the VM, filtered by C.8.7, and
     `/etc/resolv.conf` bound to one line, `nameserver <resolver>`. Jobs share no loopback and no
     ports, and cannot reach each other or the VM itself.

   Set `PR_SET_NO_NEW_PRIVS` and an empty capability bounding set, then install the job seccomp
   filter (C.5.3). Run setup steps (`run_as =
   "job"` only) and the command as `bitc-s<i>` in `work`. No services and no root steps (decision
   15). Results go to `/var/lib/bitc/results/<job>/`, with the MAC as 8.4.
7. **Watch/Output/Fetch:** `agent <sha> tail` / `read` over fresh ssh sessions, resumed by offset
   (the Sprites `__agent tail`/`read` design). No listener, no results server, no bearer.
8. **End (teardown steps, run by `teardown`, the agent's release and `--prepare`'s residue
   pass):** `cgroup.kill`, then SIGKILL every remaining `bitc-s<i>` process until none is left;
   delete the job's network namespace and `bv<i>`; `crontab -r -u bitc-s<i>` and remove the
   uid's `at` jobs (both should be empty, since cron and `at` are denied); remove
   `/var/lib/systemd/linger/bitc-s<i>` and run `loginctl terminate-user bitc-s<i>` if either
   exists (a doctor `fail` too, C.8.11); remove SysV IPC objects owned by the uid
   (`ipcrm`) in the VM's namespace; `find <each local fs> -xdev -uid <uid>` outside the slot
   dirs and delete what it finds (logged as `pod: removed <n> stray files of bitc-s<i>`);
   unmount the key tmpfs; wipe `work`, `home`, `tmp`, `vartmp` and `runlock`; remove `owner`.

#### C.5.1 Resource and attach points for ssh

1. **Before the resource point** (lease, `probe`, `install`, `check-host`): nothing on the host
   belongs to this job. A failure is a pre-resource failure (8.2).
2. **Resource point:** the `slot-boot --prepare` ssh process has been started **and** has got
   past session setup. A `--prepare` that exits 255 with exactly one of the pre-session stderr
   lines of item 3 never reached the gate, so nothing ran on the host: it is a **pre-resource**
   failure with no teardown, no orphan reservation and no breaker record. The connect, resolve and
   `kex_exchange_identification` forms are `ErrTransient` (8.2 path: one create retry, then next
   provider; the sleeping-Mac and tunnel-down case); `Host key verification failed.` and
   `Permission denied (publickey).` are `ErrUnavailable` (one `pause` record), as at `probe`. Any other `--prepare` failure means an `owner` file and a
   wiped slot may exist, so it is `ErrPreAttach{ID: <name>}`. Under an effective `resource`
   boundary, that is teardown by name and 75 (B.3.2).
3. **Attach point:** the `--run` exec request written on an authenticated session. bitc drives
   the system OpenSSH client and cannot see that moment directly, so it classifies the `--run`
   process by its result:
   - **pre-attach** only if ssh exits 255 and its stderr is exactly one line of these pre-session
     forms (with `LogLevel=ERROR`): `ssh: connect to host <h> port <p>: Connection refused`,
     `... Connection timed out`, `... No route to host`, `... Network is unreachable`, `ssh: Could
     not resolve hostname <h>: <why>`, `Host key verification failed.`, `<user>@<h>: Permission
     denied (publickey).`, or a line starting `kex_exchange_identification: `. All of them
     happen before authentication completes, so no channel and no request existed;
   - **attached**: everything else, including exit 255 with any other text, a dropped
     connection, a timeout, a signal, a `refused:` line (65) or exit 0 without `started <name>`.
     Then core tears the slot down by name (`teardown` checks `owner.name`, C.3.6), the agent's
     own deadline covers a host that cannot be reached, and the job exits 75
     (`STARVED <pool> (ssh: <why> after attach)`). No retry and no next provider.

   The whitelist is the same for `--prepare` (item 2) and `--run`. It is matched exactly, line for
   line, and pinned against the OpenSSH versions in CI and on the controller (SH13, SH21).
4. A payload that never fully arrives starts nothing (the agent refuses it), but core still
   reads it as attached, because it cannot tell a refused payload from a started job whose ack
   was lost.

#### C.5.2 Per-job values on the ssh host

"No secrets on the remote except the read-only deploy key" holds only for **long-lived**
secrets (the reachability credential of C.6.2 is the one long-lived exception). Per job, these
values reach the host, all inside the `--run` payload on ssh stdin, never in argv, env of the
gate, or a file bitc writes:

| Value | Where it lives on the host | Destroyed |
| --- | --- | --- |
| deploy key (private repos) | agent memory; a per-job root tmpfs during the fetch only | shredded and unmounted before any repo code (8.4 step 4); buffer zeroed (best effort) |
| K (result MAC key), M (marker nonce) | agent memory and the agent's fd-3 memfd (root, process-private); never on disk; M also in the agent's marker lines in the slot's `agent.log` | agent exit at the end of the job; a dead agent with its process at teardown; `agent.log` at teardown |
| B | zero for `ssh` (no results server) | — |
| argv, cwd, ref, deadline | agent memory, the memfd, `state.json` in `results/<job>/` (root 0700) | `results/<job>/` removed at teardown |
| forwarded env values (only as `providers.ssh.send_env` allows, S; default `"forwarded"`, so no forced name) | agent memory, the memfd, and the job's own process environment (`/proc/<pid>/environ`, readable by that slot uid and root) | process exit; every slot-uid process is killed at teardown |

A job that needs more than the effective `providers.ssh.send_env` is `ErrIneligible` for `ssh`
before any connect (S.3), for example
`bitc: ssh: <pool>: trying <next>: forced env not sent to ssh (providers.ssh.send_env = "forwarded"): <N1>, <N2>`.
An ssh slot is not a multi-job resource (S.4): teardown (C.5 step 8) kills every process of the
slot uid and deletes every file it owns, so the host may set `"forced"`. The default stays
`"forwarded"` because the host is a box the owner also uses and its root can read a running job.
What this cannot guarantee: VM swap can page agent memory to the VM's disk, so the runbook turns
swap off in the VM and `doctor --remote` warns while `/proc/swaps` lists any device
(`warn ssh-swap: <device> active; per-job secrets may reach disk`); root on the host can read all
of it while the job runs (C.5.4).

#### C.5.3 Job seccomp filter (decision 21)

The agent installs it after `PR_SET_NO_NEW_PRIVS`, as root, immediately before it drops to
`bitc-s<i>` for setup steps and the command; it is inherited by every job process and cannot be
removed. Default action allow; these return `EPERM` (except `clone3`):

| Syscall | Rule | Why |
| --- | --- | --- |
| `socket`, `socketpair` | `domain == AF_VSOCK` (40) | vsock reaches the Mac (CID 2) whatever nftables or a network namespace says; OrbStack and Lima `vz` run host services over it |
| `unshare` | `flags & CLONE_NEWUSER` | user namespaces are the entry point of most kernel privilege escalations, which on a shared kernel reach the Mac (C.7) |
| `clone` | `flags & CLONE_NEWUSER` | same |
| `clone3` | always, `ENOSYS` | its flags sit in a struct seccomp cannot read; glibc and Go then fall back to `clone` |
| `keyctl`, `add_key`, `request_key` | always | removes the per-uid keyring carry-over between jobs of one slot |

Arch-specific syscall numbers are generated for `amd64` and `arm64`, and the filter checks
`AUDIT_ARCH` first (a foreign arch is killed). Effect on jobs: Chromium's own sandbox needs user
namespaces, so Playwright runs with its default `chromiumSandbox: false`; `docker`/`podman` in a
job cannot work (they are unavailable on ssh anyway). `doctor --remote` proves the filter with
C.8 item 12.

#### C.5.4 What isolation holds, and what it cannot

**What isolation holds on a possibly shared host:** jobs in different slots run as different uids
with 0700 dirs. Each job gets its own `/tmp`, `/var/tmp`, `/run/lock`, `/dev/shm`, `/proc`, IPC
(SysV and POSIX message queues) and network namespace (loopback and ports included). It has no
privileges and no cron, `at` or linger, and every process of the slot uid is killed at the end.
Files the uid left anywhere on a local filesystem are deleted at the end. A job cannot read the
previous job's files in the same slot, because every slot dir is wiped and recreated.

**What it cannot guarantee (stated in 10 and the runbook):**

- The kernel is shared, so a local privilege escalation defeats all of it. The seccomp filter
  (C.5.3) removes user namespaces, the commonest entry point, not every bug. On OrbStack, if O8
  confirms that machines share one VM with OrbStack's Docker engine, that escalation reaches the
  Mac's home directory (C.7).
- Root and the owner's own processes on that host can read every job.
- Per-uid kernel state that no namespace covers can carry over between jobs in the same slot.
  The kernel keyrings are closed by the seccomp filter (C.5.3); others (for example per-uid
  rlimit counters and inotify instances) are bounded by killing every process of the uid.
- A job can fill the shared disk (no quota in phase 1) and starve the next job or the host.
- CPU, memory, disk and network bandwidth are contended with whatever else runs there, so
  timing-sensitive tests are noisy.
- Egress is the open internet minus the C.8.7 block. Unlike Sprites, there is no domain allowlist
  in phase 1.
- The host's own packages and kernel are trusted and unpinned. Caches (npm, browsers) are not
  kept between jobs in phase 1 (16.1 applies later).
- With `netns = false` (decision 18 (b)), jobs share the VM's loopback and ports and can reach
  any listener in the VM, which is why `slots` must then be 1.

### C.6 Reachability (owner decision 9)

The controller must not assume Tailscale or that it can reach the VM. The VM sits behind the
owner's home router, and the Mac may sleep.

| Option | How | For | Against |
| --- | --- | --- | --- |
| **(a) reverse tunnel** (rec) | a systemd unit in the VM runs `ssh -N -o ExitOnForwardFailure=yes -R 127.0.0.1:22022:127.0.0.1:22` to the controller host as the tunnel-only user `bitc-tunnel`, locked down by C.6.3; bitc's `host = "127.0.0.1"`, `port = 22022` | no inbound port at home, no third party, works through NAT | the tunnel user is a new account on the controller; reconnect lag after sleep |
| (b) mesh VPN (Tailscale, WireGuard) | VM joins the owner's tailnet or a WireGuard peer; `host` = its mesh IP | simplest if already in use | a third-party coordination service (Tailscale), or key management (WireGuard); the tailnet range is blocked for jobs (C.8.7) |
| (c) router port-forward | public port to the VM's sshd | none worth it | exposes sshd to the internet; not recommended |
| (d) controller on the LAN | the controller runs on the same local network as the host | no tunnel | only if the agents run on that network |
| (e) relay host | a jump host | central | **not in phase 1** (C.2.2: no pinnable `ProxyJump`) |

1. Whatever the option: an unreachable host before the resource point is `ErrTransient`
   (`ssh <host>: connect timed out after 5s`), so the walk moves on. After attach the agent keeps
   running. The controller retries `tail` for `start_timeout` and then exits 75 `STARVED <pool>
   (ssh: host unreachable since <ts>)`, leaving the slot to the agent's own deadline and the
   sweep.
2. **The reachability credential** is the one long-lived secret on the host (per-job values are
   C.5.2). Option (a) puts the tunnel's private key on the VM (on the controller it can only open
   that one listen port, by C.6.3). Option (b) puts the mesh node key
   there (`/var/lib/tailscale/` or the WireGuard private key). Either must be **root-only**: the
   file root 0600, every parent directory root and not group- or world-readable. The tunnel unit
   runs as root. No gate verb reads or prints it. Its paths are listed in `/etc/bitc/host.conf`
   `secret_paths`, and `check-host` fails if any slot user can open them (C.8.10).

3. **Controller side of option (a).** The tunnel key sits on the VM, where a job that escalates,
   or a hostile VM root, can take it. A key line of `restrict,port-forwarding,permitlisten=...`
   alone re-enables **all** forwarding (`permitlisten` limits only `-R`), so that key could open
   `-L`/`direct-tcpip` channels from the controller to its localhost services, cloud metadata or
   internal networks, and with no forced command could run commands as the tunnel user. The
   runbook therefore sets, on the controller:
   - account `bitc-tunnel`: shell `/usr/sbin/nologin`, password locked, no groups, home root-owned
     0755 with `.ssh` root 0755 and `authorized_keys` root 0644;
   - `authorized_keys`, one line:
     `restrict,port-forwarding,permitlisten="127.0.0.1:22022",command="/bin/false" ssh-ed25519 <vm tunnel key> bitc-tunnel`;
   - `/etc/ssh/sshd_config.d/bitc-tunnel.conf`:
     ```
     Match User bitc-tunnel
         AuthenticationMethods publickey
         AllowTcpForwarding remote
         PermitOpen none
         PermitListen 127.0.0.1:22022
         GatewayPorts no
         AllowStreamLocalForwarding no
         PermitTunnel no
         X11Forwarding no
         AllowAgentForwarding no
         PermitTTY no
         PermitUserRC no
         ForceCommand /bin/false
     ```
   - **check**, as root (runbook step, and `doctor --remote` when it runs as root):
     `sshd -T -C user=bitc-tunnel,host=vm,addr=127.0.0.1` must print `allowtcpforwarding remote`,
     `permitopen none`, `permitlisten 127.0.0.1:22022`, `gatewayports no`,
     `allowstreamlocalforwarding no`, `permittunnel no`, `x11forwarding no`,
     `allowagentforwarding no`, `permittty no`, `permituserrc no` and
     `forcecommand /bin/false`. Doctor lines: `ok ssh-tunnel-sshd`, `fail ssh-tunnel-sshd:
     <option> is <value>`, or `warn ssh-tunnel-sshd: needs root; run runbook step "tunnel check"`.
     Row SH22 proves the effect with a real sshd, whatever the option precedence.

### C.7 The first target: a Mac with a Linux VM

- **Host:** an Apple-silicon Mac. **VM:** Ubuntu 24.04 arm64, `arch = "arm64"`,
  `slots = 1` until probe O9 passes (then 2), `memory_per_slot = "16GiB"`. That leaves the Mac
  its own memory.
- **VM product (decision 10).** The recommended default is a **dedicated full VM** with no
  shared directories and Rosetta off: Lima, Tart or UTM. For Lima, the instance YAML is exactly
  these keys on top of the Ubuntu 24.04 template (key names per Lima 1.x; the runbook checks them
  against the installed version, O1):
  ```yaml
  vmType: vz
  arch: aarch64
  mounts: []                       # no Mac directory at all
  rosetta:
    enabled: false                 # no Rosetta virtiofs share, no binfmt handler
  ssh:
    loadDotSSHPubKeys: false       # keep the Mac's ~/.ssh/*.pub out of cidata
    forwardAgent: false
  propagateProxyEnv: false         # keep the Mac's proxy env out of cidata
  containerd:
    system: false
    user: false
  portForwards:                    # Lima auto-forwards every guest TCP listener to the Mac's
    - guestPortRange: [1, 65535]   # localhost by default; ignore them all. Reachability is the
      ignore: true                 # reverse tunnel (C.6), so no rule precedes this one.
  ```
  Lima's own ssh connection to the guest is not a `portForwards` rule and is unaffected. If the
  owner keeps a guest port for its own use (for example option (d)), that rule goes before the
  ignore-all rule (first match wins).
- **Lima cidata.** Lima attaches a cloud-init ISO each start and mounts it at `/mnt/lima-cidata`
  (iso9660). It carries the Mac username, the Lima-generated public key, and, unless switched off
  above, the Mac's `~/.ssh/*.pub` and proxy env. C.8.2 fails on any `/mnt/<anything>`, and no
  exception is made. The runbook installs `bitc-cidata-off.service` (`After=cloud-final.service`,
  `WantedBy=multi-user.target`), which runs `umount /mnt/lima-cidata` once Lima's boot scripts
  have run; each boot re-mounts it and the unit unmounts it again. The block device itself stays
  attached. It is `root:disk` 0660, so slot users (no supplementary groups, C.8.6) cannot read
  it, and with the YAML above it holds the Mac username and Lima's own public key, nothing
  secret. `check-host` sees no mount. If probe O1 shows that the installed Lima needs cidata
  mounted after boot (for example for its guest agent), the owner picks another product; the
  check does not change.
- **OrbStack residual risk.** OrbStack Linux machines are believed to be system containers on
  one Linux VM and kernel shared with OrbStack's Docker engine and every other machine
  (unverified, O8; the `fuse.lxcfs` exception of C.8.1 hints at it). That shared VM mounts the
  Mac's `/Users` for Docker whatever the per-machine sharing setting says. Turning sharing off
  per machine removes the visible mount, but the "kernel exploit defeats isolation" limit (C.5.4)
  then reaches the Mac's home directory, not only a disposable VM. OrbStack is acceptable only if O8
  shows a separate VM boundary, or if the owner accepts this in writing.
- **OrbStack defaults are unsafe for this.** It mounts the Mac's files into machines (commonly
  `/Users/...` and `/mnt/mac`), may forward the Mac's ssh agent, exposes Rosetta through a
  virtiofs share and `binfmt_misc`, may bridge a Docker socket, and ships Mac-integration
  commands that run programs on macOS from inside the machine (unverified: O1–O3, O10).
- **Requirement (any VM product):** Mac file sharing, every Mac integration, Rosetta and any
  bridged Docker or containerd socket MUST be disabled. Nothing from the Mac's home directory,
  keychain, ssh agent or command bridge may be visible to the slot users or to root in the VM.
  `check-host` enforces this at every job start (C.8). The provider does not care which product
  passes it.
- **Sleep:** the runbook sets the Mac not to sleep on power while the pool is enabled, or the
  owner accepts transient fallbacks (C.6).

### C.8 Host checks: `doctor --remote` and the agent

`agent check-host` runs as root at C.5 step 2 (pre-attach: a failure is `ErrUnavailable` with a
`pause` record), and again in the agent at C.5 step 6 (post-attach: `infra`, 75, `pause` record).
`doctor --remote` runs every item (a failure is `fail`, exit 69). Items marked *doctor* run only
there. **Mount scan:** it reads `/proc/<pid>/mountinfo` for one pid of every distinct mount
namespace on the host (grouped by the inode of `/proc/<pid>/ns/mnt`, all pids walked), so a share
mounted only inside a systemd unit with `PrivateMounts`, a container or another namespace is
seen. It **fails** on any of:

1. a mount, in any mount namespace, with fstype `virtiofs`, `9p`, `nfs`, `nfs4`, `cifs`, `smb3`,
   `vboxsf`, `prl_fs`, `fakeowner` or any `fuse.*`, except `fuse.lxcfs` (this also catches a
   Rosetta share); **or a host share that is exported but not mounted**, which root, or a job
   after an escalation, could mount: any device under `/sys/bus/virtio/devices/` whose `device`
   file reads `0x001a` (virtio-fs) or `0x0009` (9p transport), or any
   `/sys/fs/virtiofs/*/tag` (this device and tag part is only a `warn` while no share-type mount
   exists in any scanned namespace, as on an OrbStack isolated machine, where the devices belong to
   the shared VM and fall under decision 10; and a host path or `/mnt` entry holding only empty
   directories is a `warn` too, while any mount at or under a host path, and any non-directory
   entry under one, still fails);
2. a mount point, or an existing non-empty directory, at `/Users`, `/Volumes`, `/mnt/mac`,
   `/private`, `/opt/orbstack-guest` (unverified paths, O1), or `/mnt/<anything>`;
3. a Mac command bridge on `PATH` for root or a slot user (`mac`, `macctl`, `open` resolving
   under `/opt/orbstack-guest`), or any file under `/opt/orbstack-guest/bin` (O2);
4. agent forwarding possible: `sshd -T -C user=bitc,host=bitc,addr=127.0.0.1` not showing
   `allowagentforwarding no` and `disableforwarding yes`, or any socket under
   `/opt/orbstack-guest/run` or `/run/host-services` (O3). (*doctor* also runs `bitc-login` with
   `SSH_AUTH_SOCK` set and expects 64.)
5. (*doctor*, as a slot user, **in the mode `host.conf` configures**: inside a job network
   namespace when `netns = true`, in the VM's own namespace when `netns = false`) a TCP connect to
   the VM's gateway, `host.orb.internal` (O4), `192.168.1.1`, `10.0.0.1`, `100.100.100.100`,
   `198.18.0.1`, `169.254.169.254`, the VM's own address, `[fe80::1]`, and IPv6 global targets:
   the VM's own global address and `<its /64 prefix>::1` when it has one, and the public
   `[2606:4700:4700::1111]:443` (all IPv6 is dropped for jobs, so GUA devices on the home network
   are unreachable too), that completes or is refused (RST). Only a timeout or `EPERM` passes. A
   DNS query to `resolver` must succeed;
6. users: `bitc` without shell `/bin/sh`, with a usable password, with any group or sudo rule
   other than the sudoers file above, or with an `authorized_keys` other than the one
   `restrict,command="/opt/bitc/sbin/bitc-login"` line; `/etc/sudoers.d/bitc` not byte-equal to
   the text above, or `visudo -c` failing; `bitc-s<i>` missing, having a password, a login shell,
   sudo rights or a supplementary group; `/var/lib/bitc` not root 0700; the gate or login wrapper
   not root 0755; the cgroup v2 `bitc` subtree not delegable;
7. the LAN block not loaded: `nft -j list table inet bitc`, canonicalised, differs from the
   ruleset below rendered with `host.conf`'s `slots`, `resolver` and `gateway`, or
   `net.ipv4.ip_forward` is not 1. Rules in other tables cannot undo it (in nftables a `drop` in
   any base chain is final), so only this table is compared. A reboot that loses the rules fails
   here at the next job start, before attach;
8. a `binfmt_misc` handler that can run x86-64 code: any enabled entry under
   `/proc/sys/fs/binfmt_misc/` whose magic starts with the ELF64 little-endian header
   (`7f454c460201`) and has e_machine `3e00` (x86-64) at offset 18, or whose name or interpreter
   contains `rosetta` (O10);
9. a container-engine socket: any Unix socket on a local filesystem or under `/run`, `/var/run`
   or `/opt/orbstack-guest` whose name contains `docker`, `containerd`, `podman` or `orbstack`,
   or `DOCKER_HOST` set in `/etc/environment` or root's environment. Root on the shared Docker
   VM reaches the Mac's files;
10. a reachability credential (C.6.2) in `secret_paths` that is not root-owned 0600 under
    root-only directories, or that `bitc-s<i>` can open (checked by `open(O_RDONLY)` as each slot
    user, expecting `EACCES`);
11. cron, `at` or linger open to slot users: `/etc/cron.allow` or `/etc/at.allow` missing or
    listing a `bitc-s<i>`; a crontab or `at` job owned by a `bitc-s<i>`; any
    `/var/lib/systemd/linger/bitc-s<i>`;
12. (*doctor*, as a slot user under the job seccomp filter of C.5.3) any of these not failing
    with `EPERM`: `socket(AF_VSOCK, SOCK_STREAM)` and a connect to CID 2 (host) and CID 1 on the
    ports the product uses (OrbStack and Lima guest-agent ports from O2, plus 22 and 1024), and
    `unshare(CLONE_NEWUSER)`, `clone(CLONE_NEWUSER)` and `keyctl(KEYCTL_GET_KEYRING_ID)`;
    `clone3` not failing with `ENOSYS`.

The C.8.7 ruleset (`/etc/nftables.d/bitc.nft`; `<uids>`, `<resolver>` and `<gateway>` rendered
by the runbook from `host.conf`; drop, never reject, so a job learns nothing about what is
there):

```
table inet bitc {
  set slot_uids { typeof meta skuid; elements = { <uids> } }
  set lan4 { type ipv4_addr; flags interval; elements = {
    0.0.0.0/8, 10.0.0.0/8, 100.64.0.0/10, 127.0.0.0/8, 169.254.0.0/16, 172.16.0.0/12,
    192.0.0.0/24, 192.168.0.0/16, 198.18.0.0/15, 224.0.0.0/4, 240.0.0.0/4 } }
  chain input {                       # a job never talks to the VM itself
    type filter hook input priority 0; policy accept;
    iifname "bv*" drop
  }
  chain forward {                     # jobs in their network namespaces (netns = true)
    type filter hook forward priority 0; policy accept;
    iifname "bv*" ip daddr <resolver> meta l4proto { udp, tcp } th dport 53 accept
    iifname "bv*" ip daddr <gateway> drop
    iifname "bv*" ip daddr @lan4 drop
    iifname "bv*" meta nfproto ipv6 drop
    oifname "bv*" ct state established,related accept
    oifname "bv*" drop                # nothing new into a job, from the LAN or from another job
  }
  chain output {                      # slot-uid sockets in the VM's own namespace (netns = false)
    type filter hook output priority 0; policy accept;
    meta skuid != @slot_uids accept
    oifname "lo" accept
    meta skuid @slot_uids meta nfproto ipv6 drop   # all IPv6: home GUA devices included
    ip daddr <resolver> meta l4proto { udp, tcp } th dport 53 accept
    ip daddr <gateway> drop
    ip daddr @lan4 drop
  }
  chain postrouting {
    type nat hook postrouting priority srcnat; policy accept;
    ip saddr 10.203.0.0/16 oifname != "bv*" masquerade
  }
}
```

`resolver` is an IPv4 literal outside the VM. The explicit DNS accept comes before the gateway and
LAN drops, so dropping the gateway (often also the VM's resolver, believed to sit in
198.18.0.0/15 on OrbStack, unverified, O4) does not break name resolution. Setting `resolver` to
the gateway is allowed and opens only port 53 on it.

IPv6 is dropped whole for jobs in both chains: a home network usually gives the owner's devices
ISP-delegated global addresses, which no ULA or link-local set covers.

Doctor lines: `fail ssh-host-mounts: /mnt/mac (virtiofs) visible; disable host file sharing`,
`fail ssh-host-mounts: /Users (virtiofs) in mount namespace of pid <p>`,
`fail ssh-host-mounts: virtio-fs device virtio<n> (tag "<tag>") present; remove the share from the VM`,
`fail ssh-host-mounts: 9p device virtio<n> present; remove the share from the VM`,
`fail ssh-seccomp: <call> returned <result>, want EPERM`, `fail ssh-lan: bitc-s1 reached
[<gua>]:443 (refused)`, `warn ssh-swap: <device> active; per-job secrets may reach disk`,
`ok|fail|warn ssh-tunnel-sshd: ...` (C.6.3),
`fail ssh-mac-bridge: /opt/orbstack-guest/bin/mac present`, `fail ssh-agent-forward: <why>`,
`fail ssh-lan: bitc-s1 reached 192.168.1.1:80 (refused)`, `fail ssh-lan-rules: table inet bitc
<missing|differs>`, `fail ssh-rosetta: binfmt_misc handler <name> runs x86-64; disable Rosetta`,
`fail ssh-docker-socket: <path>`, `fail ssh-reach-key: <path> readable by bitc-s1`,
`fail ssh-cron: <why>`, `ok ssh-arch: arm64`, `ok ssh-host-key: ssh-ed25519 SHA256:<fp>`,
`fail ssh-users: <why>`, `warn ssh-sweep: no sweep in 30 min`. The agent's own failure prints
`pod: host check failed: <first failing item>` and exits as setup class `infra`.

### C.9 Probes (on the Mac, before the live proof)

O1. Where and how OrbStack (and the chosen VM product) shares Mac files (paths, fstype, virtio
devices and `/sys/fs/virtiofs` tags), and whether a per-machine setting removes them all and keeps
them removed across updates and restarts. Includes the Rosetta share. For Lima: that the C.7 YAML
keys exist in the installed version and take effect (`mounts: []`, `rosetta.enabled: false`,
`ssh.loadDotSSHPubKeys: false`, `propagateProxyEnv: false`, the ignore-all `portForwards` rule:
a listener on guest :3000 is not reachable on the Mac's localhost), and that unmounting
`/mnt/lima-cidata` after boot breaks nothing.
O2. Which Mac-integration binaries and sockets a machine gets, and whether they can be disabled,
Rosetta and any Docker socket bridge included. Which vsock ports the product's guest agent and
host services use (for C.8.12), and that the seccomp filter makes each `EPERM`.
O3. Whether the Mac's ssh agent is forwarded into machines by default.
O4. The VM's route to the Mac (`host.orb.internal`), its resolver and gateway addresses (whether
they sit in 198.18.0.0/15), the LAN, and whether the C.8.7 ruleset blocks them.
O5. Whether the VM supports cgroup v2 delegation, `unshare` of mount, PID, IPC, UTS and network
namespaces, and tmpfs mounts.
O6. arm64 smoke of each native dep (C.4.5) for the pools that would opt in.
O7. Reverse-tunnel stability across Mac sleep and wake (decision 9).
O8. **Machine or VM:** on OrbStack, whether a machine is a container on a VM shared with the
Docker engine (`systemd-detect-virt -c`, `/proc/1/cgroup`, kernel and boot id compared with a
Docker container's, the namespaces of PID 1), and whether that VM mounts `/Users`. Decides
decision 10.
**Result (2026-10-23, read-only probe on the owner's test machine):** the OrbStack machine is an
LXC system container (`systemd-detect-virt -c` = `lxc`, `-v` = `none`) on OrbStack's shared kernel
(`7.2.9-orbstack`); it has no VM boundary of its own and no virtio-fs or 9p mounts inside. Whether
the shared VM mounts `/Users` cannot be seen from inside the machine.
O9. Per-job network namespaces: veth, NAT and the C.8.7 `forward` chain work in the chosen VM;
two concurrent jobs both bind :3000 and :5432 and cannot reach each other. Gates `slots > 1`.
O10. How to disable Rosetta in the chosen product, and that afterwards no x86-64
`binfmt_misc` handler is registered (C.8.8).

### C.10 Validation (78)

| Case | Message (`bitc: <file>: providers.ssh.` prefix) |
| --- | --- |
| `host` empty, starts with `-`, or has whitespace or `@` | `host: invalid: "<v>"` |
| `user` not `^[a-z_][a-z0-9_-]{0,31}$` | `user: invalid: "<v>"` |
| `port` outside 1–65535 | `port: out of range: <v>` |
| `host_key` missing, unparsable, or `ssh-rsa` under 3072 bits or `ssh-dss` | `host_key: <reason>` |
| `identity_file` missing, not 0600, not owned by the user, or encrypted | `identity_file: <reason>` |
| `api_key` or `api_key_file` set | `api_key: unknown key for ssh (use identity_file)` |
| `arch` not `amd64`/`arm64` | `arch: must be "amd64" or "arm64": "<v>"` |
| `slots` < 1 or > 64 | `slots: out of range: <v>` |
| `max_live` > `slots` | `max_live: <m> exceeds slots <n>` |
| a size's `slots` < 1 or > `slots` | `sizes.<k>.slots: out of range: <v>` |
| `usd_per_hour` > 0 without `daily_usd` | `daily_usd: required when usd_per_hour > 0` |
| `daily_usd` with `usd_per_hour` = 0 | `daily_usd: not applicable to an unpriced provider` |
| `proxy_jump` or `proxy_host_key` set | `proxy_jump: not supported in phase 1` |
| `resolver` missing or not an IPv4 literal | `resolver: required, an IPv4 address: "<v>"` |
| `netns = false` with `slots` > 1 | `netns: false requires slots = 1` |
| `memory_per_slot` × `slots` > 1 TiB, or < 512MiB | `memory_per_slot: out of range: <v>` |
| `send_env` invalid or in a repo file | as S.5 |
| `agent.source = "build"` without `module_dir`, or `module_dir` lacking this module's `go.mod` | `agent.module_dir: <reason>` |
| repo `arch` entry not `amd64`/`arm64`, or duplicated | `pools.<p>.remote.arch: invalid: "<v>"` (repo file) |
| tool `arch` invalid, or `(name, arch)` duplicated | `remote.tools: <name>/<arch>: <reason>` (repo file) |

### C.11 Conformance rows (new family `SH`)

The fake is `sshfake`: a test seam `[test] ssh_exec = "local"` makes the provider run the gate
script locally under a per-test root instead of calling `ssh`, and lets a test script the ssh
client's exit code and stderr. Every row except the privileged ones runs without sshd. One
privileged CI job runs a real `sshd` and `nft` in a container to cover the argv, pinning,
`restrict`, sudoers and namespace lines.

| ID | Behaviour |
| --- | --- |
| SH1 | C.10 cases are 78 with the messages shown |
| SH2 | the ssh argv is exactly C.2.2; `known_hosts` is the one `bitc-ssh <key>` line for host `127.0.0.1` port 22022; a host key mismatch is `ErrUnavailable`; no TOFU (privileged sshd) |
| SH3 | slots: lowest free, all or none, `max_live`, busy → `trying <next>`; a host-side busy slot → `ErrTransient` |
| SH4 | cost $0 by default; only the `daily_usd` comparison is skipped; a `List` error, the breaker, `launch.lock` and `max_live` still refuse; `usd_per_hour` priced ledger |
| SH5 | arch mismatch → `ErrUnavailable`; pool arch excludes → `ErrIneligible` line; tool with no arch entry → ineligible |
| SH6 | pre-resource failures (connect, `probe`, `install`, `check-host`) → `trying <next>`; a `--prepare` failure past session setup → `ErrPreAttach` (teardown by name, one retry if confirmed, next) under `attach`, 75 under `resource` |
| SH7 | isolation (privileged): a slot-1 job cannot read slot 2's or the previous job's files; a double-forked daemon is gone at end; private `/tmp`, `/var/tmp`, `/run/lock`, `/dev/shm`; a SysV segment and a POSIX queue made by job A are invisible to job B; `crontab` and `at` are denied; a planted linger file is removed and reported; a file left in a world-writable dir is deleted at teardown |
| SH8 | `check-host` fails on each of C.8 items 1–4 and 6–11 (fake mountinfo, files, binfmt entries, sockets, nft output). Item 1 also fails for: a virtiofs mount present only in a second mount namespace (a process in its own namespace, `/proc/1` clean); an unmounted virtio device whose `device` reads `0x001a`, and one reading `0x0009`; a `/sys/fs/virtiofs/0/tag` file. Each prints its `fail ssh-host-mounts` line |
| SH9 | per-job values (C.5.2), with `send_env = "forced"` and canaries for the deploy key, K, M and a `forward_env_force` value: none appears in any file under `/var/lib/bitc`, the slot dirs, `/tmp` or `/var/tmp` after the fetch (the key) or at any time (K, M, the forwarded value; M only in `agent.log`), in no argv, and in no process environment except the job's own (forwarded value only); after teardown no process holding them remains. A job with a forced forwarded name and `send_env = "forwarded"` (the default), or with any forwarded name and `send_env = "none"` → `ErrIneligible` with the S.3 note, no connect |
| SH10 | sweep: own tag and past `E` → freed; own tag, no reservation, created > `start_timeout` ago → freed; own tag whose only reservation is an `orphan` while the job's pid is alive (it moved on to Sprites) → freed at once, orphan dropped; the next ssh launch's cap check frees it the same way; own tag with a live non-orphan reservation → kept; a tag that does not verify is never touched before `E`, and after `E` only with `--foreign` |
| SH11 | gate rejects every malformed verb or argument with 64 and empty stdout; `install` rejects a sha mismatch |
| SH12 | `--run` payload written, then the connection drops (exit 255, other stderr) → teardown by name, 75, no next provider; the same with no `started` ack, and with a `refused: truncated payload` reply |
| SH13 | `--run` ssh exits 255 with `Connection refused`, `Connection timed out` or `Host key verification failed.` → pre-attach: teardown by name, then retry or `trying <next>` per B.3 |
| SH14 | slot handoff: between `--prepare` and `--run` a second controller's `--prepare` is refused (`busy`); `--run` with a stale nonce is refused; a lease past `lease_until` can be taken by a new `--prepare` |
| SH15 | `teardown <slots> <name>` on a slot whose `owner` names another job prints `gone` and kills nothing (stale cleanup after the slot was leased again) |
| SH16 | network (privileged): two concurrent jobs both bind :3000; neither reaches the other's listener, the VM's sshd or the LAN; DNS to `resolver` works; with `netns = false` and `slots = 2` it is 78 |
| SH17 | a reachability key readable by `bitc-s1` → `check-host` fails (`ssh-reach-key`) before attach, `ErrUnavailable`, pause record |
| SH18 | gate wiring (privileged): `SSH_ORIGINAL_COMMAND` reaches the gate through `bitc-login` and sudo; `SSH_AUTH_SOCK` set → 64; `bitc` with shell `nologin` → doctor `fail ssh-users` |
| SH19 | a repo with `services` or `root_steps` → `ErrIneligible` for ssh with the C.5 note |
| SH20 | the nft table removed after a VM reboot → the next job fails `check-host` before attach (`ErrUnavailable`, pause record, `trying <next>`) |
| SH21 | `--prepare` ssh exits 255 with `ssh: connect to host <h> port <p>: Connection timed out` (and with `kex_exchange_identification: read: Connection reset by peer`) → pre-resource `ErrTransient`: no teardown, no orphan, no breaker record, one retry, then `trying <next>`; the controller's `slot.<i>.lock` is free as soon as the walk moves on, so a second job in another process gets the slot while the first runs on Sprites; with `Host key verification failed.` → `ErrUnavailable`, pause record |
| SH22 | tunnel lockdown (privileged, real sshd with C.6.3): with the tunnel key, `-R 127.0.0.1:22022` works; `-R 127.0.0.1:22023`, `-L 127.0.0.1:<port>:127.0.0.1:<any>`, a `direct-tcpip` channel, `-D`, a command (`ssh ... id`), a TTY and a Unix-socket forward are all refused; `sshd -T -C user=bitc-tunnel,...` prints the C.6.3 values |
| SH23 | seccomp (privileged): a job's `socket(AF_VSOCK)` (connect to CID 2 and CID 1), `unshare(CLONE_NEWUSER)`, `clone(CLONE_NEWUSER)` and `keyctl` fail with `EPERM`, `clone3` with `ENOSYS`; a normal `fork`/`exec`, `node` and `git` work; doctor item 12 passes with the filter and fails (`ssh-seccomp`) without it |
| SH24 | LAN block, IPv6 (privileged): with `netns = false` and `slots = 1`, a slot uid's connect to a global IPv6 address on the test network times out or gets `EPERM`, and doctor item 5 runs in the VM namespace (not a job namespace); with `netns = true` the same from the job namespace |
| SH25 | firewall self-test (C.5.3, run by the setup script from inside the host as a slot user): the LAN router :80 and :443, the controller :22, the VM gateway, the VM's own addresses (the namespace host end `10.203.0.1:22`, where the VM's own sshd answers if the firewall is off), a ULA, a link-local, a 100.64/10 and a 198.18/15 sample all fail; github.com:22 and registry.npmjs.org:443 work; any other result prints `FIREWALL NOT ACTIVE` or `EGRESS BROKEN`, exits non-zero and writes a root-only marker that makes the gate refuse `slot-boot` until a later self-test passes (decision logic with injected probe results) |
| SH26 | `ssh-host-setup.sh --print <name>` equals the text the agent's `check-host` compares or the runbook shows |
| SH27 | the setup script and the `firewall-selftest` verb refuse a real run without the controller-facing LAN address, before anything changes (the controller :22 target is mandatory) |
| SH28 | the runbook covers undoing forwarding, the `--netns false` mode, forwarding with `policy accept` and verifying the agent hash; the setup script header lists the self-test paths |
| SH29 | `--allow HOST:PORT` on the setup script and `firewall-selftest` changes only which targets the self-test expects to reach (default `github.com:22`, `registry.npmjs.org:443`): the ruleset is byte-identical with and without it; a literal private, loopback, link-local, CGNAT, 198.18/15 or IPv6 ULA target is refused unless `--allow-test-targets` is also given; host names and documentation ranges are accepted |

## S. Caller env per provider [5.3, 5.4, 8.1, 8.2, 8.4, 10 edit]

Owner decision 20 (DECIDED 2026-10-23). It replaces every provider-specific rule about forwarded
or forced env. Spec 8.4's "Remote env" rules (`forward_env` ∩ `forward_env_allow`, the
secret-name list, `forward_env_force`, the 5.2 name rule) are unchanged; this section only adds
which of the resulting names may reach a given provider.

### S.1 Terms

- **Forwarded names** of a job: the names in repo `forward_env` ∩ host `forward_env_allow` that are
  set in the caller's environment (an empty value counts as set; an unset name is not sent and
  does not count).
- **Forced names**: the forwarded names that are listed in `forward_env_force` or match the 8.4
  secret-name list (the second implies the first, else 78 at load, as today).
- **Plain names**: forwarded names that are not forced.
- The job **needs** `"none"` when it has no forwarded name, `"forced"` when it has at least one
  forced name, else `"forwarded"`. The order is `none` < `forwarded` < `forced`.
- Not caller env, and so outside this key: `[defaults]`, pool, `[remote]` and
  `[pools.<p>.remote]` `env` values (they come from config files), B, K, M, the deploy key, and a
  plugin's `env_pass` (it reaches the plugin process on the host, never a resource).

### S.2 Declaration and setting

```go
type EnvLevel uint8
const (
    EnvNone      EnvLevel = iota // no caller env crosses
    EnvForwarded                 // plain names cross; forced names never do
    EnvForced                    // forced names may cross too
)
```

Host key `providers.<t>.send_env = "none" | "forwarded" | "forced"`, on every provider section,
built-in or plugin: class **host** (5.3 table row added), host-only, no env or flag touches it.

- `"none"`: no caller env crosses to this provider at all.
- `"forwarded"`: plain names cross; forced names never do.
- `"forced"`: forced names may cross too (the spec 8.4 behaviour of v3.3).

| Provider | `Caps.SendEnv` (default) | `Caps.MultiJob` | Why |
| --- | --- | --- | --- |
| `runpod` | `forced` | false | one pod per job, deleted at the end; today's behaviour, unchanged |
| `sprites` (phase 1) | `forced` | false | a fresh Sprite per job, deleted with its checkpoints at the end (sprites-provider.md decision 10; probe 9.25) |
| `ssh` | `forwarded` | false | the slot is wiped per job (C.5 step 8), but the host is the owner's own box and its root can read a running job (C.5.2, C.5.4) |
| `fake` | test-settable | test-settable | `[providers.fake] send_env_default`, `multi_job` (test only) |
| plugin | `forwarded` | `caps.multi_job` from `hello` | bitc cannot see what a third-party backend keeps |

The default is `Caps.SendEnv` for a built-in and `"forwarded"` for a plugin. The host may set any
value, wider or narrower than the default, with one exception (S.4). The effective level is the
configured value or the default.

### S.3 Enforcement and notes

Core checks the level in preflight (8.4, network-free), before the cap check, any reservation,
any create and any `launch` request. A job whose need is above the provider's effective level is
`ErrIneligible` for that provider: no resource, no reservation, no ledger line, no breaker record.
The walk moves to the next provider in the pool's list (8.2, B.3.6); at the walk's last provider,
or with `on_failure = "stop"`, the call ends 69 as for any `ErrIneligible`. bitc never strips
names to make a job fit: a job runs with all its forwarded names or not on that provider. The
`<why>` is exact, with the names sorted and comma-separated (names are not secret, values never
appear):

- need `forwarded` or `forced`, level `none`:
  `forwarded env not sent to <t> (providers.<t>.send_env = "none"): <N1>, <N2>`
- need `forced`, level `forwarded`:
  `forced env not sent to <t> (providers.<t>.send_env = "forwarded"): <F1>, <F2>` (forced names
  only)

It appears in the usual 8.2 forms, for example
`bitc: ssh: heavy: not eligible for remote: forced env not sent to ssh (providers.ssh.send_env = "forwarded"): DEPLOY_TOKEN` (the next provider is remote, F.1 rule),
`bitc: runpod: running heavy locally: <why>`, or `bitc: <t>: <pool>: not eligible for remote:
<why>` when no `local` follows.

### S.4 The one invariant: forced names never reach a reused resource

A **multi-job resource** is a provider resource that serves more than one job: its job-writable
state (disk, files, checkpoints, memory, processes) can outlive the job and be seen by a later
job. Examples: a warm Sprite (sprites-provider.md 11), a pool of kept pods, a plugin backend that
reuses machines. A provider declares this as `Caps.MultiJob` (a plugin as `caps.multi_job`,
required). **Whatever the setting, forced names never reach a multi-job resource.** Core enforces
it twice: `send_env = "forced"` on a provider with `MultiJob = true` is 78 (S.5), and preflight
caps such a provider's effective level at `"forwarded"`, so a job that needs `"forced"` is
`ErrIneligible` there. No phase 1 built-in sets `MultiJob`; an ssh slot does not count, because
teardown kills every process of the slot uid and deletes every file it owns before the next lease
(C.5 step 8, SH7). This is the only provider-independent rule about forced names; nothing else in
this delta or in sprites-provider.md adds one.

### S.5 Validation (78)

| Case | Message (`bitc: <file>: ` prefix) |
| --- | --- |
| `send_env` not `none`/`forwarded`/`forced` | `providers.<t>.send_env: must be "none", "forwarded" or "forced": "<v>"` |
| `send_env` in a repo file | `providers.<t>.send_env: host-only key` |
| `send_env = "forced"` on a provider with `Caps.MultiJob` (a plugin: checked after `hello`, at config check, doctor and first use) | `providers.<t>.send_env: "forced" is not allowed: <t> reuses resources across jobs` |

`bitc config` prints the effective level per provider with its source (`default` or `host`).

### S.6 Conformance rows (new family `SE`, fake provider unless stated)

| ID | Behaviour |
| --- | --- |
| SE1 | defaults: `runpod` and `sprites` `forced`, `ssh` `forwarded`, a plugin `forwarded`; every value settable on each; the S.5 cases are 78 with their messages (repo file, unknown value, `forced` on a `multi_job` fake and on a reference plugin declaring `multi_job: true`) |
| SE2 | `send_env = "none"`: a job with any forwarded name set → `ErrIneligible` with the exact `none` note, no create, no reservation, no ledger line; a job with none set runs, and its `[remote] env` values still cross |
| SE3 | **Forced job on a `"forwarded"` provider.** Pool `providers = ["fake-a", "fake-b", "local"]` (two fakes), `fake-a` at `"forwarded"`, `fake-b` at `"forced"`; a job forwarding one forced and one plain name is `ErrIneligible` on `fake-a` (no call to it in `/__bitc/calls`, no breaker record) with the exact `not eligible for remote: forced env not sent to fake-a (...): <F>` line (the next provider is remote, so 8.2 prints that line, not `trying`), and runs on `fake-b` with both values present; the same job with only plain names runs on `fake-a`; with `on_failure = "stop"` it ends 69 with the `not eligible for remote` line |
| SE4 | `"forwarded"` never strips: a job is never run with a subset of its forwarded names; the forced value appears in no `fake-a` call (canary, raw/hex/base64) |
| SE5 | invariant: a `multi_job` fake defaults to `"forwarded"` even when `send_env_default = forced`, a forced job is `ErrIneligible` there, and no forced canary appears in any of its calls |

## D. Summary of spec edits

| Spec | Edit |
| --- | --- |
| 1 | scope sentence (C.1) |
| 2 | rows: fallback boundary; plugin; fixed-capacity provider; ambiguous launch |
| 4 | stderr: `trying <next>`; `torn down before attach` / `left for sweep` suffix; `launch ambiguous`; `<why> after attach`; plugin lines; `running locally` narrowed to "`local` is next" |
| 5.3 / 5.4 | `providers.<t>.fallback_until` (host, narrow=boundary, default = declared for built-ins, `resource` for plugins); `providers.<t>.send_env` (host-only, default `Caps.SendEnv` for built-ins, `forwarded` for plugins, S.2); `plugin` (with `secret_files`) and core `sizes` for plugin types; ssh keys; repo `arch`; tool `arch`; agent `build`, `url` `{arch}`, `paths`; the `daily_usd` default "required" becomes "required unless `Caps.Fixed` and `usd_per_hour` = 0" |
| 8.1 | `Caps.FallbackUntil`, `Caps.Fixed`, `Caps.SendEnv`, `Caps.MultiJob`, `Info.Arch`, `ErrPreAttach`, `ErrAttached`; the error table (B.3); breaker column "after BEGIN, and each resource discarded before attach" |
| 8.2 | B.3 rules replace the "before any resource exists" / "once a resource exists" sentences; a plugin's ambiguous launch is attached (A.5.4) |
| 8.3 | the retry sentence becomes the three clauses of B.3.3.4 (a create that left nothing: once, unchanged; a pre-attach discard under `attach`: once, after a confirmed gone; under `resource`: none after a create that left a resource); reservation kept until the ledger `end`, or as an `orphan` for an unconfirmed attempt, which never shields its resource from the sweep or the next cap check; ledger `launch`/`end` per attempt name; breaker counts pre-attach discards; plugin prices = max(host, plugin); only the `daily_usd` comparison is skipped for an unpriced fixed provider |
| 8.4 | "Remote env" gains the `send_env` preflight check and its notes (S.3); agent per arch; `--boot-stdin` and `tail`/`read` as a second delivery and results path (shared with Sprites); for `ssh` no services and no root steps |
| 8.5 | A.11 text and 8.5.1–8.5.9 (A.1–A.10) |
| 8.9 (new) | `ssh` provider (C.2–C.8) |
| 10 | plugin trust (A.9: fully trusted host code, core checks catch bugs only; no setuid, setgid or file capabilities; no-new-privs); fixed-host isolation limits (C.5.4), per-job values on an ssh host (C.5.2), job seccomp filter (C.5.3); Mac VM rule and OrbStack residual risk (C.7); the reachability credential exception (C.6.2); the caller-env invariant: forced names never reach a multi-job resource (S.4) |
| 11 | protocol suite with `PL` rows, `pluginTarget`, reference plugin, `FB`, `SE` and `SH` families, `ssh_exec` seam |
| 14 | tasks below; "Later" loses the exec/JSON protocol and gains named provider instances, plugin sandbox (decision 17), ssh relay via `ProxyCommand`, user-mode services on ssh |
| 15 | 15.8 reworded (B.5); new 15.10 (plugins), 15.11 (fixed-capacity scope) once approved |

## E. Task breakdown

Rules as spec 14: one agent per task, TDD, own worktree, one reviewed PR, calendar days with
agents in parallel. Sprites tasks (sprites-provider.md 10) depend on F.1 and X.1.

| # | Task | Depends | Done when |
| --- | --- | --- | --- |
| F.1 | Boundary: `Caps.FallbackUntil`, host key and its 78s (plugin default `resource`), `ErrPreAttach`/`ErrAttached`, Walk rules B.3, the three retry clauses, per-attempt ledger and reservations, orphans and their teardown by sweep and cap check, breaker on discards, notes (`trying <next>`, suffixes); fake `fallback_until_cap`, `preattach-fails`, `/__bitc/calls` attach marking; suite rows PS1 and FB15 | approval | FB1–FB9, FB14, FB15 and PS1 green on `fake` and RunPod; all existing Q/RR/K rows green (2.5 d) |
| V.1 | Caller env per provider (S): `EnvLevel`, `Caps.SendEnv`, `Caps.MultiJob`, plugin `caps.multi_job`, host key `send_env` and its 78s, the preflight check and its notes, `bitc config` output; fake `send_env_default`, `multi_job` | approval | SE1–SE5 on `fake`, two-fake chain, and the reference plugin; SE1 on RunPod (0.5 d) |
| X.1 | Exec delivery and results path shared by Sprites and ssh: `__agent --boot-stdin` (memfd, key stripping, length-prefixed payload, truncation refusal), no-listener mode, `__agent tail`/`read`/`list`/`teardown`, per-arch agent build (GoReleaser check, `url` `{arch}`, `paths`, `source = "build"`), `Info.Arch`, repo `arch`, tool `arch` | approval | RE/RB/RS rows in both modes; arch rows of SH5 on the fake (2 d) |
| P.1 | Plugin adapter `internal/provider/plugin`: pinning (no setuid/setgid/caps) and the `__plugin-exec` shim (no-new-privs, `execveat`), spawn/env/per-process `TMPDIR`/fd-3 secrets incl. `secret_files`, per-launch redaction, framing, handshake, `configure` (endpoints, arch, `max_name` ≥ 60), the `job` wire struct, unknown-field tolerance, methods and timeouts, error table, the A.5.5 name check, ambiguous launch and reap by name, host-priced cap inputs, marker parsing in core, respawn, `caps.multi_job` from `hello` into `Caps.MultiJob` and the S.5 78 for `send_env = "forced"` on it after `hello` (V.1 added the Go fields and the preflight cap; the reference-plugin part of SE1 and SE5 lands here) | F.1 | unit tests per A.2–A.7; 78 table of A.8; FB10–FB13 (3 d) |
| P.2 | Reference plugin `bitc-provider-fakex` with fault modes (incl. `echo-launch`, `create-then-transient-no-id`), protocol suite `conformance/plugin/` (incl. PL1–PL12 and the `job-v1.json` golden file), `pluginTarget` with the `/__bitc/` fake contract (`calls` with attach marking, `live`, `torn`, `paginate-2`) | P.1 | both suites green on the reference plugin with `-race`, PS1 and FB15 included; same scenario results as `fake` (2 d) |
| P.3 | Docs: protocol reference (`docs/plugin-protocol.md`), author guide section (trust statement, pricing), spec fold-in of A | P.2 | a fresh agent builds a toy plugin from the docs alone (0.5 d) |
| H.0 | Probes O1–O10 on the Mac (maintainer present), VM choice (decision 10), runbook draft `docs/ssh-host.md` (VM incl. the C.7 Lima YAML and cidata unit, sudoers, sshd, nft, cron/at, swap off, reachability incl. the C.6.3 controller lockdown) | approval | every probe answered or "assume worst" (1.5 d + maintainer time) |
| H.1 | Gate, login wrapper and host layout: `scripts/ssh-gate.sh`, `bitc-login`, sudoers and sshd snippets, `agent check-host` (C.8 items 1–12, every mount namespace, virtio devices), `slot-boot --prepare/--run` with owner/nonce handoff and ack, teardown steps (cgroup and uid, cron/at/linger, stray files, IPC), namespaces incl. IPC and network (veth, NAT), private tmp dirs, the job seccomp filter (both arches), nft ruleset render and compare | X.1, H.0 | SH7, SH8, SH11, SH14–SH18, SH20, SH22–SH24 (privileged) (3.5 d) |
| H.2 | `ssh` provider: config and 78s, ssh argv and pinning (`HostKeyAlias`), resource and attach classification (the pre-session whitelist for `--prepare` and `--run`), slots and rails (slot lock per attempt), `Launch`/`Watch`/`Output`/`Fetch`/`Teardown`/`List`/`Owner`, eligibility (services, root steps; `send_env` default `forwarded` via V.1), `sshfake` seam | F.1, X.1, H.1, V.1 | SH1–SH6, SH9, SH12, SH13, SH19, SH21; FB15 on `ssh` (2 d) |
| H.3 | Sweep (tag-verified rule with orphans), `doctor --remote` ssh checks (C.8 lines, tunnel sshd, swap), `status` slots section, provider suite `ssh` column | H.2 | SH10; provider suite green for `ssh` (1 d) |
| H.4 | Live proof on the Mac VM (maintainer present): reachability option chosen, every C.8 check seen failing then passing, one arm64 job per opted-in pool, two concurrent jobs if O9 passed | H.3 | runbook Results filled (0.5 d + maintainer time) |

Critical path: F.1 → P.1 → P.2 → P.3 is 8 days. H.0 ∥ X.1 → H.1 → H.2 → H.3 → H.4 is about 9
days plus owner time. Both run in parallel after approval, so the delta needs **about 9.5
calendar days** plus owner time for H.0 and H.4.

## Review notes

### r4 (2026-10-23, owner decision on caller env)

- **Owner decision 20, DECIDED 2026-10-23: "generalized, host setting".** New section S: one
  host-only key `providers.<t>.send_env = "none" | "forwarded" | "forced"` on every provider,
  default from `Caps.SendEnv` (RunPod and Sprites phase 1 `forced`, ssh `forwarded`, plugins
  `forwarded`), any value settable by the host. A job that needs more is `ErrIneligible` there
  with an exact note (S.3) and moves to the next provider; it is never run with a subset.
- **Merged and removed:** r3's ssh-only `providers.ssh.forward_env` boolean (old decision 20, C.2,
  C.5.2, C.10, SH9) and the Sprites `forward_env_force` decision (sprites-provider.md decision
  10). The C.10 row now points to S.5.
- **One invariant** (S.4): forced names never reach a multi-job resource, enforced by
  `Caps.MultiJob` (78 for `"forced"`, and preflight caps the level). Plugins declare
  `caps.multi_job` in `hello` (now seven required fields).
- **New rows** SE1–SE5, including SE3 (a job needing forced names on a provider set to
  `"forwarded"` is ineligible there and runs on the next provider). New task V.1 (0.5 d, off the
  critical path); H.2 depends on it.
- **Open:** Sprites probe 9.25 (do checkpoints survive a Sprite's delete?). If they do, the
  recommended Sprites default becomes `"forwarded"`; the host can still opt in to `"forced"`.

### r3 (2026-10-23, review round 2 applied, designer)

All 17 findings were accepted. Picks and reasons where a finding offered a choice:

1. **Proof of a plugin's `attach`.** Added suite row FB15 (A.10.2), required of every `Target`
   that declares `attach`, with canaries for B, K, M, an argv sentinel, the deploy key and a
   forced forwarded value, checked in raw, hex and base64 against the fake's recorded calls before
   the call the fake marks as attach. Decision 2 no longer cites FB3–FB5. I also took the
   suggested default change: plugins default to `resource` and the host opts into `attach`
   (decision 2 (c)), because bitc cannot see that a third-party binary passed FB15.
2. **`list` omission.** A.7.1.6 now names PS1, a provider-suite row comparing `list` with the
   fake's `/__bitc/live` across pagination and after each job's end, and states that PS1 covers
   only what the fake models.
3. **`max_name` floor.** 60 (28 head + 31 tail + 1 pool character at the 10-character caps);
   below it is 78 (A.4.5, A.8, PL9). Core caps at 63 as before and never cuts `job8` or `E`.
4. **`job` wire format.** Exact protocol-1 table in A.5.1, golden file and PL10.
5. **Per-launch redaction.** A.3.3: the deploy key, B, K, M (raw and hex) and forced forwarded
   values join the redactor before the `launch` frame is written. PL8 gains `echo-launch`.
6. **Id-less launch error.** Option (a): core's own `list` + `owner` by the full name before any
   retry or fallback (A.5.5); a match is attached (teardown, one breaker `failure`, 75); a failed
   check is ambiguous. PL11. A plugin whose `list` also hides the resource is left to PS1, as
   stated.
7. **setuid plugins.** Refused at load and at spawn (A.2.3, A.8, PL12). Go's `SysProcAttr` has no
   no-new-privs field, so a re-exec shim sets `PR_SET_NO_NEW_PRIVS` and calls `execveat`.
8. **Orphans vs sweep.** B.3.3.3: an orphan never shields its resource; the sweep and the next
   cap check tear it down at once and drop the orphan on a confirmed teardown. FB7, SH10 and the
   Sprites sweep table (2.4, SP14) updated; worked example 2a.
9. **Retry clauses.** B.3.3.4 now has three explicit clauses; the 8.1 table cell and D say which
   applies; FB9 pins RunPod's 429 retry.
10. **Sprites 5xx row.** Fixed in sprites-provider.md 1.2 (split into before-create, create, and
    after-create rows).
11. **ssh `--prepare` pre-session failures.** Pre-resource (C.5.1.2), no orphan, no breaker
    record; the controller slot lock is per attempt (C.3.1). SH21, example 11.
12. **Tunnel lockdown.** C.6.3: `Match User bitc-tunnel` block, `command="/bin/false"` in the key
    line, nologin shell, `sshd -T` check. SH22.
13. **vsock, user namespaces, keyrings.** Seccomp filter (C.5.3, decision 21), doctor item 12,
    SH23. I chose seccomp over `kernel.apparmor_restrict_unprivileged_userns` because it does not
    depend on the VM's LSM and also covers vsock and keyctl in one place. The keyring limit is
    removed from C.5.4.
14. **Mount scan.** Every mount namespace, plus virtio-fs/9p devices and `/sys/fs/virtiofs`
    tags (C.8.1). SH8 extended; three new `fail ssh-host-mounts` lines.
15. **Lima.** Exact YAML in C.7 (also `loadDotSSHPubKeys: false`, `propagateProxyEnv: false`,
    containerd off). No exception to C.8.2: a unit unmounts cidata after boot. O1 checks the keys
    and the cidata unmount. I did not detach the cidata device, because a virtio disk has no
    guest-side delete; it is root-only and, with the YAML, holds nothing secret.
16. **IPv6.** The output chain drops all slot-uid IPv6 (`lan6` set removed). Doctor item 5 runs
    in the configured mode and adds global IPv6 targets. SH24.
17. **Per-job values on ssh.** C.5.2 table (what, where, destroyed when); decision 20 makes
    forwarded env opt-in (`providers.ssh.forward_env`; replaced in r4 by `send_env`, S); SH9
    extended; "the one stated exception"
    now says "the one long-lived secret". Swap is a doctor `warn`.

Estimates: about 9.5 calendar days (F.1 +0.5, P.1 +0.5, P.2 +0.5, H.0 +0.5, H.1 +0.5).

Still open: the OpenSSH stderr forms (C.5.1) against the CI and controller versions; the Lima
key names and cidata behaviour (O1); the vsock ports per product (O2); `sshd -T` needs root, so a
non-root `doctor` can only warn for C.6.3.

### r2 (2026-10-23, review round 1 applied, designer)

All 23 findings were accepted. Where a finding offered a choice, the pick and its reason:

- **Ambiguous plugin launch (findings 1 and 8).** One rule, A.5.4: any `launch` outcome that
  is not a definite result or classified error is read as attached under both boundaries. It is
  reaped by name and exits 75, with no retry and no fallback. Finding 8 allowed spec 8.2's
  "lookup found nothing → fall back" under `resource`. I chose the stricter finding 1 instead,
  because a plugin's `list` is plugin-reported (finding 3), so "not found" proves nothing. The
  only post-request fallback paths are a classified error without `id`, and an explicit
  `pre_attach` under `attach`. Rows FB10–FB13.
- **Trust (finding 2).** A.1.3, A.5.2, A.7, A.9, decision 5 and the summary now say that core
  checks catch a buggy plugin and that a hostile one defeats them. A sandbox is decision 17
  (Later).
- **Money (finding 3).** A host price per size is required for plugin types, and core uses max(host,
  plugin) everywhere. Out-of-range values and foreign names count as over the cap. Rows PL3–PL6,
  decision 19. An omitted resource after its job ended remains undetectable; this is stated in
  A.7.1.6.
- **Handshake order (finding 4).** `endpoints`, `arch` and `max_name` moved to the `configure`
  result. `caps` stay in `hello`, because the `fallback_until` and `secret_files` 78s need them.
- **TMPDIR (finding 5).** Per process, with dead siblings pruned. Rail-like shared plugin state
  goes in the fixed accounting dir (`BITC_PLUGIN_SHARED`).
- **Unknown fields (finding 6).** They are ignored everywhere. Row PL1.
- **Extra secrets (finding 7).** `plugin.secret_files` on fd 3, plus `hello` `secrets` names that
  core refuses inline. Non-core keys are never secrets. Row PL8.
- **ssh boundary (finding 9).** C.5.1: the resource point is the `--prepare` request, and the
  attach point is the `--run` request. Pre-attach is limited to an exact list of OpenSSH
  pre-session errors, the `--run` stderr is kept off the channel, and an ack line and truncation
  refusal are added. I kept the system OpenSSH client rather than `x/crypto/ssh`. The
  stderr-form whitelist is the price, and SH13 pins it against a real sshd.
- **Retry safety (finding 10).** B.3.3: retry only after a confirmed gone. Ledger and reservation
  are per attempt, and an unconfirmed reservation is orphaned until `E`. The 8.3 edit is stated.
  Sprites 5.2.3 and SP16 are aligned.
- **Sweep and teardown (finding 11).** C.3.6 now uses the Sprites rule, and teardown checks
  `owner.name`. Rows SH10 and SH15.
- **Cap wording (finding 12).** C.3.3 and the 5.4 edit in D.
- **Breaker (finding 13).** Decision 16: each discard is a `failure`. Row FB14.
- **Gate wiring (finding 14).** A `bitc-login` wrapper passes the command as one argument, so
  sudo needs no `env_keep`. `!use_pty` keeps the payload binary-safe, and `bitc` has `/bin/sh`.
  The agent-socket check moved to the wrapper and to `sshd -T`. Row SH18.
- **Slot handoff (finding 15).** The `owner` file with a nonce and `lease_until` is the lease, and
  the flock is only a per-verb mutex. Row SH14.
- **Cross-job residue (finding 16).** `/var/tmp`, `/run/lock`, IPC and the mqueue namespace are
  covered. Cron, `at` and linger are denied and cleaned, and stray files are found by uid. Keyrings
  and disk filling are listed as limits. Rows SH7 and SH8.
- **Network (finding 17).** Decision 18: a per-job network namespace with veth and NAT. `slots =
  1` stays the Mac default until O9 passes, and `netns = false` forces `slots = 1`. Row SH16.
- **Services (finding 18).** No services on ssh in phase 1 (ineligible, exact note), and
  pgvector jobs stay on Sprites or RunPod. User-mode services are Later.
- **LAN block (finding 19).** An exact nft ruleset with drop (not reject), CGNAT, 198.18/15, IPv6
  ULA and link-local, the gateway and the DNS exception. It is checked at every job start (C.8.7).
  Row SH20.
- **Reachability credential (finding 20).** C.6.2 makes it root-only, and C.8.10 checks that.
  Row SH17.
- **`proxy_jump` (finding 21).** Dropped from phase 1. `HostKeyAlias=bitc-ssh` removes the
  `[host]:port` form and the 127.0.0.1 collisions.
- **OrbStack (finding 22).** Probe O8 added. Decision 10 now recommends a dedicated full VM, with
  OrbStack only on a separate VM boundary or the owner's written acceptance. The risk is stated
  in C.5, C.7 and the spec 10 edit.
- **Rosetta and Docker sockets (finding 23).** C.8.8 and C.8.9, probe O10, and Rosetta-off
  required in C.4.5 and C.7.

Estimates grew by 1 day (F.1 +0.5, P.1 +0.5, H.0 +0.5, H.1 +1, overlapping): about 9 calendar
days.

Still open for a second round: the OpenSSH stderr forms (C.5.1) must be confirmed against the
OpenSSH version in CI and on the controller; 198.18.0.0/15 and the OrbStack paths are unverified
(O4, O8); and the nft ruleset is untested until O9.

### r1 (designer)

Choices a reviewer should challenge (still valid unless r2 says otherwise):

- **fd-exec pinning and "ELF only"** (A.2). This rules out shebang plugins on purpose. The
  alternative (pin the interpreter too) doubles the pin surface for little gain.
- **Markers parsed in core** (A.5.2). This forces plugins to forward raw logs. It catches a
  buggy plugin, not a hostile one (r2). A plugin that cannot see raw logs (LiveLogs false)
  still works through `output` after the end.
- **`pre_attach` must be explicit** (A.6). A plugin cannot accidentally fall back after a
  create, because any error with an id, and any ambiguous launch, is read as attached.
- **One pre-attach retry** (B.3.3) generalises the 8.3 create retry. It also removes the Sprites
  r2 `Caps.OwnRetry` special case.
- **ssh via system OpenSSH**, not `x/crypto/ssh`. Spec 13 keeps the dependency list short, and
  OpenSSH's host-key pinning is battle-tested. The cost is one argv contract (C.2.2) and the
  stderr classification (C.5.1), both tested with a real sshd.
- **No waiting for ssh slots.** A busy fixed host is a fallback, never a queue, so the walk order
  expresses preference and `local` admission stays the only queue (spec 6).
- **The C.8 path list is best effort** until O1–O3 answer. The fstype rule (C.8.1), the ruleset
  comparison (C.8.7) and the LAN-connect proof (C.8.5) do not depend on OrbStack's paths.
