# bitc: design spec

Status: **v3.3** (v3.2 plus the remote-failure decisions, 15.8 and 15.9). bitc
("big ideas, tiny computer") is open source under the MIT license at
`github.com/Exploded-views/bitc`; the binary is `bitc`.

bitc has **providers** (`local` built in, RunPod first) and replaces a flock-based shell pool.
Behaviour IDs (`P1`, `S3`, ...) name that pool's behaviours; each has a neutral one-line entry in
`conformance/behaviours.toml` (11). Background: prior art on flock-based pools and job queues.

## 1. Purpose and scope

**Problem.** Many AI coding agents and a few humans share one Linux host and run heavy commands
(lint, typecheck, test suites, coverage, browser e2e, database tests, git pushes) whenever they
like. Unco-ordinated, they exhaust memory, OOM each other and turn test results into noise. Some
jobs should run on a cheap cloud machine instead, with the caller noticing only a log line.
**What bitc is.** One static Go binary for Linux, configured by TOML files, with no project
knowledge built in. `bitc run heavy -- npm run lint` waits for a slot in the named pool (and any
shared group it consumes), runs the command with the pool's environment and returns its exit code.
If it cannot start in time it exits 75 ("try again later"), never a fake failure. A pool may send
the command to a remote **provider** (RunPod first) when the host allows it, within a daily cap.

**Users:** agent sessions (one Unix account, many work trees, ~10 min tool-call caps), humans on
the host, the host owner (capacities, priorities, providers, caps) and repo maintainers (env,
hooks, refusals, remote setup). **Goals:** parity with the replaced shell pool via a ported
conformance suite; any language or repo; config validated before any side effect; daemonless
(`flock(2)`); safe remote overflow (account-wide daily cap per provider account, leak-proof, no
secrets in argv or logs);
agent-grade contracts; small. **Non-goals (v1):** CI, sharding, build caching; multi-host
scheduling; daemon or UI; macOS and Windows (`/proc` is required); cgroup limits, PSI admission;
GPU pods; job DAGs or retries (callers retry on 75); sandboxing the account from itself (10).

## 2. Concepts

| Term | Meaning |
| --- | --- |
| **pool** | named queue with N slots, a wait budget, an optional memory reservation, the groups it consumes and its provider list. Names `[A-Za-z0-9_-]+`; `-` and `_` are the same name (canonical `-`). |
| **slot** | one of a pool's N places, **numbered 1..N everywhere** (`slot.<i>.lock`, `{slot}`, `BITC_SLOT`, `hook-env --slot`, compat locks). Group units are numbered 1..k. |
| **group** | cross-pool semaphore with a capacity in units; a pool consumes `k` units of each group it lists. |
| **ticket** | a waiter's file in the pool's queue, flocked by its owner for its whole life. |
| **hold** | an admitted ticket (slot and units taken, command running), with a random hold token. |
| **provider** | where a command runs: `local` (built in) or a remote provider such as `runpod`; at most one instance per type. |
| **fallback boundary** | the latest point at which a failed remote job may move on to the next provider: `resource` (until a provider-side resource exists) or `attach` (until job input first reaches it); 8.2. |
| **host / repo config** | `bitc.toml` owned by the account (capacities, providers, grants) / `.bitc.toml` in a work tree (project behaviour; its `[remote]` tables are the **remote profile**). |
| **state dir** | local state (locks, tickets, jobs, history); movable with `BITC_DIR`. |
| **accounting dir** | remote state (ledger, launch lock, reservations, breaker, install secret), fixed at `<passwd home>/.local/state/bitc/account`; no env, flag or key moves it. |

## 3. CLI

Global flags before the subcommand: `--config <file>`, `--state-dir <dir>`, `-v`, `--json`.

| Command | Purpose |
| --- | --- |
| `bitc run [flags] <pool> [--] <cmd> [args...]` | run a command under a pool |
| `bitc wait <job> \| --log PATH [--timeout 9m] [--exit zero\|job]` | bounded wait for a detached job |
| `bitc logs <job> [-f] [--tail N]`, `bitc cancel <job> [--kill]` | print or follow a job's log; signal a detached job |
| `bitc status [<pool>...]` | pools, holders, waiters, groups, memory, remote jobs, spend, breaker |
| `bitc sweep [--dry-run] [--provider P] [--foreign]` | clean stale local state and leaked resources (8.3) |
| `bitc doctor [--remote]` | check host, dirs, config, providers |
| `bitc config check \| explain [key] \| hook-env <pool> --slot N` | validate; show each effective value and its source; run an `env` hook without a lock and print the parse |
| `bitc version [--json]` | version, commit, schema versions, `release` stamp |

Hidden: `bitc __supervise <job>` (detached supervisor), `bitc __agent` (remote side, 8.4).

### 3.1 `run`

```
bitc run [--detach] [--name N] [--log PATH] [--wait DUR] [--local | --provider P]
         [--artifacts a,b] [--result PATH] <pool> [--] <cmd> [args...]
```

- Flags precede `<pool>`; the rest is the argv, passed to `execve` verbatim (no shell). Steps run
  in the order of the pipeline in 6.
- `--wait` may only shorten the pool's wait. `--local` forces `local`. `--provider P`: P must be a
  remote provider in the pool's effective list (else 64); the list becomes `[P]` plus `local` if
  listed; eligibility, cap and breaker still apply. `--artifacts`: a subset of the declared ones.
- `--detach`: runs pipeline steps 1–4, opens the log and writes `spec.json` (any failure:
  64/69/75/78, empty stdout; an uncreatable log is 64); starts a supervisor in a new session; prints
  exactly `started <name> pid <pid> log <path>` (absolute). Everything from step 6 on, `pre_run`
  included, runs in the supervisor and reaches the caller only through `wait`.
- `--name`: `[A-Za-z0-9._-]+`, default 8 random hex, scoped to the work tree. **Log:** a job has one
  log, `--log PATH` if given (a rerun unlinks PATH and creates it `O_EXCL`; no `out.log` is
  written), else the record's `out.log`. It always ends with `__EXIT__ <code>` on its own line;
  bitc writes `\n` first unless the last byte is `\n` (so also after a CR or NUL).
- `--result PATH`: also write `result.json` there.

### 3.2 `wait`

Polls every 10 s until the job ends or `--timeout` (`detach.wait_timeout`); binds to the record's
generation at first read.

| Outcome | stdout first line | exit (default `--exit zero`) | `--exit job` |
| --- | --- | --- | --- |
| ended: the command ran, or the supervisor refused (64) or found a hook unusable (69) | `RESULT exit <code>`, `--- last 60 lines of <log> ---`, lines | 0 | the code |
| starved or lost (`origin = bitc`, 75) | `RESULT starved` + tail | 75 | 75 |
| internal error (`origin = bitc`, `outcome = "internal"`, 69) | `RESULT internal error` + tail | 69 | 69 |
| still running | `RUNNING (no exit line yet after <s>s); last lines:` + 5 lines | 124 | 124 |
| unknown job / no log | `NO LOG <path> ...` | 66 | 66 |

By default 124, 66, 75 and 69 are `wait`'s own outcomes; `--exit job` can collide, as documented. A
command that itself exits 75 shows `RESULT exit 75`. **`--log PATH`** waits on a log with no bitc
record (a job a legacy launcher started, 12): legacy J8/J9 apply, so the last line starting
`__EXIT__ ` wins, code 75 prints `RESULT starved`, and every ended job exits 0.

### 3.3 `status`, `sweep`, `doctor`, `cancel`

- **status** (`--json`: `docs/status.schema.json`): holders (job, slot, pid, cwd, argv, provider,
  `nested`), waiters, last STARVED reason, groups, memory, per-provider resources, spend, breaker.
- **sweep**: exit 0 unless 64/78; prints `swept <kind> <id> (<why>)`. Local: dead tickets and their
  process groups, dead supervisors' records (`lost`), stale staging dirs, rotation, `retention`.
  Remote: 8.3. Run it from a timer (`docs/sweep-timer.md`: systemd user timer, 15 min).
- **doctor**: `ok|warn|fail <check>: <detail>`; exit 0, 69 on a fail, 78 for config (`--remote` also 78 for an agent `module_dir` that is not a clean git tree). Checks Linux,
  dirs (flock, local fs, 0700, owner), config, `/proc/meminfo`, schema. `--remote`: credentials
  resolvable, API and billing reachable, cap headroom, agent source, ref namespace, known hosts.
- **cancel**: SIGTERM to the supervisor, which applies `on_signal` (6.4); `--kill` or a second
  `cancel` escalates; cleanup always runs. Remote: tear down, ledger `interrupted`, exit 143.

## 4. Exit codes and output text

| Code | Meaning | Caller should |
| --- | --- | --- |
| command's own (`128+N` if killed by signal N) | the command ran, locally or remotely | treat as the real result |
| 64 `EX_USAGE` | bad arguments or malformed `BITC_*` value; unknown/invalid pool; group used as a pool; refused by policy; `--provider` not allowed; uncreatable detached log | fix the call |
| 66 `EX_NOINPUT` | `wait`/`logs`/`cancel`: no such job | check the name |
| 69 `EX_UNAVAILABLE` | not Linux; hook could not start, timed out, printed unparsable output or exited with an unlisted code; no provider left and the last was unusable (a remote-only pool's job not eligible for remote, 8.2); a remote job's own setup step failed by itself (15.9); internal error (a bitc bug) | do not retry blindly |
| 75 `EX_TEMPFAIL` | starved; hook exited 75; remote infrastructure failure after start; no provider left after a transient failure | wait and retry |
| 78 `EX_CONFIG` | invalid config file; host-only key in a repo file; `min_version` unmet; insecure, NFS or newer-schema dirs | fix config |
| 124 | `wait`: job still running | call `wait` again |

A remote problem surfaces only as local fallback, 69 or 75 (8.2). An internal error or panic
(top-level `recover` in `main` and the supervisor) prints `bitc: internal error: <msg>`, releases
everything and exits 69, never Go's 2 and never 75: a bug that fails every time must not loop a
retrying agent forever. 75 means only a genuine wait-and-retry (starved, lost, provider transient).
`result.json`: `{origin: "bitc"|"command", outcome:
"ok"|"command-failed"|"starved"|"refused"|"config"|"unavailable"|"interrupted"|"lost"|"internal",
exit_code, reason, pool, slot, provider, job, generation, hold_started, wait_ms, run_ms}`; every
detached job writes it in its record, a foreground run only with `--result PATH`.

stderr lines (stdout belongs to the command):

| Line | When |
| --- | --- |
| `STARVED <pool>` | 75 from admission: every slot busy, no one ahead |
| `STARVED <pool> (<reason>)` | 75 from admission otherwise: `queue`, `group <g> busy`, `memory: <n> MiB available, floor <f> MiB`, `priority lane`, `nested child busy`, `legacy pool: <what> busy`, `pre_run: <why>`, `env hook: <why>` |
| `STARVED <pool> (<provider>: <why>)` | 75 from a remote job after it started; `<why>` names the resource, e.g. `pod <id> timed out after 61 min` |
| `bitc: <provider>: running <pool> locally: <why>` | remote preflight or launch fell back (only when `local` is the next provider in the walk, 8.2) |
| `bitc: <provider>: <pool>: trying <next>: <why>` | the same, when the next provider in the walk is remote (8.2); after a resource was discarded before attach `<why>` ends `; <id> torn down before attach`, or `; <id> left for sweep` when its teardown failed |
| `bitc: <provider>: <pool>: not eligible for remote: <why>` | the job is ineligible for the provider and the next provider is not `local` (a remote provider, or none in a remote-only pool); 69 at the walk's last provider (8.2) |
| `bitc: <provider>: <pool> job <job> on <resource> (<size>) at <sha12>` | remote launch |
| `bitc: <provider>: the log stream missed the end of the job; the rest of its output follows` | `Output` completed a stream (V4) |
| `pod: <line>` | remote setup output (marker lines stripped) |
| `bitc: <pool>: refused: <reason>` | 64 from a refuse rule or hook |
| `bitc: <message>` | every other note, warning or error |

## 5. Configuration

### 5.1 Files and discovery

TOML, decoded strictly (`pelletier/go-toml/v2`, unknown fields rejected). Layers, lowest first:
built-in defaults (5.4); the host file `$XDG_CONFIG_HOME/bitc/bitc.toml` (or `--config` /
`BITC_CONFIG`); one repo file; env (5.6); flags. Any invalid file exits 78; there is no fallback.

- **Repo file:** `.bitc.toml` at the work tree's top (`git rev-parse --show-toplevel`; outside git,
  the first one walking up from cwd, stopping at `$HOME` or `/`); never merged with nested files.
  If absent, the first host `[[repos]]` entry whose `match` fits and names a `config` supplies a
  host-chosen file, validated as a repo file (old commits, repos the owner cannot edit). **Why
  `.bitc.toml`:** one stat after `git rev-parse`, hidden like `.golangci.yml`, never confused with
  the host `bitc.toml`.
- **Work tree** = that canonical top; outside git, the repo file's dir, else the canonical cwd.
  `<wt8>` (8 hex of its sha256) and `{worktree}` derive from it.
- **Checks:** files not group/world-writable, owned by the user or root, 0600 if holding a
  credential. Every file starts `version = 1`; `min_version = "1.2.0"` makes an older binary exit
  78 (`bitc: <file> needs bitc >= 1.2.0 (this is 1.0.3)`) before reporting unknown keys.

### 5.2 Value rules (validated at load, before any lock, hook or network call)

| Kind | Accepted | Rejected (message names file, key, quoted value) |
| --- | --- | --- |
| integer | TOML integer in range; env/flags `0` or `[1-9][0-9]*` | leading zero, sign, unit, fraction |
| duration | `<int><unit>`, units `ms s m h d`, combinable (`1h30m`) | bare number, negative, `0.5h`, leading zero |
| size | `<int><unit>`, units `KiB MiB GiB TiB` | `MB`/`GB`, bare number, leading zero |
| money | number ≥ 0, ≤ 4 decimals; stored as integer micro-dollars | negative, NaN, inf, string |
| enum; argv | exact listed value (no case folding); array of strings | anything else |
| names | pool/group `[A-Za-z0-9_-]+` unique after `_`→`-`; size key and name prefix `[a-z0-9]+`; env (keys and every `forward_env`, `forward_env_allow`, `forward_env_force` entry) `^[A-Za-z_][A-Za-z0-9_]*$` not starting `BITC_` | anything else |
| glob | `path.Match` plus `**`, against the canonical path | invalid pattern |
| template | `{name}`, `{name+N}`, `{name-N}`; `{{`/`}}` escape | unknown name, malformed brace |

