# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project
adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

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
