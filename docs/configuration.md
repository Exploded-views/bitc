# Configuration

There are two files, and both must start with `version = 1`, use strict decoding (an unknown key
is an error) and not be group- or world-writable.

## The host file

The **host file** (`~/.config/bitc/bitc.toml`, or `--config`, or `BITC_CONFIG`) belongs to the
account. It sets capacities, memory floors, groups, priorities, providers, credentials, spend caps
and per-repo grants. A file named by `--config` or `BITC_CONFIG` must exist, or every command
exits 78; a missing default file just means built-in defaults:

```toml
version = 1

[host]
memory_floor = "6GiB"          # a pool that sets memory holds a job back below this much free memory

[pools.lint]
slots = 1

[pools.heavy]
slots = 1
wait = "1h"
memory = "8GiB"                # an admission reservation, not a limit on the command
```

## The repo file

The **repo file** (`.bitc.toml` at the top of a work tree) belongs to the project. It can add
environment, hooks and refuse rules, and can only narrow what the host allows:

```toml
version = 1

[pools.heavy]
wait = "45m"                   # narrow only
env = { NODE_OPTIONS = "--max-old-space-size=8192" }

[pools.lint]
refuse = [{ tool = "tsc", reason = "typecheck runs in heavy" }]
```

A repo file cannot raise a capacity, lengthen a wait, enable a provider or remove a host refusal.
A host-only key in a repo file is an error (exit 78).

## Checking and reference

`bitc config check` validates both (`--repo PATH` checks a repo file other than the one in the
current work tree). Every key, its class (host, repo, narrow) and its default is in
[config-reference.md](config-reference.md); worked examples are in
[examples/config/](../examples/config/), [examples/node-monorepo/](../examples/node-monorepo/) and
[examples/python-monorepo/](../examples/python-monorepo/). Concepts (pools, groups, tickets, nested
holds, providers) are in [concepts.md](concepts.md).

A wrapper that needs to know whether bitc is usable should run `bitc config check`, not
`bitc version` (which reads no config). Exit 0 means a usable config; 78 means config errors.
From 0.1.2 on it also checks the state directory's permissions.