An empty string never means "default". Unknown keys are errors; `config check` lists them all.
`BITC_*` values and flags pass the same validators; a malformed one exits 64, a file value 78.

### 5.3 Key classes and merge rules

Each key has one class, as a struct tag (`bitc:"host,narrow=min,default=5s"`). **host**: host file
only (repo: 78). **narrowable**: others may only narrow it (widening: 78 in a file, 64 as a flag).
**project**: both layers, merged as stated. **repo**: repo layer only.

| Key | Class | Repo / env / flag may | Merge |
| --- | --- | --- | --- |
| `[host]` except `state_dir`, `poll` | host | — | — |
| `host.state_dir` / `host.poll` | host | `BITC_DIR`, `--state-dir` / `BITC_POLL` (≥ 100ms) | replaces |
| `slots`, `memory`, `groups`, `groups.<g>.units`, `[priority]` | host | — | — |
| `[defaults] wait`, `pools.<p>.wait` | host, narrow=min | repo `wait`, `--wait` | minimum |
| `pools.<p>.providers` | host, narrow=exclude | repo `providers_exclude`, `BITC_PROVIDER=local`, `--local`, `--provider` | host order; removals only |
| `pools.<p>.remote_limits.timeout` | host, narrow=min | repo `[pools.<p>.remote] timeout` | minimum |
| `[remote_limits]`, `pools.<p>.remote_limits.size`, `[providers.*]`, `[compat]`, `[[repos]]`, `[test]` | host | — | — |
| `providers.<t>.fallback_until` | host, narrow=boundary | — (a repo file, env or flag: 78; a repo file's line is `providers.<t>.fallback_until: host-only key`) | host value, never later than the provider's declaration |
| `providers.<t>.send_env` | host-only | — (a repo file, env or flag: 78; a repo file's line is `providers.<t>.send_env: host-only key`) | host value (`none`, `forwarded` or `forced`) |
| `providers.<t>.plugin` (`path`, `sha256`, `env_pass`, `secret_files`), core `sizes` of a plugin type | host | — (a repo file: 78; its line is `providers.<t>.plugin: host-only key`) | host value |
| `[detach]` | host | `wait --timeout` | flag replaces |
| `[vars]`; `env`, `env_default` (defaults and pools) | project | repo | key by key, **host wins** |
| `env_unset` (defaults and pools) | project | repo | union |
| `pools.<p>.refuse` | project | repo | host rules, then repo; none removed |
| `pools.<p>.on_signal` | project | repo | repo wins; bounded by `host.max_signal_wait` |
| `hooks.pre_run`, `post_run` / `env` / `wrap` | project | repo | both run, host first / at most one (two: 78) / host entries first, first match wins |
| `[remote]`, `[pools.<p>.remote]` | repo | — | — |
| artifacts | repo (remote profile) | `--artifacts`, `BITC_ARTIFACTS` | subset only |

**Env merge:** each layer first resolves its own `[defaults]` < pool, then the host layer wins key
by key (legacy P7/P8: the per-host file beat the in-repo one). A repo section for a pool the host
lacks is ignored with one warning (with `unknown_pools = "default"` it applies to the ad-hoc pool).
**Remote keys:** host `[remote_limits]` takes `worktrees`, `forward_env_allow`,
`forward_env_force`, `on_failure`; host `pools.<p>.remote_limits` takes `size` (a table per
provider type) and `timeout`. Repo `[remote]` takes `forward_env`, `forbid_ignored`,
`allow_ignored`, `env`, `env_unset`, `packages`, `tools`, `setup`, `services`; repo
`[pools.<p>.remote]` takes `allow`, `artifacts`, `timeout`, `env`, `env_unset`, `arch` (pool `env` wins key
by key over `[remote].env`; `env_unset` unions). A key in the wrong table is unknown (78).
**Architectures:** `pools.<p>.remote.arch` lists the linux architectures a pool's jobs may run on,
`amd64` and `arm64` only, each once, default `["amd64"]` (anything else: 78 `pools.<p>.remote.arch:
invalid: "<v>"`; `arm64` is opt-in per pool, once its native dependencies are checked there). A
tool entry may carry `arch` (default `amd64`); `(name, arch)` is unique (78
`remote.tools: <name>/<arch>: ...`).

### 5.4 Built-in defaults

Defaults and classes live in the struct tags; `docs/config-reference.md` is generated from them,
and a test fails when a tag, the reference and this table disagree.

| Key | Default | Class |
| --- | --- | --- |
| `version` / `min_version` | required (`1`) / unset | every file |
| `host.state_dir`, `poll`, `memory_floor`, `reservation_window` | `$XDG_STATE_HOME/bitc` (`~/.local/state/bitc`), `5s`, `6GiB`, `60s` | host |
| `host.unknown_pools`, `grace`, `max_signal_wait`, `history_max` (keep 3), `retention` | `refuse`, `10s`, `10m`, `50MiB`, `14d` | host |
| `defaults.slots` / `defaults.wait` (pools inherit both) | `1` / `10m` | host / narrowable |
| `pools.<p>.memory`, `groups` / `providers` | none / `["local"]` | host / narrowable |
| `pools.<p>.on_signal`, `refuse`, hooks; hook `timeout`, `refuse_exit` | `terminate`, none, none; `60s`, `[64]` | project |
| `detach.wait_timeout` / `priority.high_cwd_contains`, `high_cwd` | `9m` (fits a 10-minute tool cap) / `[]` | host |
| `remote_limits.worktrees`, `forward_env_allow`, `forward_env_force` / `on_failure` | `[]` (nothing goes remote) / `next` (`stop`: fail) | host |
| `pools.<p>.remote_limits.size.<type>` / `.timeout` | unset = ineligible for that provider / `90m` | host / narrowable |
| `providers.<t>.name_prefix`, `daily_usd`, `max_live`, `launch_wait`, `start_timeout` | `bitc`, required, `2`, `5m`, `20m` | host (core, 8.5) |
| `providers.<t>.plugin` / `sizes` | required for an external provider (the pinned `path` and `sha256`; `env_pass` `[]`, `secret_files` none) / a `usd_per_hour` > 0 per size, required for a plugin | host (core) |
| `providers.<t>.fallback_until` | the provider's declaration (`Caps.FallbackUntil`: `resource` for RunPod); `resource` for an external provider until the host opts in | host (core) |
| `providers.<t>.send_env` | the provider's declaration (`Caps.SendEnv`: `forced` for RunPod); `forwarded` for an external provider; at most `forwarded` for a provider with `Caps.MultiJob`; `forced` on one is 78 (8.4) | host (core) |
| `providers.<t>.breaker` / `agent` | `{failures = 3, window = "60m"}` / `{source = "release"}`; `source = "build"` needs `commit` (40 hex), `image` (`<name>@sha256:<64 hex>`) and `sha256` (64 hex), or alone a `module_dir` (an absolute path, `~/` allowed, holding this module's `go.mod`), else 78; `source = "url"` takes `url` (`{arch}` expands to the architecture) and `path` or `paths = { amd64 = ..., arm64 = ... }` (8.4) | host (core) |
| `pools.<p>.remote.arch` / `remote.tools[].arch` | `["amd64"]` / `amd64` | repo |
| `providers.<t>.transport` | `{remote = "origin", ref_prefix = "refs/bitc/jobs/", known_hosts = <built-in GitHub keys>}` | host (core) |
| `providers.<t>.artifacts` | `{max_compressed = "200MiB", max_unpacked = "2GiB", max_entries = 100000}` | host (core) |
| `[[repos]]` grants (`match`, `config`, `clone_url`, `deploy_secret`, `root_steps`) / repo artifact `replace` | none (no deploy key or root steps) / `true` | host / repo |

Fixed, not keys: `wait` polls every 10 s; processes left in a hold's group are killed at its end;
an unparsable `sh -c` string is checked only by its leading simple command (6.7); remote status polls every 15 s, results every 2 s.

### 5.5 Full example

`examples/config/host.bitc.toml` (host) and `examples/config/repo.bitc.toml` (repo) use every
section above: pools `lint`, `db`, `e2e`, `heavy`; a `bigmem` group; a `[[repos]]` grant; RunPod
for `heavy`; repo env templates such as `postgresql://localhost:{slot+db_port_base}/app_test` (with
`db_port_base = 55431`, slot 1 gets port 55432); refuse rules; hooks; a remote profile with tools,
setup and a root service. CI runs `bitc config check` on both.

### 5.6 Environment overrides (complete)

`BITC_DIR` (state dir; never moves accounting), `BITC_CONFIG` (host file), `BITC_POLL` (≥ 100ms),
`BITC_PROVIDER=local` (the only value), `BITC_ARTIFACTS` (comma list, subset of the declared paths;
empty = none; trailing `/` stripped), `BITC_LOG` (`info`, `debug`, `quiet`), `BITC_HOLD` (set by
bitc, 6.6). Values cannot widen host keys; empty means unset; malformed exits 64; other `BITC_*`
names are ignored.

### 5.7 Templates

`{pool}`, `{job}` and every `[vars]` key work everywhere; `{slot}` (1..N), `{slots}`, `{hold}`,
`{state_dir}`, `{worktree}`, `{cwd}` are host-only; `{week}` (ISO year-week) and
`{sha256:<path>}` (16 hex of a work-tree file) are remote-only (and later in cache keys, 16.1).
Arithmetic: `+`/`-` of one integer var and one integer literal or var. **Remote env check:** in a
remote-capable pool (host list names a remote provider; repo has `[pools.<p>.remote]`), every env
key whose template uses a host-only variable must be overridden by the remote profile's `env` or
listed in its `env_unset`; else 78 at load naming the key.

## 6. Admission and local execution

**Pipeline** (one order for every run): 1 parse argv and flags (64); 2 load and merge config (78;
64 for env/flags); 3 resolve the pool (6.1); 4 apply refuse rules (6.7), whatever provider will
run the job; 5 `--detach` only: open the log, start the supervisor, print `started`, and continue
there; 6 walk the providers (8.2). A remote provider runs 8.4 and ends the walk or passes to the
next. `local` runs: 7 `pre_run`; 8 queue and admission (6.2); 9 `env` hook; 10 `wrap` and exec
(6.4); 11 `post_run` and end of hold.

### 6.1 Pool names

Lookup uses the canonical name. Invalid → 64 `invalid pool name`; unknown → 64 `unknown pool <p>
(known: ...)` (with `unknown_pools = "default"`, `[defaults]` apply); a group → 64 `<g> is a
group, not a pool`.

### 6.2 Queue and locks (nq-style tickets, no daemon)

1. **Ticket:** `pools/<pool>/tickets/<class>-<unix_ns>-<rand8>.json`, created by temp file +
   `flock(LOCK_EX)` + rename (the flock follows the inode). Holds pid and start time
   (`/proc/<pid>/stat` field 22), argv, cwd, job, class (`0` high, `1` normal), `nested`, state,
   `group_wait`, slot, groups, `inherited_groups` (groups verified ancestors hold, at any depth,
   including a verified legacy claim: skipped, never released), hold token, `pgid`, `leader_start`. The owner keeps the flock until
   the hold ends and updates in place (`pwrite` + `ftruncate`) under `admit.lock`. Lock fds are
   `O_CLOEXEC`. A ticket is **high** if `high_cwd_contains` (literal substring) or `high_cwd`
   (glob) matches the logical `$PWD` (when it names the cwd) or the canonical cwd.
2. **Attempt** under the host-wide `admit.lock` (milliseconds), in order:
   **prune** tickets of this and group-sharing pools (`LOCK_NB` success = owner dead; for a dead
   `running` ticket SIGKILL `-pgid` if its leader still matches `leader_start`, else every process
   of that pgid started since the hold; unlink; print `bitc: reaped orphaned <pool> job <job>
   (supervisor died)`); **priority** (a normal ticket waits while the pool has a live high ticket,
   waiting or running; per pool, never per group); **queue** (eligible iff its `(class,
   timestamp)` position < free slots); **slot** (lowest free `slot.<i>.lock`, i = 1..N); **groups**
   (`g` is busy if an older live ticket of any consumer pool lists it in `group_wait`, else take
   `units` free `groups/<g>/unit.<j>.lock`, j = 1..k; all or none, setting `group_wait` on failure,
   cleared on admission or another failure cause); **compat locks** (12); **memory** (pools with
   `memory`, outside a verified nested hold: admit iff `min(MemAvailable, tightest cgroup headroom)
   − reservations within reservation_window ≥ memory_floor`; unreadable meminfo admits with one
   warning per call). Success records the reservation and marks the ticket `running`; any failure
   releases everything taken in the attempt.
3. **Wait:** sleep `poll` and retry; inotify on the same `tickets/` dirs wakes it early (a hold
   ends by unlocking, then unlinking its ticket).
4. **Deadline:** `start + wait` passed → `STARVED <pool>` with the last reason, 75. `wait = "0s"`
   makes one attempt. Fairness: FIFO per class for slots, FIFO by ticket time for group units.

### 6.3 Environment

From the caller's env: `[defaults].env_unset`, `[defaults].env`; pool `env_default` (unset keys
only), `env`, `env_unset`; `env` hook output; then `BITC_POOL`, `BITC_SLOT` (`<pool>.<i>`, i in
1..N, an "inside a slot" guard), `BITC_HOLD` (`<pool>:<ticket>:<token>`), `BITC_JOB` (job name),
`BITC_DIR` (absolute). Host and repo values merge per 5.3 first. Nothing project-specific is built
in.

### 6.4 Running the command

- `exec` the argv as the supervisor's child with the caller's stdio (never replaced);
  `WaitDelay = grace`. Non-TTY stdin: own process group (`Setpgid`) for signals and straggler
  cleanup. TTY stdin: the caller's group, so a prompt cannot stop on SIGTTIN holding the slot.
- **Supervisor death:** `Pdeathsig = SIGKILL` from a goroutine holding `runtime.LockOSThread`
  until `Wait` returns; `pgid` and `leader_start` in the ticket let the next prune kill survivors.
- **Signals** (INT, TERM, HUP, QUIT): `on_signal = "terminate"` (default) forwards to the group
  (leader on a TTY), waits `grace`, SIGKILLs, exits `128+N`. `"forward-and-wait"` (for commands
  with their own cleanup, such as an e2e stack) forwards and keeps the hold until the command
  exits, bounded by `max_signal_wait`; a second signal or `cancel --kill` escalates to `terminate`.
- **End of hold:** `post_run`; group stragglers SIGTERM then SIGKILL after `grace`; unlock all,
  unlink ticket, drop reservation, history `end`, return the command's code (`128+N` if it died of
  signal N).

### 6.5 Hooks

A hook is a templated argv run with the job's env, cwd = work-tree top, stdin `/dev/null`,
`timeout` and `refuse_exit`; an argv[0] with `/` resolves against the work tree. Hooks run only
for `local`, at the pipeline steps in 6. Results, after bitc releases what it holds: could not
start → 69 `bitc: <pool>: <hook> hook could not start: <err>`; exit 0 → continue; exit in
`refuse_exit` → 64 `bitc: <pool>: refused: <first stderr line>`; exit 75 → 75 `STARVED <pool>
(<hook>: <first stderr line>)`; timeout, other non-zero or unparsable output → 69 with the first
stderr line.

`pre_run`: before queueing, no slot held. `env`: after admission; the history `start` line follows
its success. `post_run`: after the command, gets `BITC_EXIT`; failure only warns. `wrap`:
`[{when_worktree, argv}]`, first match prefixed to the command. **`env` output grammar** (parsed,
never evaluated): one `[export ]NAME=VALUE` per line, blank and `#` lines skipped; VALUE
concatenates unquoted chars other than blanks and `` ' " \ $ ` ; & | < > ( ) ``, `'...'` (literal)
and `"..."` (escapes `\"`, `\\`, `` \` ``, `\$` only; a bare `$` or backtick is an error). Anything
else is unparsable output.

### 6.6 Nested holds

A child trusts `BITC_HOLD` only if the ticket exists in the state dir (bare name), is flocked now,
holds the same token, and its `(pid, starttime)` is an ancestor of the caller (`/proc` ppid walk,
depth ≤ 256); else it is ignored silently. **Other pool:** own slot; skips group units the parent
holds and the memory floor. **Same pool:** one nested child per hold at a time through
`slot.<i>.nest.<d>.lock` (`d` = depth), waiting as `nested child busy`, with a `nested = true`
ticket. `run --detach` clears `BITC_HOLD`.

### 6.7 Refuse rules

Before queueing (and before any provider is chosen), the argv is **unwrapped** into simple
commands and the pool's `refuse` rules applied to each. Unwrapping: `VAR=value` prefixes; `env nice
time timeout ionice stdbuf exec command nohup setsid` with option values; `sh/bash/zsh/dash/ksh -c`
strings parsed with `mvdan.cc/sh/v3/syntax` (lists, pipelines, subshells, compounds, nested `-c`);
`npx/pnpx/bunx`, `npm exec|x`, `pnpm dlx|exec`; `node <opts> <path>/<tool>(.js)`. An unparsable
`-c` (or `env -S`) string is not parsed further (debug note), but its leading simple command is
still checked: the words before the first unquoted operator or redirection character (`;` `&` `|`
`<` `>` `(` `)`, newline), split on blanks with quotes removed, leading `!`/`{`/`if`/`then`/`elif`/
`else`/`while`/`until`/`do` dropped, a digit-only word glued to `<`/`>` dropped, and unwrapped like
an argv; a match refuses (a shell still runs that command, e.g. `npx eslint << EOF` with the
here-document never closed). Later commands of such a string are not checked. Rules: `{tool,
when = "always"|"repo-wide", unless_args, skip_option_values}`, `{npm_script = [...]}` (`npm|pnpm|yarn run <s>`), `{argv_prefix = [...]}`.
`repo-wide` = no path argument, or one (after expanding `$PWD`, `${PWD}`, `$(pwd)` and `` `pwd` ``,
and resolving symlinks) that is the cwd, an ancestor, `/`, the work-tree root or a top-level dir of
it. Any other `$(...)` or backtick word is a path argument that is not repo-wide. A match → 64, no
history line.

## 7. Detached jobs

- `run --detach` re-execs `bitc __supervise <job>` (`Setsid`, stdin `/dev/null`, output to the log)
  and returns once it has opened the log and written `spec.json`; it survives SIGHUP.
- Record `jobs/<wt8>/<name>/`: `gen` (bumped per rerun under a record flock), `spec.json` (with the
  log path), `status.json` (`queued|running|ended`, atomic), `out.log` (unless `--log`),
  `result.json`. A rerun moves the old generation to `prev-<gen>/`; a superseded supervisor keeps
  its fds and never writes the new one.
- `wait`, `logs`, `cancel` resolve `<job>` in the cwd's work tree, or take `<wt8>/<name>`. A
  supervisor that died without `result.json` is marked `lost`; `wait` prints `RESULT starved`, 75.

## 8. Providers

### 8.1 Interface and capabilities

A provider runs one job away from this host's admission path. Each is a Go package implementing:

```go
type Provider interface {
    Info() Info                                                    // Type, Caps, Endpoints, Arch
    Preflight(ctx context.Context, j *JobSpec) error               // network-free; ErrIneligible{why}
    Estimate(ctx context.Context, j *JobSpec) (Micros, error)      // price x (expiry - now), 8.3
    Launch(ctx context.Context, j *JobSpec) (Handle, error)        // stamps install/pool/size/job/expiry
    Watch(ctx context.Context, h Handle, out Sink) (Result, error) // phases, live output, exit code
    Output(ctx context.Context, h Handle, s Stream, off int64, w io.Writer) error // full output from off
    Fetch(ctx context.Context, h Handle, a ArtifactSpec, w io.Writer) error       // Caps.Artifacts
    Owner(ctx context.Context, id string) (string, error)          // the BITC_OWNER tag it was created with
    Teardown(ctx context.Context, id string) error                 // idempotent, bounded (10s)
    List(ctx context.Context, prefixes []string) ([]Resource, error) // account resources with these name prefixes
    Spend(ctx context.Context) (Micros, error)                     // Caps.Spend: provider spend today (UTC)
}
type Caps struct {
    LiveLogs, Artifacts, Spend, SelfDelete bool
    FallbackUntil Boundary // the latest fallback boundary this provider supports: UntilResource or UntilAttach (8.2)
    SendEnv  EnvLevel // the default caller-env level: EnvNone < EnvForwarded < EnvForced (host key send_env; 8.4)
    MultiJob bool     // a resource serves more than one job: forced names never reach it (8.4, 10)
}
// Info.MaxName: the longest resource name the provider accepts, at most 63 (0 means 63); a plugin declares it in configure and it is at least 60
// Endpoints: every host the provider talks to (API and results)
// Arch: the linux architectures it can run ("amd64", "arm64"); empty means amd64 only
// Resource: ID, Name, Install, Pool, Job, SizeKey, USDPerHour, Created, Expires, State (starting|running|exited|gone)
```

Every provider must launch, report the exit code, return full output after exit, tear down and
list its resources. `Watch` alone defines the phases `booting`, `begun`, `ended`, `lost`. `JobSpec`
carries job id, pool, argv, cwd relative to the work tree, remote env, code ref (SSH clone URL, ref,
sha), size key, timeout, absolute deadline, artifacts, packages, tools, setup, services,
root-step grant, repo id, the deploy secret name and three independent 256-bit randoms: the
results **bearer** B, the result **MAC key** K and the **marker nonce** M (8.4). Without an
optional cap the shared code: `LiveLogs`, prints output once from `Output` after the end;
`Artifacts`, treats a pool with declared artifacts as ineligible; `Spend`, caps on the ledger and
live resources alone; `SelfDelete`, relies on cleanup and the sweeper (`doctor --remote` warns).

`Launch` returns a `Handle` only once job input has reached the resource (the job has **attached**).
Two error types carry a resource id: `ErrPreAttach{ID, Err}` (a resource was made, but no job input
reached it; `Err` is `ErrTransient`, `ErrUnavailable`, `ErrIneligible` or `ErrSetup`) and
`ErrAttached{ID, Err}` (the failure came after attach; for an external provider `Ambiguous` marks a launch with no definite outcome, 8.2 rule 5a). `ErrConfig{Why}` is a configuration error found only when a provider starts (an external provider's `configure`): 78. A `Launch` error with neither is a
pre-resource failure: the provider returns one only when it knows nothing was created, or after
its own name lookup (8.2) found nothing. RunPod declares `resource`: its create request carries the
job's spec, so the create is the attach.

| Error | Before the boundary | At or after it | Breaker |
| --- | --- | --- | --- |
| `ErrIneligible` (argv, size, architecture, request too large, caller env above `send_env`) | next provider | — | no |
| `ErrTransient` (capacity, 429/5xx, network, cap, `max_live`) | no resource: one create retry (8.3), then next provider. `ErrPreAttach{ErrTransient}` under `attach`: one retry only after the discard is confirmed gone (8.3), then next provider | 75 | after BEGIN; and each resource discarded before attach |
| `ErrUnavailable` (auth, no balance, account disabled) | next provider; one `pause` record | 75 | pause |
| `ErrSetup` `repo` (8.4) | 69 | 69 | no |
| `ErrSetup` `infra` | as `ErrTransient` (provisioning before attach arrives wrapped in `ErrPreAttach`) | 75 | after BEGIN; and each resource discarded before attach |
| command failure | — | its own code | no |

### 8.2 Selection and fallback

Walk the pool's host `providers` list (minus `providers_exclude`, `--local`, `BITC_PROVIDER`; or
the `--provider` list) in order. `local` runs locally (6). A remote provider is **eligible** iff
configured on the host, the pool has a `remote_limits.size` for its type, the repo has
`[pools.<p>.remote]`, the work tree matches `remote_limits.worktrees`, the caller is not nested,
and preflight (8.4) passes; otherwise try the next, silently when remote was never asked for (no
profile, no size, tree unmatched, nested, forced local), else with one `running <pool> locally:
<why>` note when `local` is the next provider in the walk, or one `<pool>: trying <next>: <why>` note
when the next provider is remote (4). When the next provider is not `local` (a remote provider
follows, or nothing does: a remote-only pool's last provider, or `local` listed only before it),
an `ErrIneligible` failure prints `bitc: <provider>: <pool>: not eligible for remote: <why>` instead, never the `running
locally` or `trying <next>` note; the walk goes on to a remote next provider, and at the walk's
last provider (or with `on_failure = "stop"`) the call ends with 69 and that line alone: no
STARVED line and no local run. Other failures there print no note and end by the no-provider-left
rule below (75 for the cap, `max_live`, the launch lock or a failed push; a present but bad
credential file is 78 at load; no key at all, or an `api_key_file` that does not exist, is not a
load error but preflight's `ErrUnavailable` and ends 69 with `bitc: <pool>: no provider left:
<provider>: no credentials (set api_key or api_key_file)`). A failure **before the fallback boundary** (credentials, cap, `max_live`, breaker, a create that
left nothing, and under `attach` a resource discarded before attach) with `on_failure = "next"`
notes it (as above) and tries the next provider (a local `wait` starts afresh); with `"stop"` it
stops.

**Fallback boundary.** Job input is anything specific to this job: the spec (argv, cwd, remote env,
B, K, M, deadline), the code ref or repo content, the deploy key and forwarded env values; the
remote profile's `packages` and `tools` are provisioning, not job input. The `resource` boundary
is the first moment a provider-side resource for this job exists or may exist (`Launch` returns a
handle, or a create request has an outcome other than a definite "nothing was created"). The
`attach` boundary is the first moment job input has been sent to the resource, or a request that
may have started the agent with job input has been sent and not definitely refused (an error
status received before the provider accepted it). `attach` is never earlier than `resource`. The
effective boundary is the host key `providers.<t>.fallback_until` (5.3), default and maximum the
provider's `Caps.FallbackUntil`; a value past the declaration is 78. Rules, in order:

1. **After attach the job never falls back.** `ErrAttached`, or any failure once `Launch` returned
   a handle, tears the resource down and exits 75 with `STARVED <pool> (<provider>: <why>)`, so a
   half-started remote job never re-runs its load on this host (15.8).
   `on_failure` and `fallback_until` play no part.
2. **`ErrPreAttach` with effective boundary `resource`** is treated as rule 1: teardown, 75.
3. **`ErrPreAttach` with effective boundary `attach`:** the resource is torn down (10 s bound) and
   is **confirmed gone** when the teardown succeeded or answered "gone"; otherwise it is left for
   the sweep. Each attempt has its own resource name, ledger `launch` and `end` (8.3) and
   reservation. One breaker `failure` is written per resource discarded (8.3). A discard whose
   inner error is `ErrTransient`, whose resource is confirmed gone and which is the job's first,
   is retried once on a new resource (new name and reservation, cap and `max_live` re-checked);
   `ErrUnavailable`, `ErrIneligible`, an unconfirmed discard and the second discard are never
   retried. Then the inner error follows the pre-boundary path: `on_failure` decides between
   stopping and the next provider, `ErrUnavailable` writes its `pause` record.
4. **A `repo`-class setup failure** (8.4), before or after attach, exits 69 with the line below,
   whatever the boundary, because the next provider would fail the same way (15.9).
5. **A create with an unknown outcome** under `resource`: the name lookup applies as follows. A
   create that failed but left a resource named like the job (found and torn down by name) counts
   as a resource that existed: 75, never a retry or a local run. Only a create after which the
   name lookup succeeded and found no such resource falls back; a failed lookup proves nothing and
   is 75 too (no retry, no local run). Under `attach`, a create that carries no job input and has
   an unknown outcome is `ErrPreAttach` with the name as `ID`; a failed lookup leaves it
   unconfirmed (rule 3): no retry, and the sweep proves ownership from the owner tag.
5a. **An external provider's launch with no definite outcome** (a timeout, a signal, the plugin's death, a malformed or second final frame, a forbidden phase event, an error of an unknown class) is read as attached under both boundaries: the adapter lists by the job's full name on a fresh plugin process, tears down each match whose owner tag verifies (`; <name> left for sweep` when it cannot confirm), and the call is 75 `STARVED <pool> (<provider>: launch ambiguous: <why>)` with one breaker failure; there is no retry and no next provider, `local` included. A classified error without an `id` is a pre-resource failure only if a list by the job's full name then finds no resource of this job; if it finds one (owner verified) the resource is torn down and the call is 75 `launch said <class> but <name> exists`, and a failing list or owner call is ambiguous. A plugin's `pre_attach` (its `id` the assigned name) is definite only under an effective `attach`; under `resource` it is read as attached.
6. **Notes.** One per provider passed over: `running <pool> locally: <why>` when `local` is the
   next provider, `<pool>: trying <next>: <why>` when the next provider is remote. After a
   discarded resource `<why>` ends `; <id> torn down before attach`, or `; <id> left for sweep`.

The one exception to rule 1: a setup failure the agent classifies as the repo's own (8.4, `repo`) exits 69
with `bitc: <provider>: <pool>: setup step <name> failed: <first error line>`, because retrying
cannot help (15.9). **No provider left:** 69 if the last failure
was `ErrUnavailable` or `ErrIneligible`, else 75. A remote job takes no local slot, group unit or
priority ticket; `max_live` and the cap bound it.

### 8.3 Shared rails (every remote provider)

Rail state is per provider in the accounting dir (9), shared by every state dir of the account on
this host. The cap is **account-wide per provider account**, not per host: `committed` below counts
the provider's own spend figure for the whole account (`Spend()`) and every live resource on the
account with the configured prefixes, whichever host or install launched it. Every host using one
provider account should set the same `daily_usd`; each checks it against the account total, so a
second host does not add its own allowance. Only reservations (a launch between its check and its
create) and the ledger are host-local, so hosts launching at the same moment can overshoot by at
most one job estimate each; a provider without `Caps.Spend` cannot see other hosts' finished jobs,
so there the cap is account-wide for live resources only. Each provider type has its own
`daily_usd`. **Times:** deadline
`D = create + start_timeout + timeout`; expiry `E = D + 15m` (in the name); estimate
`price × (E − now)` (for an external provider the larger of the host's `sizes.<k>.usd_per_hour` price and the plugin's own figure, for the estimate and for every listed resource; a negative or non-integer figure from the plugin counts as over the cap). **Cap and `max_live`**, checked under `launch.lock` (wait ≤ `launch_wait`;
released before the job runs):

```
L         = List(name_prefix, compat name_prefix if set)          # error counts as over the cap
committed = max(Spend() if Caps.Spend else 0, ledger_today)       # error counts as over the cap
          + Σ own-prefix resources in L × price × (E − Created)
          + Σ compat-prefix resources in L × highest size price × (start_timeout + longest timeout + 15m)
          + Σ reservations whose name is not in L × their estimate
          + this job's estimate
launch iff committed ≤ daily_usd  and  |L| + |reservations not in L| < max_live
```

Day = UTC. A reservation (`{pid, start, name, estimate, expiry}`) is added under the lock before
the create and kept until the job's ledger `end`; it counts only while its resource is unlisted,
so a running job is counted once. Dead pids' and expired reservations are pruned. Each attempt
(8.2 rule 3) has its own reservation. For an attempt whose resource was discarded before attach
and not confirmed gone, the reservation is kept as an **orphan** (`orphan: true`, tied to no pid)
until a later teardown confirms the resource gone or its `E` passes; it counts like any
reservation while its name is unlisted. **An orphan never shields a resource:** the sweep and the
cap check of any later launch for that provider (under `launch.lock`) tear down at once every
listed resource whose owner tag verifies and whose only reservation on this host is an orphan,
and drop the orphan on a confirmed teardown. Own resources
past `E` whose owner tag verifies are torn down during the check and not counted (K8). Over the cap
→ fallback note `daily cap $<daily_usd>: spent $<s> + committed $<c> + this job $<j>` (two
decimals; K7's pinned cases and figures are ported as tests); at `max_live` → `<n> live of
max_live <m>`. Ledger lines: `launch`, `begin`, `end {minutes, est_usd, exit|"fallback"|
"interrupted"}`, one `launch` and one `end` per attempt name (a resource discarded before attach
ends `fallback` with `created: true`, and its minutes count in `ledger_today`). **A create that
left nothing is retried once (unchanged; the wait before the retry ends at once on a signal).
Under an effective `attach`, a pre-attach discard is retried once, only after the discarded
resource is confirmed gone. Under `resource`, no retry follows a create that left a resource.**
A job has at most one retry in total. After a failed or interrupted create, resources named like
the job are torn down. Signal handling stays active until all cleanup has finished:
a later signal is absorbed and the exit code stays 128+the first signal.

- **Breaker:** `failures.jsonl` holds `{ts, kind: "failure"}` (each 75 after BEGIN, each
  resource discarded before attach, and each ambiguous plugin launch) and `{ts,
  kind: "pause", until, why}` (each `ErrUnavailable`, `until` = now + `window`). Launch is refused
  while `failures` records fall within `window` or a pause is ahead (`<provider> is paused: ...`);
  pre-launch capacity errors never count, nor does a create that left nothing.
- **Local fallback and the 75 contract:** 8.2 and 4.
- **Cleanup on interrupt** (idempotent, every exit path and signal, in order): tear down (10s
  bound); reap by name if a create was interrupted; remove the artifact staging dir; ledger `end`;
  drop the reservation; release the launch lock; hand ref deletion to a detached process; remove
  temp files; signal → `128+N`.
- **Leak control**, three layers: agent self-delete at `D + 10m`; host cleanup; `bitc sweep`. The
  sweep tears down `name_prefix` resources past `E` whose `Owner` tag equals
  `HMAC(install_secret, name)` (or that have a ledger `launch` line), and, at once and whatever
  their `E`, resources whose only reservation is an orphan and whose tag verifies (printing
  `swept <type> <name> (orphaned by a pre-attach discard)`); it deletes own refs older than 3h, and
  prunes dead reservations (an orphan is pruned only at its `E`). `--foreign` also tears down, after listing them, other
  installs' `name_prefix` resources past `E` (a lost install secret, another host). Without
  it they stay counted and `status` and `doctor --remote` name them. A stopped resource (self-delete
  fell back to stop) still bills for disk until swept, so run the sweep timer (3.3).

### 8.4 Remote jobs (shared by remote providers)

**Preflight**, in order, network-free until the last step: git work tree; clean tree (no
uncommitted or untracked changes); argv matches an `allow` entry; no argument names a host path
outside the tree or a git-ignored path; stdin is `/dev/null`, a TTY, an empty file, or a
pipe/socket with nothing readable on a zero-timeout `poll(2)`, and no `--stdin` or lone `-`
argument; no `forbid_ignored` files; artifact paths relative, inside, untracked, not `.git`, no
symlink component; root steps only with the repo's `root_steps` grant; a usable clone URL; request
under the provider's limit; the architecture (the job runs on the first entry of the pool's `arch`
that the provider's `Info().Arch` lists; with none in common the job is ineligible, `arch arm64 not
allowed by pool (arch = ["amd64"])`, and a tool name with no entry for that architecture is
ineligible, `tool node has no arm64 entry`; the provider receives only the tool entries for it);
then credentials, breaker, launch lock, cap and `max_live`. Remote
stdin is `/dev/null`. **`allow`** entries are argv prefixes, matched (and sent) with an absolute
`argv[0]` reduced to its basename: a plain entry matches exactly; one ending in `"--"` also allows
`-- <args>`; one ending in `"*"` allows any trailing args.

**Code transport** (`git-push`, the only v1 transport). Clone URL: the repo grant's `clone_url`,
else derived from `transport.remote`: `https://[userinfo@]<host>/<path>[.git]` →
`git@<host>:<path>.git` (userinfo never sent; a port → ineligible, set `clone_url`); SSH and
scp-style URLs are used minus any password. `repo_id` = 16 hex of sha256 of that URL. The clone
host's keys must be in `transport.known_hosts` (OpenSSH format; default: GitHub's, built in and
updated per release), else ineligible. bitc pushes exactly `HEAD` (through the `git` pool if
defined) to `<ref_prefix><install16>/<pool>/<sha12>-<unix>-<rand8>`; the agent fetches it with the
repo's read-only deploy key (the provider secret named by the repo grant's `deploy_secret`) and
checks the tip is the sha. Default prefix `refs/bitc/jobs/` (no
branch CI fires; `refs/heads/bitc/` if the forge refuses custom refs). The ref is deleted once the
command starts; the sweeper removes own refs older than 3h. Refs on a public forge are
world-readable until deleted.

**Remote env**, built from empty: forwarded (repo `forward_env` ∩ host `forward_env_allow`; a name
containing, in any case, `TOKEN`, `SECRET`, `KEY`, `PASSWORD`, `PASS`, `CREDENTIAL`, `AUTH`, `DSN`,
`PRIVATE`, `COOKIE`, `SESSION`, `CERT`, `DATABASE_URL` or `DIRECT_URL` must also be in
`forward_env_force`, else 78; every entry of `forward_env`, `forward_env_allow` and
`forward_env_force` must be a valid env name (5.2), else 78); `[defaults]` and pool env keys as in 6.3 (remote templates); then `[remote]`, then
`[pools.<p>.remote]` `env` and `env_unset`. No internals or secrets. Forwarded values travel in the
create env, which every member of the provider account can read.

**Caller env per provider.** The host key `providers.<t>.send_env = "none" | "forwarded" |
"forced"` (host-only, 5.3) says how much of the caller's forwarded env may reach that provider.
The job's *forwarded names* are those of `forward_env` ∩ `forward_env_allow` that are set in the
caller's environment (an empty value counts, an unset name does not); *forced names* are the
forwarded ones listed in `forward_env_force`; the rest are *plain*. `none`: no caller env crosses.
`forwarded`: plain names cross, forced names never. `forced`: forced names may cross too (the
behaviour of v3.3). The default is `Caps.SendEnv` (RunPod `forced`, so nothing changes for it; an
external provider `forwarded`). Not caller env, so outside this key: `[defaults]`, pool and
`[remote]` `env` values, B, K, M and the deploy key. Preflight (network-free) checks the level
before the cap check, any reservation, create or launch: a job whose need (`none` with no
forwarded name, `forced` with at least one forced name, else `forwarded`) is above the provider's
effective level is `ErrIneligible` there, with no resource, reservation, ledger line or breaker
record, and the walk goes on (8.2). bitc never strips names to make a job fit. The note names the
names, sorted and comma-separated, never a value:
`forwarded env not sent to <t> (providers.<t>.send_env = "none"): <N1>, <N2>`, or, for a forced
job at `forwarded`, `forced env not sent to <t> (providers.<t>.send_env = "forwarded"): <F1>`
(forced names only). A provider whose resources serve more than one job (`Caps.MultiJob`; a
plugin declares it as `caps.multi_job` in `hello`) never gets a forced name: `send_env = "forced"`
on it is 78 (`providers.<t>.send_env: "forced" is not allowed: <t> reuses resources across jobs`;
for a plugin, checked after `hello` at config check, doctor and first use), its default
is at most `forwarded`, and preflight caps its level at `forwarded` whatever is configured. A bad
value is 78 (`providers.<t>.send_env: must be "none", "forwarded" or "forced": "<v>"`).
`config explain providers.<t>.send_env` prints the effective level and its source (`default` or
`host`).

**The agent (`bitc __agent`)**, the same binary (linux/amd64 and linux/arm64) for any remote provider:

1. **Delivery:** a POSIX `sh` start stub (< 2 KiB) fetches the agent with curl or wget (else
   installs curl), all under `env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin`, checks
   `BITC_AGENT_SHA256` and execs it. Source `release` (default): this version's GitHub release
   asset `bitc_<ver>_linux_<arch>` (the bare binary; the sha256 is the one `checksums.txt` lists for
   it, or `sha256(os.Executable())` when this is that release build), usable only by a
   release-pipeline build (stamped `release=true`) once a public release exists (15, item 4). Other builds need `url`:
   an owner-hosted HTTPS URL (`{arch}` in it expands to `amd64` or `arm64`) plus `path`, a local
   linux/amd64 bitc whose sha is sent, or `paths = { amd64 = ..., arm64 = ... }`, one such file per
   architecture (`path` is the alias of `paths.amd64`; an architecture with no entry makes the job
   ineligible, `agent paths has no arm64 entry`; a `url` without `{arch}` serves amd64 only).
   Source `build` (`agent = { source = "build", commit, image, sha256 }`) publishes nothing: the
   host pushes `commit` of the job repo to the hidden ref `refs/bitc/agent/<install>/<pool>/<commit12>-<unix>-<rand>` beside
   the job ref (both deleted with it; `bitc sweep` ages out a leftover one after 3h), the pod runs `image` (the provider image is overridden;
   `<name>@sha256:<digest>`, a golang image with git), fetches that one ref shallow with the
   deploy key under `transport` known hosts, shreds the key, builds with the one recipe in
   `internal/agent/buildrecipe.sh` (`CGO_ENABLED=0 GOFLAGS=-mod=readonly GOTOOLCHAIN=local go build
   -trimpath -buildvcs=false`, stamped ldflags, fresh caches) and execs the result only if its
   sha256 equals `sha256`, else prints `bitc agent: build sha256 mismatch` and exits 1. Get the
   value with `scripts/agent-build-sha.sh COMMIT` after `scripts/repro-check.sh COMMIT` (both need
   the network for modules and are not part of `make ci`). `bitc doctor --remote` reports
   `build <commit12> on <image>`. The scripts take an optional architecture
   (`scripts/agent-build-sha.sh COMMIT [amd64|arm64]`, the same for `repro-check.sh`): the recipe
   cross-compiles with `GOOS=linux GOARCH=<arch>` (CGO is off), and the sha is reproducible per
   architecture. `source = "build"` with `module_dir` instead (no `commit`, `image` or `sha256`)
   is for development hosts and for providers that send the agent's bytes over their own
   session: bitc runs `CGO_ENABLED=0 GOOS=linux GOARCH=<arch> go build -trimpath -ldflags=-buildid=
   ./cmd/bitc` in `module_dir` into `<account>/agents/<arch>/<sha256>`, once per architecture and
   tree state (`git rev-parse HEAD` of a clean tree; an unclean or non-git tree is ineligible at
   run; `doctor --remote` prints a `fail` and exits 78). A provider that fetches the agent by URL cannot use it.
2. **Boot (root), before any package, tool or repo code:** bind the results listener on the
   provider's port and hold it for the container's life; set `oom_score_adj` to -1000; move the
   spec (`BITC_SPEC`) and secrets from PID 1's env through a pipe and re-exec (same PID, empty env,
   spec on fd 3, listener on fd 4); print `__BITC_BOOT__ <M>`. **Exec delivery** (providers whose
   channel is an exec session, not a container environment) boots with `bitc __agent --boot-stdin`
   instead: one payload on stdin, an 8-byte big-endian length then that many JSON bytes (`spec`,
   `deadline`, `deploy_key`, optional `results_root`; at most 8 MiB). EOF before the declared length,
   a zero length or unparsable JSON prints `refused: truncated payload` on stdout and exits 65, and
   a spec whose `port` is not -1 is refused too; nothing is started. Stage 1 puts the payload
   without the deploy key in a memfd on fd 3 and the key in a second one on fd 5, re-execs with an
   empty env, and stage 2 reads and closes both before any package, tool or repo code. Port -1 is
   **no-listener mode**: nothing is bound, the agent's stdout is also appended to `agent.log` in
   the results directory, it records `agent.pid` (`<pid> <start time>`; the command's group goes
   in `job.pid`), and it exits after `result.json` (a second boot of the job prints
   `__BITC_LOST__ <M>` and exits).
