# Installing bitc

The current release is [v0.1.3](https://github.com/Exploded-views/bitc/releases/tag/v0.1.3). It
runs on Linux (amd64 or arm64); the macOS builds (Apple silicon or Intel) run local pools and exec plugins
([macOS status](#macos-status)).

## Release binaries

Each release publishes a tarball `bitc_0.1.3_<os>_<arch>.tar.gz` (`<os>` is `linux` or `darwin`,
`<arch>` is `amd64` or `arm64`; it holds `bitc`, `LICENSE` and `README.md`), the bare binary
`bitc_0.1.3_<os>_<arch>`, and `checksums.txt`. Download an asset and `checksums.txt` and verify the
asset before use:

```sh
curl -LO https://github.com/Exploded-views/bitc/releases/download/v0.1.3/bitc_0.1.3_linux_amd64.tar.gz
curl -LO https://github.com/Exploded-views/bitc/releases/download/v0.1.3/checksums.txt
grep bitc_0.1.3_linux_amd64.tar.gz checksums.txt | sha256sum -c -
tar -xzf bitc_0.1.3_linux_amd64.tar.gz bitc
install -m 0755 bitc ~/.local/bin/bitc      # or any other directory on your PATH
bitc version --json
```

On macOS, use `shasum -a 256 -c -` in place of `sha256sum -c -`.

`bitc version --json` prints one compact JSON line, for example
`{"commit":"9bc24e8","config_schema":1,"release":true,"state_schema":1,"version":"0.1.3"}`.
`release` is `true` only for release builds. Checksums are unsigned for now; signed checksums are
planned after 1.0. [RELEASING.md](RELEASING.md) describes what a release contains.

[crewkit](https://github.com/Exploded-views/crewkit) can also install a pinned bitc release for
you, with the same checksum checks.

## From source

To build from source you need Go 1.26 or newer (CI tests 1.27.4):

```sh
go install github.com/Exploded-views/bitc/cmd/bitc@v0.1.3
```

A build made this way reports `"release":false`. That matters only for remote providers: the
agent source `release` downloads the matching release binary, so a source build needs
`agent = { source = "url", ... }` or `source = "build"` instead (see [RELEASING.md](RELEASING.md)).

From a clone, `make build` writes `dist/bitc`. [CONTRIBUTING.md](../CONTRIBUTING.md) covers the
toolchain.

## macOS status

macOS builds run **local pools and exec plugins**: `run`, `status`, `wait`, `logs` and `cancel` over
`flock` pools, plus detached jobs, groups, priority lanes, nested holds, `doctor` and `config`.
A `[providers.<type>]` table with a `plugin` pin loads and runs once the host file sets
`[host] experimental_remote = true`, as on Linux (without it: exit 78; 0.1.5 loaded it without the
flag, which was a bug fixed after it); memory floors (a pool's `memory` key) and the built-in
`ssh`, `runpod` and `sprites` tables are refused with exit 78, flag or not. Tracked in:

- [#3](https://github.com/Exploded-views/bitc/issues/3): macOS parity (umbrella);
- [#4](https://github.com/Exploded-views/bitc/issues/4): macOS as a client to the built-in remote
  providers (planned for 0.1.6);
- macOS as a remote build target: planned but not scheduled ([ROADMAP.md](ROADMAP.md)).

Linux-only features, and other differences on macOS:

- the built-in remote providers (`ssh`, `runpod` and `sprites`);
- the remote agent (`bitc __agent`): one `bitc: ... runs on Linux only` line and exit 69;
- the parent-death signal for a local command: on macOS it outlives a bitc that was killed with
  SIGKILL (an exec plugin does not: its shim kills the plugin's process group);
- the inotify wake-up: a waiting caller re-checks every `poll` (5s by default) instead;
- the read-only lock view (`/proc/locks`): on macOS, `status` reads lock holders with
  `fcntl(F_GETLK)` instead; it takes no lock, but it cannot name a holder's pid (macOS reports -1
  for flock locks);
- a detached job's log may have an empty line before its `__EXIT__` line;
- **plugin privileges:** macOS has no `no_new_privs`, so a plugin's process cannot be stopped from
  gaining privileges through setuid or setgid programs, as Linux does. This is accepted, known
  behaviour: only run plugins you trust as you would any program run as your user. bitc runs a
  private 0500 copy of the pinned plugin (under `<state>/plugins/<type>/bin/`, never setuid or
  setgid), refuses a plugin that carries `com.apple.quarantine` (clear it with
  `xattr -d com.apple.quarantine <path>`), and accepts thin and universal Mach-O binaries.
