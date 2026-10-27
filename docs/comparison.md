# bitc compared with alternatives

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

## When not to use bitc

- You need one lock around a short critical section in a script. `flock` is simpler and already
  installed.
- You are fanning a list of inputs out over cores in one command. Use `parallel` or `xargs -P`.
- Your contention is between CI runs rather than processes on a machine. Use your CI system's
  concurrency controls.
- You need Windows as the gate host, or macOS with memory admission or remote providers. bitc
  is Linux first; its macOS build runs local pools only.
- You need a cluster scheduler with resource accounting across many machines. Use one.