3. **Deadline:** absolute `BITC_DEADLINE` (D) from the create env only, never a volume or disk.
   The command is killed at `min(BEGIN + timeout, D)` (a host gives up on a begun job 2 minutes after that, counting from the BEGIN it saw, and ends it as lost with the timeout error; 8.7); at `D + 10m` the agent runs the self-delete
   hook registered for `BITC_PROVIDER_TYPE` (8.5), against the API base in `BITC_API_BASE`; in a test run (unprivileged, test root) the agent refuses a non-loopback base and makes no call.
4. **Setup as root:** packages (`^[a-z0-9][a-z0-9+.-]*$`, from the image's distro); tools (name `^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`, as are setup step and service names, rejected at load and by the agent), each
   downloaded to root-only local disk, sha256-verified on that copy (any setup failure prints
   `__BITC_SETUP_FAILED__ <M> <name> <class> <first error line>`, class below), extracted in-process to `/opt/bitc/tools/<name>/` (a single top dir
   stripped; `.tar`, `.tar.gz`/`.tgz`, `.tar.bz2`/`.tbz2`, `.tar.xz`/`.txz`, `.tar.zst`/`.tzst` only; modes masked to `0755`, never chowned; device, fifo and hard-link entries refused as `repo`), run only from there, `<dir>/bin` (else `<dir>`) prepended to the job PATH; code fetch
   with the pinned known hosts; then shred the deploy key, remove `~/.ssh`, unset
   `GIT_SSH_COMMAND` **before any repo code or step runs**; root steps and services (in order,
   each with a readiness argv and timeout) only with `root_steps`.
5. **Job user** (unprivileged, remote env): `run_as = "job"` steps, then the command
   in `<workdir>/<cwd>`, stdin `/dev/null`, stdout and stderr to separate `out.log` and `err.log`.
   The agent runs under umask 022 whatever it inherited (the build stub holds 077 only while the key exists), and the directories the job user traverses are chmodded explicitly; a command that cannot start ends 127 with `bitc agent: cannot start <argv0>: <error>` (no env) in `err.log` and the agent log.
   Provider-log markers `__BITC_BEGIN__ <M>`, `__BITC_EXIT__ <M> <code>`; others are plain output.
   **Setup failure class:** `repo` when a repo `setup` or root step, or a service process, exited
   non-zero by itself, or a tool's sha256 did not match its pin; `infra` for everything else
   (package install, any download or network error, code fetch, a readiness timeout, a step killed
   by a signal or the OOM killer, an agent error). When in doubt the agent says `infra`.
6. **Results** in `/run/bitc/<job>/` on container disk (root, 0700, `.part` then rename):
   `out.log`, `err.log`, `artifacts.tar.gz` (regular files and dirs under declared paths, symlinks
   not followed), `result.json` (`exit`, `out_bytes`, `err_bytes`, `out_sha256`, `err_sha256`,
   `artifacts_sha256`, `mac` = HMAC-SHA256(K, the other fields as canonical JSON)).
   `state.json` (`phase`, `exit` when ended, `job`, `nonce` M, a per-job monotonic `seq`, `ts`, `mac`)
   is MACed and bound like `result.json`: HMAC-SHA256(K, `bitc-state-v1\n` + the other fields as
   canonical JSON). The host counts a state only if the MAC verifies under K for this job and M and
   its `seq` is not below the highest accepted; anything else is ignored. The job's exit and outcome
   always come from the verified `result.json`, never from `state.json`.
7. **Stay alive** serving results until torn down (a container whose main process exits is
   restarted; in no-listener mode the agent exits instead and the host reads results by exec). On finding `/run/bitc/<job>/ran` it prints `__BITC_LOST__ <M>` and idles.

**Exec results path** (no-listener mode; authorization is the exec session itself, so there is no
bearer): `bitc __agent [--root DIR] read <job> <file> <offset>` writes the file from the offset
(`<file>` is one of `agent.log`, `out.log`, `err.log`, `result.json`, `artifacts.tar.gz`,
`state.json`; never a `.part`, a link or any other name; `<job>` matches
`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`; every open is `O_NOFOLLOW`); `tail <job> <off_agent> <off_out>
<off_err>` streams JSON lines `{file, off, data}` (data base64, at most 64 KiB) from those offsets,
reading a log's `.part` while the job runs, with `{hb, alive}` every 15 s (`alive` is the agent's
recorded pid and start time) and a final `{done}` once `result.json` exists and everything is
sent, or after a heartbeat with `alive` false when the agent is gone without a result; `list`
prints one JSON line `{job, phase, alive, exit}` per job; `teardown <job>` kills the job's group
and the agent only while their recorded pid and start time still match, removes the job directory
and prints `torn <job>` (`gone` for an unknown job). A bad argument exits 64, an offset past the
end 65, a missing file or job 66, all with nothing on stdout. `--root` defaults to `/run/bitc`.

