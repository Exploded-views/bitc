# Releasing bitc

Release steps are maintainer-only: nothing here runs in CI. Releases are
published at `https://github.com/Exploded-views/bitc/releases`. The pipeline is GoReleaser
(`.goreleaser.yaml`); the release is created as a draft and stays private until a maintainer
publishes it.

## What a release contains

| Asset | Purpose |
| --- | --- |
| `bitc_<version>_linux_amd64.tar.gz`, `bitc_<version>_linux_arm64.tar.gz` | the binary, `LICENSE`, `README.md` |
| `bitc_<version>_linux_amd64`, `bitc_<version>_linux_arm64` | the bare binaries: what the remote agent `release` source downloads |
| `checksums.txt` | sha256 of every asset above (unsigned until after v1) |

The agent `release` source builds its URL from the running binary's version:
`https://github.com/Exploded-views/bitc/releases/download/v<version>/bitc_<version>_linux_<arch>`, and
takes the expected sha256 from `checksums.txt` in the same directory. Only a binary built by
this pipeline (`release=true` in `bitc version --json`) can use it; every other build needs
`agent = { source = "url", ... }` or `source = "build"`.

`bitc version` reports the stamped version. A binary built with
`go install github.com/Exploded-views/bitc/cmd/bitc@v<x.y.z>` carries no stamp, so it reports the module
version from the Go build info instead (`release` stays `false`).

## CI and badges

`.github/workflows/ci.yml` runs on pushes to `main` and on pull requests: `make ci` (vet,
golangci-lint, unit and conformance tests with `-race`, examples, link check) and
`goreleaser check`, with `setup-go` given the same Go version as the image pinned in `scripts/dev.sh`
(`scripts/dev.sh` itself is not used in CI) and the tool versions pinned by `make tools-ci`. It does not build or upload a release. The README badges point at that workflow,
the latest GitHub release (they show nothing useful until the first tag), pkg.go.dev and Go Report
Card. There is no coverage badge: it needs a coverage service.

## Steps

1. Update `config.BuiltinKnownHosts` if the forge's published SSH host keys changed: check the
   three fingerprints against the forge's documentation page.
2. `scripts/dev.sh make ci && scripts/dev.sh make release-check`. The second command builds
   the snapshot (nothing is uploaded), runs `goreleaser check`, verifies every line of `checksums.txt` with
   `sha256sum -c` (the four assets: both tarballs and both bare binaries) and checks that the
   tarballs hold `bitc` and the bare binaries are executable.
3. `scripts/dev.sh make vuln` must be clean. This is the release gate (it is advisory
   during development). Otherwise list each finding in the release notes as a documented exception: the
   finding id and either why it does not reach bitc or the planned bump.
4. Tag: `git tag -s v<x.y.z>` (or `git tag -a v<x.y.z>`), then push the tag.
5. `scripts/dev.sh goreleaser release --clean`, with a `GITHUB_TOKEN` that only a maintainer holds,
   exported on the host (`scripts/dev.sh` passes it into the container with `-e GITHUB_TOKEN`).
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
