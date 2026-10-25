# python-monorepo example

A neutral example for a Python repository with a unit-test pool, a type-check pool and one
heavy pool that may also run on a remote provider.

- `host.bitc.toml` is the host file: capacities, the memory floor and a shared `bigmem` group.
  Copy it to `~/.config/bitc/bitc.toml` and run `chmod 600` on it.
- `.bitc.toml` is the repo file, at the top of the work tree: a per-slot database port
  (`{slot+db_port_base}`), a refuse rule that keeps repo-wide `mypy` out of the `mypy` pool, and a
  remote profile with a tool pinned by sha256. Replace the placeholder sha256 and URL with your
  tool's before you send anything to a remote provider.

Try it from this directory:

```
BITC_CONFIG=host.bitc.toml bitc config check --repo .bitc.toml
BITC_CONFIG=host.bitc.toml bitc run pytest -- python -m pytest -q
```

bitc refuses (78) a config file that is group- or world-writable: run `chmod go-w .bitc.toml`
after a checkout under umask 002.
