# Contributing to bitc

Thank you for helping. Issues and pull requests are welcome. Releases are cut by the maintainers
only. By taking part you agree to follow the [Code of Conduct](CODE_OF_CONDUCT.md). Report security
problems privately, as described in [SECURITY.md](SECURITY.md).

For a larger change, open an issue first so we can agree on the shape before you write code.

## Build and test

You need Linux. Either install Go (the version in `go.mod` or newer, plus `gcc` for `-race`) and
run the Makefile directly, or use the pinned toolchain container, which needs only `docker`:

```sh
make tools-ci               # once: golangci-lint and goreleaser, at the pinned versions
make ci                     # lint, unit tests, conformance suite (also with -race), examples, link check

scripts/dev.sh make ci      # the same, inside one pinned toolchain image; nothing else to install
```

Smaller runs while you work: `go test ./internal/<package>/` (or `scripts/dev.sh go test ...`) for
one package. `make ci` must pass before a pull request is ready; CI runs the same target.
`make vuln` (govulncheck) is run by the maintainers at checkpoints and before a release.

Tests never touch the network beyond loopback and never call a real provider.

## The conformance register

Every behaviour bitc promises has a row in `conformance/behaviours.toml` and a test named
`Test<ID>_<Words>` in `conformance/` (for example `TestS1_OneSlotNeverOverlaps`). A row is
`todo`, `ported`, `changed` or `dropped`; a meta test checks that the rows and the tests agree.
A new or changed behaviour updates its row and its test in the same change.

## Test-driven development

Write the test first and watch it fail for the reason it asserts (not only fail to compile), then
write the code. Put the failing line of each new test in the commit message under `Failing first:`.

## Documentation and links

Code blocks marked `<!-- doc-test -->` in the README and docs are executed by the test suite.
`scripts/linkcheck.sh` (part of `make ci`) fails on a relative markdown link that does not
resolve. `docs/config-reference.md` is generated: run `go generate ./internal/config` rather than
editing it. User-visible changes get a line under Unreleased in [CHANGELOG.md](CHANGELOG.md).

## Commits and pull requests

- One commit per green step, with a message of the form `<type>(<area>): <what>`, for example
  `feat(admit): FIFO tickets`. Keep a change small and inside the area it names.
- There is no DCO or CLA. Contributions are accepted under the project's [MIT license](LICENSE).
- Fill in the pull request template. Rebase on `main`; do not merge `main` into your branch.
- Add no Go dependency without discussing it in an issue first.
- Never commit secrets, tokens, host names or personal data, in code, tests or fixtures.

## Adding a provider

See [docs/provider-author-guide.md](docs/provider-author-guide.md): a provider is either an exec
plugin (`bitc-provider-<type>`) or a compiled-in package, and both are checked by the provider
conformance suite.

## Privileged tests

Some tests (for example the ssh host setup) must act as root: they create users, mounts and
namespaces, and use a firewall. They are tagged privileged, they are not part of `make ci`, and
they never run on a contributor's or a CI machine by default. The maintainers run them on a
dedicated host or an isolated runner after review. Do not run `scripts/privileged.sh` yourself.
If your change touches privileged code, say so in the pull request and a maintainer will run the
suite; you can still add and run the unprivileged tests with `make ci`.

## Maintainer checks

Before a release the maintainers also run `make secrets`, a whole-history scan with gitleaks
(installed by `make tools`, configured by `.gitleaks.toml`). It prints only the number of findings
and the rule, file and commit of each, never the match.
The release steps are in [docs/RELEASING.md](docs/RELEASING.md).
