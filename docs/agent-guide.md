# Guide for AI agents

You are an AI coding agent on a machine shared with other agents. Heavy commands (lint,
typecheck, test suites, coverage, browser tests, database tests) run through bitc so that they
take turns instead of exhausting the machine's memory. Follow these rules.

1. **Run heavy commands through `bitc run <pool> -- <command>`.** Never run them bare. The pool
   names are the owner's; the repo's docs or `bitc status` list them.
2. **Exit 75 means retry.** The pool was busy, or a remote machine failed after the job started.
   Wait a little and run the same command again. Never report 75 as a test failure.
3. **Exit 124 from `wait` means call `wait` again.** The job is still running. Never treat it as
   a failure or start the job again.
4. **Exit 69 means stop and report.** It is an internal error, an unusable hook, a remote setup
   step that failed by itself or no usable provider. Do not retry it: tell the person what the
   `bitc:` line said.
5. **Exit 64 means fix your call.** The pool name, a flag or the command was refused. A line
   `bitc: <pool>: refused: <reason>` tells you which pool to use instead.
6. **Exit 66 means check the job name.** There is no such detached job.
7. **Exit 78 means a config file is invalid.** Stop and report it; it is not yours to retry.
8. **Any other code is the command's own** and is the real result, for example a failing test.
9. **Use `--detach` and `wait` for anything that can outlast your tool-call limit** (often about
   ten minutes). `bitc run --detach --name <job> <pool> -- <command>` returns at once;
   `bitc wait <job>` waits at most 9 minutes per call and exits 124 when the job is still going.
10. Do not shell out around bitc (no `kill` of other agents' holders, no deleting files in the
    state directory). `bitc status` shows who holds what.

## Check that bitc is ready

Do not probe readiness with `bitc version`: it reads no config, so it succeeds even when a bad
config makes every other command exit 78. Use `bitc config check` instead. Exit 0 means a usable
config; 78 means config errors. From 0.1.2 on it also checks the state directory's permissions
(a planned fix).

## Retry on 75

<!-- doc-test -->
```sh
mkdir -p ~/.config/bitc
printf 'version = 1\n[pools.lint]\nslots = 1\n' > ~/.config/bitc/bitc.toml
chmod 600 ~/.config/bitc/bitc.toml
tries=0
while :; do
  rc=0
  bitc run --wait 2s lint -- echo checked || rc=$?
  [ "$rc" -eq 75 ] || break
  tries=$((tries + 1))
  [ "$tries" -lt 5 ] || break
  sleep 2
done
exit "$rc"
```

## Wait again on 124

This loop starts a detached job and calls `wait` until it ends. It retries on 124 and stops on
anything else. A real agent should also stop after its own overall time limit.

<!-- doc-test -->
```sh
bitc run --detach --name slowjob lint -- sh -c 'sleep 3; echo finished'
while :; do
  rc=0
  bitc wait --timeout 1s slowjob || rc=$?
  [ "$rc" -eq 124 ] || break
done
exit "$rc"
```

The first line `wait` prints is `RESULT exit <code>`, where `<code>` is the command's own exit code.
Read it: `wait` itself exits 0 by default whenever the command ran. Use `--exit job` to get the
command's code as `wait`'s exit code instead (`75`, `69` and `124` from the command then look
like bitc's own, so prefer the default and read the first line).

## Cancel a detached job

`bitc cancel <job>` stops a detached job. In 0.1.1, right after `bitc run --detach`, it can
report success while the job keeps running. This is a known issue; the fix is planned for 0.1.2.
Do not trust the first `cancel`: run `bitc status` (or `bitc wait <job> --exit job`) to confirm
the job has ended, and cancel again if it is still running.
