# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project
adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html). bitc has not been tagged
yet, so everything below is unreleased.

## [Unreleased]

## [0.1.0] - UNRELEASED

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
- Release pipeline configuration (GoReleaser) for linux amd64 and arm64 with sha256 checksums.
- Project documentation, contribution guide, code of conduct and security policy.

### Fixed

- The agent now keeps inherited file descriptors out of the job and its setup steps. Found by the
  first public CI run; covered by a regression test that leaks fds into the agent.
