# A heavy-work pool for one machine

This recipe shares the performance cores of one machine between commands of different weight.
Run it as the account's host file; the pools are the names agents and people use with
`bitc run`. Read [`concepts.md`](concepts.md) first for pools, groups and tickets.

## The gate-class recipe

One group, `cpu`, has one unit per performance core. N is the number of performance cores
(8 below). Four pools take different numbers of those units:

| Pool | Takes | For |
| --- | --- | --- |
| `gate` | N - 1 | the full check before a merge: it wants nearly the whole machine |
| `heavy` | 3 | a test suite or build that uses a few cores |
| `light` | 1 | lint, typecheck, a quick script |
| `bench` | N | a timing run that must have the machine to itself |

```toml
version = 1

groups = { cpu = { units = 8 } }      # N: the performance cores

[pools.gate]
slots = 1
wait = "2h"
groups = { cpu = 7 }                  # N - 1

[pools.heavy]
slots = 8                             # slots to spare: the group is the limit
wait = "1h"
groups = { cpu = 3 }
groups_max = { cpu = 6 }              # a run may ask for at most 6 with --units

[pools.light]
slots = 8
wait = "30m"
groups = { cpu = 1 }

[pools.bench]
slots = 1
wait = "4h"
groups = { cpu = 8 }                  # N
```

A run takes all of its units or none, so it never holds some while it waits for the rest.
Runs waiting on the group are served first in first out by the time they queued. (A run that
waits only for its own pool's slots records no group wait and holds nothing behind it back. In
the example `gate` and `bench` have one slot each, so a second `gate` or `bench` run waits on its
pool's slot; `heavy` and `light` have slots to spare, so for them the group is the limit.)

## What waits for what

With N = 8:

- Two `heavy` runs (3 + 3) and two `light` runs (1 + 1) fit together: 8 of 8 units.
- A third `heavy` run waits: 3 more would be 9.
- A `gate` run (7) fits beside one `light` run and nothing else. If two `heavy` runs are going
  (6 units), the `gate` run waits until both end; the `heavy` runs are never stopped for it.
- Once a `gate` run is waiting on the group, a `heavy` or `light` run that arrives after it queues behind it,
  even when the free units would fit the newcomer. That keeps a gate from being starved by a
  stream of small runs. A run that arrived before the `gate` run is ahead of it.
- A `light` run fits in the free unit beside a `gate` run only until a `heavy` run queues behind
  the gate. Once a `heavy` run is waiting, a later `light` run waits behind it, even with a unit
  free. Strict first in first out is deliberate: it keeps a waiting `heavy` or `gate` run from being
  starved by a stream of small runs.
- A `bench` run (8) waits for every run ahead of it and holds back every run behind it.

If a run waits longer than its pool's `wait`, it exits 75 and prints one line that
says what it wanted and what expired, for example `STARVED heavy (group cpu busy: wanted 3 of 8
units; waited 1h)`; run it again. The same text is `reason` in `result.json` and in the history
line. A run that only waited for its pool's slots reads `STARVED gate (slots busy; waited 2h)`.
The wait is printed the way the config writes it.

## One-off sizes

`--units` changes the count for one run:

```
bitc run --units cpu=6 heavy -- make test        # 6 units instead of 3, this run only
bitc run --units cpu=2 light -- ./slow-lint      # 2 units instead of 1
```

The group must be one the pool lists. The count is 1 to the group's units, and at most the pool's
`groups_max` for that group when it sets one: above either is exit 64 with a line naming the
group and the limit. A bad `groups_max` in the host file is exit 78 naming the key. `--units` also
works with `--detach`, and is refused (64) for a pool that has no local provider. On a pool with
both local and remote providers it is accepted but has no effect when the run goes remote.

`bitc status` shows the units each running and waiting run holds or asks for, and `result.json`
and the history `start` and `end` lines record them:

```
heavy: 1/2 slots held, 1 waiting (wait 1h, oldest 3m)
  slot 1  a1b2c3d4  pid 4012  units cpu=6  since 2026-11-02T09:14:03Z  make test
  waiting  e5f6a7b8  normal  pid 4107  units cpu=3  since 2026-11-02T09:14:41Z (3m)
group cpu: 6/8 units held
```