**Results server** (providers with HTTPS ingress): only `GET|HEAD /v1/{state.json,out.log,
err.log,artifacts.tar.gz,result.json}` with `Range`, only with `Authorization: Bearer <B>`
(constant-time compare, never in a URL); otherwise 404 without body, or 405 for other methods. B
is never logged and K never leaves the spec; logs carry only M.

**Host side.** Output streams as `Watch` delivers it; on exit `Output` completes each stream to
`result.json`'s byte counts (printing the V4 note) and checks the sha256s. `result.json` counts only
if its MAC verifies under K and its exit equals the `__BITC_EXIT__` marker when one was seen; else
75 `<id> result failed verification`. A `repo` setup failure → teardown, the 69 line of 8.2, no breaker
record. An `infra` setup failure, an unparsable or missing class, restart, loss or start timeout
before BEGIN → teardown and `STARVED <pool> (<provider>: <why>)`, 75, never fallback (8.2); after BEGIN, LOST, a
second BOOT, a changed pod start, resource gone, or `min(BEGIN_seen + timeout, D)` plus the 2 min grace passed → `STARVED <pool> (<provider>: <why>)`, 75, breaker
failure. Marker lines never reach `pod:` echo or history; B, K, M
and secret values join the redacting writer. **Artifacts:** HTTPS only, size-capped, resumable;
validate the whole archive first (regular files and dirs; declared paths and parents; no absolute
or `..`; unpacked bytes and entries capped); extract into `<wt>/.bitc/staging/<job>` (bitc creates
`<wt>/.bitc/.gitignore` = `*`, so the tree stays clean) with `O_NOFOLLOW` walking; under the work
tree's artifacts lock re-check that no declared path has a symlink component, then swap in.
`replace = true` (default) removes each declared path first, even when the archive lacks it (A4);
`false` merges, archive files overwriting. Staging is removed on every exit path. Failure after a
passing command → 75; after a failing one the code stands.

