# Roadmap

Work that is planned but not scheduled. An issue for a row here is closed as "not planned" until
the work is scheduled; then it is reopened (or, for a row with no issue, one is filed), and the
row leaves this page. Scheduled work is in the README's
[Status and roadmap](../README.md#status-and-roadmap) section, and shipped work in
[CHANGELOG.md](../CHANGELOG.md).

## macOS

| Item | Issue | State |
| --- | --- | --- |
| macOS as a remote build target | [#5](https://github.com/Exploded-views/bitc/issues/5) | Needs a design first |

## Remote providers

| Item | Issue | State |
| --- | --- | --- |
| Sprites: a catch-all egress deny, so Sprites jobs can be admitted | [#6](https://github.com/Exploded-views/bitc/issues/6) | Sprites stays refused until then |
| RunPod: an ordered data-centre walk with a per-pool US-only option | [#7](https://github.com/Exploded-views/bitc/issues/7) | Not scheduled |
| RunPod: fallback CPU types per size | [#8](https://github.com/Exploded-views/bitc/issues/8) | Not scheduled |

## Remote-runner features (from [#28](https://github.com/Exploded-views/bitc/issues/28))

| Item | State |
| --- | --- |
| Exit-code contract between the gate and the remote runner | Not scheduled |
| Caller-env scrub of runner-owned variables | Not scheduled |
| Test seams honoured only in a provably private pool directory | Not scheduled |
| Job-token secrecy on remote runners | Not scheduled |
| Result-file contract and tree attestation | Not scheduled |
| Machine-readable refusal reasons, plus a summary command | Not scheduled |
| Worktree opt-in by path patterns | Not scheduled |
| Remote-only mode and no-tiny-run | Not scheduled |
| Host-wide remote-job semaphore with an orphan guard | Not scheduled |
| Spend-caps engine (reservations, billing-lag floor, per-session sub-cap, alert) | Not scheduled |
| Sharded runs with a verified coverage merge | Not scheduled |
| Canary before fan-out, with a breaker budget | Not scheduled |
| Path-based test selection shared by the runner and the gate | Not scheduled |
| Remote lint pool | Not scheduled |
| Host install/setup pool, separate from the heavy pool | Not scheduled |
| Dirty-tree snapshot upload with layered secret refusal | Spec only |

## Later

Signed checksums after 1.0, packages under the name `bitc`, and volume caches for remote jobs
([spec.md](spec.md) section 16.1). The spec's own "Later" list (section 14) has the full set.
