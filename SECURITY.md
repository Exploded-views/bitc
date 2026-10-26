# Security policy

## Supported versions

bitc is pre-1.0. Security fixes go to the `main` branch, and the latest release (currently
[v0.1.0](https://github.com/Exploded-views/bitc/releases/tag/v0.1.0)) is supported; older ones are
not.

| Version | Supported |
| --- | --- |
| `main` | yes |
| latest release | yes |
| anything older | no |

## Reporting a vulnerability

Report a vulnerability privately through GitHub security advisories on this repository: open the
Security tab and choose **Report a vulnerability**
(<https://github.com/Exploded-views/bitc/security/advisories/new>). Please do not open a public
issue, pull request or discussion for it.

Include the bitc version (`bitc version --json`), what you did, what you expected and what
happened. A minimal config that shows it helps; remove credentials first. You will get an answer
as soon as a maintainer can read the report; there is no service-level promise. Fixes and
advisories are published together, and reporters are credited unless they ask not to be.

## What counts

- a secret that leaks: an API key, token or key file appearing in argv, a child environment, a
  log, the history, a ledger, a URL or any output;
- a spend cap bypass: a remote job started past the configured cap or the live-job limit;
- a sandbox escape from repository config: a repository file that makes bitc do something the host
  config forbids, or run something outside the argv it was given;
- results forgery: a pod or another job producing a result, artifact or exit code that bitc
  accepts as genuine.

## Out of scope

- Problems that need a hostile host owner or a hostile local administrator.
- A command that does harm with the permissions the caller already has: bitc runs the argv it is
  given and is not a sandbox for it.
- Findings in dependencies with no path to bitc's behaviour; report those upstream.

[docs/threat-model.md](docs/threat-model.md) says what bitc defends against and what it does not.
