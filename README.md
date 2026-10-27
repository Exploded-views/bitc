<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/assets/bitc-dark.svg">
  <img alt="bitc: big ideas, tiny computer. Heavy commands, offloaded under a spend cap or queued locally." src="docs/assets/bitc-light.svg" width="100%">
</picture>

Run heavy commands on a shared machine without letting them knock it over. bitc queues them in
named pools of slots on your machine, and can offload them to a remote provider (a RunPod pod, a
machine you own over SSH, or a plugin of your own) under a daily spend cap. When a command cannot start in time, bitc exits
**75**, "try again later", and never pretends your command failed.

[![ci](https://github.com/Exploded-views/bitc/actions/workflows/ci.yml/badge.svg)](https://github.com/Exploded-views/bitc/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/Exploded-views/bitc.svg)](https://pkg.go.dev/github.com/Exploded-views/bitc)
[![Go Report Card](https://goreportcard.com/badge/github.com/Exploded-views/bitc)](https://goreportcard.com/report/github.com/Exploded-views/bitc)
[![Latest release](https://img.shields.io/github/v/release/Exploded-views/bitc?include_prereleases&sort=semver)](https://github.com/Exploded-views/bitc/releases)
[![License: MIT](https://img.shields.io/github/license/Exploded-views/bitc)](LICENSE)
[![Go version](https://img.shields.io/github/go-mod/go-version/Exploded-views/bitc)](go.mod)

```console
$ bitc run heavy -- npm test               # waits for a free slot, then runs; exit code is npm's
$ bitc run --wait 1s heavy -- npm test     # from another terminal, while the first still runs
STARVED heavy
$ echo $?
75
```

bitc is one static binary with no daemon. It is built for machines shared by AI coding agents,
and works the same for a person at a terminal.

## Why it exists

Ten coding agents on one 16 GiB machine will each start a type checker, a browser suite and a
database at the same moment, and the machine falls over. Locks alone do not fix that: the agents
also need to know whether to wait, retry or give up, and someone has to keep remote compute from
running up a bill.

- **A queue, not a crash.** A pool with N slots runs N commands at a time, in FIFO order, with
  priority lanes, groups shared across pools and a memory floor that holds a job back instead of
  letting the kernel kill something.
- **Exit codes callers can act on.** 75 means wait and retry, 69 means stop and report, and every
  other code is your command's own. Agents get a [ten-rule guide](docs/agent-guide.md).
- **No daemon.** State is files and `flock(2)` locks. A caller that is killed releases everything
  it held, and the next caller cleans up after it.
- **Spend-capped offload (experimental).** A daily cap per provider account, a live-job limit, a
  ledger and a circuit breaker bound what remote jobs can cost. A job that has not started remotely
  falls back to the next provider and finally to `local`.
- **A trust split.** The host file sets capacities, spend caps and providers. A repository's
  `.bitc.toml` can only narrow those limits, never widen them.

**Maturity, plainly:** the local gate is the mature part. Every remote provider is experimental
and off by default; read [Status](#status-and-roadmap) and [Known issues](#known-issues) before
pointing one at a real account.

## Install

Linux (amd64 or arm64) is the main platform. The macOS builds run local pools only
([macOS status](docs/install.md#macos-status)).

From a release, verified against its checksums:

```sh
curl -LO https://github.com/Exploded-views/bitc/releases/download/v0.1.0/bitc_0.1.0_linux_amd64.tar.gz
curl -LO https://github.com/Exploded-views/bitc/releases/download/v0.1.0/checksums.txt
grep bitc_0.1.0_linux_amd64.tar.gz checksums.txt | sha256sum -c -
tar -xzf bitc_0.1.0_linux_amd64.tar.gz bitc
install -m 0755 bitc ~/.local/bin/bitc      # or any other directory on your PATH
```

Swap `linux_amd64` for `linux_arm64`, `darwin_arm64` or `darwin_amd64`. With Go 1.26 or newer:

```sh
go install github.com/Exploded-views/bitc/cmd/bitc@v0.1.0
```

[docs/install.md](docs/install.md) lists every release asset and what `bitc version --json`
reports.

## Quickstart (local, about two minutes)

This needs no account and no network. Every block below is run by the test suite exactly as
written; the outputs come from a real run, with paths, ids and memory figures that will differ on
your machine.

**1. Describe your machine.** One pool, `heavy`, with one slot:

<!-- doc-test -->
```sh
mkdir -p ~/.config/bitc
cat > ~/.config/bitc/bitc.toml <<'EOF'
version = 1

[pools.heavy]
slots = 1
EOF
chmod 600 ~/.config/bitc/bitc.toml   # bitc refuses a group- or world-writable config
bitc config check
```

```text
ok host /home/you/.config/bitc/bitc.toml repo (none)
```

**2. Run a command through the pool.** bitc waits for a free slot, runs the command and returns
the command's own exit code. stdout is the command's; bitc writes only to stderr.

<!-- doc-test -->
```sh
bitc run heavy -- echo hello
bitc status
```

```text
hello
heavy: 0/1 slots held, 0 waiting
memory: 15350 MiB available (MemAvailable), floor 6144 MiB
```

**3. See "try again later".** While one command holds the only slot, a second caller waits up to
the pool's `wait` (10 minutes by default; `--wait` shortens it here), then gives up with
`STARVED heavy` and exit 75:

<!-- doc-test -->
```sh
bitc run heavy -- sleep 3 &     # takes the only slot
sleep 1
bitc run --wait 1s heavy -- echo second || echo "exit $?"
bitc status
wait
```

```text
STARVED heavy
exit 75
heavy: 1/1 slots held, 0 waiting
  slot 1  19e3b439  pid 10745  since 2026-10-27T18:15:22Z  sleep 3
  last STARVED: all slots busy at 2026-10-27T18:15:24.777Z
memory: 15354 MiB available (MemAvailable), floor 6144 MiB
```

**4. Outlast a tool-call limit.** A detached job returns at once; `bitc wait` blocks for at most 9
minutes per call and exits 124 while the job is still running, so an agent just calls it again:

<!-- doc-test -->
```sh
bitc run --detach --name demo heavy -- sh -c 'echo hi'
bitc wait demo
```

```text
started demo pid 14890 log /home/you/.local/state/bitc/jobs/22bb2cbb/demo/out.log
RESULT exit 0
--- last 60 lines of /home/you/.local/state/bitc/jobs/22bb2cbb/demo/out.log ---
hi
__EXIT__ 0
```

The other commands are `logs`, `cancel`, `sweep`, `doctor` and `version`; `bitc` alone prints
the usage line. Next: add a repository file ([configuration](docs/configuration.md)), give a pool
a memory reservation, or point your agents at [the agent guide](docs/agent-guide.md).

## Offload to a provider (experimental)

> [!WARNING]
> Remote providers are experimental and **off by default**. Their spend accounting is still being
> hardened (see [Known issues](#known-issues)). The `experimental_remote` switch shown here is on
> `main` and ships in the next release; the v0.1.0 binary rejects it as an unknown key (exit 78).

A pool can list providers in order of preference. The host file says which providers a pool may
use, how much it may spend and which work trees may go remote:

```toml
# ~/.config/bitc/bitc.toml
version = 1

[host]
experimental_remote = true             # remote providers are off by default

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
# release build of bitc; this example uses a URL instead.
agent = { source = "url", url = "https://example.com/bitc_linux_{arch}", path = "/opt/bitc/bitc" }
```

The repository says what may leave the machine:

```toml
# .bitc.toml at the top of the work tree
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
real ([docs/exit-codes.md](docs/exit-codes.md)). Both files pass `bitc config check`; a real
repository usually also needs a `[remote]` profile for setup steps (complete files are in
[examples/](examples/)). Run `bitc sweep` from a timer ([docs/sweep-timer.md](docs/sweep-timer.md))
so a job that dies at the wrong moment does not leave a paid resource behind. The provider table
(RunPod, SSH, exec plugins, and Sprites, which is refused in this release) is in
[docs/providers.md](docs/providers.md).

## How it works

`bitc run <pool> -- <command>` walks the pool's `providers` list in order (a pool with no list is
just `local`). Simplified:

```mermaid
flowchart TD
    start(["bitc run heavy -- cmd"]) --> next{"Next provider in<br/>the pool's list?"}
    next -- "remote" --> eligible{"Allowed to go remote?<br/>host opted in, work tree granted,<br/>repo allows this argv"}
    eligible -- "no" --> next
    eligible -- "yes" --> budget{"Under the daily cap and<br/>live-job limit, provider healthy?"}
    budget -- "no" --> next
    budget -- "yes" --> launch["Launch the remote job"]
    launch -- "failed before the job started" --> next
    launch -- "started" --> remote["Runs remotely. Exit code is the command's;<br/>an infrastructure failure from here on is 75"]
    next -- "local" --> slot{"Slot free (and memory floor met)<br/>within the pool's wait?"}
    slot -- "yes" --> local["Runs here in the slot.<br/>Exit code is the command's"]
    slot -- "no" --> starved["STARVED, exit 75: try again later"]
    next -- "none left" --> none["No provider left:<br/>75 (cap, busy) or 69 (unavailable)"]
```

Once a remote job has started it never falls back, so a half-started job never runs its load
twice. Every rule is in [docs/spec.md](docs/spec.md) section 8.2.

Two files configure bitc, and they are trusted differently:

```mermaid
flowchart LR
    host["Host file<br/>~/.config/bitc/bitc.toml<br/>owned by the account"] -- "sets pools, slots, waits,<br/>memory floors, providers,<br/>spend caps, grants" --> eff(["Effective limits"])
    repo["Repo file<br/>.bitc.toml<br/>owned by the project"] -- "can only narrow:<br/>env, hooks, refuse rules,<br/>what may go remote" --> eff
```

A host-only key in a repo file is an error (exit 78). Both files start with `version = 1`, are
decoded strictly and must not be group- or world-writable. More in
[docs/concepts.md](docs/concepts.md) and [docs/configuration.md](docs/configuration.md).

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
not only the code. The stderr lines are in [docs/exit-codes.md](docs/exit-codes.md); the rules
for agents are in [docs/agent-guide.md](docs/agent-guide.md).

## Documentation

| Read | For |
| --- | --- |
| [docs/agent-guide.md](docs/agent-guide.md) | the rules an AI agent follows when it runs commands through bitc |
| [docs/concepts.md](docs/concepts.md) | pools, slots, groups, tickets, nested holds, providers, state |
| [docs/configuration.md](docs/configuration.md), [docs/config-reference.md](docs/config-reference.md) | the two config files, and every key with its class and default |
| [docs/exit-codes.md](docs/exit-codes.md) | exit codes, stderr lines, `wait` outcomes, `result.json` |
| [docs/providers.md](docs/providers.md) | remote providers, their state, and running them safely |
| [docs/install.md](docs/install.md) | release assets, building from source, macOS status |
| [docs/comparison.md](docs/comparison.md), [docs/faq.md](docs/faq.md) | bitc next to `flock`, GNU `parallel` and CI concurrency; common questions |
| [docs/threat-model.md](docs/threat-model.md), [docs/spec.md](docs/spec.md) | what bitc defends against; the full design |

## Status and roadmap

bitc is pre-1.0 ([v0.1.0 release notes](https://github.com/Exploded-views/bitc/releases/tag/v0.1.0)),
so interfaces may still change before 1.0.

**Shipped in v0.1.0**

- The local gate, stable in intent: pools, slots, groups, priority lanes, memory admission,
  nested holds, detached jobs, `status` (with `--json`), `sweep` and `doctor`, the exit codes and
  the config schema.
- Remote providers, experimental: `runpod` (proven live on CPU pods earlier; a live re-proof of
  the v0.1.0 release path is pending), `ssh` and exec plugins (the
  [plugin protocol](docs/plugin-protocol.md) may still change).
- Release binaries for Linux and macOS (amd64, arm64) with sha256 checksums; macOS runs local
  pools only.

**On `main`, in the next release** ([CHANGELOG](CHANGELOG.md#unreleased))

- Remote providers off by default behind `[host] experimental_remote`.
- Sprites jobs refused until its egress is restricted
  ([#6](https://github.com/Exploded-views/bitc/issues/6)); `bitc sweep` still cleans up Sprites
  left by 0.1.0.
- The shipped repository example fixed, and history lines bounded to one atomic write.

**Next** (open issues)

- [#4](https://github.com/Exploded-views/bitc/issues/4): macOS as a client to remote providers
  (planned for v0.1.1), part of [#3](https://github.com/Exploded-views/bitc/issues/3), macOS parity
- [#9](https://github.com/Exploded-views/bitc/issues/9): the launch lock must exclude across
  implementations during a mixed rollout (spend-cap safety)
- [#6](https://github.com/Exploded-views/bitc/issues/6): Sprites network policy ends in a
  catch-all deny, so its jobs can be enabled
- [#7](https://github.com/Exploded-views/bitc/issues/7) and
  [#8](https://github.com/Exploded-views/bitc/issues/8): RunPod ordered data-centre walk with a
  per-pool US-only option, and fallback CPU types per size
- [#10](https://github.com/Exploded-views/bitc/issues/10): IO-pressure admission
  (`io_pressure_max`)

**Later**

- [#5](https://github.com/Exploded-views/bitc/issues/5): macOS as a remote build target (design
  first).
- Signed checksums after 1.0, and packages under the name `bitc`.
- Volume caches for remote jobs (designed in [spec.md](docs/spec.md) section 16.1, not scheduled).
- Provisioning compute for [crewkit](https://github.com/Exploded-views/crewkit). The direction is
  `crewkit up --provider <p>`: crewkit composes the stack, bitc provisions the compute. This is a
  direction, not a promise.

## Known issues

- Remote providers are experimental and off by default (`experimental_remote`): their spend
  accounting is still being hardened. In 0.1.0 specifically: same-named concurrent jobs in one
  pool can collide, and some interrupted-create paths can leave a resource unpriced until its
  expiry. Local pools are not affected.

  A host that ran remote jobs with an earlier version and has no flag gets one more line from
  every refused command: set the flag and run `bitc sweep` to clean up what is left. If you
  deleted a provider's table instead, `bitc doctor` and `bitc sweep` still point out its
  unfinished jobs.
- The v0.1.0 release predates the `experimental_remote` switch: it rejects the key as unknown
  (exit 78) and runs configured remote providers without it, Sprites included. Configure remote
  providers on 0.1.0 only with the cautions above in mind.
- Sprites jobs are refused in this release (exit 78, even with `experimental_remote`): its
  network policy does not restrict egress yet, and a job would receive the forwarded environment
  and the deploy key. Fix tracked in [#6](https://github.com/Exploded-views/bitc/issues/6). To
  clean up Sprites left by 0.1.0, keep the `[providers.sprites]` table, set
  `experimental_remote = true` under `[host]` and run `bitc sweep` (the sweep timer keeps
  working): it only lists Sprites, checks their owner tag and deletes this install's.
- In 0.1.0 the shipped example `examples/config/repo.bitc.toml` declares `min_version = "1.0.0"`,
  so a 0.1.0 build refuses it (exit 78); set it to `"0.1.0"`. Fixed in the next release.

## Part of Exploded Views

bitc is one of a small family of tools for running crews of AI coding agents:

- [crewkit](https://github.com/Exploded-views/crewkit) is the front door: Claude Code plugins,
  practices and an installer that installs pinned, checksum-verified releases of bitc and
  claude-memsync.
- [claude-utils](https://github.com/Exploded-views/claude-utils) ships claude-memsync, which syncs
  Claude Code memory across machines through a private git repository. It is a fork of
  [MarimerLLC/claude-utils](https://github.com/MarimerLLC/claude-utils) by Rockford Lhotka.
- yo-human, a CLI that lets an agent reach its human through a notification channel, is in
  design.

## Contributing

Bug reports and feature requests go to
[GitHub issues](https://github.com/Exploded-views/bitc/issues/new/choose) (use the issue
templates). Pull requests are welcome. Start with [CONTRIBUTING.md](CONTRIBUTING.md); it covers
building, `make ci`, and the conformance register every behaviour must appear in. Please follow the
[Code of Conduct](CODE_OF_CONDUCT.md) and report security problems through private vulnerability
reporting as described in [SECURITY.md](SECURITY.md), never in a public issue.

## License

MIT. See [LICENSE](LICENSE).
