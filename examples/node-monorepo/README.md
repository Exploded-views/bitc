# node-monorepo example

This neutral example models a Node monorepo with lint, unit-test, database, coverage and
end-to-end pools. `host.bitc.toml` holds the capacities (slots, memory, groups) and
`.bitc.toml` holds the project's environment, hooks and refuse rules.

To try it, run `BITC_CONFIG=examples/node-monorepo/host.bitc.toml bitc config explain` from
this directory. The conformance suite runs its project-flavoured rows against this example.

bitc refuses (78) a config file that is group- or world-writable. Git records no mode bits but
the exec bit, so a `.bitc.toml` checked out under umask 002 is 0664 and is refused until you run
`chmod go-w .bitc.toml`.