### 8.5 Adding a provider

A provider is either a compiled-in Go package `internal/provider/<type>`, registered in one table
`type → {decoder, constructor, agent self-delete hook}`, or a **plugin**: an executable
`bitc-provider-<type>` pinned in the host file (8.5.1–8.5.9). The full wire reference, with a JSON
example for every message, is `docs/plugin-protocol.md`; how to build, pin and test one is
`docs/writing-a-provider.md`. **Core decodes** the common keys of every `[providers.<type>]`
section: `api_key` / `api_key_file` (credentials only from the section or the file it names, never
argv), `name_prefix`, `daily_usd`, `max_live`, `launch_wait`, `start_timeout`, `fallback_until`,
`send_env`, `breaker`, `transport`, `agent`, `artifacts` (a plugin section also `plugin` and
`sizes`). The provider's strict decoder, or the plugin's `configure`, gets only the remaining keys
(a plugin also gets `sizes`). **Non-core keys are never secrets.** A known built-in type decodes its
common keys; a section with a `plugin` table is a plugin; any other unknown type is 78. Either kind
must pass the shared provider conformance suite (11) against a fake of its API, and a plugin also
the protocol suite. The rails stay shared.

1. A provider type is **built in** or a **plugin**. Built-in types: `local` (not an implementation,
   8.6), `runpod` (8.7), `sprites`, `ssh`, and `fake` (test only). A plugin may not use a built-in
   type name (78).
2. The core wraps a plugin in an adapter that implements the Go `Provider` interface (8.1), so
   the walk, preflight (8.4), rails (8.3), orchestration, artifact validation and sweep code are
   the same for both kinds. Nothing in 8.2–8.4 branches on "is a plugin".
3. A plugin is an **API client only**. bitc core still names resources, computes the owner tag,
   holds `launch.lock`, reservations, ledger, breaker and cap, parses markers with M, verifies the
   result MAC under K, validates artifacts, and runs cleanup and sweep. The remote side is still
   bitc's own agent (8.4), which the plugin delivers with `JobSpec.AgentURL` and `AgentSHA256`.
   Keeping these in core makes a **buggy** plugin fail safe. It does not contain a hostile one
   (8.5.8).

#### 8.5.1 Discovery and pinning (host-only)

```toml
[providers.modal]                                  # <type> = "modal"
plugin = { path = "/usr/local/libexec/bitc/bitc-provider-modal",
           sha256 = "<64 lowercase hex>",
           env_pass = ["HTTPS_PROXY", "NO_PROXY"],   # optional; default []
           secret_files = { token_secret = "~/.config/bitc/modal.token-secret" } }  # optional
api_key_file = "~/.config/bitc/modal.key"           # core keys as 8.5
daily_usd = 5
max_live = 2
sizes = { cpu8 = { usd_per_hour = 0.60 } }          # core for plugin types: required (8.5.6.1)
# every other key goes to the plugin's configure request, as JSON; never a secret (8.5.2.3)
region = "eu"
```

1. `plugin` is a **host** key. In a repo file it is 78, like every `[providers.*]` key (5.3).
2. bitc never looks the plugin up on `PATH`. The basename of `path` must be
   `bitc-provider-<type>`.
3. **At load:** open `path` with `O_NOFOLLOW`. It must be a regular ELF file, executable, owned by
   the user or root and not group- or world-writable, and so must every parent directory. It must
   have no setuid or setgid bit and no `security.capability` extended attribute (file
   capabilities), because either would run it above the account. Its sha256 must equal `sha256`.
   Any failure is 78 (8.5.7).
4. **At spawn:** open again with `O_NOFOLLOW|O_CLOEXEC`, re-check the mode bits and the xattr on
   the open fd, hash it, compare, then exec that same fd, so the bytes hashed are the bytes run.
   Go's `SysProcAttr` can set `Pdeathsig` and `Setpgid` but has no `PR_SET_NO_NEW_PRIVS` field
   and no `execveat`, so the spawn goes through a shim: bitc starts its own binary as
   `/proc/self/exe __plugin-exec` with the plugin fd as fd 4 (fd 3 is the secrets pipe, 8.5.2.3).
   The shim locks its OS thread, calls `prctl(PR_SET_NO_NEW_PRIVS, 1)`, then sets `FD_CLOEXEC` on fd 4 again, then calls
   `execveat(4, "", argv, envp, AT_EMPTY_PATH)` by raw syscall. No-new-privs is inherited across
   the exec, so even a bit set after the check (a race with root) cannot raise privileges. A
   mismatch here (the file changed since load) is `ErrUnavailable{Local}`:
   `bitc: <type>: plugin <path> changed since load (sha256 <want12>, now <got12>)`. A mode or
   xattr change is `bitc: <type>: plugin <path>: insecure: setuid/setgid/capabilities`.
5. Scripts are not plugins, because a `#!` interpreter breaks the fd-exec pin. A plugin written
   in an interpreted language ships as a single ELF (for example a PyInstaller or Deno compile).

#### 8.5.2 Process, environment and secrets

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
     lowercase hex, 8.5.4.1). Forwarded env values sit in `job.env`, and only the names that
     `providers.<t>.send_env` lets cross (8.4); a job that needs more never reaches `launch`.
   - **Redaction before write.** The spawn-time secrets (`api_key`, each `secret_files` value)
     join the redacting writer before the spawn. Each `launch`'s per-job secrets join it **before
     the `launch` frame is written**: the deploy key bytes, B, K and M in raw and lowercase-hex
     form, and every forwarded value whose name is in `forward_env_force` or matches the 8.4
     secret-name list. The plugin's stderr (8.5.2.4, debug lines and the 20-line protocol-error
     tail) passes through that writer, so a plugin that echoes its request on stderr leaks none
     of them. They leave the redactor when the bitc process exits.
4. **stderr** lines are redacted and printed only at `BITC_LOG=debug`, as
   `bitc: <type>: plugin: <line>`. The last 20 lines are attached to a protocol error.
5. **Exit:** bitc closes stdin when done. The plugin exits within 2 s, or gets SIGTERM, then
   SIGKILL after 2 s. A plugin that dies mid-session is respawned once for later calls (cleanup,
   `Teardown`, sweep); a second death in the same bitc process makes that type
   `ErrUnavailable{Local}` for the rest of the process. A death while a `launch` is open makes
   that launch **ambiguous** (8.5.4.4), never `ErrUnavailable{Local}`.

#### 8.5.3 Framing and handshake

1. **Framing:** one JSON object per line (NDJSON, UTF-8, `\n`), each line ≤ 1 MiB. Binary data
   (`Output`, `Fetch`) is base64 in `data`, ≤ 512 KiB decoded per frame.
