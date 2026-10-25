# Provider author guide

A provider decides where a job runs. There are two ways to add one:

- an **exec plugin**: a separate executable `bitc-provider-<type>` that talks to bitc over a
  versioned NDJSON protocol. No change to bitc is needed. Start with
  [`writing-a-provider.md`](writing-a-provider.md), then use [`plugin-protocol.md`](plugin-protocol.md) as the
  reference and [`../examples/plugins/bitc-provider-fakex/`](../examples/plugins/bitc-provider-fakex/)
  as a working example.
- a **built-in provider**: a Go package in this repository (`runpod`, `sprites` and the in-process
  `fake` are built this way). This page covers that path. Both paths meet the same interface and
  the same conformance suite.

## The interface

A provider runs one job away from the host's admission path. It implements the Go interface in
[`internal/provider/provider.go`](../internal/provider/provider.go) (spec 8.1):

| Method | Job |
| --- | --- |
| `Info` | type, `Caps`, the endpoints it talks to, the architectures it can run |
| `Preflight` | decide, without any network call, whether this job can go here; otherwise `ErrIneligible` |
| `Estimate` | price times the time until the job's expiry, in micro-dollars (feeds the spend cap) |
| `Launch` | create the resource, send the job, return a `Handle` once job input has reached it |
| `Watch` | report the phases `booting`, `begun`, `ended`, `lost`, stream output, return the exit code |
| `Output` | the full output from an offset, after the end (recovers a dropped tail) |
| `Fetch` | stream one artifact (only with `Caps.Artifacts`) |
| `Owner` | the owner tag a resource was created with |
| `Teardown` | delete a resource; idempotent and bounded to 10 seconds |
| `List` | the account's resources whose names start with given prefixes |
| `Spend` | the provider's spend today in UTC (only with `Caps.Spend`) |

`Caps` declares what the provider can do: `LiveLogs`, `Artifacts`, `Spend`, `SelfDelete`,
`FallbackUntil` (`resource` or `attach`), `SendEnv` (the default level of caller environment that
may cross) and `MultiJob` (a resource serves more than one job). Without an optional capability the
shared code degrades safely: no live logs means output is printed once after the end, no artifacts
means a pool that declares artifacts is ineligible, no spend call means the cap is enforced on the
ledger and live resources alone.

## Registering

A built-in package registers itself from `init` with `provider.Register(type, provider.Factory{...})`
(see [`internal/provider/registry.go`](../internal/provider/registry.go)). The factory supplies:

- `Endpoints`: every host the provider talks to (the config loader uses them to reject test seams
  that would meet a real service);
- `New`: builds the provider from the decoded core section and the effective config;
- `Validate`: checks the provider-specific keys at load, returning host-file problems (exit 78);
- `FallbackCap`, `SendEnv`, `MultiJob`: the capabilities that depend on the keys;
- `SelfDelete`: the hook the remote agent uses to delete its own resource at the deadline.

Add one blank import in `internal/cli/register_<type>.go` so the type is linked in. The core keys
(`api_key`, `api_key_file`, `name_prefix`, `daily_usd`, `max_live`, `launch_wait`, `start_timeout`,
`fallback_until`, `send_env`, `breaker`, `transport`, `agent`, `artifacts`; see
[`config-reference.md`](config-reference.md)) are decoded by bitc itself. A provider decodes only
its own extra keys, strictly: an unknown key is an error.

## What you get for free

Every remote provider sits behind the same rails, implemented once in bitc core and priced from
host configuration, so a provider cannot weaken them:

- the daily spend cap per provider account, `max_live`, the launch lock and per-job reservations;
- an append-only ledger and a circuit breaker;
- resource naming and the owner tag, and a sweep that tears down only what it can prove it owns;
- the git-push transport, the remote agent, setup steps, services and tools, marker lines and the
  result MAC, artifact validation, and output completion after a dropped log tail;
- selection, fallback and the exit-code rule (69 or 75) of spec 8.2.

## Error mapping

Return the error class that matches what happened; core decides the rest.

| Error | Meaning | Before the fallback boundary | After it | Breaker |
| --- | --- | --- | --- | --- |
| `ErrIneligible` | this job cannot run here (argv, size, architecture, request too large, caller env above `send_env`) | next provider | n/a | no |
| `ErrTransient` | capacity, 429 or 5xx, network | one create retry, then next provider | 75 | yes |
| `ErrUnavailable` | auth, no balance, disabled account | next provider and a pause record | 75 | pause |
| `ErrSetup` (`repo`) | the repository's own setup step failed | 69 | 69 | no |
| `ErrSetup` (`infra`) | provisioning failed | as `ErrTransient` | 75 | yes |
| `ErrPreAttach{ID, Err}` | a resource was made but no job input reached it | depends on `fallback_until` | n/a | yes |
| `ErrAttached{ID, Err}` | failure after job input reached the resource | never falls back | 75 | yes |
| `ErrConfig` | bad configuration found when the provider starts | 78 | 78 | no |

The one invariant: after job input has reached a resource, the job never falls back to another
provider. `Launch` returns a handle only at that moment. A provider that can discard a resource
before any job input reaches it declares `FallbackUntil = attach`; one whose create request carries
the job declares `resource`.

## The provider conformance suite

[`conformance/provider/`](../conformance/provider/) runs the same scenarios against every
provider: launch and exit codes, fallback on a failed create, setup failures, a lost or restarted
resource, a dropped log tail, a forged result, listing with pagination, teardown and the
boundary rules. Each provider supplies a `Target` (in `suite.go`) that wraps **its own fake API**:

| `Target` method | Purpose |
| --- | --- |
| `Name` | the provider type |
| `Configure` | write the host and repo config for a pool `heavy` that goes to this provider |
| `Behave` | set the fake's next behaviour: `ok`, `create-fails`, `setup-fails-after-create`, `setup-fails-repo-after-create`, `lost-after-begin`, `restart-after-begin`, `drop-log-tail`, `forge-result`, `preattach-fails`, `paginate-2` |
| `Calls` | the fake API calls seen so far |
| `TornDown` | the ids of resources torn down so far |
| `Supports` | whether a scenario applies, by capability |

To add a provider: write a fake of its API on loopback (see `internal/provider/runpod/runpodfake`),
write a `Target` for it in `conformance/provider/<type>_test.go`, run the suite and make every
applicable scenario pass. A target fails the test on a behaviour it cannot produce, so none is
silently skipped. Tests never use the network beyond loopback and never call a real provider API.

## The ssh provider

A provider for a machine you already own is a fixed-capacity target: it declares `attach` and a
fixed number of slots instead of a price per hour. It is implemented and experimental. The host
side (a restricted login, a gate program, a setup script with a firewall self-test and
`check-host`) is documented in [`ssh-host.md`](ssh-host.md); the design is in
[`design/providers-delta.md`](design/providers-delta.md).
