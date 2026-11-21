# Running a pool's commands in a container

A local pool can run its admitted commands in a docker container instead of directly on the host.
Admission does not change: slots, groups, FIFO, waits, the time limit and detached jobs all work as
for any local pool. Only the command itself runs differently: bitc starts it with `docker run`,
with the work tree mounted and the command as the container's argv. The runtime is `docker`
only; on a Mac, [OrbStack](https://orbstack.dev) and Docker Desktop provide the docker CLI.

```toml
# ~/.config/bitc/bitc.toml (host file only)
version = 1

[pools.sandbox]
slots = 2
timeout = "30m"
container = { image = "node:22-bookworm", cpus = 4, memory = "8GiB", mounts = ["/Users/you/.npm:/root/.npm"] }
```

```console
$ bitc run sandbox -- npm test      # runs in node:22-bookworm, in the same directory, as a child of bitc
```

The key is host-only: an image and its mounts decide what a command can reach, so a repo file that
sets `container` is a config error (78). bitc never pulls an image for you: `docker run` pulls a
missing one the way docker always does, so pull it first if you want that cost outside a run.

## Keys

| Key | Meaning | Default |
| --- | --- | --- |
| `image` | the image to run (required): letters, digits and `. _ / : @ -`, not starting with `-` | none |
| `runtime` | the container runtime; `docker` is the only value | `docker` |
| `mounts` | extra bind mounts, `"<host path>:<container path>"` or with `:ro` / `:rw`; both paths absolute, with no comma, double quote or newline (docker reads `--mount` as CSV) | none |
| `cpus` | `--cpus`: a number from 0.01 | no limit |
| `memory` | `--memory`: a size such as `8GiB`, at least `6MiB` | no limit |

There is no CPU or memory limit unless you set one. (This is separate from the pool's own
`memory`, the admission reservation, which is Linux-only and still works beside it.) A container pool
cannot also have a `wrap` hook: the container is the wrapper, and combining them is a config error.

## What the command sees

- **Directory.** The work tree (with no work tree, the run's directory) is
  mounted at the same path, read-write, and is the working directory. A git work tree that lives
  elsewhere, such as a `git worktree` whose `.git` file points at another directory, needs that
  directory in `mounts` for git to work inside the container.
- **Streams and status.** stdin, stdout and stderr are the command's (`-i`, and `-t` when bitc's
  stdin is a terminal). The exit status is the command's, `128+N` if it was killed by signal N. A
  status of 125, 126 or 127 from the command is the command's own once the container exists (docker
  has written its `--cidfile`). A 125 can then still be docker's own failure to start the
  container, such as an OCI runtime or cgroup error: it is not told apart from the command's 125.
- **Environment.** Only a minimal one, not the host's: exactly `TERM`, `LANG`, `LC_ALL`, `LC_CTYPE`
  and `TZ` when the caller has them (no other `LC_*` variable); the pool's `env`, `env_default` and
  `env_unset`, and an `env` hook's output; and `BITC_POOL`, `BITC_SLOT` and `BITC_JOB`. Tokens,
  `HOME`, `PATH`, `SSH_AUTH_SOCK` and the rest of the caller's environment stay on the host, and so
  do `BITC_HOLD` and `BITC_DIR`: a `bitc` inside the container cannot verify a hold, and the state
  dir is not mounted. No value is on the `docker run` command line, which every local user can
  read with `ps`: bitc writes the environment to `<state dir>/containers/<ticket id>.env`, a file
  of mode 0600 in the 0700 state dir, passes `--env-file` and removes the file when the run ends
  (a normal end, the time limit, a signal, `cancel`) and, if bitc is SIGKILLed, the guard does. A
  value that cannot be one line of that file (a line break or a NUL, invalid UTF-8, a line of 64 KiB
  or more: docker's parser refuses the last two) is a config error (78) naming the variable, before
  docker sees it. The file is the container's environment and only that: the docker CLI, its
  removals, the guard and `bitc sweep` all run with the environment bitc itself was given
  (`DOCKER_HOST`, `DOCKER_CONFIG`, `PATH`...), so they reach one daemon. A pool's `env` or an `env`
  hook's output of `DOCKER_HOST` is the container's variable and never docker's. `docker inspect`
  shows a container's environment to anyone who can use the docker socket, whatever bitc does.
- **Not carried over.** The host's environment, the process group (the container has its own),
  and the caller's user: the command runs as the image's user, so files it creates in the work
  tree belong to that user on a Linux host (OrbStack on a Mac maps them to you).

## How the container is named, labelled and removed

Each run's container is named from its ticket, `bitc-<ticket id>` (for example
`bitc-1-1794824899741835000-f5e7a842`), and labelled. The name is unique per run, because the job
name (`--name`) is the caller's, can be reused and can be shared by runs going at the same time.
Nothing is ever removed by name: a removal goes by the container id docker wrote to the run's
`--cidfile`, and the sweep by the id the listing gave (`docker ps --no-trunc`). Only a full id (64
hex digits) counts: a shorter one could be resolved by the daemon as a name or as a prefix of
another container's id, so anything else in the cidfile is no id. A run that never got an id
created no container, so it removes none. The job stays a label, and `docker ps --filter label=bitc.job=<job>`
finds it:

| Label | Value |
| --- | --- |
| `bitc.job` | the job name (`--name`, or the random id) |
| `bitc.pool` | the pool |
| `bitc.state` | the first 12 hex digits of the sha256 of the state dir path: one bitc state dir never touches another's containers |
| `bitc.ticket` | the run's ticket id: what tells a live run from an orphan |

`docker run` is started with `--rm --init` and a `--cidfile` under the state dir's `containers/`
(docker writes the id there once the container exists). Killing the docker CLI does not stop its
container, so bitc removes it itself with `docker rm -f <container id>`:

- at the **time limit** (`pools.<p>.timeout`, `run --timeout`: exit 124, the usual line) and for
  INT, TERM or HUP to bitc (`128+N`) and `bitc cancel [--kill]` of a detached job: the CLI gets
  SIGTERM first, then SIGKILL after `grace`, and the container is force-removed once the CLI has
  ended or at that SIGKILL. Without a TTY the docker CLI proxies that TERM to the container (the
  default `--sig-proxy=true`; the container's init, `--init`, passes it on), so the command can stop
  gracefully. With a TTY (bitc's stdin is a terminal, so `-t`) docker proxies no signal: the
  command gets no TERM, and the container is removed when the CLI has ended;
- when **bitc is SIGKILLed**: no handler can run, so each run also starts a small guard process
  (`bitc __container-guard`, in its own session) that holds the read end of a pipe whose write end
  is bitc's. The kernel closes bitc's end when it dies, even by SIGKILL; the guard reads EOF, runs
  `docker rm -f <id>` with the id in the cidfile (none there, none removed) and removes the env
  file. A run that finishes normally tells the guard `done` first. This works the same on Linux and
  macOS. (If bitc dies between docker creating the container and writing the id, the guard sees no
  id and the sweep below catches the container.);
- by the **orphan sweep**, as the backstop for a guard that never ran (the machine lost power, the
  guard was killed): a container with this state dir's `bitc.state` label whose `bitc.ticket` is a
  ticket nobody holds is removed by the next admission to any container pool and by `bitc sweep`
  (`swept container bitc-x (its run is gone)`; `--dry-run` only lists them). A live run's
  container, a container of another state dir and one without the labels are never touched, and
  neither is one whose ticket cannot be checked: any error looking for the ticket file other than
  "it is not there" (a permission or I/O error, a path that is not a directory) keeps the container
  and `bitc sweep` says so with the reason (`kept container <name> (holder unknown: its ticket
  cannot be checked: <the error>)`, or `its ticket lock cannot be inspected on this host`). A stray
  regular file in `pools/` (a `.DS_Store`) is not a pool and is skipped; a symlink to a pool
  directory is one. A sweep error aborts the sweep: the state dir's `pools/` cannot be read
  (nothing says which runs are live) or the listing fails, and nothing is deleted; a removal
  fails, and nothing more is deleted, no further container and no cidfile or env file (containers
  removed earlier in that sweep stay removed). `bitc sweep` prints `container sweep aborted: <why>`,
  carries on with the rest of its work and exits 69 (unavailable: the docker side could not be
  used; stop and report). An admission that swept goes on and runs its command (a debug line). The
  sweep also removes the cidfile and env file of runs that are gone. The admission's orphan
  removal appears in the history as a `container-reaped` event with the job from the `bitc.job`
  label (not `reaped`, which counts dead tickets).

The admission's orphan check is one `docker ps` per admission to a container pool, before the run
queues: about 100 to 300 ms on Docker Desktop or OrbStack, and bounded at 30 s if the daemon hangs.
Its failure is never the run's (a debug line only). Run `bitc sweep` from a timer instead if that
matters to you; the check stays on every admission.

## Errors

| Situation | Result |
| --- | --- |
| `runtime`, `image`, `cpus`, `memory` or `mounts` wrong, or `container` beside a `wrap` hook or in a repo file | 78 at load, naming the key |
| the `docker` CLI is not on `PATH` | 69 before admission: `bitc: <pool>: container runtime "docker" not found on PATH (install it, or remove pools.<pool>.container)` |
| docker fails before the container exists (daemon down, image not found) | 69: `bitc: <pool>: docker could not start the container (it exited 125 before the command ran; its message is above)`; the result is `outcome = "unavailable"` |

69 is the class for "the thing this run needs is not available, stop and report": a missing
runtime or a refusing daemon is a host problem, like a hook that cannot start, and not a usage
error (64), a configuration error (78: the config is valid) or a temporary failure (75: retrying
blindly will not help). The command never ran, so no exit status of its own exists.

## Testing without docker

bitc's tests run a fake `docker` on `PATH` (`internal/container/testdata/fakedocker`). One test is
opt-in against a real docker: `BITC_TEST_DOCKER=1 go test ./conformance -run TestDK10` needs docker on
`PATH` and a local `alpine` or `busybox` image; it never pulls.