2. **Request:** `{"id": <uint>, "method": "<name>", "params": {...}}`.
   **Event** (streaming methods only): `{"id": <same>, "event": "<kind>", ...}`.
   **Final:** `{"id": <same>, "result": {...}}` or `{"id": <same>, "error": {...}}`; exactly one
   per request, after its events. A frame for an unknown or finished id is a protocol error
   (8.5.5), with one exception: once bitc has **cancelled** a call (a timeout or an interrupt,
   8.5.4.3) or has stopped a `watch` on a `lost` phase (8.5.4.2), the events and the final that the
   plugin still sends for that id are dropped silently, since the plugin cannot know that bitc has
   stopped listening. The drop ends with that id's final; any later frame for it is an unknown-id
   protocol error again.
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
   `Caps.MultiJob` (8.4)). Optional: `secrets`
   (default `[]`). A `type` that differs, or a `protocol` not in `protocols`, is a protocol error.
   `caps` come before `configure` because the 78 checks of `fallback_until` (8.2) and
   `secret_files` need them.
5. **configure** follows `hello`: `{"config": {<the section's non-core keys, as decoded TOML>},
   "sizes": {<the core sizes table>}}`. It must be network-free. The result carries everything
   that depends on config:
   ```json
   {"endpoints": ["api.eu.modal.example"], "arch": ["amd64"], "max_name": 63}
   ```
   All three are required: a missing member is a protocol error, while an empty `endpoints: []`
   is valid (a plugin that contacts no host, such as a local one or the author guide's toy).
   `max_name` must be at least **60**, else it is a protocol error that
   `config check`, `doctor` and the walk report as 78 (8.5.7). 60 is the shortest name core can
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
   out-of-range value (8.5.6.1) is a protocol error.
7. **Versioning:** `protocol` is an integer. A change that a v1 peer could misread bumps it: a new
   required field, a changed meaning or type, or a new method that bitc depends on. New optional
   fields do not (8.5.3.6). bitc 1.x supports protocol 1 for its whole major version.

#### 8.5.4 Methods (1:1 with `Provider`, 8.1)

| Method | params | result / events | bitc timeout |
| --- | --- | --- | --- |
| `hello`, `configure` | 8.5.3 | 8.5.3 | 5 s each |
| `preflight` | `{job}` | `{}` | 5 s (network-free) |
| `estimate` | `{job}` | `{"usd_micros": <int>}` | 10 s |
| `launch` | `{job, secrets?: {"deploy_key": "..."}}` | `{"id", "name", "results_url"?}`; events `setup {line}` (pre-attach output) | `start_timeout`; expiry → ambiguous (8.5.4.4) |
| `watch` | `{handle}` | events `phase {phase, why}`, `log {line}` (raw provider log, markers included), `out {stream, data}`, `heartbeat {}` at least every 30 s; result `{exit, began, why}` | none; 90 s with no event → `lost` |
| `output` | `{handle, stream, offset}` | events `data {data}`; result `{bytes}` | 60 s with no event |
| `fetch` | `{handle, max_compressed, offset}` | events `data {data}`; result `{bytes}` | 60 s with no event |
| `owner` | `{id}` | `{"owner": "<tag>"}` | 30 s |
| `teardown` | `{id}` | `{}` (idempotent; "gone" is success) | 10 s (8.3) |
| `list` | `{prefixes}` | `{"resources": [Resource...]}` (8.1 fields, snake_case) | 30 s |
| `spend` | `{}` | `{"usd_micros": <int>}` (only with `caps.spend`) | 30 s |

1. `job` is the 8.1 `JobSpec` in the **protocol-1 wire schema** below, with `name` and `owner`
   already set by core. The plugin must create the resource with exactly that `name` (8.5.3.5) and
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
   | `tools` | array of `{"name", "url", "sha256", "arch"}` strings | `Tools` (8.4) |
   | `setup` | array of `{"name": string, "run_as": "root" \| "job", "argv": [string]}` | `Setup` |
   | `services` | array of `{"name", "run_as", "argv", "ready": [string], "ready_timeout_s": int}` | `Services` |
   | `root_steps` | bool | `RootSteps` |
   | `test` | `{"unprivileged": bool, "test_root": string}`, **present only under `[test]`** | `Unprivileged`, `TestRoot` |

   Every key except `test` is always present (empty arrays as `[]`, an unused B as 64 zeros).
   **Omitted:** `DeploySecret` (a RunPod secret name, meaningless to a plugin); the deploy key
   bytes travel only in `secrets.deploy_key`, never in `job`. A field added to `JobSpec` later
   reaches plugins only by an edit of this table (optional, so no protocol bump, 8.5.3.7). The
   protocol suite holds the golden file `conformance/plugin/testdata/job-v1.json` (row PL10).
   The agent does not read this object: it reads `BITC_SPEC` (or a `--boot-stdin` payload), the
   base64 JSON `agent.Spec` of 8.4, whose fields differ from the wire job (durations as strings,
   artifacts as path strings, plus `known_hosts`, `port`, `unprivileged`, `test_root`). The
   translation is specified in `docs/plugin-protocol.md` 6A.
2. **Markers stay in core.** The plugin forwards raw provider log lines as `log` events. The
   adapter parses `__BITC_BOOT__`, `__BITC_BEGIN__`, `__BITC_EXIT__`, `__BITC_LOST__` and
   `__BITC_SETUP_FAILED__` with M, and turns them into `Sink.Phase` and `Sink.Setup` exactly as a
   built-in does. A plugin's own `phase` event can only report `booting` or `lost` (a resource gone
   or restarted). `begun` and `ended` come only from markers. On a `lost` phase bitc stops the
   `watch`, sends `cancel` for it and takes `lost` as the outcome: the final the plugin still
   sends (any `transient` error will do) and any further events for that id are discarded
   (8.5.3.2), so the plugin must not invent an `exit` or `began` for a resource it cannot see. `result.exit` must equal the
   `__BITC_EXIT__` code, or the core treats the result as failed verification (8.4). Core then
   completes output by `output` and checks `result.json`'s MAC under K. **What this protects:** a
   buggy plugin that mislabels phases, drops a log tail or mixes up results is caught. **What it
   does not:** the plugin holds K, B and M, so a hostile plugin can emit
   `__BITC_BEGIN__ <M>` and `__BITC_EXIT__ <M> 0` and write a `result.json` whose MAC verifies.
   Only trust in the pinned binary covers that (8.5.8).
3. **Timeouts:** on expiry bitc sends `{"id": 0, "method": "cancel", "params": {"id": <n>}}`. For
   every method except `launch` and `watch`, the call is then an `ErrTransient` with `why = "<method> timed
   out after <d>"`. A `watch` with no event for 90 s is read as `lost` (a `lost` phase, then
   `<id> was lost: no event from the plugin for <d>`), not as a transient. A bitc interrupt cancels
   a call the same way but is not reported as `timed out`. A `launch` that times out or is cancelled is ambiguous (8.5.4.4). If the plugin
   has not answered 5 s after the `cancel`, bitc kills and respawns it (8.5.2.5).
4. **Ambiguous launch.** Once bitc has written a `launch` request, its outcome is **definite**
   only if it ends in a well-formed final frame that is a `result`, or an `error` of a class in
   8.5.5 whose rules below say "definite". Every other outcome is ambiguous: a timeout, a bitc-side
   cancel (signal), the plugin's death or exit, a malformed frame, a second final, a phase event
   that 8.5.5 forbids, or an `error` with an unknown class (with or without `id`). On an ambiguous
   launch, the adapter returns `ErrAttached{ID: <the name core assigned>, Err: ErrTransient{why}}`
   and core then:
   1. reaps by name: `list` with the job's full name as the only prefix (on the respawned plugin,
      8.5.2.5), checks `owner` against the tag core computed, and calls `teardown` on the match. A
      dead or failing plugin leaves the name to the sweep (`; <name> left for sweep`);
   2. exits 75 with `STARVED <pool> (<type>: launch ambiguous: <why>)` under **both** boundaries.
      There is no retry and no next provider, `local` included. Only the plugin knows whether job
      input was sent, so core reads every ambiguous launch as attached. This is stricter than the
      built-in rule of spec 8.2 (a lookup that finds nothing may fall back): a plugin's `list` is
      plugin-reported, so "not found" proves nothing here;
   3. keeps the job's reservation until `E` if the reap did not confirm the name gone (8.3).

   The only way a plugin's launch failure can reach the next provider after a request was written
   is a definite error: a classified error **without** `id` (the plugin states that nothing was
   created) that core's own name check then confirms (8.5.4.5), or `pre_attach` from a plugin whose
   effective boundary is `attach` (8.5.5).
5. **Name check after an id-less error.** A plugin bug can create a resource and still answer
   `transient` without an `id`. Before any retry or next provider after an id-less classified
   `launch` error, core runs `list` with the job's full name as the only prefix and, for each
   returned resource, `owner`:
   1. nothing returned: the error stands as pre-resource (8.2 path, retry once if `transient`);
   2. a resource whose `owner` equals the tag core computed: read as attached. Core tears it down,
      writes one breaker `failure`, and exits 75 with
      `STARVED <pool> (<type>: launch said <class> but <name> exists)`;
   3. the `list` or `owner` call fails or times out: read as ambiguous (8.5.4.4: reap by name, 75).

   This closes "two live resources for one job" for a buggy plugin whose `list` works. A plugin
   whose `list` also omits the resource is not caught here; provider-suite row PS1 (8.5.9.2) is the
   defence for that.

#### 8.5.5 Error classes (8.5.4 `error` object)

`{"class": "<class>", "why": "<one line>", "id"?: "<resource id>", "step"?: "...",
"setup_class"?: "repo" | "infra"}`

| class | Maps to | Notes |
| --- | --- | --- |
| `ineligible` | `ErrIneligible{why}` | |
| `transient` | `ErrTransient{why}` | |
| `unavailable` | `ErrUnavailable{why}` | one `pause` record (8.3) |
| `unavailable_local` | `ErrUnavailable{why, Local: true}` | no pause |
| `pre_attach` | `ErrPreAttach{ID: id, Err: <inner>}` (8.1, 8.2) | `launch` only; needs `id` = the assigned name and `inner: {class, why}` with inner class `transient`, `unavailable` or `ineligible`; definite only when the effective boundary is `attach`, else read as attached |
| `attached` | `ErrAttached{ID: id, Err: <inner>}` (8.1, 8.2) | `launch` only; needs `id`; definite |
| `setup` | `ErrSetup{step, setup_class, why}` (8.4) | `watch`, or `launch` after attach (then definite, attached) |
| `config` | 78 (8.5.3.5) | `configure` only |
| anything else, a malformed frame, a second final for one id, a frame for an unknown or finished id, a `begun`/`ended` `phase` event; a `configure` result missing `endpoints`, `arch` or `max_name` | protocol error: `ErrUnavailable{Local}` | `bitc: <type>: plugin protocol error: <why>`; no pause; 69 if it was the last provider. **During an open `launch` it is an ambiguous launch instead (8.5.4.4): 75**. Not a protocol error: events and the final for an id bitc has cancelled or stopped on `lost` (8.5.3.2), and `endpoints: []` |

**`launch` errors, in full.** `ineligible`, `transient`, `unavailable` and `unavailable_local`
**without** `id` are pre-resource failures (the 8.2 pre-resource path) once the name check of
8.5.4.5 finds nothing. The same classes
**with** an `id` are read as attached under both boundaries (teardown by that id and by name, 75).
`pre_attach` is definite only under an effective `attach`; under `resource` (declared or narrowed)
it is read as attached. Any other `launch` outcome is ambiguous (8.5.4.4), which is also read as
attached. A plugin that can fall back after creating a resource must say so with `pre_attach`.

#### 8.5.6 Rails stay in core

The rails run in core so that a buggy plugin cannot weaken them. They are not a boundary against
a hostile plugin, which runs as the account and can read and write the accounting dir itself
(8.5.8).

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
      detect this at run time. The defence is provider-suite row **PS1** (8.5.9.2), run with
      `pluginTarget` against the author's fake: it compares `list` with the fake's own live set
      across pages and after each job's end. The protocol suite (8.5.9.1) cannot see provider state
      and does not cover it. PS1 proves only what the fake models: a fake that paginates or
      filters unlike the real API leaves the omission uncovered.
2. **Concurrency:** several bitc processes on one host (and several hosts) may run the same plugin
   at once. A plugin must be stateless across processes, or keep shared state in
   `BITC_PLUGIN_SHARED` (8.5.2.2) with its own `flock`s.
3. **Sweep:** `bitc sweep` calls `list`, `owner` and `teardown` as for a built-in. Only resources
   whose owner tag verifies are torn down (10).
4. **Self-delete:** the agent's `D + 10m` self-delete hook (8.4 step 3) exists only for built-in
   types. For a plugin type the agent makes no call. `caps.self_delete = true` is allowed only when
   the platform itself enforces a TTL from the job (for example a max-runtime field set at create);
   else `doctor --remote` warns as for any provider without it.

#### 8.5.7 Validation (78) and stderr lines

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
`STARVED <pool> (<type>: launch said <class> but <name> exists)` (8.5.4.5); `bitc: <type>: plugin:
<line>` (debug only, redacted). `doctor` adds
`ok|fail plugin-<type>: protocol 1, <plugin_version>, sha256 <12>`.

#### 8.5.8 Security

A plugin is fully trusted host code (repeated in 10). It runs as the account with the account's
real home (never above it: setuid, setgid and file capabilities are refused and it runs under
`PR_SET_NO_NEW_PRIVS`), receives the provider credential on a pipe and each job's spec (K, B, M
included), and is trusted like a built-in provider. bitc hands it no other secret, but nothing stops it from
reading the install secret (and so forging owner tags), the ledger, reservations or another
provider's key file, because they are files of the same account. Core's marker parsing, result
MAC check and rails protect against a buggy plugin, not a hostile one, which can defeat them all.
Pinning by absolute path and sha256 stops an accidental or `PATH`-based substitution, not a hostile
plugin the owner installed. Install only plugins you would run as yourself. A plugin sandbox
(a separate uid or Landlock) is a later design (14).

#### 8.5.9 Conformance for plugins

