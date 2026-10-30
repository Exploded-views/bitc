# Installing bitc

The current release is [v0.1.1](https://github.com/Exploded-views/bitc/releases/tag/v0.1.1). It
runs on Linux (amd64 or arm64); the macOS builds (Apple silicon or Intel) run local pools only
([macOS status](#macos-status)).

## Release binaries

Each release publishes a tarball `bitc_0.1.1_<os>_<arch>.tar.gz` (`<os>` is `linux` or `darwin`,
`<arch>` is `amd64` or `arm64`; it holds `bitc`, `LICENSE` and `README.md`), the bare binary
`bitc_0.1.1_<os>_<arch>`, and `checksums.txt`. Download an asset and `checksums.txt` and verify the
asset before use:

```sh
curl -LO https://github.com/Exploded-views/bitc/releases/download/v0.1.1/bitc_0.1.1_linux_amd64.tar.gz
curl -LO https://github.com/Exploded-views/bitc/releases/download/v0.1.1/checksums.txt
grep bitc_0.1.1_linux_amd64.tar.gz checksums.txt | sha256sum -c -
tar -xzf bitc_0.1.1_linux_amd64.tar.gz bitc
install -m 0755 bitc ~/.local/bin/bitc      # or any other directory on your PATH
bitc version --json
```

On macOS, use `shasum -a 256 -c -` in place of `sha256sum -c -`.

`bitc version --json` prints one compact JSON line, for example
`{"commit":"86b2cdb","config_schema":1,"release":true,"state_schema":1,"version":"0.1.1"}`.
`release` is `true` only for release builds. Checksums are unsigned for now; signed checksums are
planned after 1.0. [RELEASING.md](RELEASING.md) describes what a release contains.

[crewkit](https://github.com/Exploded-views/crewkit) can also install a pinned bitc release for
you, with the same checksum checks.

## From source

To build from source you need Go 1.26 or newer (CI tests 1.27.3):

```sh
go install github.com/Exploded-views/bitc/cmd/bitc@v0.1.1
```

A build made this way reports `"release":false`. That matters only for remote providers: the
agent source `release` downloads the matching release binary, so a source build needs
`agent = { source = "url", ... }` or `source = "build"` instead (see [RELEASING.md](RELEASING.md)).

From a clone, `make build` writes `dist/bitc`. [CONTRIBUTING.md](../CONTRIBUTING.md) covers the
toolchain.

## macOS status

The v0.1.1 macOS builds are **local pools only**: `run`, `status`, `wait`, `logs` and `cancel` over
`flock` pools, plus detached jobs, groups, priority lanes, nested holds, `doctor` and `config`.
Memory floors (a pool's `memory` key) and any `[providers.*]` table are refused with exit 78.
Tracked in:

- [#3](https://github.com/Exploded-views/bitc/issues/3): macOS parity (umbrella);
- [#4](https://github.com/Exploded-views/bitc/issues/4): macOS as a client to remote providers
  (planned for 0.1.2);
- [#5](https://github.com/Exploded-views/bitc/issues/5): macOS as a remote build target (design
  first).

Linux-only features, and other differences on macOS:

- remote providers (`ssh`, `runpod`, `sprites` and exec plugins, so `sweep` and
  `doctor --remote` have nothing to manage);
- the remote agent (`bitc __agent`) and the plugin exec shim: one `bitc: ... runs on Linux only`
  line and exit 69;
- the parent-death signal: on macOS a command outlives a bitc that was killed with SIGKILL;
- the inotify wake-up: a waiting caller re-checks every `poll` (5s by default) instead;
- the read-only lock view (`/proc/locks`): `status` probes a lock with a brief non-blocking
  `flock`;
- a detached job's log may have an empty line before its `__EXIT__` line.
