# Exit codes and output

bitc returns the command's own exit code when the command ran. Otherwise it uses these codes,
never Go's default 2. stdout belongs to the command: bitc writes only the stderr lines below.

| Code | Name | Meaning | What the caller should do |
| --- | --- | --- | --- |
| the command's own | | the command ran, locally or remotely; `128+N` if a signal N killed it | treat it as the real result |
| 64 | usage | bad arguments or a malformed `BITC_*` value; unknown pool; refused by a refuse rule or hook; `--provider` not allowed; an uncreatable detached log | fix the call |
| 66 | no input | `wait`, `logs` or `cancel`: no such job | check the job name |
| 69 | unavailable | not Linux or macOS; a Linux-only feature in the macOS build (`bitc: <feature> runs on Linux only (this is a darwin build)`); a hook could not start, timed out or printed unparsable output; no provider is usable; a remote job's own setup step failed; an internal error (a bitc bug) | do not retry blindly: stop and report |
| 75 | temporary failure | starved; a hook asked to wait; a remote infrastructure failure after the job started | wait, then run again |
| 78 | config | invalid config file; a host-only key in a repo file; a pool's `memory` or a `[providers.*]` table in the macOS build; `min_version` not met; an insecure, NFS or newer-schema state dir | fix the configuration |
| 124 | still running | `wait` only: the job has not ended | call `wait` again |

An internal error or panic prints `bitc: internal error: <msg>`, releases everything and exits 69.
It is never 75, so a bug that fails every time cannot keep a retrying caller looping.

## stderr lines

| Line | When |
| --- | --- |
| `STARVED <pool>` | 75 from admission: every slot is busy and nobody is ahead |
| `STARVED <pool> (<reason>)` | 75 from admission otherwise. Reasons: `queue`, `group <g> busy`, `memory: <n> MiB available, floor <f> MiB`, `priority lane`, `nested child busy`, `pre_run: <why>`, `env hook: <why>` |
| `STARVED <pool> (<provider>: <why>)` | 75 from a remote job after it started, for example `pod <id> timed out after 61 min` |
| `bitc: <provider>: running <pool> locally: <why>` | a remote provider could not take the job and `local` is next |
| `bitc: <provider>: <pool>: trying <next>: <why>` | the same, when the next provider is remote |
| `bitc: <provider>: <pool>: not eligible for remote: <why>` | the job cannot go to that provider and no local fallback follows; 69 at the last provider |
| `bitc: <provider>: <pool> job <job> on <resource> (<size>) at <sha12>` | a remote job was launched |
| `pod: <line>` | output of the remote setup steps |
| `bitc: <pool>: refused: <reason>` | 64 from a refuse rule or hook |
| `bitc: <message>` | every other note, warning or error |

## `wait` outcomes

`bitc wait <job> [--timeout 9m] [--exit zero|job]` polls until the job ends or the timeout passes.

| Outcome | First stdout line | Exit (default `--exit zero`) | Exit with `--exit job` |
| --- | --- | --- | --- |
| the command ran, or the supervisor refused (64) or found a hook unusable (69) | `RESULT exit <code>` then the last 60 lines of the log | 0 | the job's code |
| starved or lost (75) | `RESULT starved` and the tail | 75 | 75 |
| internal error | `RESULT internal error` and the tail | 69 | 69 |
| still running | `RUNNING (no exit line yet after <s>s); last lines:` and 5 lines | 124 | 124 |
| unknown job or no log | `NO LOG <path> ...` | 66 | 66 |

A command that itself exits 75 shows `RESULT exit 75`: read the first line, not only the code.

## `result.json`

Every detached job writes `result.json` in its record; a foreground run writes one with
`--result PATH`. Fields: `origin` (`bitc` or `command`), `outcome` (`ok`, `command-failed`,
`starved`, `refused`, `config`, `unavailable`, `interrupted`, `lost`, `internal`), `exit_code`,
`reason`, `pool`, `slot`, `provider`, `job`, `generation`, `hold_started`, `wait_ms`, `run_ms`.
`bitc status --json` follows [`status.schema.json`](status.schema.json).