1. **Protocol suite** (`conformance/plugin/`, new): runs a plugin binary against a scripted bitc
   side and checks framing, handshake and version refusal, one final per id, concurrent ids,
   `cancel`, timeouts, crash and respawn, the error-class table (8.5.5), that `begun`/`ended` phase
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
   | PL9 | `configure` with `max_name` 59 → 78 with the 8.5.7 message; 60 → names of at most 60 characters with `job8` and `E` intact, for a 10-character prefix and size key |
   | PL10 | the `job` object of a scripted `launch` equals the golden file `testdata/job-v1.json` byte for byte after key sorting: snake_case keys, hex B/K/M, RFC 3339 times, `timeout_s`, no `deploy_secret`, no `test` without `[test]`; the deploy key only in `secrets` |
   | PL11 | fault mode `create-then-transient-no-id`: the plugin creates the resource and answers `transient` without `id` → the 8.5.4.5 name check finds it, teardown, one breaker `failure`, 75 (`launch said transient but <name> exists`), no retry, no next provider; the same with `list` failing → reaped as ambiguous, 75; a true id-less `transient` (nothing created) → one retry, then `trying <next>` |
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
   | FB15 | **Nothing crosses before attach.** Required of every `Target` that declares `attach`, `pluginTarget` included. The suite seeds canaries: B, K and M set to fixed values (`fixed_secrets`), the argv sentinel `ARGV-CANARY-7f3a9c` and the env value `ENV-CANARY-51d2e8` forwarded through `forward_env`; a deploy-key canary is not yet implemented. It runs `ok`, `preattach-fails` and `setup-fails-after-create`, and asserts that no canary, in raw, hex or any of the four base64 forms (and, for B, K and M, their hex-decoded bytes and encodings), appears in the `METHOD path?query`, body or `Name: value` header lines of any `GET /__bitc/calls` entry before the first one marked `attach`. Under `ok` the attach call must contain the argv sentinel and the env value as raw text (so the check is not vacuous). A `Target` with no marked call fails under `ok` and `setup-fails-after-create`; under `preattach-fails` a marked call is not required and every call is searched. A plugin that declares `attach` and fails FB15 is not conformant |
3. **Reference plugin** `examples/plugins/bitc-provider-fakex/` (Go): the existing `fake`
   provider's logic behind the protocol. CI runs both suites against it, so the adapter is proven
   to behave exactly like the compiled-in `fake`. The two must give the same scenario results.
4. **Author guide** `docs/writing-a-provider.md`: build, pin, write the fake, run both suites. A
   plugin that passes both may call itself "bitc conformant (protocol 1)". The wire reference,
   with a JSON example for every message, is `docs/plugin-protocol.md`.

### 8.6 Provider: local

Built in, always available, and not an implementation of the interface: it is section 6. It may
sit anywhere in a pool's `providers` list (default `["local"]`), is the only fallback target and
costs and leaks nothing, so no rail applies. Hooks run only here.

### 8.7 Provider: RunPod

CPU pods; the first remote provider and the only one in M2. Caps: all four. Keys: `image` (needs
`sh` and curl, wget, apt or apk) and `sizes` required; `api_base` (`https://api.runpod.io`),
`proxy_domain` (`proxy.runpod.net`), `cloud` (`SECURE`), `disk` (`40GiB`), `port` (`8090`);
optional `data_center`. `Endpoints` = `api_base` and `*.<proxy_domain>`. M2 mounts no network
volume (caches: 16.1).

- **API:** REST v2 only (`POST /v2/pods`, `GET /v2/pods`, `GET/DELETE /v2/pods/{id}`, billing for
  `Spend`); create bodies under 100 KiB, checked before sending.
- **Name** (no labels on create): `<name_prefix>-<install16>-<pool>-<size>-<job8>-<E unix>`; only
  the pool may contain `-`, so it parses from both ends (pool truncated if too long). **Owner tag:**
  `BITC_OWNER` in the create env, read back by `Owner` via `GET /v2/pods/{id}`. **Pod status** is the `status` field of `GET /v2/pods[/{id}]`. **Price:**
  `max(usd_per_hour of the size key, API cost)`; unknown size keys get the highest price.
