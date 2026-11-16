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
| `mounts` | extra bind mounts, `"<host path>:<container path>"` or with `:ro` / `:rw`; both paths absolute, no commas | none |
| `cpus` | `--cpus`: a number from 0.01 | no limit |
| `memory` | `--memory`: a size such as `8GiB`, at least `6MiB` | no limit |

There is no CPU or memory limit unless you set one. (This is separate from the pool's own
`memory`, the admission reservation, which is Linux-only and still works beside it.) A container pool
cannot also have a `wrap` hook: the container is the wrapper, and combining them is a config error.

## What the command sees

- **Directory.** The work tree (the run's directory, when it is outside the work tree) is
  mounted at the same path, read-write, and is the working directory. A git work tree that lives
  elsewhere, such as a `git worktree` whose `.git` file points at another directory, needs that
  directory in `mounts` for git to work inside the container.
- **Streams and status.** stdin, stdout and stderr are the command's (`-i`, and `-t` when bitc's
  stdin is a terminal). The exit status is the command's, `128+N` if it was killed by signal N. A
  status of 125, 126 or 127 from the command is the command's own once the container exists.
- **Environment.** Only a minimal one, not the host's: `TERM`, `LANG`, `LC_ALL`, `LC_CTYPE` and
  `TZ` when the caller has them; the pool's `env`, `env_default` and `env_unset`, and an `env` hook's
  output; and `BITC_POOL`, `BITC_SLOT` and `BITC_JOB`. Tokens, `HOME`, `PATH`, `SSH_AUTH_SOCK` and
  the rest of the caller's environment stay on the host, and so do `BITC_HOLD` and `BITC_DIR`: a
  `bitc` inside the container cannot verify a hold, and the state dir is not mounted. The values
  appear in the `docker run` command line (`-e NAME=VALUE`), so they are visible to `ps`
  on the host: put no secret in the pool's `env`.
- **Not carried over.** The host's environment, the process group (the container has its own),
  and the caller's user: the command runs as the image's user, so files it creates in the work
  tree belong to that user on a Linux host (OrbStack on a Mac maps them to you).

## How the container is named, labelled and removed

Each run's container is named from the job, `bitc-<job>`, and labelled:

| Label | Value |
| --- | --- |
| `bitc.job` | the job name (`--name`, or the random id) |
| `bitc.pool` | the pool |
| `bitc.state` | the first 12 hex digits of the sha256 of the state dir path: one bitc state dir never touches another's containers |
| `bitc.ticket` | the run's ticket id: what tells a live run from an orphan |

`docker run` is started with `--rm --init` and a `--cidfile` under the state dir's `containers/`
(docker writes the id there once the container exists). Killing the docker CLI does not stop its
container, so bitc removes it itself with `docker rm -f bitc-<job>`:

- at the **time limit** (`pools.<p>.timeout`, `run --timeout`: exit 124, the usual line) and for
  INT, TERM or HUP to bitc (`128+N`) and `bitc cancel [--kill]` of a detached job: the CLI gets
  SIGTERM first, which docker passes on to the container (it is the init's child), then SIGKILL after
  `grace`, and the container is force-removed once the CLI has ended or at that SIGKILL;
- when **bitc is SIGKILLed**: no handler can run, so each run also starts a small guard process
  (`bitc __container-guard`, in its own session) that holds the read end of a pipe whose write end
  is bitc's. The kernel closes bitc's end when it dies, even by SIGKILL; the guard reads EOF and runs
  `docker rm -f`. A run that finishes normally tells the guard `done` first. This works the same on
  Linux and macOS;
- by the **orphan sweep**, as the backstop for a guard that never ran (the machine lost power, the
  guard was killed): a container with this state dir's `bitc.state` label whose `bitc.ticket` is a
  ticket nobody holds is removed by the next admission to any container pool and by `bitc sweep`
  (`swept container bitc-x (its run is gone)`; `--dry-run` only lists them). A live run's
  container, a container of another state dir and one without the labels are never touched.

## Errors

| Situation | Result |
| --- | --- |
| `runtime`, `image`, `cpus`, `memory` or `mounts` wrong, or `container` beside a `wrap` hook or in a repo file | 78 at load, naming the key |
| the `docker` CLI is not on `PATH` | 69 before admission: `bitc: <pool>: container runtime "docker" not found on PATH (install it, or remove pools.<pool>.container)` |
| docker fails before the container exists (daemon down, image not found, name in use) | 69: `bitc: <pool>: docker could not start the container (it exited 125 before the command ran; its message is above)`; the result is `outcome = "unavailable"` |

69 is the class for "the thing this run needs is not available, stop and report": a missing
runtime or a refusing daemon is a host problem, like a hook that cannot start, and not a usage
error (64), a configuration error (78: the config is valid) or a temporary failure (75: retrying
blindly will not help). The command never ran, so no exit status of its own exists.

## Testing without docker

bitc's tests run a fake `docker` on `PATH` (`internal/container/testdata/fakedocker`). One test is
opt-in against a real docker: `BITC_TEST_DOCKER=1 go test ./conformance -run TestDK10` needs docker on
`PATH` and a local `alpine` or `busybox` image; it never pulls.
