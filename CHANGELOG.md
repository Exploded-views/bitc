# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project
adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- A per-run time limit for local runs. The host key `pools.<p>.timeout` (a duration such as `30m`
  or `1h30m`; unset means no limit; `0s` is a config error, 78) and `bitc run --timeout DUR`. The
  flag can only narrow: the effective limit is the shorter of the two, and a `--timeout` longer
  than the pool's is accepted with the pool's value still in force. The clock starts when the
  command starts, so the wait for a slot does not count, and `--detach` jobs are limited the same
  way. At the limit bitc prints `bitc: <pool>: time limit <dur> reached; terminated`, terminates
  the command's process group as for a signal (the leader alone on a terminal; SIGTERM, SIGKILL
  after `grace`) and exits 124, the code `timeout(1)` uses; `result.json` and the history `end`
  line say `outcome = "timed-out"`. A remote run ignores `--timeout`; `remote_limits.timeout`
  limits it.
- `result.json` of a local run and its history `start` and `end` lines now carry `head` (the
  work tree's `git rev-parse HEAD`, taken when the hold starts) and `tree_clean` (true when
  `git status --porcelain` is empty, ignored files excluded); `result.json` also carries `cwd`, the
  run's working directory. Outside a git work tree `head` is empty and `tree_clean` is omitted.
- `bitc run --units <group>=<k>` (repeatable, one per group) replaces the pool's unit count of a
  group for that run only. The group must be one the pool lists, `k` must be 1 to the group's
  units, and a pool may cap it with the new host-only key `pools.<pool>.groups_max.<group>`. A bad
  `--units` exits 64 with one line naming the group and the limit, a bad `groups_max` exits 78
  naming the key, and a remote-only pool refuses the flag (64). Admission is unchanged: units are
  all or none, first in first out by ticket time, and a running hold is never preempted.
- `status` (text and `--json`), `result.json` and the history `start` and `end` lines now show the
  units held per group (`"units": {"cpu": 7}`); a waiting ticket shows the units it asks for.
- The 75 line (and `reason` in `result.json` and the history `starved` line) now says what the run
  wanted and what expired: `STARVED heavy (group cpu busy: wanted 2 of 8 units; waited 1d)`,
  `STARVED gate (slots busy; waited 3h)`, `STARVED lint (queue; waited 10m)`. The wait is the
  pool's, after `--wait`, in the config's duration form. Before, a slot wait printed a bare
  `STARVED <pool>` and a group wait named no units or wait. Other reasons are unchanged.
- `docs/local-heavy-pool.md`: a recipe for one machine with a `cpu` group and gate, heavy, light
  and bench pools.

### Fixed