- **Create body:** `image`, `cpu {id, vcpuCount}`, `cloud`, `dataCenterIds`, `disk`, `ports
  ["<port>/http"]`, `entrypoint ["/bin/sh","-c"]`, `cmd [stub]` (no other top-level key: the API refuses unknown ones with a 422), env `BITC_SPEC` (base64),
  `BITC_AGENT_SHA256`, `BITC_API_BASE` (the provider's configured API base, for self-delete), `BITC_DEADLINE`, `BITC_OWNER`, `BITC_PROVIDER_TYPE`, and the repo's deploy
  key as a `{{ RUNPOD_SECRET_<name> }}` reference, never a literal.
- **HTTP mapping:** 2xx ok; 400 with capacity text → transient; other 400/422 → ineligible (logged
  at debug without env; the 4xx error line carries the API's message, first line only, never the env); 401/402/403 → unavailable; 413 → ineligible; 429/5xx → transient.
- **Watch:** before BEGIN, container-log SSE in ≤ 60s slices, each a `tail=5000` read (never
  `since=`: the live API answered it with nothing, 2026-10-23), deduped by timestamp and line, as
  `pod: <line>`. Log reads are spaced (2s before BEGIN, 5s after, when they only have to deliver
  markers); a 429 backs them off (its `Retry-After` when sent, else 2s doubling to 30s) without
  delaying the status poll (15s), the start timeout or the deadline. After BEGIN the results server
  `https://<pod>-<port>.<proxy_domain>/v1/<file>` carries the output: `out.log` and `err.log` are
  served while they grow (the agent serves the `.part` under the final name), tailed by `Range` from
  the forwarded offset every 2s, so output reaches the caller while the job runs; `result.json`, the
  artifacts and `state.json` are served only once committed. Proxy calls never carry the API key.
  **The log stream can miss the end of a job** (seen live), so `Output` always completes stdout and
  stderr from the results server.
- **Self-delete hook:** DELETE the pod with the pod-scoped key; if that is refused, stop it (a
  stopped pod bills for disk until swept). The live proof (2.8) checks which happens.
- **Restarts** (seen live): a pod restarts a container whose command exits, so the agent stays
  alive after the job and a restart ends the job as lost. A restart that prints no second BOOT
  (build mode: the host already deleted the agent ref, so the stub's fetch fails and the container
  crash-loops) is seen from the pod's `startedAt`: the host records it at its first status read and
  a later different value ends the job as restarted (while the command ran, or before it started).
- **Timings** (live, `cpu3c` 8 vCPU): cold job with full `npm install` ≈ 90 s, warm ≈ 64 s (the
  ~25 s a volume cache would save is deferred, 16.1; `start_timeout = "20m"` is ample); a 190 MB
  artifact through the HTTPS proxy ≈ 43 s end to end (`max_compressed = "200MiB"` stays; downloads
  resume with `Range`).

## 9. State on disk

State dir default `$XDG_STATE_HOME/bitc` (`~/.local/state/bitc`); never under `XDG_RUNTIME_DIR`
or a cache dir. Dirs 0700, files 0600; refused (78) if group/world-accessible, not owned by the
user, or on NFS.

```
STATE/   SCHEMA (newer → 78, older → migrated), admit.lock, admit/reservations.json,
         pools/<pool>/{slot.<i>.lock, slot.<i>.nest.<d>.lock, tickets/<class>-<ns>-<rand>.json},
         groups/<g>/unit.<j>.lock, history.jsonl (+ .1 .2 .3),
         jobs/<wt8>/<name>/{gen, spec.json, status.json, result.json, out.log}, prev-<gen>/
ACCOUNT/ install-secret (32 bytes; install16 = 16 hex of its sha256),
         remote/<type>/{launch.lock, ledger.jsonl, reservations.json, failures.jsonl},
         locks/artifacts.<sha256(wt)[:16]>.lock
<wt>/.bitc/ .gitignore (*), staging/<job>/   (only during an artifact fetch)
```

**History:** `O_APPEND` lines under 4 KiB: `{ts (UTC, ms), event: "start"|"end"|"starved"|
"fallback"|"reaped", pool, slot, job, pid, nested?, argv: [...], cwd, provider, resource?, sha?,
wait_ms?, run_ms?, exit?, outcome?, reason?}`; refusals write nothing. **Concurrency:** one writer
per file (appends under 4 KiB, temp + rename, or a ticket's in-place write by its owner under
`admit.lock`); no read-modify-write without the owning lock; every lock attempt non-blocking or
bounded; no hold-and-wait; lock files never deleted while in use.

## 10. Security

**Threat model.** bitc guards a shared host against accidents and untrusted repository content,
not against a process acting as the account owner. A repo file cannot raise capacities, enable a
provider, grant root steps or deploy keys, widen the env allowlist, lengthen waits or
remove host refusals; a private `BITC_DIR` or `BITC_CONFIG` shares the fixed accounting dir, so it
cannot multiply the cap; test seams cannot meet real endpoints. Out of scope: a same-user process
can read the key and call the provider itself; members of the provider account can read create
envs (forwarded env, the spec).

- **Secrets** never in argv, child env, logs, history, ledger, URLs or output. Provider credentials
  come only from the provider's section (`api_key`; host file 0600) or a file it names
  (`api_key_file`: key alone on one line, 0600, owned by the user, else 78), used only by Go's
  HTTP client. A redacting writer wraps every log sink; `config` and `doctor` print `<redacted>`.
- **Trust split** (5.3); repo hooks run as the user, like the command, and never remotely.
- **Caller env** (8.4): the host decides per provider how much of the forwarded env may reach it
  (`send_env`). Whatever the setting, forced names never reach a resource that serves more than
  one job (`Caps.MultiJob`): its disk, files, checkpoints and processes can outlive the job.
- **Remote least privilege** (8.4): host-granted per-repo deploy keys; deploy key shredded before
  repo code; env-less agent; listener held from boot; MACed, marker-checked results; bearer never
  logged; no shared volume; validated artifacts; sha-verified agent and tools;
  teardown only with a verified owner tag; narrowest API key.
- **Plugins** (8.5): a pinned plugin is fully trusted host code. It runs as the account with the
  account's real home (never above it: setuid, setgid and file capabilities are refused, and it
  runs under `PR_SET_NO_NEW_PRIVS`), receives the provider credential on a pipe and each job's
  spec (K, B and M included), and is trusted like a built-in provider. Nothing stops it from
  reading the install secret (and so forging owner tags), the ledger, reservations or another
  provider's key file, because they are files of the same account. Core's marker parsing, result
  MAC check and rails catch a buggy plugin, not a hostile one, which can defeat them all. Pinning
  by absolute path and sha256 stops an accidental or `PATH`-based substitution, not a hostile
  plugin the owner installed. Install only plugins you would run as yourself.
- **Root steps:** a repo granted `root_steps` runs code as root in the pod and is trusted with its
  own results; it still cannot reach other repos' keys.
- **No shell, no eval:** commands, hooks and steps are argv arrays; hook output is parsed.
- **Supply chain:** `-trimpath`, `CGO_ENABLED=0`, sha256 `checksums.txt`, Dependabot, `govulncheck`
  in CI. cosign-signed checksums come after v1 (16.2).

## 11. Observability and testing

**Observability:** `status [--json]`, `history.jsonl`, `result.json`, the ledger,
`BITC_LOG=debug`.

| Layer | What | Where |
| --- | --- | --- |
| Unit | config, key classes, defaults table, merges, validators, templates, remote env check, env grammar, unwrapper, refuse rules, tar validator, markers, result MAC, cap arithmetic, name encoding, clone URL derivation | `internal/...`; parsers fuzzed |
| Conformance | black box: run the built binary; assert exit code, stdout/stderr, state-dir files; one test per behaviour ID; the `SE` rows cover the caller-env levels on the fake (two fakes in one pool) and RunPod | `conformance/` (`go test ./conformance -bin=./dist/bitc`) |
| Protocol suite | a plugin binary driven from a scripted bitc side (framing, handshake, one final per id, concurrency, `cancel`, exit on EOF, secrets in no argv or env), and the `PL` rows that pin core's reading of a plugin's output (unknown fields, `configure` data, estimate and `list` range checks, per-process `TMPDIR`, fd 3 secrets and redaction, `max_name`, the `job` golden file, the id-less error name check, pin and no-new-privs) against the reference plugin in fault modes (8.5.9) | `conformance/plugin/` |
| Provider suite | one scenario set (launch, fallback, cap, breaker, lost, restart, interrupt, sweep, dropped log tail, forged result, artifacts) per provider against a fake of its API, plus two rows required of every target: PS1, the provider's `list` equals the fake's live set across pagination and after each job's end, and FB15, for a provider that declares `attach` no job input appears in a fake API call before the call marked `attach`; the RunPod fake runs pods as local `__agent` processes; `pluginTarget` runs it against any plugin (`-plugin`, `-plugin-sha256`, `-plugin-fake`) and the plugin author's own loopback fake, and the reference plugin `examples/plugins/bitc-provider-fakex/` gives the same scenario results as the compiled-in `fake` | `conformance/provider/` |
| Privileged | privilege drop, env-less re-exec, root-owned results, listener held | CI container (`//go:build privileged`) |
| Live proof | real RunPod, maintainer-run, worst-case spend printed first | maintainer-run, not in this repository |

`conformance/behaviours.toml` lists every behaviour ID with a neutral one-line description and
exactly one of `ported = "<test>"`, `changed = "<reason>"` (with its test) or `dropped =
"<reason>"`; a meta-test fails on an unlisted ID, a missing test or a `changed` row absent from
12.1, and each test names its ID. The legacy pool's own tests stay private. **Behavioural
reference:** the IDs, their one-line entries and every ported expectation come from the reference
implementation at a pinned commit, recorded privately with its test count per file. A re-pin (for
example when the reference's pending change lands) diffs the reference's tests between the two
commits and updates `behaviours.toml` before any task cites a changed ID. Project-flavoured rows run
against neutral configs in `examples/node-monorepo/`. `go test -race` everywhere. **Test seams**
(`procfs_root`, `time_scale`, `http_results`, `agent_url`, `accounting_dir`, `fixed_secrets`) are keys of a host
`[test]` table, accepted only when every configured provider's `Info().Endpoints` are loopback
literals (else 78); no env variable or marker file enables a seam.

## 12. Migrating from a shell-based pool (compat mode, time-boxed)

The host table `[compat]` (`legacy_dir`, `hold_env`, `name_prefix`, `groups = { <group> = "<lock
stem>" }`) makes the two pools exclude each other while callers move one at a time. While set:

- bitc slot i also needs exactly `<legacy_dir>/locks/<pool>.<i>.lock` (same index, so a derived
  port or stack slot is never shared); if a legacy caller holds it, slot i is busy. Group unit j
  needs `locks/<stem>.<j>.lock`. Both are taken in the same all-or-none step (`legacy pool: <what>
  busy`); bitc writes its supervisor pid into group locks and exports `hold_env=<stem>.<j>` so
  nested legacy calls verify, and treats a verified legacy claim as nested.
- High tickets are mirrored as `locks/<pool>.prio.<pid>`, and live legacy markers are honoured.
- Remote (only if M2 lands while compat is set): `List` includes the compat prefix; compat
  resources are counted (8.3), never destroyed. bitc also takes `<legacy_dir>/locks/runpod-launch.lock`
  (after its own, within `launch_wait`) and appends `<pid> <name> <usd> <expiry>` to
  `<legacy_dir>/runpod-reservations` for each job, removed at its end, so legacy launches count it.
- Jobs a legacy launcher started before the shim switch have no record: the `wait` shim passes
  `--log <their log>` (3.2).

`status` and `doctor` show legacy holders. Remove `[compat]`, this section and its code a week
after M1 exit.

### 12.1 Deliberate changes from the legacy pool

Each is a `changed` row in `behaviours.toml`; they are listed in 15.

- Config errors exit 78 (legacy 64). Unknown pools are refused (legacy: 1 slot, 600 s);
  `unknown_pools = "default"` restores the old rule.
- `wait`: `RESULT starved` exits 75 under `--exit zero` (legacy 0); a command's own 75 shows
  `RESULT exit 75` (legacy `RESULT starved`), so a loop counting starves by text must also treat
  exit 75 as a retry.
- Detached logs live in the state dir unless `--log`; an uncreatable log exits 64 (legacy 1);
  `pre_run` of a detached job runs after `started`.
- Remote text: notes are `bitc: <provider>: ...` (legacy `runpod: ...`); the launch line names the
  job and size key (legacy: type and vCPU); STARVED is `(<provider>: <why>)`.
- `BITC_ARTIFACTS` may only select declared paths (legacy replaced the list).
- STARVED reasons for groups and memory use the 4 wording; admission is FIFO per class.
- Processes left in a hold's group are killed at its end (legacy left them running).
- A create that left a resource (reaped by name after a failed create) is 75 and never falls back
  to local or retries; signals stay absorbed through all cleanup and cut the create-retry wait short.
- Sweep tears down only resources with this install's owner tag; others need `--foreign` (legacy
  deleted any pool pod past timeout + 15 min, account-wide).
- The host sampler is dropped (`status --json` from a timer replaces it).
- State lives in `~/.local/state/bitc` (`$XDG_STATE_HOME/bitc`), never in a cache dir (legacy kept it
  under the cache dir, which cache cleaners may wipe).
- Internal errors (bitc's own bugs) exit 69 with `bitc: internal error: <msg>` (legacy:
  unspecified); 75 is only for a genuine wait-and-retry.

## 13. Repository layout and release

`cmd/bitc/`; `internal/{config,lock,admit,run,classify,jobs,state,remote,provider,agent,redact}`
(`provider/` holds the interface, registry, `runpod/` and `fake/`); `conformance/`;
`examples/{node,python}-monorepo/`; `docs/`. Module `github.com/Exploded-views/bitc`;
deps `go-toml/v2`, `x/sys/unix`, `mvdan.cc/sh/v3/syntax`; Go ≥ 1.26. GoReleaser on tag: linux
amd64/arm64 tarballs and the bare `bitc_<ver>_linux_<arch>` binaries (what the agent `release` source downloads), sha256 `checksums.txt` listing both (unsigned until after v1, 16.2), `release=true` stamp.
License MIT (`LICENSE`). Install:
`go install github.com/Exploded-views/bitc/cmd/bitc@latest` (Homebrew and npm later, as
`bitc`). SemVer; the public API is the exit codes, stderr lines, config schema and `status --json`.

## 14. Milestones

Each task is test-driven and lands as one reviewed change; "parallel" = no shared files beyond the
first task's interfaces. Letters in "Done
when" are behaviour-ID families in `conformance/behaviours.toml`.

**M1: local parity (≈ 4 days).** Parallel after 1.1: 1.2–1.6.

| # | Task | Depends | Done when |
| --- | --- | --- | --- |
| 1.1 | Skeleton: module, CI (lint, `go test -race`, `govulncheck`), GoReleaser snapshot, conformance harness (temp state dirs, loopback test config, fake procfs), `behaviours.toml`, interfaces stubbed | — | CI green on a `run` that execs |
| 1.2 | Config: key classes, defaults table (5.4), loader, merges, `[[repos]]`, `min_version`, `[test]` gate, validators, templates, remote env check, provider registry stubs (8.5), `config check/explain`, generated reference | 1.1 | every 5.2–5.4 row tested; fuzzed; `examples/config/*` pass `config check`; a host file with `[providers.runpod]` loads |
| 1.3 | Locks: tickets, slots 1..N, groups with cross-pool order, admit lock, priority, prune and reaping, nest locks, inotify + poll | 1.1 | S, T, R, M rows |
| 1.4 | Memory admission: meminfo, cgroup headroom, reservations | 1.1 | F rows |
| 1.5 | Local runner: process groups, TTY, `Pdeathsig`, signals, stragglers, env, hooks | 1.1 | E, X, Y rows; a SIGKILLed supervisor leaves no process |
| 1.6 | Classifier: unwrapper and refuse rules | 1.1 | L rows (~180 table fixtures) |
| 1.7 | `run`: pipeline, exit contract, STARVED text, internal errors, nesting, history, `result.json`, pool names | 1.2–1.6 | P, M, H, N rows |
| 1.8 | Detached jobs: `__supervise`, generations, log terminator, `wait` (incl. `--log`), `logs`, `cancel`, lost | 1.7 | J rows |
| 1.9 | `status`, local `sweep`, `doctor`, `version` | 1.7 | Z rows; status schema test |
| 1.10 | Compat mode (12) | 1.3, 1.7 | no double admission either way against a fake legacy lock dir; legacy holds `db.1` → bitc takes slot 2 and gets port base+2; nested legacy call does not starve; `wait --log` on a legacy log |

M1 exit: a maintainer's host runs a week in compat mode with no STARVED regressions. Its host file may
keep `[providers.runpod]` (an M1 stub, ineligible); the legacy RunPod path stays on meanwhile.
Callers that branch on the legacy `wait` exit codes are
updated before any shim switch (that work lives with the migrating project, not here).

**M2: RunPod provider (≈ 4 days).** Parallel after 2.1: 2.2–2.5.

| # | Task | Depends | Done when |
| --- | --- | --- | --- |
| 2.1 | Provider interface, `Caps`, registry with core/provider key split, selection, preflight, fallback, error mapping and the 69/75 rule, `fake` provider, provider conformance suite | M1 | Q and fallback rows on the fake |
| 2.2 | Agent: stub, boot (listener, OOM shield, env scrub), absolute deadline, self-delete hook, setup, tools, services, job user, results and result MAC, results server, markers, stay-alive | 2.1 | RE, RB, RS rows; privileged job green; forged result rejected |
| 2.3 | RunPod client and fake API (pods, billing, SSE logs, status, owner tag); name encoding | 2.1 | RL rows; HTTP mapping tested |
| 2.4 | Rails: cap, `max_live`, launch lock, reservations, ledger, breaker | 2.1 | K, C rows incl. K7's figures; a running job is counted once; a second state dir cannot exceed the cap; another install's live resource and account spend count against it |
| 2.5 | `git-push` transport, clone URL derivation, known hosts | 2.1 | K1 and clone rows; origin with credentials never sent |
| 2.6 | Host orchestration: launch, watch, output completion, result verification, phases, artifacts, cleanup, `cancel` | 2.2–2.5 | RR, V, A rows; a dropped log tail is recovered |
| 2.7 | Remote `sweep` (owner tag, `--foreign`), `doctor --remote`, `status` remote section, sweep timer runbook | 2.3, 2.4 | RW rows |
| 2.8 | Live proof (maintainer-run; worst-case spend printed first; agent via `source = "url"`) | 2.6, 2.7 | runbook run; self-delete, restart and dropped-tail cases seen live |

**M3: release (≈ 1.5 days).**

| # | Task | Done when |
| --- | --- | --- |
| 3.1 | GoReleaser release, sha256 checksums, `release` stamp, `go install` path; agent `release` source tested on a snapshot | a tagged `v0.x` installs both ways |
| 3.2 | Docs: README quick start, concepts, generated config reference, exit codes, agent guide (retry on 75, re-wait on 124, stop and report on 69), threat model, provider-author guide | a fresh agent sets up a sample repo from the docs alone |
| 3.3 | CONTRIBUTING, issue templates, pre-public scrub of all history, then a fresh initial commit before the first push (LICENSE is already in) | first push; `v1.0.0` |

**Later:** volume caches (16.1, past M2), cosign-signed checksums (16.2, after v1), cgroup limits,
PSI brake, repo-defined pools, `upload` transport, Modal and Fly, named provider instances, a plugin sandbox (separate uid or
Landlock, 8.5.8), command-sourced credentials, macOS, Homebrew/npm, Prometheus.

## 15. Decided

Decisions, 2026-10-09:

1. **License:** MIT, copyright holder `gitgat` (`LICENSE`).
2. **Repository:** `github.com/Exploded-views/bitc`, binary `bitc`.
3. **RunPod volume caches:** deferred past M2; M2 ships with no network-volume cache. Design kept
   in 16.1.
4. **Publish:** only after the M3 scrub; the public history starts as one fresh initial commit
   before the first push. Until a public release exists remote jobs use
   `source = "url"`.
5. **Adopted defaults and the 12.1 list:** approved (78 for config errors; no last-known-good
   config; stragglers killed; state in `~/.local/state/bitc`; `wait --exit zero` with starved =
   75; unknown pools refused; the rest of 12.1), with one change: internal errors exit **69**, not
   75, so a bug that fails every time never loops agents forever; 75 stays only for genuine
   wait-and-retry (starved, provider transient).
6. **Cap:** account-wide per provider account (8.3); `daily_usd = 25` on the maintainer's host.
7. **Checksums:** plain sha256 in M3; cosign signing after v1 (16.2).
8. **No fallback once job input reaches a resource** (owner, 2026-10-09, relayed: "Yes a go";
   amended 2026-10-23): a remote job whose provider's fallback boundary (8.2: `resource` or
   `attach`; a built-in's declaration or a plugin's `resource`, either narrowed or, up to the
   declaration, set by the host) has passed never runs on another provider, `local` included.
   Failures tear down and exit 75 (8.2), as the shell pool it replaces does. A request whose
   outcome is unknown counts as past the boundary if it may have carried job input (a create under
   `resource`, unless a name lookup succeeded and found nothing, 8.2; any attach request; any
   plugin `launch` without a definite outcome, and any plugin `launch` error after which bitc
   finds the job's resource). Before the boundary, failures tear down what exists and follow
   `on_failure`; at most one retry happens, and never while a discarded resource may still be
   live. RunPod's boundary is `resource`, so for RunPod this is the 2026-10-09 rule unchanged:
   fallback only before any resource exists.
9. **Setup failures** (owner, 2026-10-09, relayed: "69 for setup"): a repo's own setup step that
   fails by itself (e.g. a broken `npm ci`) exits 69, do not retry; 75 stays for infrastructure
   (launch, restart, loss, start timeout, post-start). A failure that cannot be classified
   reliably is treated as infrastructure (75). The classes are in 8.4.

The week-long compat trial on a live host (M1 exit) is a separate, later step.

## 16. Later designs (kept, not scheduled)

### 16.1 Volume caches (deferred past M2)

Saves about 25 s a job on RunPod (8.7 timings) at the cost of a provider secret per repo, the
cache MAC, `openat2` and cache-uid code and a privileged test. When it lands it adds `Caps.Caches`
and `Info().CacheMount` (RunPod: `/cache` when the new key `volume_id` is set; the create body
mounts the volume there), the repo grant keys `cache_secret` and `cache_save_branches`, the repo
`[remote] caches` list and a `JobSpec` cache secret name; a provider without the cap ignores
`caches`. Until then those keys are unknown (78). Agent order: cache restore after the code fetch
and deploy-key shred, before root steps; cache save as the job user after setup steps, before the
command.

**Caches** (`Caps.Caches` and a repo grant with `cache_secret`: a per-repo provider secret of 32
random bytes, so no key in a pod opens another repo's caches). Layout
`<CacheMount>/bitc/<repo_id>/<name>/<cache key>.{tar,mac}`, MAC = HMAC-SHA256(key, repo_id ‖ name ‖
cache key ‖ sha256(tar)). The mount is untrusted (any job can read, write or delete it): volume IO
runs as an unprivileged `cache` uid via `openat2(RESOLVE_NO_SYMLINKS|RESOLVE_BENEATH)`; root does
MAC work only on root-only local copies. **Restore** (after code fetch, before any step): copy
`.tar` and `.mac` to local disk, verify the MAC over that copy, hand it to the job user to unpack; a
missing or bad MAC is a miss with a `pod:` note. **Save** (after setup steps, before the command;
only on a miss and when the job's branch matches `cache_save_branches`): root tars `paths` taking
only regular files and dirs (symlinks never followed or stored), MACs it, and the `cache` uid copies
both to the volume by temp + rename. Never cache secrets.

**Network volume** (seen live on RunPod): a MooseFS FUSE mount that enforces **no** Unix
permissions; an unprivileged pod user can write everything on it. Hence the rules above; results,
run marks and the deadline never live there, and `BITC_DEADLINE` comes only from the create env.
Tests then: cache MAC unit tests, a permissionless fake `/cache` in the RunPod fake, and a
privileged case where a tampered or symlinked cache is a miss.

### 16.2 Signed checksums (after v1)

cosign-keyless signatures over `checksums.txt` in the release pipeline, with a verification step
in the install docs. v1 ships plain sha256 checksums.

## Review notes

Review of v3 (2026-10-09) applied, except: (1) the cache key is a per-repo provider secret, not a
host-derived key, because the create env is readable through the API (now 16.1, deferred); (2) other
installs' expired resources are torn down only by `sweep --foreign`, because the owner-tag finding
forbids automatic teardown; (3) `on_signal = "forward-and-wait"` stays, because the e2e pool needs
its cleanup; (4) `daily_usd` is per provider, because each provider bills separately.

## Appendix: open verification items (settle in M2; none blocks M1)

Can the RunPod pod-scoped key delete its own pod, or only stop it? Restricted key scopes; billing
day bucket (UTC assumed); `cost` for CPU pods; does `GET /v2/pods/{id}` return create env
(owner tag); does the proxy pass `Authorization` unchanged, and its connection time limit; does a
restarted container keep its disk (the second-BOOT rule holds either way); does the forge serve
`refs/bitc/...` to a deploy key; RunPod's pod name length limit.
