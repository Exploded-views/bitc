# Providers

A **provider** is a place where a command can run. A pool lists the providers it may use, in
order, for example `providers = ["runpod", "local"]`. [concepts.md](concepts.md#providers) explains
selection and fallback; [spec.md](spec.md) section 8 has every rule.

## Remote providers are opt-in

Remote providers are experimental and off by default: the host file must opt in with
`experimental_remote = true` under `[host]`, or any `[providers.<type>]` table (or a pool naming
one) is a config error (exit 78; see [Known issues](../README.md#known-issues)). A repository file
cannot set it and no environment variable does.

```toml
[host]
experimental_remote = true
```

The switch is new on `main` and ships in the next release. The v0.1.0 release does not know the
key (an unknown key is exit 78) and accepts provider tables without it; the cautions in
[Known issues](../README.md#known-issues) apply to it all the same.

## What every remote provider shares

Remote jobs take no local slot, need the host to allow them, and share the same safety rails: a
daily spend cap per provider account, a live-job limit, a ledger and a circuit breaker. A job
that has not started remotely falls back to the next provider in the pool's list, and finally to
`local`. Once a remote job has started it never falls back to another provider: an
infrastructure failure after that point is exit 75.

## The providers

| Provider | Where the job runs | State |
| --- | --- | --- |
| `local` | this machine, in a slot | stable, built in |
| `runpod` | a RunPod pod created for the job and deleted afterwards | experimental: tested against a fake API and proven live on CPU pods earlier; a live re-proof of the v0.1.0 release path is pending |
| `sprites` | a fresh microVM (Fly Sprite) for every job | jobs **refused in this release** (exit 78, even with `experimental_remote`), see [#6](https://github.com/Exploded-views/bitc/issues/6): its network policy does not restrict egress yet, and a job would receive the forwarded environment and the deploy key. With `experimental_remote`, `bitc sweep` still cleans up Sprites left by 0.1.0 (list, owner check, delete only) |
| `ssh` | a Linux machine you own, over ssh | experimental: implemented and tested against fakes and, in maintainer-run tests, a real sshd. Host setup: [ssh-host.md](ssh-host.md) |
| exec plugin | anything, through an executable `bitc-provider-<type>` pinned by path and sha256 | experimental: [writing-a-provider.md](writing-a-provider.md), [provider-author-guide.md](provider-author-guide.md), [plugin-protocol.md](plugin-protocol.md) |

## Running them safely

Remote providers leave resources that cost money if a job dies at the wrong moment, so run
`bitc sweep` every 15 minutes from a timer ([sweep-timer.md](sweep-timer.md)) and check setup
with `bitc doctor --remote`. [threat-model.md](threat-model.md) says what bitc defends against
and what it does not.

Complete host and repository files with a provider are in
[examples/config/](../examples/config/), [examples/node-monorepo/](../examples/node-monorepo/) and
[examples/python-monorepo/](../examples/python-monorepo/). Every key is in
[config-reference.md](config-reference.md).
