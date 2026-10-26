# Concepts

This page explains the words bitc uses. The full design is in [`spec.md`](spec.md); every key is
in [`config-reference.md`](config-reference.md).

## Pools and slots

A **pool** is a named queue with N **slots**. `bitc run heavy -- npm test` waits until pool
`heavy` has a free slot, runs the command with the pool's environment and returns the command's
own exit code. Pool names are `[A-Za-z0-9_-]+`; `-` and `_` are the same name.

Slots are numbered **1 to N** everywhere: the `{slot}` template, `BITC_SLOT`, `hook-env --slot`.
A pool with two slots can give each slot its own database port with an env template such as
`{slot+db_port_base}` (see [`examples/node-monorepo/`](../examples/node-monorepo/)).

## Groups

A **group** is a semaphore shared by several pools, with a capacity in units. A pool lists the
units it takes of each group (`groups = { bigmem = 1 }`). Two pools in the same group never run
at the same time when the group has one unit, even though each pool has its own slots. Group
units are numbered 1 to k.

## Tickets, FIFO and priority

A caller that cannot start takes a **ticket**, a file in the pool's queue that it keeps locked for
as long as it waits. Callers in the same priority class are admitted in first-in-first-out order.
A work tree whose path matches `priority.high_cwd` or `priority.high_cwd_contains` (host file)
jumps to the high lane. When a caller is admitted it holds the slot (a **hold**) until its
command ends. There is no daemon: the kernel's `flock(2)` locks do the work, so a caller that is
killed releases everything it held, and the next caller reaps what it left.

If the caller cannot start within the pool's `wait`, it exits **75** and prints `STARVED <pool>`
with a reason. Exit 75 always means "wait and retry", never "your command failed".

## Nested holds

A command that is itself running under a bitc hold can call `bitc run` again. The child is a
**nested** hold: it does not take a second slot of the same pool and cannot deadlock against its
parent. `bitc status` marks nested holders.

## Memory floor

A pool that sets `memory = "8GiB"` reserves that much when it is admitted. A caller is held back
(`STARVED <pool> (memory: ...)`) when the host's available memory (and the cgroup's headroom, when
there is one) would fall below `host.memory_floor` after the reservation. The reservation is an
admission figure, not a heap size and not a limit on the command.

## Providers

A **provider** is a place where a command can run.

| Provider | Where the command runs |
| --- | --- |
| `local` | this machine, in a slot (built in) |
| `runpod` | a RunPod pod, created for the job and deleted afterwards |
| `sprites` | a fresh Fly Sprite (microVM) for every job; jobs **refused in this release** (exit 78, even with `experimental_remote`; `bitc sweep` still cleans up Sprites left by 0.1.0), see [#6](https://github.com/Exploded-views/bitc/issues/6): its network policy does not restrict egress yet, and a job would receive the forwarded environment and the deploy key |
| exec plugin | any platform, through an executable `bitc-provider-<type>` that you pin by absolute path and sha256 |
| `ssh` | a machine you already own, reached over ssh (experimental) |

A pool lists the providers it may use in order: `providers = ["runpod", "local"]`. A
remote job needs the host to allow it (a `remote_limits.size` for the provider, a matching work
tree) and the repo to describe it (`[pools.<pool>.remote]` and the `[remote]` profile). If a
remote provider cannot take the job before it has started, bitc moves to the next provider and
prints one `bitc: <provider>: running <pool> locally: <why>` note (or `trying <next>`). Once a job
has started remotely it never falls back: a half-started job would otherwise run its load twice.
Every remote provider shares the same safety rails: a daily spend cap per provider account,
`max_live`, a launch lock, reservations, a ledger and a circuit breaker.

A remote job takes no local slot. See [`threat-model.md`](threat-model.md) for what runs where,
and [`provider-author-guide.md`](provider-author-guide.md) to add a provider.

## Two config layers and the trust split

- The **host file** (`~/.config/bitc/bitc.toml`, or `--config`, or `BITC_CONFIG`) belongs to the
  account. It sets capacities, memory floors, groups, priorities, providers, credentials, spend
  caps and per-repo grants (`[[repos]]`).
- The **repo file** (`.bitc.toml` at a work tree's top) belongs to the project. It sets
  environment, hooks, refuse rules and the remote profile.

Every key has a class (see the table in [`config-reference.md`](config-reference.md)). A repo file
can only **narrow**: it cannot raise a capacity, lengthen a wait, enable a provider, grant root
steps or a deploy key, or remove a host refusal. A host-only key in a repo file is an error (78).
Both files must start with `version = 1`, are decoded strictly (an unknown key is an error) and
must not be group- or world-writable.

## State and accounting directories

The **state dir** (`$XDG_STATE_HOME/bitc`, i.e. `~/.local/state/bitc`; movable with `BITC_DIR` or
`--state-dir`) holds locks, tickets, detached jobs and `history.jsonl`. It is mode 0700, must be
owned by you and must be on a local file system.

The **accounting dir** holds the state of remote providers (the ledger, launch lock,
reservations, breaker and an install secret). It is fixed at `<your home>/.local/state/bitc/account`
and nothing moves it, so a second state dir on the same account shares the same spend cap.

## Detached jobs

`bitc run --detach --name build heavy -- make` starts a supervisor in its own session and returns
at once. `bitc wait build` waits for the end (bounded, so it fits a tool-call cap), `bitc logs
build` prints the log and `bitc cancel build` stops it. See [`exit-codes.md`](exit-codes.md) for
the `wait` outcomes.