- With `[compat]` set, a RunPod launch is now refused when the legacy launch lock
  (`<legacy_dir>/locks/runpod-launch.lock`) cannot be opened or is not taken within
  `launch_wait`, with `legacy launch lock <path>: busy` (or the open error). It is refused as a
  temporary failure like a busy launch lock: nothing is created, no reservation is written, and
  that provider is skipped. The run ends 75 with
  `STARVED <pool> (no provider left: legacy launch lock <path>: busy)` only when no provider is
  left, and runs locally if `local` comes next in the pool's providers. Before, such a launch
  went ahead without the lock and could slip past the spend cap a legacy launcher was checking
  ([#9](https://github.com/Exploded-views/bitc/issues/9)). Only RunPod takes the legacy lock and
  writes the legacy reservation line; other provider types are unaffected.

## [0.1.3] - 2026-11-02

### Changed

- The remote accounting directory (`~/.local/state/bitc/account`: the launch lock, ledger and
  reservations behind the spend cap) is now refused like the state dir when it is on a network
  filesystem, with `accounting dir <path> is on NFS; bitc needs a local filesystem for flock`. On
  Linux that means an account dir on NFS: a remote job falls back to local with that reason (or
  exits 69 or 75, as for any remote problem) and launches nothing; `bitc config check` and
  `bitc status` exit 78 (status, on a host with remote providers or unfinished reservations, with
  one line on stderr, as for the state dir); `bitc doctor --remote` prints
  `fail accounting dir: ...` where it used to skip the account-dir checks silently. On macOS a
  filesystem that statfs does not flag `MNT_LOCAL` (FUSE, which the type-name list missed) is now
  refused too, for both the state and accounting dirs.
- `bitc sweep` on a host whose home is on a network filesystem no longer just prints the refusal and
  exits 0. It tears down the resources it can prove orphaned without trusting flock (past their own
  expiry, with a verified owner tag or a ledger launch line), lists every other own resource with
  its age and hourly cost without touching it (a live job on another host sharing the home is never
  killed), reports an expired resource it could not verify or tear down on its own line, writes
  nothing to the accounting dir and skips the git ref sweep, and exits 78 afterwards naming what it
  tore down, listed, kept and failed on, so a sweep timer fails visibly. With no usable install
  secret or an unusable accounting dir it tears down nothing and exits 78. Run `bitc sweep` before
  upgrading such a host, then move the home to a local filesystem.

### Fixed

- On macOS, `bitc config check` now reports an unknown provider name in a pool, as Linux does.
- On macOS, `status`, `doctor` and `sweep --dry-run` no longer make a free lock look busy to a
  concurrent admission (a spurious wait, or with `wait = 0` a spurious STARVED or queue exit)
  or to a legacy `flock -n`. They now read lock holders with `fcntl(F_GETLK)`, which takes no
  lock, instead of a brief `flock` probe. Linux is unchanged.

### Known issues

- On Linux without a readable `/proc/locks` (no procfs, an LSM or seccomp denial, or gVisor),
  `bitc status`, `bitc doctor` and `bitc sweep --dry-run` read pool slot holders with a brief
  try-lock, so for that moment a concurrent admission can find a free slot busy. Linux cannot see
  a flock without taking it; tracked for 0.1.4 in
  [#24](https://github.com/Exploded-views/bitc/issues/24).
- macOS builds still refuse every remote provider: any `[providers.*]` table is a config error
  (78). macOS as a client to remote providers
  ([#4](https://github.com/Exploded-views/bitc/issues/4)) needs the exec-plugin work in
  [#26](https://github.com/Exploded-views/bitc/issues/26), and both are planned for 0.1.4.

## [0.1.2] - 2026-10-30

### Changed

- **Breaking:** a host file named by `--config` or `BITC_CONFIG` must now exist. If it does not,
  every command that reads the config (all but `bitc version`) exits 78 with
  `host file <path> (from --config|BITC_CONFIG) does not exist`, where before it ran silently with
  no host config. A missing default host file (`$XDG_CONFIG_HOME/bitc/bitc.toml`) is still fine:
  built-in defaults apply.

### Fixed

- `bitc cancel` right after `bitc run --detach` no longer signals a supervisor that is still
  starting up. A cancel of a queued job is recorded in `status.json`, and the supervisor reads it as
  it starts and ends the job 128+N without running the command. The 5 s start-up wait is gone.
  `cancel` exits 69 when it cannot read or lock `status.json`, keeps a `--kill` request, and reports
  a job whose supervisor never started as ended.
- A cancel signal (`bitc cancel`, or INT, TERM or HUP to a foreground `bitc run`) that arrived in the
  instant before the command started could be lost, and the command then ran to completion. The
  run now ends 128+N.
- `bitc config check` now runs the state dir's checks, so it exits 78 on an existing state dir
  that is group- or world-accessible, not yours, on a network filesystem, a regular file, or
  has an unreadable, unparsable or newer SCHEMA. It also refuses a state dir that is a
  dangling symlink, or missing under one. That is the same exit (78) `status` and `run` give;
  before, `config check` passed and the next command failed. A state dir that does not exist
  yet still passes, and `config check` does not create it.
- `bitc status` now counts a slot held by a legacy shell-pool caller (`<legacy_dir>/locks/<pool>.<n>.lock`
  under `[compat]`) as held, so the pool line reads `lint: 1/1 slots held` where admission already
  reported STARVED, and lists the holder under the pool. In `status --json` the holder is an
  additive entry in the pool's `holders` with `"legacy": true` (job `"legacy"`, empty `since`,
  `cwd` and `argv`, and `pid` 0 when the lock file holds none); `version` stays `"1"` and the
  report-level `legacy` list is unchanged
  ([#15](https://github.com/Exploded-views/bitc/issues/15)).

## [0.1.1] - 2026-10-29

### Changed

- **Breaking:** remote providers are off by default behind `[host] experimental_remote`. Without
  it in the host file, any `[providers.<type>]` table, or a pool naming a provider other than
  `local`, is a config error, so every command (`run`, `status`, `sweep`, `doctor`, `config
  check`, ...) now exits 78 on such a config, pointing at the README's known issues. A repository
  file cannot set the key and no environment variable does. `bitc doctor` warns while it is on.
  On a host whose accounting dir holds unfinished remote jobs, and for `sweep` and
  `doctor --remote`, the error adds a line: set the flag and run `bitc sweep` to clean up what an
  earlier version left. With no provider of that type configured, `bitc doctor` warns and
  `bitc sweep` prints the same hint.
  What to do: a host that runs jobs on a remote provider adds `experimental_remote = true` under
  `[host]` in the host file (a repository file cannot set it). A host that uses only the local pool
  needs no change.
- **Breaking:** Sprites jobs are refused in this release: with a `[providers.sprites]` table or a
  pool naming it, every command except `bitc sweep` and `bitc doctor --remote` exits 78
  (including `run`, `status`, `doctor`, `config`, `wait`, `logs` and `cancel`; `version` reads no
  config), even with `experimental_remote`, because its network policy does not restrict egress
  yet and a job would receive the forwarded environment and the deploy key
  ([#6](https://github.com/Exploded-views/bitc/issues/6)). The refusal comes before any token or
  deploy key file is read. With `experimental_remote`, `bitc sweep` and `bitc doctor --remote` still clean up
  Sprites left by 0.1.0, through a handle that can only list them, check their owner tag and
  delete this install's; the sweep timer keeps working.
  What to do: move Sprites pools to `local` or another provider, and remove the
  `[providers.sprites]` table and any pool naming it. To clean up Sprites a 0.1.0 install left
  behind, keep the table for now, set `experimental_remote = true`, run `bitc sweep`, then remove
  the table.
- The README is now a short landing page: install, a local quickstart the test suite runs, how a run
  is placed, exit codes, status and known issues. Install details, macOS status, providers,
  configuration, a comparison with other tools and the FAQ moved to [docs/install.md](docs/install.md),
  [docs/providers.md](docs/providers.md), [docs/configuration.md](docs/configuration.md),
  [docs/comparison.md](docs/comparison.md) and [docs/faq.md](docs/faq.md).

### Fixed

- `bitc run` no longer loses the process record of a command that exits at once: its pid and
  start time were read after the command could already have been reaped, so a very short command
  (or a loaded host) left the slot's hold without them. They are now read before the command is
  waited on.
- The shipped repository example (`examples/config/repo.bitc.toml`) declared
  `min_version = "1.0.0"`, so a 0.1.0 release build refused it with exit 78; it now declares
  `"0.1.0"`, and every shipped example is checked against a release-stamped build.
- History lines stay a single atomic write (at most 4 KiB after JSON escaping): a long reason, cwd,
  resource, and as a last resort pool, job, provider or sha, is cut at a character boundary and
  marked ` [truncated]`; argv is shortened as before and sets `argv_truncated`.
- A tool archive's symlink can no longer resolve outside the tool dir by climbing through another
  of the archive's links (`sub/s -> ..`, then `sub/u -> s/../..`): a `..` in a link target is
  allowed only before its first name. Writes through links were already refused.
- On an ssh host, the `check-host --doctor` probes (including the reachability-key probe) and the
  job launcher no longer fall back to uid or gid 0 (root), or wrap to it, when an account's ids do
  not parse: a bad id, root's, or one outside 1..4294967294 is an error.

## [0.1.0] - 2026-10-26

### Security

- Built with Go 1.27.3, which fixes GO-2026-6809 (crypto/x509 now distrusts compromised
  intermediate certificates; bitc reaches it through TLS handshakes to provider APIs).

### Added

- The local gate: named pools with N slots, FIFO tickets, priority lanes, groups shared across
  pools, nested holds, and a memory floor for admission. No daemon: state is files and `flock(2)`
  locks.
- Commands `run`, `status`, `wait`, `logs`, `cancel`, `sweep`, `doctor`, `config` and `version`,
  including detached jobs (`run --detach`, `wait`) and `status --json`
  ([schema](docs/status.schema.json)).
- Exit codes that separate "try again" (75) from the command's own result, with 64, 66, 69, 78
  and 124 for the other cases ([docs/exit-codes.md](docs/exit-codes.md)).
- Two config layers: a host file and a repository `.bitc.toml` that can only narrow the host's
  limits; strict decoding and `bitc config check`.
- Remote providers, all experimental and none proven live yet: `ssh`, `runpod`, `sprites` and
  exec plugins. Sprites (a fresh microVM per job, for parallel scale-out) is not enabled yet: its
  egress allow-list is not proven enforced and its exec stream is not yet reliable; bitc does not
  refuse it yet, so do not configure it for real work until a release enables it
  (`bitc-provider-<type>`, pinned by path and sha256). They share a daily spend cap, a live-job
  limit, a ledger and a circuit breaker, with fallback to the local machine before a job starts.
- `bitc sweep` and `bitc doctor --remote` to find and clean up leaked remote resources, and a
  systemd timer recipe ([docs/sweep-timer.md](docs/sweep-timer.md)).
- A conformance suite with a register of behaviours (`conformance/behaviours.toml`).
- Release pipeline configuration (GoReleaser) for linux and darwin, amd64 and arm64, with sha256
  checksums.
- macOS client builds (darwin arm64 and amd64): local pools, detached jobs, `status`, `wait`,
  `logs`, `cancel`, `doctor` and `config`, cross-built and smoke-tested on a macOS runner in CI.
  Linux-only, refused on macOS with one line ([full parity: issue #3](https://github.com/Exploded-views/bitc/issues/3)):
  - memory admission (a pool's `memory` key; config error 78);
  - remote providers (`ssh`, `runpod`, `sprites`, exec plugins; any `[providers.*]` table is a
    config error 78), and with them `sweep` and `doctor --remote`;
  - the remote agent and the plugin exec shim (`bitc __agent`, `bitc __plugin-exec`; exit 69);
  - the parent-death signal (a command outlives a SIGKILLed bitc), the inotify wake-up (waiters
    re-check every `poll`), the read-only `/proc/locks` view in `status` (a brief `flock` probe
    instead), and the last-byte check of a detached log (an empty line may precede `__EXIT__`).
- Project documentation, contribution guide, code of conduct and security policy.

### Fixed

- The agent now keeps inherited file descriptors out of the job and its setup steps. Found by the
  first public CI run; covered by a regression test that leaks fds into the agent.
