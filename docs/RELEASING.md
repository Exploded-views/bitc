# Releasing bitc

Releases are published at `https://github.com/Exploded-views/bitc/releases`. The pipeline is
GoReleaser (`.goreleaser.yaml`), run by `.github/workflows/release.yml` when a maintainer pushes a
`v*` tag; the release is created as a draft and stays private until a maintainer publishes it.
No personal token is involved: the workflow uploads with the release job's own `GITHUB_TOKEN`.

## What a release contains

| Asset | Purpose |
| --- | --- |
| `bitc_<version>_linux_amd64.tar.gz`, `bitc_<version>_linux_arm64.tar.gz` | the binary, `LICENSE`, `README.md` |
| `bitc_<version>_darwin_amd64.tar.gz`, `bitc_<version>_darwin_arm64.tar.gz` | the macOS client build, `LICENSE`, `README.md` |
| `bitc_<version>_linux_amd64`, `bitc_<version>_linux_arm64` | the bare binaries: what the remote agent `release` source downloads |
| `bitc_<version>_darwin_amd64`, `bitc_<version>_darwin_arm64` | the bare macOS client binaries (never an agent: the agent runs on Linux) |
| `checksums.txt` | sha256 of every asset above, eight lines (unsigned until after v1) |

The agent `release` source builds its URL from the running binary's version:
`https://github.com/Exploded-views/bitc/releases/download/v<version>/bitc_<version>_linux_<arch>`, and
takes the expected sha256 from `checksums.txt` in the same directory. Only a binary built by
this pipeline (`release=true` in `bitc version --json`) can use it; every other build needs
`agent = { source = "url", ... }` or `source = "build"`.

`bitc version` reports the stamped version. A binary built with
`go install github.com/Exploded-views/bitc/cmd/bitc@v<x.y.z>` carries no stamp, so it reports the module
version from the Go build info instead (`release` stays `false`).

## CI and badges

`.github/workflows/ci.yml` runs on pushes to `main`, on pull requests and in the merge queue
(`merge_group`: the queue builds the merge commit and runs these same jobs). It does not build or
upload a release. Most of its jobs are one `make` command that also runs locally (`build`, `secrets` and
`macos-smoke` are not); `make ci` still runs everything, unsharded:

- `lint`: `make lint` (vet for linux and darwin, golangci-lint, the script self-checks, and the
  check that every conformance test is in exactly one shard).
- `unit`: `make test-unit LEG=<leg>` under `-race`, legs `providers` and `core`; each leg uploads
  its output.
- `coverage`: needs `unit`; `make coverage-merge UNIT_LEGS="providers core"` merges the legs and
  checks the coverage floors on the merged run. It runs even when a leg failed (that leg's own `unit` check is red), and then fails by name on a
  missing, empty or failed leg output.
- `conformance` and `conformance-race`: one leg per shard of `scripts/conformance-shards.sh`
  (`sh`, `fb-sp`, `dj-rr-re`, `rest`, `plugin`, `provider`), against a plain and a `-race` bitc;
  `rest` is every test no other shard claims, so a new test is never skipped.
- `docs`: `make docs` (the example configs and the link check).
- `build`: `goreleaser check` and the darwin cross-builds.
- `secrets`: the gitleaks scan. `macos-smoke`: the client on a macOS runner.

`setup-go` is given the same Go version as the image pinned in `scripts/dev.sh` (`scripts/dev.sh`
itself is not used in CI), and the tool install (`make tools-ci`, versions pinned) is retried up to
three times, 20 seconds apart, in both workflows.
`.github/workflows/release.yml` runs the release gates of steps 2 and 3 as hosted jobs, on pull requests, in the
merge queue and on pushes to `main`: the dry run runs in the merge queue as the `dry-run` job:
`make release-check` (`goreleaser check`, the snapshot build that uploads nothing, `sha256sum -c` over
`checksums.txt` and the tarball and binary checks), and the `vuln` job runs `make vuln`. The hosted run at the
release sha on `main` is the gate for both: step 4 needs that run green for the sha to release, except that a red `vuln` run is acceptable only when every finding is listed as an exception in the release notes (step 3). `vuln` is
advisory on a pull request (`continue-on-error`: a new advisory against code a PR did not touch must not block
every PR) and strict in the merge queue and on `main`; only `dry-run` is a required check, and `vuln` is not
in the ruleset. The same workflow also runs, on a pushed
`v*` tag, the release itself (steps 5 and 6). Its token is read-only except in the release job,
which alone has `contents: write`. The README badges point at the ci workflow,
the latest GitHub release, pkg.go.dev and Go Report
Card. There is no coverage badge: it needs a coverage service.

## Steps

1. Update `config.BuiltinKnownHosts` if the forge's published SSH host keys changed: check the
   three fingerprints against the forge's documentation page.
2. `scripts/dev.sh make ci && scripts/dev.sh make release-check` (the `dry-run` job runs the second command hosted
   too; its run at the release sha on `main` is the gate). The second command builds
   the snapshot (nothing is uploaded), runs `goreleaser check`, verifies every line of `checksums.txt` with
   `sha256sum -c` (the eight assets: the linux and darwin tarballs and bare binaries, amd64 and
   arm64) and checks that the tarballs hold `bitc` and the bare binaries are executable.
3. `scripts/dev.sh make vuln` must be clean (the `vuln` job runs it hosted; its push-to-`main` run at the release
   sha is strict). This is the release gate (it is advisory
   during development, and on pull requests). Otherwise list each finding in the release notes as a documented exception: the
   finding id and either why it does not reach bitc or the planned bump.
4. Get the owner's OK on the exact commit SHA to release (`git rev-parse HEAD`). Only then tag
   that commit (`git tag -s v<x.y.z> <sha>`, or `git tag -a`) and push the tag.
5. The tag push starts `.github/workflows/release.yml`: its release job builds the assets with
   GoReleaser and uploads them as a draft release with the job's own `GITHUB_TOKEN`
   (`contents: write` for that job only). No personal token is used or needed; nobody runs
   GoReleaser against the forge by hand.
6. Review the draft release: the assets above, `checksums.txt`, and the vuln exceptions if any.
   Then publish it.
7. Verify both install paths report `bitc <x.y.z>`:
   - `scripts/dev.sh go install github.com/Exploded-views/bitc/cmd/bitc@v<x.y.z>`, then
     `scripts/dev.sh bitc version`;
   - the tarball's `bitc version`.

Cosign signing of `checksums.txt` comes after v1.

## Module path

The release URLs, the GoReleaser repository settings and the Go module path all use
`github.com/Exploded-views/bitc`, so the `go install` line in step 7 works once a tag exists.
