# Design: Fly Sprites provider, phase 1 (spec delta)

Status: **draft r6, 2026-10-23, for review.** r6 applies the caller-env decision
(providers delta S, `send_env`). r5 follows the providers delta r3 (retry
clauses, orphan reservations, FB15). r4 followed the providers delta r2 (retry and breaker
rules). r3 aligned r2 with `docs/design/providers-delta.md` (the "providers delta") and
scoped it to **Sprites phase 1: a fresh Sprite per job**. Warm reuse (r2's warm pool, baseline checkpoints and scrub) moves to 11,
"Later". Phase 1 uses the generic fallback boundary `fallback_until = "attach"` (providers delta B)
instead of a Sprites-only amendment to 15.8. It is a delta against `docs/spec.md` v3.3 plus the
providers delta. Once both are approved it folds in as spec 8.8 (spec v3.4). r2 is in git history
(commit `503a6ee`) for the full warm-reuse text.

Direction (2026-10-23): Sprites is the **primary** remote provider, RunPod second, `local`
last. Facts about Sprites come from the 2026-10-23 research fact sheet (docs, OpenAPI 0.1.12,
`superfly/sprites-go` v0.2.2). Items marked **(unverified)** are probes in 9, run before the live
proof.

Terms:

- **Sprite**: a persistent Fly microVM. In phase 1, one Sprite serves one job and is then deleted.
- **Provisioning**: everything bitc does to a new Sprite before the job: create, policies, agent
  delivery, `packages` and `tools`. It carries no job input (providers delta B.1).
- **Attach**: the agent exec that carries the boot payload (1.3). It is the fallback boundary.

## 0. Summary and decisions

**Summary.** A built-in `sprites` provider (`internal/provider/sprites`) runs each remote job in a
new Sprite in a separate Fly org, then deletes it.

- **Lifecycle.** create → network and resources policies (set, then verified by `GET`) → agent and
  provisioning by exec → **attach** (the agent exec with the boot payload on stdin) → job →
  results read over exec → delete. No checkpoint, no reuse, no scrub.
- **Fallback.** `Caps.FallbackUntil = attach`. An infra failure during provisioning deletes the
  Sprite and retries once on a fresh one, but only if the delete is confirmed (204 or 404). Then
  it moves to RunPod (providers delta B.3). Each discarded Sprite is one breaker `failure`, so a
  broken mirror pauses Sprites instead of burning the day's cap. After attach, nothing falls
  back: 75, or 69 for the repo's own setup failure.
- **Transport.** The Sprites exec API (WebSocket). Boot payload, spec, results MAC key and deploy
  key go in on exec stdin, never in a URL, an env variable, the Sprite's `environment` or a file
  on its disk. Results come back through `bitc __agent tail` and `bitc __agent read` (the shared
  exec results path, providers delta task X.1). There is no results server and no bearer.
- **Reuse of shared code.** The agent, markers, result MAC, artifact validation and every rail of
  8.3 are used unchanged. Phase 1 needs no Sprites-specific core hook: r2's `Selector`,
  `Releaser`, `Caps.Reuse`, `Caps.OwnRetry` and the state-aware cap are all gone.
- **No billing API.** The cap relies on bitc's own estimates (`Caps.Spend = false`), so
  `daily_usd` is a per-host share.
- **Caller env.** Generic `providers.sprites.send_env` (providers delta S), default `"forced"`
  in phase 1 because the Sprite dies with the job (decision 10, probe 9.25).
- **No self-delete.** No token enters the Sprite (`Caps.SelfDelete = false`), and the sweep timer
  is mandatory. A leak costs little once the agent exits at `D`, because an idle Sprite stops
  billing compute after about 30 s (2.3).

**Decisions needed** (each with a recommendation):

1. **Phase 1 = fresh Sprite per job.** **Rec: approve** (direction). Warm reuse (11) comes
   later and is gated on the phase 1 live proof plus probes 9.1, 9.4, 9.14, 9.17–9.21.
2. **Boundary.** **Rec: `fallback_until = "attach"`** (the default from `Caps`). With
   `"resource"`, a flaky apt mirror during provisioning turns every Sprites job into a 75.
3. **Deploy key delivery.**
   - (a) The host holds the repo's read-only key in a 0600 `deploy_key_file` (a new repo-grant
     key) and sends it on exec stdin. The agent keeps it on a private tmpfs and shreds it before
     repo code runs.
   - (b) A Connector (unverified for git, 9.9).
   - (c) The `upload` transport: a host-made `git bundle` over `fs/write`, so no key ever enters
     the Sprite.
   - **Rec: (a) now, (c) later** (spec 14 "Later").
   - **Contingency:** (a) is void, and (c) or (d) ships first, if probe 9.5 shows that a root tmpfs
     is not allowed, or live-proof step 3a finds the key, or SSH to `github.com:22` fails under the
     DNS policy (9.3). (d) is a fine-grained, read-only, single-repo GitHub token over HTTPS, sent
     on stdin under the same tmpfs rules.
4. **Egress.** Two phases. **Rec: yes.**
   - The **build** policy allows `egress_allow`, `egress_build_allow` and the agent URL host
     only. A tool whose host is not in it is `ErrIneligible`. A repo file can never widen egress
     (spec 10), so tool hosts are never added automatically.
   - The **job** policy, applied before attach, allows `egress_allow` only (GitHub and npm).
   - Both are explicit lists, never `include: defaults`, never `*`.
   - The docs say the default is unrestricted, so bitc sets a policy on every Sprite before its
     first exec.
5. **Cost model.**
   - The estimate is an upper bound on allocation: 8 vCPU (fixed on Sprites), the memory bound
     and the disk bound.
   - There is no Spend API, so a maintainer reconciles against the Fly invoice by hand.
   - **Rec: `daily_usd = 10` for `sprites`, as a per-host share** (5.2), separate from RunPod's
     25. The worst combined day on one host is $35.
   - Fixed plan fees are outside the cap. The cap tracks marginal use only.
6. **Memory per size.** **Rec: measure the app repo's type check peak RSS locally**
   (`/usr/bin/time -v`) before choosing a size.
   - Set `memory` to at least 1.5 × the peak. Use 16 GiB if the peak exceeds about 5 GiB.
   - **Price at the 16 GiB bound ($0.7571/h) until probe 9.6 proves that `limit_mb` is
     enforced.**
   - An OOM-killed type check is a command failure, not a fallback, so it must not happen
     routinely.
7. **Client library.** **Rec: a small client of our own** (`net/http` plus
   `github.com/gorilla/websocket` v1.5.3), not `sprites-go`. The SDK is pre-1.0 and pulls in
   Prometheus and protobuf, and spec 13 keeps the dependency list short.
8. **Sweep timer.** **Rec: mandatory for `sprites`.** `doctor --remote` reports `fail` (not
   `warn`) when no sweep has run in the last 30 min, because there is no self-delete.
9. **Org guard.** **Rec: a required key `org = "sandbox-org"`**, checked three ways:
   - **At load:** if the token's org slug (the first `/` field of the token) differs, load fails
     with 78.
   - **At runtime:** `organization` in every Sprite response must equal `org` (6.1).
   - **In `doctor`:** the org must hold no Connector and no Sprite outside `name_prefix`.

   Never use a token for a production, staging or preview org.
