# bitc

Offload heavy commands to pluggable cloud providers (RunPod, Sprites, SSH, or your own) under a spend cap, with local slot gating as the fallback. Built for machines shared by AI coding agents.

<!-- badges -->
[![ci](https://github.com/Exploded-views/bitc/actions/workflows/ci.yml/badge.svg)](https://github.com/Exploded-views/bitc/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/Exploded-views/bitc.svg)](https://pkg.go.dev/github.com/Exploded-views/bitc)
[![Go Report Card](https://goreportcard.com/badge/github.com/Exploded-views/bitc)](https://goreportcard.com/report/github.com/Exploded-views/bitc)
[![Latest release](https://img.shields.io/github/v/release/Exploded-views/bitc?include_prereleases&sort=semver)](https://github.com/Exploded-views/bitc/releases)
[![License: MIT](https://img.shields.io/github/license/Exploded-views/bitc)](LICENSE)
[![Go version](https://img.shields.io/github/go-mod/go-version/Exploded-views/bitc)](go.mod)
<!-- TODO: coverage badge (needs a coverage service) -->

bitc (big ideas, tiny computer) runs a heavy command (type check, test suite, browser tests) on a
remote provider instead of your machine, as long as the pool's daily spend cap allows it. If the
job cannot be offloaded, or the provider is down, it runs locally in a slot of a named pool. In
the one case where it can do neither in time, it exits **75** ("try again later") and never
pretends your command failed.

The host file says which providers a pool may use, how much it may spend and which work trees may
go remote; the repository says what may leave the machine.

Host file, `~/.config/bitc/bitc.toml`:

```toml
version = 1

[pools.heavy]
slots = 1
providers = ["runpod", "local"]        # preference order; local is the fallback
remote_limits = { size = { runpod = "cpu8" } }

[remote_limits]
worktrees = ["**/worktrees/agent-*"]   # only these work trees may go remote

[providers.runpod]
api_key_file = "~/.config/bitc/runpod.key"
daily_usd = 25                         # the spend cap for this provider account
image = "docker.io/library/debian:bookworm-slim"
sizes = { cpu8 = { cpu_type = "cpu3c", vcpu = 8, usd_per_hour = 0.24 } }
# The pod downloads the bitc agent from a URL you host. The default source, "release", needs a
# release build of bitc, and none has been published yet.
agent = { source = "url", url = "https://example.com/bitc_linux_{arch}", path = "/opt/bitc/bitc" }
```

Repository file, `.bitc.toml`:

```toml
version = 1

[pools.heavy.remote]
allow = [["npm", "run", "coverage", "--"]]   # only these argv prefixes may go remote
artifacts = [{ path = "coverage" }]          # copied back after the job
```

```console
$ bitc run heavy -- npm run coverage --
bitc: runpod: heavy job heavy-3f2a91 on x7k2p0q9 (cpu8) at 9c1e5b0d4a77
...
$ bitc run heavy -- npm run coverage --        # later, near the cap: runs locally instead
bitc: runpod: running heavy locally: daily cap $25.00: spent $24.80 + committed $0.00 + this job $0.40
...
```

The output is shortened and the job and pod ids are examples; the shape of both `bitc:` lines is
real (see [docs/exit-codes.md](docs/exit-codes.md)). Complete files are in [examples/](examples/);
a repository usually also needs a `[remote]` profile for setup steps. Offload is experimental:
see Providers below.

The other half is the shared-host story. Ten coding agents on one 16 GiB machine will each start a
type checker, a browser suite and a database at the same moment, and the machine falls over. A pool
with N slots turns that into a queue, with or without a provider:

```console
$ bitc run lint -- npm run lint        # holds the only slot of pool "lint" for the whole run
$ bitc run --wait 1s lint -- echo hi   # from a second terminal, while the first still runs
STARVED lint
$ echo $?
75
$ bitc status
lint: 1/1 slots held, 0 waiting
  slot 1  d0b9172c  pid 2306089  since 2026-10-25T06:38:03Z  npm run lint
  last STARVED: all slots busy at 2026-10-25T06:38:05.299Z
memory: 12230 MiB available (MemAvailable), floor 6144 MiB
```

## Why it exists

- **Spend-capped offload.** A daily cap per provider account, a live-job limit, a ledger and a
  circuit breaker bound what remote jobs can cost. A job that has not started falls back to the
  next provider and finally to `local`.
- **Pluggable.** Built-in providers for RunPod, Sprites and SSH hosts, and an exec plugin protocol
  for anything else.
- **No daemon.** State is files and `flock(2)` locks. A caller that is killed releases everything
  it held, and the next caller cleans up after it.
- **Exit codes callers can act on.** 75 means wait and retry, 69 means stop and report, 64, 66, 78
  and 124 are bitc's own (see the table below), and every other code is your command's. Agents get a small rule set ([docs/agent-guide.md](docs/agent-guide.md)).
- **Admission, not just locking.** FIFO tickets, priority lanes, groups shared across pools and a
  memory floor that holds a job back instead of letting the kernel kill something.
- **Two config layers with a trust split.** The host file sets capacities, spend caps and
  providers. A repository's `.bitc.toml` can only narrow those limits, never widen them.

**Maturity, plainly:** the local gate is the mature part. Every remote provider is pre-release;
read the Providers and Status sections before pointing one at a real account.

## Install

bitc runs on Linux (amd64 or arm64). macOS (Apple silicon or Intel) gets a client build that runs
local pools; the Linux-only features below are refused there. To install from source you need
Go 1.26 or newer (CI tests 1.27.3):

```sh
go install github.com/Exploded-views/bitc/cmd/bitc@latest
```

Release tarballs for linux and darwin (amd64 and arm64), bare binaries and a `checksums.txt` are published at
<https://github.com/Exploded-views/bitc/releases> once a release exists (no release has been
tagged yet). Download the asset and `checksums.txt` into one directory and verify before use:

```sh
sha256sum -c --ignore-missing checksums.txt
```

Checksums are unsigned for now; signed checksums are planned after 1.0.
[docs/RELEASING.md](docs/RELEASING.md) describes what a release contains.

### macOS client build

The darwin build runs local pools: slots, queues, groups, priority lanes, nested holds, detached
jobs, `status`, `wait`, `logs`, `cancel`, `doctor` and `config`. These stay Linux-only; full
parity is tracked in [issue #3](https://github.com/Exploded-views/bitc/issues/3):

- memory admission (a pool's `memory` key): a config error (78) on macOS;
- remote providers (`ssh`, `runpod`, `sprites` and exec plugins, so `sweep` and
  `doctor --remote` have nothing to manage): any `[providers.*]` table is a config error (78);
- the remote agent (`bitc __agent`) and the plugin exec shim: one `bitc: ... runs on Linux only`
  line and exit 69;
- the parent-death signal: on macOS a command outlives a bitc that was killed with SIGKILL;
- the inotify wake-up: a waiting caller re-checks every `poll` (5s by default) instead;
- the read-only lock view (`/proc/locks`): `status` probes a lock with a brief non-blocking
  `flock`;
- a detached job's log may have an empty line before its `__EXIT__` line.

## Quickstart

Write a host file with one pool, check it, run a command, then run a detached job. The block
below is run by the test suite exactly as written.

<!-- doc-test -->
```sh
mkdir -p ~/.config/bitc
printf 'version = 1\n[pools.lint]\nslots = 1\n' > ~/.config/bitc/bitc.toml
chmod 600 ~/.config/bitc/bitc.toml
bitc config check
bitc run lint -- echo hello
bitc run lint -- sleep 1 & bitc run lint -- echo second; wait
bitc status
bitc run --detach --name demo lint -- sh -c 'echo hi'
bitc wait demo
```

The pool `lint` has one slot, so two callers take turns: the second waits (up to the pool's `wait`,
10 minutes by default) and, if it cannot start in time, exits 75 with a `STARVED lint` line on
stderr. `bitc status` shows holders, waiters and spend. A detached job returns at once with
`started demo pid <pid> log <path>`; `bitc wait demo` waits (by default at most 9 minutes per call,
`detach.wait_timeout`, or `--timeout`) and
prints `RESULT exit <code>` and the last lines of the log.

Commands: `run`, `status`, `wait`, `logs`, `cancel`, `sweep`, `doctor`, `config` and `version`.
`bitc` alone lists the commands; a bad flag prints that command's usage.

## Configuration

There are two files, and both must start with `version = 1`, use strict decoding (an unknown key
is an error) and not be group- or world-writable.

The **host file** (`~/.config/bitc/bitc.toml`, or `--config`, or `BITC_CONFIG`) belongs to the
account:

```toml
version = 1

[host]
memory_floor = "6GiB"          # admission holds a job back below this much free memory

[pools.lint]
slots = 1

[pools.heavy]
slots = 1
wait = "1h"
memory = "8GiB"                # an admission reservation, not a limit on the command
```

The **repo file** (`.bitc.toml` at the top of a work tree) belongs to the project. It can add
environment, hooks and refuse rules, and can only narrow what the host allows:

```toml
version = 1

[pools.heavy]
wait = "45m"                   # narrow only
env = { NODE_OPTIONS = "--max-old-space-size=8192" }

[pools.lint]
refuse = [{ tool = "tsc", reason = "typecheck runs in heavy" }]
```

`bitc config check` validates both. Every key, its class (host, repo, narrow) and its default is in
[docs/config-reference.md](docs/config-reference.md); worked examples are in
[examples/config/](examples/config/), [examples/node-monorepo/](examples/node-monorepo/) and
[examples/python-monorepo/](examples/python-monorepo/). Concepts (pools, groups, tickets, nested
holds, providers) are in [docs/concepts.md](docs/concepts.md).

## Providers

A pool lists the providers it may use, in order, for example `providers = ["runpod", "local"]`.
Remote jobs take no local slot, need the host to allow them, and share the same safety rails: a
daily spend cap per provider account, a live-job limit, a ledger and a circuit breaker. Once a
remote job has started it never falls back to another provider.

| Provider | Where the job runs | State |
| --- | --- | --- |
| `local` | this machine, in a slot | stable, built in |
| `runpod` | a RunPod pod created for the job and deleted afterwards | experimental: implemented and tested against a fake API; a live proof against the real service has not been run |
| `sprites` | a fresh microVM (Fly Sprite) for every job, so many jobs run in parallel | first-class scale-out provider, **not enabled yet**: the egress allow-list is not yet proven enforced and the WSS exec stream is not yet reliable. bitc does not refuse it yet, but do not configure it for real work until a release enables it: today a job would receive the forwarded environment and the deploy key |
| `ssh` | a Linux machine you own, over ssh | experimental: implemented and tested against fakes and, in maintainer-run tests, a real sshd; not yet proven on a live host. Host setup: [docs/ssh-host.md](docs/ssh-host.md) |
| exec plugin | anything, through an executable `bitc-provider-<type>` pinned by path and sha256 | experimental: [docs/writing-a-provider.md](docs/writing-a-provider.md), [docs/provider-author-guide.md](docs/provider-author-guide.md) |

Remote providers leave resources that cost money if a job dies at the wrong moment, so run
`bitc sweep` every 15 minutes from a timer ([docs/sweep-timer.md](docs/sweep-timer.md)) and check
setup with `bitc doctor --remote`. [docs/threat-model.md](docs/threat-model.md) says what bitc
defends against and what it does not.

## Exit codes

| Code | Meaning | What to do |
| --- | --- | --- |
| the command's own | the command ran; `128+N` if signal N killed it | treat it as the real result |
| 64 | usage: bad arguments, unknown pool, refused by a rule or hook | fix the call |
| 66 | no such detached job | check the job name |
| 69 | unavailable: not Linux or macOS, a Linux-only feature on macOS, unusable hook, no usable provider, internal error | stop and report; do not retry blindly |
| **75** | **temporary failure: starved, or a remote infrastructure failure after the job started** | **wait, then run again** |
| 78 | invalid configuration | fix the configuration |
| 124 | `bitc wait` only: the job is still running | call `wait` again |

A command that itself exits 75 shows up as `RESULT exit 75` in `bitc wait`: read the first line,
not only the code. Details and the stderr lines are in [docs/exit-codes.md](docs/exit-codes.md).

## Comparison with alternatives

| | bitc | `flock` | GNU `parallel` / `sem` | GitHub Actions concurrency |
| --- | --- | --- | --- | --- |
| Where it works | processes on one host (plus optional remote providers) | processes on one host | `parallel`: one invocation; `sem`: processes on one host | jobs in one CI system |
| Counting semaphore (N slots) | yes | one lock per file; N needs N lock files | yes (`sem -j N`) | one running job per group |
| FIFO order, priority lanes | yes | no ordering guarantee | `parallel` starts jobs in input order; `sem` does not guarantee FIFO; no priorities | at most one pending run per group (GitLab `resource_group` and Buildkite concurrency groups differ) |
| Memory-aware admission | yes (`memory_floor`) | no | `--memfree` in `parallel` | no |
| Distinct "busy, retry" exit code | 75 | configurable with `-E` | no | not applicable |
| Per-slot environment | yes (`{slot}`, `BITC_SLOT`) | no | job slot number via `{%}` / `$PARALLEL_JOBSLOT` | no |
| Repo-supplied rules under host limits | yes | no | no | no |
| Remote execution with spend cap | optional providers | no | `parallel --sshlogin`, no cap | account-level spending limit, not per-pool |
| Install | one static binary | in util-linux | Perl, GNU parallel | built in |

When not to use bitc:

- You need one lock around a short critical section in a script. `flock` is simpler and already
  installed.
- You are fanning a list of inputs out over cores in one command. Use `parallel` or `xargs -P`.
- Your contention is between CI runs rather than processes on a machine. Use your CI system's
  concurrency controls.
- You need Windows as the gate host, or macOS with memory admission or remote providers. bitc
  is Linux first; its macOS build runs local pools only.
- You need a cluster scheduler with resource accounting across many machines. Use one.

## FAQ

**Is there a daemon or a server?** No. Everything is files and `flock(2)` locks under the state
directory, which must be on a local file system (not NFS).

**Does exit 75 mean my command failed?** No. It means the command did not get to run (or, for a
remote job, that the infrastructure failed after it started; a remote job is not retried
automatically). Run it again.

**What happens when a caller is killed?** The kernel drops its locks. The next caller reaps the
tickets and slots it left behind.

**Can a repository raise its own limits?** No. A repo file can only narrow host limits; a host-only
key in a repo file is an error (exit 78).

**How does an agent wait for a job that outlasts its tool-call limit?** Start it with
`bitc run --detach --name build ...` and call `bitc wait build` repeatedly; `wait` returns 124 until
the job ends. See [docs/agent-guide.md](docs/agent-guide.md).

**Does it enforce a memory limit on my command?** No. `memory` is an admission reservation.

**Where is the full design?** [docs/spec.md](docs/spec.md). The machine-readable status format is
[docs/status.schema.json](docs/status.schema.json).

## Status and roadmap

bitc is pre-1.0 and has not been tagged. Interfaces may still change before 1.0.

- **Stable in intent:** the local gate (pools, slots, groups, priorities, memory admission, nested
  holds, detached jobs, `status`, `sweep`, `doctor`), the exit codes and the config schema.
- **Experimental / pre-release:** every remote provider (see the table above): none has been
  proven live yet, and Sprites is not enabled yet (egress allow-list unproven, exec stream not yet reliable). The
  exec plugin protocol ([docs/plugin-protocol.md](docs/plugin-protocol.md)) may also change.
- **Planned:** a first tagged release, signed checksums, live proofs of the remote providers, and
  packages under the name `bitc`.

The changelog is [CHANGELOG.md](CHANGELOG.md).

## Contributing

Issues and pull requests are welcome. Start with [CONTRIBUTING.md](CONTRIBUTING.md); it covers
building, `make ci`, and the conformance register every behaviour must appear in. Please follow the
[Code of Conduct](CODE_OF_CONDUCT.md) and report security problems privately as described in
[SECURITY.md](SECURITY.md).

## License

MIT. See [LICENSE](LICENSE).