10. **Caller env on Sprites.** **DECIDED, 2026-10-23** ("generalized, host
    setting"): no Sprites-specific rule. Sprites uses the generic host key
    `providers.sprites.send_env = "none" | "forwarded" | "forced"` (providers delta S). Phase 1
    declares `Caps.SendEnv = forced` and `Caps.MultiJob = false`, because the Sprite and every
    checkpoint of it are deleted at the end of the job and forwarded values travel only in the
    stdin boot payload. The host may set any value; a job that needs more than the setting is
    `ErrIneligible` for Sprites and moves to the next provider (RunPod in 7). **Open: probe
    9.25.** If checkpoints survive the Sprite's delete, the recommended default becomes
    `"forwarded"` (the host can still opt in to `"forced"`). Warm reuse (11) sets `MultiJob`,
    which rules out `"forced"` by the providers delta's one invariant (S.4).
11. **Root steps.** A fresh Sprite holds nothing of another job. **Rec: allow `root_steps`** on
    Sprites as on RunPod (the grant still comes only from the host).
12. **Job sandbox in phase 1.** r2's namespace sandbox (11) protected the *next* job on a reused
    Sprite. **Rec: not in phase 1**, provided probe 9.23 shows that `/.sprite/api.sock` reaches
    only its own Sprite. If it reaches other Sprites or the org, the sandbox's tmpfs over
    `/.sprite` and `NO_NEW_PRIVS` become phase 1 work (phase 1, +0.5 d).
13. **Leak when the host dies.** With no self-delete, a Sprite that a dead host leaves behind
    bills compute only until the agent exits at `D` and the Sprite idles (about 30 s), then storage
    until the sweep deletes it. **Rec: accept,** relying on the sweep timer, `doctor --remote`
    alerts and the reconciliation. A second sweep host (r2 decision 13) is optional.

## 1. Provider ordering and fallback

### 1.1 Walk

1. Host `pools.<p>.providers = ["sprites", "runpod", "local"]`. The walk, removals and
   silent-skip rules are spec 8.2. A pool with no `remote_limits.size.sprites` skips Sprites
   silently.
2. Notes, retry and the boundary are the providers delta B.3. The next provider gets
   `bitc: sprites: <pool>: trying runpod: <why>`, and `local` gets the unchanged
   `running <pool> locally` line.
3. **After attach, nothing falls back.** Any failure deletes the Sprite and exits 75 with
   `STARVED <pool> (sprites: <why>)`. The one exception is a `repo` setup failure, which exits 69
   (15.9).

### 1.2 Error classes

| Signal | Class | Before attach | After attach |
| --- | --- | --- | --- |
| HTTP 429, `sprite_creation_rate_limited` | `ErrTransient` (capacity; nothing created) | retry once (providers delta B.3.3.4 clause 1), then next provider | 75 |
| `concurrent_sprite_limit_exceeded` (any status), "max sprites per org exceeded"; `running ≥ running_limit` before a create (unverified, 9.8) | `ErrTransient` (quota; nothing created) | retry once (clause 1), then next provider | 75 |
| 5xx, network or timeout on a request made **before** `POST /v1/sprites` was sent (no Sprite exists for this attempt) | `ErrTransient` | retry once (clause 1), then next provider | — |
| 5xx, network or timeout on `POST /v1/sprites` itself | ambiguous create (1.3.3): `ErrPreAttach{ID: name}` | look up and delete; retry once only if the delete (or a lookup that found nothing) is confirmed; else `left for sweep`, orphan, no retry; then next provider | — |
| 5xx, network or timeout on a policy, `GET` or provisioning call after the 201, or a provisioning stream that ended without `complete` | `ErrPreAttach{ErrTransient}` | delete; retry once only if the delete is confirmed (clause 2); else `left for sweep`, orphan; then next provider; breaker `failure` | — |
| 5xx, network, a stream that ended without `complete`, after attach | — | — | 75, breaker failure after BEGIN |
| WSS exec upgrade for the attach: HTTP error response before `101` | `ErrPreAttach` (transient) | delete; retry once if the delete is confirmed; then next provider; breaker `failure` | — |
| WSS exec upgrade for the attach: network error or timeout after the request was sent | attached (1.3) | — | 75 |
| cap, `max_live`, launch-lock wait | `ErrTransient` | next provider | — |
| 401; 403 (incl. `Your account is restricted`) | `ErrUnavailable` (one `pause` record) | next provider | 75 |
| `organization` in a Sprite response ≠ `org` (6.1) | `ErrUnavailable` (`pause` record); Sprite deleted at once | next provider only if before attach | 75 |
| no credentials (`api_key` / `api_key_file`) | `ErrUnavailable{Local}` | next provider; 69 if it was the last | — |
| size key not in `sizes`; boot payload over 8 MiB; a tool host not in the build policy; a private repo with no `deploy_key_file`; pool `arch` excludes `amd64` | `ErrIneligible` | next provider | — |
| other 400, 413 | `ErrIneligible` (logged at debug, never with payload) | next provider | 75 |
| provisioning `infra` failure (package install, download, agent fetch) | `ErrPreAttach{ErrTransient}` | delete; retry once if the delete is confirmed; then next provider; breaker `failure` | — |
| provisioning `repo` failure (a tool pin mismatch, a package the distro lacks) | `ErrSetup` `repo` | 69, Sprite deleted (providers delta B.3.4) | — |

Capacity and quota are **transient**, never `ErrUnavailable`. They pass to RunPod and write no
pause and no breaker record (8.3: pre-launch capacity errors never count). Only a Sprite that
was created and then discarded counts (providers delta B.3.3.5). `ErrUnavailable` is
kept for account and credential faults, because retrying cannot fix those within the breaker
window. The pay-as-you-go creation limit is 10 Sprites per minute, so `max_live` and the one retry
keep a host well inside it.

### 1.3 The attach point

1. **Attach is the agent exec (J4).** It happens when the WSS exec request for
   `__agent --boot-stdin` has been sent and any response other than an HTTP error status follows:
   a `101`, a network error or a timeout. Exec commands keep running after the client disconnects,
   so a half-finished upgrade may already have started the agent, and it counts as attached. An
   HTTP error status (4xx/5xx without `101`) is pre-attach.
2. **Everything before J4 is job-neutral:** the create, the policies and their `GET`s, the stub
   and agent delivery, and `__agent provision` (packages and tools). None of it carries the spec,
   the code ref, the deploy key or forwarded env. A failure returns
   `ErrPreAttach{ID: <name>, Err}`, and the core deletes the Sprite and applies the providers
   delta B.3.
3. **Ambiguous create** (a 5xx or a timeout after `POST /v1/sprites`) is `ErrPreAttach` with the
   name as id. The core looks the name up and deletes it. If the lookup or the delete fails, the
   Sprite is unconfirmed: it is left for the sweep, **not retried**, and its reservation becomes
   an `orphan`, counted until a later teardown confirms the Sprite gone or its `E` passes; the
   job goes straight to `on_failure` (providers delta B.3.3). The orphan never shields the Sprite:
   the next sweep, or the cap check of the next Sprites launch, deletes it at once (2.4). The name carries
   a verifiable owner tag (2.1), so the sweep can always prove ownership.
   The core's generic `reapByName` runs on the Sprite name, because phase 1 names follow one rule
   (2.1).
4. `Launch` returns a `Handle` only after attach. Any later failure, inside `Launch` or after it,
   is post-attach.
5. If the host narrows `fallback_until` to `"resource"`, everything after `POST /v1/sprites`
   returned 201 is post-resource and exits 75 (providers delta B.3.2).

## 2. Lifecycle: one Sprite per job

### 2.1 Name and owner tag

- **Name** (≤ 63 characters (unverified, 9.13), `[a-z0-9-]`):
  `<name_prefix>-<install16>-<size>-<job8>-<E unix>-<tag8>`. With `name_prefix = "bitc"` and a
  four-character size, that is 55 characters.
  - `job8` is the first 8 hex of sha256(job), as `ResourceName`.
  - `E` is the expiry (8.3), so the sweep can read it from the name.
  - `tag8` is the first 8 hex of `HMAC(install_secret, <name without "-tag8">)`.
  - The pool is not in the name (no room). The ledger and `status` carry it.
- **Owner proof is in the name.** `Owner(id)` returns `tag8`, and the provider's owner check
  recomputes it. Spec 10's rule "teardown only with a verified owner tag" never depends on labels
  (9.13).
- **Labels**, when supported: `bitc`. They are secondary.
- `List` filters with `?prefix=<name_prefix>-`, since the API cannot filter by label.

### 2.2 Local state

None beyond the spec's: the reservation, keyed by the Sprite name, and the ledger. No pool record
and no lease lock.

### 2.3 Idle, suspend and the keep-alive

- **bitc never suspends or resumes a Sprite**; there is no API for it. A Sprite drops to *warm*
  about 30 s after its last activity, and later to *cold*. Neither state is billed for compute.
  Any request wakes it: 100–500 ms from warm, 1–2 s from cold.
- **During a job** the agent (root) holds a Tasks-API task: `POST /v1/tasks` on
  `/.sprite/api.sock`, with `expire = 5m`, renewed every 60 s and deleted at the end. Without it, a
  quiet command (a type check that prints nothing) would let the Sprite pause and drop the job. If
  the task cannot be created, that is an `infra` setup failure (post-attach, 75). If a renewal
  fails, the agent prints `pod: keep-alive renewal failed` and keeps going.
- **End of job.** At `D`, or right after it writes `result.json`, the agent kills the job's
  process tree, deletes its Task and exits. So nothing keeps the Sprite running after the job,
  even if the host never deletes it.

### 2.4 Sweep [8.3 "Leak control" edit]

`bitc sweep --provider sprites` runs from the timer every 15 min (decision 8). It lists Sprites
with `?prefix=<name_prefix>-`.

| Sprite | Action |
| --- | --- |
| own (`tag8` verifies), past `E` from its name | delete (`swept sprite <name> (past its expiry)`) |
| own, not past `E`, its only reservation on this host is an `orphan` (a pre-attach discard whose delete was not confirmed; the job's pid may well be alive, running on RunPod) | delete at once (`swept sprite <name> (orphaned by a pre-attach discard)`); a 204 or 404 drops the orphan |
| own, not past `E`, no reservation of that name on this host, and created more than `start_timeout` ago | delete (`swept sprite <name> (job process gone)`) |
| own, not past `E`, a non-orphan reservation whose pid is alive | kept |
| other install, `--foreign`, past `E` | delete after listing |
| other install, no `--foreign` | kept; counted (5.2); named by `status` and `doctor --remote` |

- The cap check of every Sprites launch (under `launch.lock`) applies the orphan row too, so a
  leaked pre-attach Sprite is deleted by the next job, not only by the timer.
- Deleting a Sprite also deletes all of its checkpoints, the platform's auto checkpoints included.
- Teardown is always `DELETE /v1/sprites/{name}` (bounded at 10 s; 404 means done).

## 3. Provisioning (before attach)

Provisioning runs on every new Sprite, before any job input, as root through exec:

1. **Stub and agent.** The stub fetches the agent for `linux/amd64` (Sprites arch, unverified,
   9.24) from the `agent` source, checks `BITC_AGENT_SHA256`, and installs it at
   `/opt/bitc/bin/bitc` (root `0755`).
2. **`bitc __agent provision`** installs the repo's `[remote] packages` (from the distro) and
   `tools` (sha256-pinned, into `/opt/bitc/tools/<name>/`) exactly as in 8.4 step 4. It creates
   the job user `bitcjob` (no sudo, no password) and `/var/lib/bitc` (root `0700`). Its output is
   printed as `pod:` lines. Failure classes are 8.4's, so `repo` gives 69 and `infra` gives
   `ErrPreAttach`.
3. **Job policy.** Replace the build egress policy with the job policy (6.6) and verify it with
   `GET`.

At attach the agent skips `InstallPackages` and `installTool`, because a provision marker
`/var/lib/bitc/provisioned.json` (root `0600`: `{agent_sha256, packages, tools}`) matches the
spec. It re-hashes each installed tool against its pin, and a mismatch is `infra` (post-attach,
75). `setup`, `services` and root steps run after the code fetch, as in 8.4.

## 4. Job flow on Sprites (mapped onto 8.4)

| 8.4 step | RunPod | Sprites phase 1 |
| --- | --- | --- |
| Preflight | as spec | same, plus: size in `sizes`; `deploy_key_file` present (private clone URL); every tool host inside the build policy; boot payload ≤ 8 MiB; pool `arch` allows `amd64` |
| Code transport | git-push ref, deploy key from a RunPod secret | git-push ref unchanged; the deploy key read from the host's `deploy_key_file` and sent on exec stdin |
| Delivery | stub in the container cmd | stub by exec during provisioning (3); the agent's sha is checked again at J4 |
| Boot | spec and secrets from PID 1 env; listener on the port | `bitc __agent --boot-stdin`: one length-prefixed JSON boot payload on stdin, then stdin closed, then the memfd re-exec (fd 3). The agent parses the deploy key out and rewrites the memfd without it before the re-exec. **No listener** (`spec.Port = -1`); fd 4 unused |
| Deadline | `BITC_DEADLINE` from the create env | `deadline` in the boot payload (never disk). At `D` the agent kills the job, writes the result, deletes its Task and exits. **No self-delete hook** |
| Setup as root | as spec | same. The key is written only to a fresh tmpfs mounted by the agent at `/run/bitc-key` (mode `0700`); a failed mount is `infra`. Then shred, unmount and remove `~/.ssh`, all before repo code. The agent zeroes its key buffer after `ssh` exits (best effort in Go) |
| Job user | as spec | same (`bitcjob`, no sudo) |
| Results | `/run/bitc/<job>/` | same path and files, MAC as spec, plus `agent.log` (the agent's own stdout), so the host can resume the setup stream by offset |
| Stay alive | until torn down | **exits** after `result.json`. The host reads results by exec, which works on an idle Sprite (it wakes it). The `ran` mark and a second BOOT keep their meaning |
| Results server | HTTPS proxy plus bearer B | **none.** The host streams by exec (J5). Authorization is the org token on the API call. B is zero-valued and omitted |
| Artifacts | HTTPS only | over exec (J7). **[8.4 edit]**: "HTTPS only" becomes "over the provider's authenticated transport" |

**Host steps** (`Launch`, `Watch`, `Output`, `Fetch`, cleanup):

1. **J1 Rails** (8.3 unchanged): breaker, cap and `max_live` under `launch.lock`, then a
   reservation keyed by the Sprite name.
2. **J2 Provision** (pre-attach):
   1. `POST /v1/sprites {name, labels}`; check that `organization = org`.
   2. `POST .../policy/network` with the **build** policy, then `GET` it to verify.
   3. `POST .../policy/resources {memory: {limit_mb, autoscale: false}}` from the size; `GET` it to
      verify.
   4. Exec the stub, then `__agent provision` (3).
   5. Apply and verify the job policy.

   Nothing is exec'd before the policy `GET`s match. `j.Created` = the create's 201, for the
   ledger.
3. **J4 Start (attach).**
   1. Open `WSS /v1/sprites/{name}/exec` with
      `cmd = sh -c 'echo "<agent_sha>  /opt/bitc/bin/bitc" | sha256sum -c - >/dev/null && exec
      sudo -n /opt/bitc/bin/bitc __agent --boot-stdin'`. Omit `sudo` if exec runs as root (9.15).
      The sha is not a secret.
   2. Set `max_run_after_disconnect = D + 10m − now`.
   3. Send the boot payload as `0x00` frames, then the `0x04` EOF. The payload holds: spec,
      `deadline`, deploy key, provider type and Sprite name.
   4. **The `env` and `cmd` query parameters carry no secret.**
4. **J5 Watch.**
   - Open one long-lived root exec, `__agent tail <job> <off_agent> <off_out> <off_err>`. It
     streams frames tagged by file and offset from `agent.log`, `out.log` and `err.log`, plus a
     heartbeat every 15 s that carries `alive` (the agent pidfile plus the `/proc` start time).
   - Lines from `agent.log` become `pod:` output until BEGIN. Markers are parsed with nonce M.
   - After BEGIN, `out.log` and `err.log` become the job's streams, as with RunPod.
   - A dropped tail is not an error. Reopen it from the last offsets. If two reopens fail within
     30 s, poll with `__agent read` every 5 s until a reopen succeeds.
   - Every 60 s, and after any drop: `GET` the Sprite. A 404 means the job is lost.
   - `alive = false` with no `result.json` means the job is lost.
   - The start timeout, `D`, a second BOOT and LOST behave as in the 8.4 host side.
5. **J6 Complete.** `Output` reads `result.json`. The host verifies the MAC under K and the exit
   against `__BITC_EXIT__`, and completes both streams by offset (`remote.complete`, unchanged).
6. **J7 Artifacts.** `Fetch` = `__agent read <job> artifacts.tar.gz <off>`. It resumes by offset
   and is capped by `max_compressed`. Validation, staging and swap-in run as in 8.4, unchanged.
7. **J8 Cleanup** (8.3 order, unchanged): delete the Sprite (10 s bound), reap by name if a create
   was interrupted, remove staging, ledger `end`, drop the reservation, release the launch lock,
   hand off the ref deletion, remove temp files, and on a signal exit `128+N`.

**`__agent read <job> <file> <off>` and `__agent tail`** validate their arguments:

- `<job>` must match the job-id grammar;
- `<file>` must be one of `agent.log`, `out.log`, `err.log`, `result.json`, `artifacts.tar.gz`,
  `state.json`;
- every open uses `O_NOFOLLOW`;
- anything else exits 64 with nothing on stdout.

These exec commands also go through the J4 sha check.

**[8.1 edit] interface deltas:** none beyond the providers delta (`Caps.FallbackUntil`,
`ErrPreAttach`, `ErrAttached`, `Info.Arch`). Sprites declares
`Caps{LiveLogs: true, Artifacts: true, Spend: false, SelfDelete: false, FallbackUntil:
UntilAttach, SendEnv: EnvForced, MultiJob: false}`, `Info().Arch = ["amd64"]` and
`Info().Endpoints = [api_base]`, which also covers WSS on the same host. `Handle.ResultsURL` is empty. `JobSpec.DeployKey` is not added: the key is
read from `deploy_key_file` inside `Launch` and joins the redactor, so it never sits in the spec
struct the host logs.

## 5. Cost accounting (shared daily cap)

### 5.1 Price per hour of a size

```
GB(x)        = bytes(x) / 1e9                                  # GiB → GB, rounded up to 0.01
mem          = max(size.memory, memory_bound)                  # memory_bound default 16GiB (9.6)
usd_per_hour = 8 × rates.cpu + GB(mem) × rates.memory + GB(disk_bound) × rates.hot_storage
```

- **CPU.** Sprites have 8 vCPU, and users cannot change it, so CPU is always priced at 8. A `vcpu`
  value other than 8 in `sizes` is a 78 until probe 9.6 shows that `config.cpus` is honoured.
- **Memory.** The size's memory is set as the Resources Policy `limit_mb` with `autoscale = false`.
  It is an **assumed** bound (9.6). Until the probe proves that the limit is enforced, memory is
  priced at `memory_bound`, which defaults to 16 GiB, the largest a Sprite gets on request.
  Billing is metered on actual CPU and memory use, so real charges are lower.
- **Storage.** A phase 1 Sprite lives one job, so hot storage at `disk_bound` is priced into the
  hourly figure and nothing is charged per day.

| Term | 16 GiB bound (the default) | 8 GiB (after 9.6 proves the limit) |
| --- | --- | --- |
| CPU | 8 × 0.0385 = 0.3080 | 0.3080 |
| Memory | 17.18 × 0.021875 = 0.3758 | 8.59 × 0.021875 = 0.1879 |
| Hot disk | 107.37 × 0.000683 = 0.0733 | 0.0733 |
| **Total $/h** | **0.7571** | **0.5692** |

`usd_per_hour` in a size overrides the computed price, but only upward: bitc uses the maximum of
the two.

### 5.2 Cap check [8.3, 15.6 edit]

Spec 8.3 unchanged: `committed = ledger_today + Σ listed own Sprites × price × (E − Created) + Σ
foreign Sprites × highest price × (start_timeout + longest timeout + 15m) + reservations not
listed + this job's estimate`, with `Spend()` absent (`Caps.Spend = false`).

1. **Estimate:** `price × (E − now)`, with `D = create + start_timeout + timeout`.
   `start_timeout` covers provisioning. Example: `start_timeout = "10m"` and `timeout = "30m"`
   give `E − now = 55 min`, so the estimate is $0.6940 at the 16 GiB bound.
2. **Per-host share.** For a provider without `Caps.Spend`, other hosts' finished jobs are
   invisible, so `daily_usd` is **each host's share** of the account total, not the account total
   itself. `doctor --remote` warns when the org holds Sprites from more than one `install16`.
3. **The one pre-attach retry** happens only after attempt 1's Sprite is confirmed deleted (204
   or 404, or a successful lookup that found nothing). It takes a new name and a new reservation
   under `launch.lock`, with the cap and `max_live` re-checked (providers delta B.3.3). If attempt
   1 is unconfirmed, there is no retry, and its reservation stays counted as an `orphan` until
   the sweep or a later cap check confirms the delete, or its `E`. So a job never holds two
   possibly-live Sprites under one estimate, and a leaked Sprite is not left alive until `E`. Each attempt writes its own
   `launch` and `end` ledger lines.

### 5.3 Ledger lines

- `launch` and `begin` are unchanged.
- `end {minutes, est_usd, cost_usd, created, exit|"fallback"|"interrupted"}`: `cost_usd = price ×
  minutes / 60`, with `minutes` from the create's 201 to the delete. A Sprite discarded before
  attach writes `end {exit: "fallback", created: true, ...}`, and `LedgerToday` counts it
  (providers delta B.3.2).
- **Example** at the 16 GiB bound: 3 min provisioning plus a 2 min type check is 5 min ×
  $0.7571/h = $0.0631. That is an upper bound; metered use is lower.
- **Pre-create wakes** do not exist in phase 1 (no warm Sprites to read).
- **Reconciliation:** monthly, by hand, against the Fly invoice (the dashboard meter lags).
  `status --json` shows `est_usd_today` per provider. Plan fees are outside the ledger.

## 6. Security and trust split [10 edit]

1. **Separate org.**
   - **Config.** `[providers.sprites] org` is required.
   - **The token** is an org token with full power over the org (no scoped tokens, 9.10). So the
     org holds only bitc Sprites, is never a production, staging or preview org, and has no
     Connectors and no other members' apps.
   - **Runtime guard.** After the create, and on every `GET` of a Sprite, `organization` (per
     OpenAPI 0.1.12, 9.11) must equal `org`. On a mismatch, bitc deletes the Sprite at once,
     writes a `pause` record, and does not fall back if the job was attached.
   - **`doctor --remote` fails** (not warns) on any Connector (`GET /v1/oauth/connections`),
     because a Connector whose selector matches `bitc*` would hand its credential to hostile jobs,
     and on any Sprite outside `name_prefix`.
2. **No token in the Sprite.** It is never in the Sprite's `environment`, labels, files or boot
   payload. As a result there is no self-delete (decision 8).
3. **No app secrets in persistent places.** Forwarded env follows the 8.4 rules and travels inside
   the boot payload (stdin). It never goes in the Sprite's `environment` (which persists) or in an
   exec `env` query parameter (that is a URL). Which names cross is `providers.sprites.send_env`
   (default `"forced"` in phase 1; decision 10, providers delta S).
4. **Deploy key lifetime** (decision 3a):
   - **Host:** a 0600 `deploy_key_file` from the repo grant, read per job and redacted.
   - **Transit:** WSS stdin only.
   - **Sprite:** the agent parses the key out of the payload, rewrites the fd-3 memfd without it
     before the re-exec, writes it to the private tmpfs, and runs `ssh -i` for the fetch. It then
     shreds the key, unmounts the tmpfs and removes `~/.ssh`, all before any repo code or step
     runs, and zeroes its own key buffer (best effort in Go).
   - **Assumed off-disk** (probes 9.5 and 9.21, live-proof step 3a). The key is meant never to be
     on the overlay, so no auto checkpoint can hold it. In phase 1, the Sprite and its checkpoints
     are deleted at the end of the job anyway. Until step 3a passes, this is an assumption; if it
     fails, decision 3's contingency applies.
   - Tests assert that the key's bytes appear in no fake-Sprite file or checkpoint (SP10).
5. **Results MAC key K and nonce M.** Each is a fresh 256-bit value per job, inside the boot
   payload only (memfd), never on disk. K never leaves the spec.
6. **Egress** (decision 4):
   - **Job policy:** `egress_allow`, explicit domains. `*` and `include` are rejected (78).
   - **Build policy:** `egress_allow` plus `egress_build_allow` plus the agent URL host. Nothing
     else is added.
   - **Setting it.** Set after create and before the first exec, with a `GET` check; the job
     policy is set and checked again before attach.
   - **Unverified:** SSH to `github.com:22` under a DNS policy (9.3; contingency in decision 3);
     whether refused names are forwarded upstream, which would allow DNS tunnelling (9.22).
   - Private IPs are always blocked by the platform.
7. **Privileges.** The job user is unprivileged as in 8.4. The platform privileges policy is left
   unset until probed (9.4), because `noNewPrivileges` on the whole Sprite may break the agent's
   own `sudo`.
8. **URL auth:** stays `sprite` (org-only). There is no HTTP service, and bitc never sets
   `url_settings`.
9. **Threat-model wording.** Unchanged from spec 10 for phase 1: one Sprite holds one job, so
   "untrusted repository content" cannot reach another job through the Sprite. **Out of scope:**
   an org-token holder (who can read auto checkpoints and exec into any Sprite).

## 7. Config example (host `bitc.toml`)

```toml
version = 1

[groups.bigmem]
units = 1                                  # one-heavy rule for local runs

[pools.heavy]
slots = 2
groups = ["bigmem"]
providers = ["sprites", "runpod", "local"] # primary, fallback, last
remote_limits = { size = { sprites = "cpu8", runpod = "cpu8" }, timeout = "30m" }

[remote_limits]
worktrees = ["~/src/app*"]
on_failure = "next"

[providers.sprites]
api_key_file = "~/.config/bitc/sprites.token"   # 0600; org token for sandbox-org only
org = "sandbox-org"                             # token's org slug must match, else 78
name_prefix = "bitc"
daily_usd = 10                                  # this host's share (5.2)
max_live = 2
start_timeout = "10m"                           # covers provisioning
# fallback_until = "attach"                     # default (Caps); "resource" narrows (providers delta B)
# send_env = "forced"                           # default (Caps); "forwarded" keeps forced names off Sprites (decision 10, probe 9.25)
disk_bound = "100GiB"
memory_bound = "16GiB"                          # pricing bound until probe 9.6
egress_allow = ["github.com", "codeload.github.com", "objects.githubusercontent.com",
                "registry.npmjs.org"]           # exact list settled by probe 9.3
egress_build_allow = ["archive.ubuntu.com", "security.ubuntu.com", "ports.ubuntu.com"]
agent = { source = "url", url = "https://example.invalid/bitc-{arch}", paths = { amd64 = "/opt/bitc/linux-amd64/bitc" } }
[providers.sprites.sizes]
cpu8 = { memory = "16GiB" }                     # set from measured peak RSS (decision 6)
# [providers.sprites.rates] defaults: cpu = 0.0385, memory = 0.021875,
#   hot_storage = 0.000683, cold_storage = 0.000027 (USD per hour)

[providers.runpod]
# unchanged from 8.7; daily_usd = 25

[[repos]]
match = "~/src/app*"
clone_url = "git@github.com:example-org/app.git"
deploy_secret = "APP_DEPLOY"                    # RunPod secret name
deploy_key_file = "~/.config/bitc/keys/app"     # Sprites; 0600, read-only deploy key
```

**Keys** (all host class, decoded by the provider except where noted):

| Key | Type | Default |
| --- | --- | --- |
| `org` | string | **required** |
| `api_base` | URL | `https://api.sprites.dev` |
| `sizes.<k>` | `{memory size, vcpu? = 8 only, usd_per_hour? money}` | **required** (at least one) |
| `rates` | `{cpu, memory, hot_storage, cold_storage}`, money per hour | the values above |
| `disk_bound` | size | `100GiB` |
| `memory_bound` | size | `16GiB` |
| `egress_allow` | domain list (exact or `*.d`) | GitHub and npm (above) |
| `egress_build_allow` | domain list | Ubuntu mirrors (above) |
| `[[repos]] deploy_key_file` (core) | path, credential-file rules (0600, owner, else 78) | none |

**Validation (78)**, prefix `bitc: <file>: providers.sprites.`:

| Case | Message |
| --- | --- |
| `org` missing, or the token's org slug differs | `org: required` / `org: token belongs to "<slug>", not "<org>"` |
| no `sizes`, or a size without `memory` | `sizes: at least one size with memory is required` |
| `vcpu` ≠ 8 | `sizes.<k>.vcpu: Sprites have 8 vCPU (got <v>)` |
| `memory` > `memory_bound` | `sizes.<k>.memory: exceeds memory_bound <b>` |
| `egress_allow` or `egress_build_allow` containing `*`, `include`, an IP, or an invalid domain | `<key>: "<v>" not allowed (explicit domains only)` |
| `disk_bound` > `100GiB` | `disk_bound: exceeds the 100 GB Sprite quota` |
| a warm-reuse key (`reuse`, `max_warm`, `idle_ttl`, `idle_grace`, `max_age`, `max_jobs`, `scrub_timeout`) | `<key>: unknown key` (phase 2, 11) |
| `fallback_until` invalid (providers delta B.6) | as there |
| `send_env` invalid or in a repo file (providers delta S.5) | as there |
| `deploy_key_file` insecure (core) | `[[repos]] deploy_key_file: <reason>` |

## 8. Testing

### 8.1 `spritesfake` (`internal/provider/sprites/spritesfake`, non-test package)

An `httptest` REST server plus a WSS exec endpoint.

- **A Sprite** is a directory `<root>/<name>/overlay/`.
- **Exec** runs `bitc __agent ...` as a local process in the Sprite's process group, unprivileged,
  with `TestRoot` = the overlay.
- **Auto checkpoints** are modelled: the fake takes `auto-N` copies on a timer, and tmpfs paths
  (`/run/bitc*`) are excluded, as on the platform (probe 9.5). Delete removes them.
- **`api.sock`** is emulated as a Unix socket in the overlay serving `tasks`.
- **Policies** are stored and served by `GET`. **`organization`** is served on every Sprite
  response, and `Behave` can set it wrong.
- **Pause:** with no activity (no Task, no exec in flight) for 30 s (scaled), the fake pauses the
  Sprite by SIGSTOP. A cold drop is SIGKILL.
- **`Behave` hooks:** `429-create`, `concurrent-limit`, `403-restricted`, `create-then-500` (leaves
  a Sprite), `create-then-500-lookup-fails`, `policy-get-mismatch`, `provision-infra-fails`,
  `provision-repo-fails`, `drop-exec-socket`, `exec-upgrade-4xx`, `exec-upgrade-timeout`,
  `cold-drop-mid-job`, `labels-unsupported`, `wrong-org`, plus the suite's generic names
  (providers delta A.10.2).
- It records every call, URL, header and body, for the secrecy assertions.

### 8.2 Conformance rows (family `SP`, phase 1)

| ID | Behaviour |
| --- | --- |
| SP1 | Sprites keys have the documented defaults; every validation case of 7 is 78 with its message; warm-reuse keys are unknown |
| SP2 | token org slug ≠ `org` is 78 at load; response `organization` ≠ `org` → delete, pause record |
| SP3 | price per size from rates, memory at `max(memory, memory_bound)`; override only raises |
| SP4 | 429, `sprite_creation_rate_limited`, `concurrent_sprite_limit_exceeded` → transient, one retry, then `trying runpod`; no breaker or pause record |
| SP5 | 401 and 403 (restricted) → unavailable, one pause record, next provider |
| SP6 | provisioning infra failure → delete (204), one retry on a fresh Sprite with a new name and reservation, then `trying runpod: ...; <name> torn down before attach`; one ledger `launch` and `end fallback created=true` per attempt; two breaker `failure` records |
| SP7 | attach classification: exec upgrade 4xx is pre-attach (SP6 path); upgrade timeout counts as attached → delete, 75, RunPod and local never tried |
| SP8 | network and resources policies set and verified before the first exec; job policy verified before attach (call order) |
| SP9 | no job input before J4: no spec, code ref, key or forwarded value in any call before the attach exec |
| SP10 | the deploy key, K and M appear in no URL, query, `environment`, label, overlay file or auto checkpoint; the post-re-exec memfd holds no key |
| SP11 | Task keep-alive: a silent 2 min (scaled) command is not paused; a cold drop mid-job → LOST, 75 |
| SP12 | cap: a running Sprite and its reservation are counted once; per-host share; estimate example of 5.2 |
| SP13 | interrupt (before and after attach) → delete; exit 128+N |
| SP14 | sweep table (2.4), every row; the orphan row with the job's process still alive (moved on to RunPod), from the timer and from the next launch's cap check |
| SP15 | provisioning `repo` failure (tool pin) → 69, Sprite deleted, no retry, no fallback |
| SP16 | `create-then-500`: found and deleted → retry once, then fall back; lookup fails, or the delete fails → `left for sweep`, **no retry**, orphan reservation counted, then `on_failure`; the next sweep deletes the Sprite at once and drops the orphan; with `fallback_until = "resource"` both are 75. A 5xx on a policy call after the 201 takes the same path; a 5xx on the cap-check `list` before any create is retried once with no lookup |
| SP19 | breaker: three discarded Sprites within `window` pause Sprites (`sprites is paused: ...`); the next job goes to RunPod without a create; 429s write no record |
| SP17 | `__agent read`/`tail` reject a bad job id, an unlisted file name and a symlink |
| SP18 | the agent exits after `result.json`; the host completes output and artifacts over exec from the idle Sprite, then deletes it |
| SP20 | `send_env`: default `"forced"`, a forced value travels only in the stdin boot payload (never `environment`, a URL or an exec `env` parameter); with `send_env = "forwarded"`, a job forwarding a forced name is `ErrIneligible` for Sprites (no create, no reservation), prints `not eligible for remote: forced env not sent to sprites (providers.sprites.send_env = "forwarded"): <F>` (the merged core's wording for an ineligible job whose next provider is remote, providers delta S.3), and runs on RunPod |

Existing rows run against Sprites unchanged where provider-neutral: the K cap rows, C, RR, V
(dropped tail via the `__agent tail` and offset path), A (artifacts), forged-result, FB1–FB8,
FB14, SE1–SE5, FB15 (nothing crosses before attach; SP9 is its Sprites-specific twin, which also checks
the code ref) and PS1 (`list` completeness with `?prefix=` pagination).

### 8.3 Provider suite scenarios (`conformance/provider/`)

- **A `sprites` column** for the scenario set from 11: launch, fallback (sprites → runpod → local
  with both fakes up), cap, breaker, lost, restart (cold drop), interrupt, sweep, dropped log
  tail, forged result, artifacts, and `preattach-fails`.
- **The privileged suite** gains the tmpfs key directory and the `--boot-stdin` re-exec with memfd
  stripping, in a CI container (shared with the ssh provider, providers delta X.1).

### 8.4 Live proof runbook outline

1. **Setup.** A maintainer is present. Use org `sandbox-org` only, with a fresh token and
   `name_prefix = "bitcproof"`. Set `daily_usd = 2` and `max_live = 1`. Get a written OK on the
   worst case from `scripts/live-proof-worst-case.sh --sprites` at $0.7571/h with this config's
   minutes (8.5).
2. **Probes.** Run the probes of 9 first. Each is a separate scripted step whose output is
   recorded in the runbook's Results.
3. **Jobs.** Run one job with a public test repo, then a private one with the deploy key. Check the
   egress denial (`npm view` works, `curl example.com` is `REFUSED`), result verification,
   artifacts and the delete.
   - **3a. Key search.** After the private job, before the delete: a root exec searches `/`
     (excluding `/proc` and `/sys`) and every mounted checkpoint for a fingerprint of the key.
     List the auto checkpoints (`--include-auto`, through the `sprite` CLI if REST lacks it) and
     search them the same way. Record the result. Any hit voids decision 3a.
4. **Pre-attach fallback.** Leave `ports.ubuntu.com`/`archive.ubuntu.com` out of
   `egress_build_allow` for one run with a non-empty `packages`. Expect two Sprites created and
   deleted, then `trying runpod: ...; <name> torn down before attach`, and RunPod running the job.
5. **Forced fallback.** Run with `--provider runpod` (or `daily_usd = 0.01`). Expect the
   `trying runpod` line, or the cap note.
6. **Reconciliation.** Reconcile the ledger against the Fly dashboard the next day, and record
   both figures.

### 8.5 Worst-case script

`scripts/live-proof-worst-case.sh` gains a `--sprites` mode:

```
per job = 2 × provision_minutes × price / 60                    # pre-attach Sprite + its one retry
        + (start + timeout + 15 + sweep) × price / 60            # no self-delete on Sprites
```

## 9. Open verification items (probe before the live proof)

### 9.0 S.0 results (live run 2026-10-24) and the rules they set

The S.0 probe kit ran once against `sandbox-org`, at a total cost of $0.0206. The per-item table is
in the live-proof results. Two rules follow from it. They hold until
a probe disproves them:

- **Rule E (egress): no Sprites job runs until allow-list enforcement is proven.** Item 3 reached
  `registry.npmjs.org` under an empty allow policy. Either an empty list means allow-all, or the
  policy was not in force at the exec. The proof is a follow-up probe:
  - set an allow list of one host and read the policy back;
  - that host must be reachable, and a second host must be refused.
  Until it passes, the provider stays unavailable for jobs.
- **Rule I (isolation): assume no isolation inside a Sprite.**
  - Exec runs as `sprite` (uid 1001) with passwordless sudo. The privileges policy
    `profile=minimal` was accepted, and `sudo -n` still worked (items 4 and 15).
  - `/.sprite/api.sock` is world-writable. A non-root user gets 200 on checkpoints, services and
    tasks (item 23).
  - Root can unshare namespaces and mount over `/.sprite` (item 20).
  So **no secret ever goes into a Sprite**: no deploy key, no forced env. Any job is treated as
  able to drive checkpoints, services and tasks.

  This overrides decision 10's phase-1 default: `send_env` for `sprites` becomes `"forwarded"`, and
  `"forced"` is refused at load (78). That is a code change, tracked as its own task and not built
  yet. Item 25 (checkpoint retention after delete) also stayed assume-worst, which points the same
  way.

Other findings:
- Item 6: `limit_mb` is enforced (a 1280 MiB tmpfs allocation over a 1024 MiB limit was killed).
  MemTotal shows the limit, not the 16 GiB policy. The nproc reading needs a recheck (a likely
  probe parse bug).
- Items 8 and 11: the organization answer carries the limits (10 active, 10 warm, 10 creations).
  Its slug is empty, but `organization` is present on every GET and list item, so the
  response-side org check (SP2) holds.
- Item 13: names over 63 characters, or with uppercase, `_` or `.`, get 400. A label value of 300
  characters is accepted.
- Item 16: a 200 MiB read over WSS exec failed at 0 bytes ("stream ended without an exit status"),
  while `fs/read` gave 54 MiB/s. The follow-up probe narrows this down (client or platform).
- Item 19: GET, list and `fs/read` do not wake a Sprite. Item 24: x86_64. Item 7: no
  `/v1/regions` (404). Item 17: the Tasks API gave 400, so the keep-alive uses the heartbeat exec.
- Items 1, 2, 5 and 25: checkpoint create over REST gave an unusable answer (the request shape was
  a guess), so they stay assume-worst.

Phase 1 needs items 2, 3, 5, 6, 8, 10–13, 15, 16, 22, 23, 24 and 25. Items 1, 4, 14 and 17–21 gate warm
reuse (11) and can run in the same session.

1. Checkpoints: can a checkpoint be deleted over REST? Can code inside the Sprite delete or
   restore checkpoints through `api.sock`, and can a non-root user reach it? Does
   `GET .../checkpoints` include auto checkpoints, or is that CLI-only?
2. Do the network, privileges and resources policies survive a restore? Can a policy be set before
   first boot? If not, keep "create → policy → verify → exec".
3. The exact egress hosts for GitHub (SSH on port 22 under a DNS policy, `codeload`,
   `objects.githubusercontent.com`) and npm. The domains inside `include: defaults` (unused).
4. Does the privileges policy (`minimal`, `noNewPrivileges`) block `api.sock`, `sudo` or
   `/.sprite/checkpoints`? Do mode bits hold under the `/.sprite/checkpoints/<id>/` mount?
5. Tmpfs and auto checkpoints: can auto checkpoints be turned off, or made to exclude tmpfs? Is a
   tmpfs mount by root allowed, and kept out of the overlay and out of checkpoints?
6. Memory and CPU: is `limit_mb` with `autoscale = false` enforced? Is `config.ram_mb` honoured, and
   is `config.cpus`? Is 16 GB available?
7. Regions and region pinning.
8. The HTTP status of `concurrent_sprite_limit_exceeded`; the pay-as-you-go `running_limit` and
   `warm_limit` values.
9. Can Connectors serve `git fetch` (decision 3b)?
10. Scoped tokens (assumed absent).
11. Is `GET /v1/organization` stable? Is `organization` present on every Sprite response (6.1)?
12. The `POST /exec` response body (unused; bitc uses WSS).
13. Sprite name length and charset; labels over REST (create and update), their charset and
    length.
14. `fs/read` and `fs/write`: can `fs/read` read root-only files and `/proc` paths? Which user does
    `fs/write` write as? Does `boot_id` change on a restore? How long does a restore take on sbd
    versus old storage?
15. Exec: which user does exec run as, and does that user have passwordless `sudo -n`? What is the
    format and maximum of `max_run_after_disconnect`? Can a WSS exec session's output be
    re-attached after a drop? Is there an exec rate limit, and what is the session-creation
    latency? Does an exec on an idle Sprite whose agent has exited wake it reliably (SP18)?
16. WSS exec throughput for a 200 MiB artifact read (compare `fs/read`, whose read limit is
    unknown).
17. Tasks: does the Tasks API work from a root process started by exec? Does an active Task really
    hold `running`? Does a Task outlive a restore?
18. Is a *warm* Sprite (suspended, NVMe cache kept) billed for hot storage?
19. Does a `GET /v1/sprites/{name}`, a list call or an `fs/read` wake a Sprite?
20. Can root inside a Sprite `unshare(CLONE_NEWNS | CLONE_NEWPID)` and mount a tmpfs over
    `/.sprite`?
21. Is `/run` a tmpfs on Sprites, and are tmpfs mounts excluded from checkpoints? See also 9.5.
22. DNS tunnelling: a query from inside to a unique subdomain of an owner-run domain must not
    reach that domain's authoritative server.
23. What `api.sock` offers a non-root user, such as exec, services or other Sprites. This gates
    decision 12.
24. The Sprite CPU architecture (`uname -m`), assumed `x86_64` (`Info().Arch = ["amd64"]`).
25. Checkpoint retention after delete: once a Sprite is deleted (204), can any of its checkpoints
    (auto included) still be listed, restored or read by the org, and for how long? If any
    survive, decision 10's recommended `send_env` default becomes `"forwarded"`.

Item 17 also matters to phase 1 for the keep-alive: if a Task does not hold `running`, the agent
falls back to a 20 s heartbeat exec from the host during the job (S.2).

## 10. Task breakdown

These follow spec 14's rules: one agent per task, TDD, its own worktree, one reviewed PR, and
estimates in calendar days with agents in parallel. F.1, X.1 and V.1 are the providers delta's tasks.

| # | Task | Depends | Done when |
| --- | --- | --- | --- |
| S.0 | Probes for 9 (scripted, `sandbox-org`, maintainer present, under $1); measure the app repo's peak RSS; spec v3.4 fold-in of this delta | approval | every phase 1 item answered or marked "assume worst" (0.5 d + maintainer time) |
| S.1 | Sprites client (REST plus WSS exec, error mapping, NDJSON streams, attach classification) and `spritesfake` (8.1) | F.1 | SP2, SP4, SP5, SP8; fake self-tests incl. pause, cold drop, auto checkpoints (1.5 d) |
| S.2 | Provider: settings and 78s, price, names and owner tag, create, policies, provisioning (`__agent provision`, marker, install skip), attach, `ErrPreAttach` mapping, `Watch` (tail), `Output`, `Fetch`, `Teardown`, `List`, `Owner`, Task keep-alive, `Caps.SendEnv`/`MultiJob` | S.1, X.1, V.1 | SP1, SP3, SP6, SP7, SP9–SP13, SP15–SP20 (2 d) |
| S.3 | Sweep (2.4), `doctor --remote` (org guard, Connectors, foreign Sprites, multi-install, sweep-timer fail), `status` section, worst-case script `--sprites` | S.2 | SP14, RW rows for sprites (0.5 d) |
| S.4 | Provider suite column; two-fake fallback chain sprites → runpod → local | S.3 | 8.3 green with `-race` (0.5 d) |
| S.5 | Live-proof runbook and worst-case figure; run it (maintainer present) | S.4, S.0 | runbook Results filled; ledger reconciled with the invoice (0.5 d + maintainer time) |

Critical path: F.1 (1.5 d) → S.1 (1.5 d) → S.2 (2 d) → S.3 → S.4 → S.5, with X.1 (2 d) in parallel
with F.1 and S.1. **About 6.5 calendar days** plus S.0 and maintainer time. S.0 needs a maintainer to
create the `sandbox-org` org and token.

## 11. Later: warm reuse (phase 2)

Not scheduled. The r2 design (commit `503a6ee`, sections 2–3 and 5.2) stays the starting point,
re-expressed in the shared vocabulary:

- **Mode:** `reuse = true`, pooled per `(install, repo_id, size, baseline key)`, `max_warm ≥
  max_live`, eviction by `idle_ttl`, `max_age` and `max_jobs`.
- **Baseline:** a bitc checkpoint after provisioning (agent, packages, tools, `bitcjob`, a root
  sentinel), restored only by recorded id.
- **Scrub on release** (R1–R5), run by a detached `bitc __release` that inherits the lease lock
  and takes over the reservation. Verification goes only through the platform API: the checkpoint
  list, `create_time`, a dirty marker, the sentinel, the manifest, `boot_id`, policies, services
  and exec sessions. Any mismatch deletes the Sprite.
- **Job sandbox:** mount and PID namespaces, an empty tmpfs over `/.sprite`, `NO_NEW_PRIVS` and an
  empty bounding set, and a per-job results tmpfs.
- **Core hooks it needs:** `Selector` (selection under `launch.lock`, reservation keyed by the
  Sprite name), `Releaser` (skips `Teardown`), `Caps.Reuse`, the state-aware cap table, and the
  `release` and `storage` ledger lines. `Caps.OwnRetry` is no longer needed, because the providers
  delta's one pre-attach retry (B.3.3) covers it.
- **Boundary:** `attach` is unchanged. The warm-attach checks become part of the pre-attach
  sequence, and a failure there makes the Sprite `suspect` and is an `ErrPreAttach`.
- **Decisions to re-take then:** pool scope per repo (r2 decision 2), root-step repos always fresh
  (r2 3), `Caps.MultiJob = true` (so `send_env = "forced"` is 78 and the effective level is at
  most `"forwarded"`, providers delta S.4; this replaces r2 12's refusal), and a second sweep host
  (r2 13).
- **Gates:** the phase 1 live proof; probes 9.1, 9.4, 9.14, 9.17–9.21; a live-proof reuse step
  where a hostile job A's planted files, double-forked daemon, `api.sock` and checkpoint attempts
  and `sudo` all fail, and job B sees none of A's state.
- **Out of band:** `bitc __warm` fills `max_warm − live` Sprites ahead of demand.

## Review notes

**S.2 implementation notes (2026-10-23), for the S.0 spec fold-in.**

- `__agent provision` reads one `ProvisionSpec` frame (packages and tools, nothing of the job) on
  stdin; it exits 0, 71 (the repo's own failure) or 75 (anything else is read as infra); its last
  line on failure is `setup step <name> failed: <why>`. The marker is
  `/var/lib/bitc/provisioned.json`; a matching Run skips installs and re-hashes the kept download
  of each tool (`/opt/bitc/tools/.dl/<name>`).
- The agent writes the deploy key only to a tmpfs it mounts on `/run/bitc-key` (a failed mount is
  `infra`, `key-mount`); `scripts/privileged.sh` now runs with `SYS_ADMIN` and no AppArmor profile.
- The nonce M is in `agent.log` (the markers carry it); 6.5 "never on disk" holds for K, B and the key.
- `[[repos]] deploy_key_file` is a new core host key (credential-file rules, 78 otherwise).
- Rates accept six decimals (`rates`); `name_prefix` plus a size key may total 16 characters.
- An HTTP error before 101 on the attach is `ErrPreAttach{ErrTransient}` (one retry); the client's
  `Run` keeps the exit status of an exec that ends before reading all stdin.
- The ledger bills from the create's 201 (`Handle.Created`), not from the attach.
- Client-wide `NoSudo` option exists; no host key yet (probe 9.15).
- SP20's note wording follows the merged core (`not eligible for remote:`), as SE3.

**r6 (2026-10-23, caller env).**

- Decision 10 is **DECIDED**: the Sprites-only `forward_env_force` rule is gone,
  replaced by the generic host key `providers.sprites.send_env` (providers delta S), default
  `"forced"` in phase 1 (`Caps.SendEnv`), any value settable by the host.
- New probe 9.25 (checkpoint retention after delete); if checkpoints survive, the recommended
  default becomes `"forwarded"`.
- 5 (`Caps`), 6.3, 7 (config, validation), 8.2 (SP20; SE rows run against Sprites), 10 (S.2
  depends on V.1) and 11 (warm reuse sets `MultiJob`, so the S.4 invariant replaces r2 12)
  updated.

**r5 (2026-10-23, after the providers delta review round 2).**

- 1.2: the "5xx, network → retry once" row allowed a second live Sprite. It is split into four
  rows: before any create (clause 1 retry, nothing to delete), on the create itself (ambiguous
  create, 1.3.3), after the 201 and before attach (`ErrPreAttach`, retry only after a confirmed
  delete), and after attach (75). 429 and quota rows cite clause 1, the unchanged 8.3 retry of a
  create that left nothing.
- 1.3.3, 2.4, 5.2.3, SP14 and SP16: an orphan reservation never shields its Sprite; the sweep and
  the next launch's cap check delete it at once (providers delta B.3.3.3).
- 8.2: FB15 and PS1 run against Sprites.

**r4 (2026-10-23, after the providers delta review round 1).**

- 5.2.3, 1.3.3, the 1.2 table, SP6 and SP16 follow the corrected retry rule: retry only after a
  confirmed delete; an unconfirmed attempt keeps its reservation until `E` and is not retried.
  The r3 claim that "a retried job can never commit more than one estimate at a time" was false
  when attempt 1 was not confirmed gone.
- Each discarded Sprite is a breaker `failure` (providers delta decision 16), row SP19.
- No other change: Sprites is a built-in, so the plugin-only rules (ambiguous launch, host-priced
  cap inputs) do not apply; its own ambiguous attach (1.3.1) was already attached.

**r3 (2026-10-23, alignment with the providers delta).**

- **Scope cut to phase 1 = fresh Sprite per job** (direction). Removed from the main text:
  the warm pool and its records and lease locks, `Select`, eviction, baseline checkpoints, the
  scrub and its verification, the job sandbox, the state-aware cap, `release`/`storage` ledger
  lines, `Selector`/`Releaser`/`Caps.Reuse`/`Caps.OwnRetry`, and the warm-reuse keys, decisions,
  fake hooks and SP rows. Their summary is in 11.
- **r2 decision 4b (a Sprites-only 15.8 amendment) is replaced** by the providers delta B: a
  generic `Caps.FallbackUntil`, where Sprites declares `attach` and RunPod keeps `resource`. The
  attach point (r2 1.3, after money review 8) is kept verbatim. The `trying <next>` note moved to
  the providers delta.
- **Decisions reversed for phase 1** because the Sprite dies with the job: `forward_env_force` is
  allowed (r2 12: refuse), `root_steps` is allowed (r2 3: always fresh, now moot), and the second
  sweep host becomes optional (r2 13).
- **Kept:** every research fact (rates, fixed 8 vCPU, creation limit, idle billing, wake times,
  Tasks keep-alive, exec outliving disconnects, org-wide token, no billing API, default-open
  egress), every open probe (now 24, with arch added), the org guard, the key delivery
  contingency, two-phase egress and the client-library choice.
- Neutralised the example `[[repos]]` entry (clone URL and key path).

**r2 (2026-10-23).** Three adversarial reviews applied: security and isolation (18 findings), money
and fallback (24) and feasibility (23). The choices that still apply in phase 1: the precise attach
point (an HTTP error before `101` is pre-attach; a network error or timeout during the upgrade is
attached); the pre-attach wake cost is accepted (moot in phase 1, since no warm reads happen); and
"best effort" zeroing of the deploy key in Go, with the memfd rewrite as the enforceable part. The
choices specific to warm reuse (the job-made checkpoint defences, release reaching warm, eviction
under the lock) stand in r2 for phase 2.
