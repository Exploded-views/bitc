# bitc configuration reference

Generated from `internal/config/schema.go` by `go generate ./internal/config`. Do not edit.

| Key | Class | Default |
| --- | --- | --- |
| `version` | file | `required` |
| `min_version` | file | unset |
| `host.state_dir` | host | `$XDG_STATE_HOME/bitc` |
| `host.poll` | host | `5s` |
| `host.memory_floor` | host | `6GiB` |
| `host.reservation_window` | host | `60s` |
| `host.unknown_pools` | host | `refuse` |
| `host.grace` | host | `10s` |
| `host.max_signal_wait` | host | `10m` |
| `host.history_max` | host | `50MiB` |
| `host.retention` | host | `14d` |
| `host.experimental_remote` | host | `false` |
| `defaults.slots` | host | `1` |
| `defaults.wait` | narrow | `10m` |
| `defaults.env` | project | unset |
| `defaults.env_default` | project | unset |
| `defaults.env_unset` | project | unset |
| `pools.<pool>.slots` | host | `1` |
| `pools.<pool>.wait` | narrow | `10m` |
| `pools.<pool>.memory` | host | unset |
| `pools.<pool>.groups` | host | unset |
| `pools.<pool>.providers` | host | `["local"]` |
| `pools.<pool>.providers_exclude` | repo | unset |
| `pools.<pool>.remote_limits.size` | host | unset |
| `pools.<pool>.remote_limits.timeout` | host | `90m` |
| `pools.<pool>.env` | project | unset |
| `pools.<pool>.env_default` | project | unset |
| `pools.<pool>.env_unset` | project | unset |
| `pools.<pool>.refuse[].tool` | project | unset |
| `pools.<pool>.refuse[].when` | project | `always` |
| `pools.<pool>.refuse[].unless_args` | project | unset |
| `pools.<pool>.refuse[].skip_option_values` | project | unset |
| `pools.<pool>.refuse[].npm_script` | project | unset |
| `pools.<pool>.refuse[].argv_prefix` | project | unset |
| `pools.<pool>.refuse[].reason` | project | `required` |
| `pools.<pool>.on_signal` | project | `terminate` |
| `pools.<pool>.hooks.pre_run.argv` | project | `required` |
| `pools.<pool>.hooks.pre_run.timeout` | project | `60s` |
| `pools.<pool>.hooks.pre_run.refuse_exit` | project | `[64]` |
| `pools.<pool>.hooks.post_run.argv` | project | `required` |
| `pools.<pool>.hooks.post_run.timeout` | project | `60s` |
| `pools.<pool>.hooks.post_run.refuse_exit` | project | `[64]` |
| `pools.<pool>.hooks.env.argv` | project | `required` |
| `pools.<pool>.hooks.env.timeout` | project | `60s` |
| `pools.<pool>.hooks.env.refuse_exit` | project | `[64]` |
| `pools.<pool>.hooks.wrap[].when_worktree` | project | `required` |
| `pools.<pool>.hooks.wrap[].argv` | project | `required` |
| `pools.<pool>.remote.allow` | repo | unset |
| `pools.<pool>.remote.artifacts[].path` | repo | `required` |
| `pools.<pool>.remote.artifacts[].replace` | repo | `true` |
| `pools.<pool>.remote.timeout` | narrow | unset |
| `pools.<pool>.remote.env` | repo | unset |
| `pools.<pool>.remote.env_unset` | repo | unset |
| `pools.<pool>.remote.arch` | repo | `["amd64"]` |
| `groups.<group>.units` | host | `required` |
| `priority.high_cwd_contains` | host | `[]` |
| `priority.high_cwd` | host | `[]` |
| `detach.wait_timeout` | host | `9m` |
| `vars` | project | unset |
| `remote_limits.worktrees` | host | `[]` |
| `remote_limits.forward_env_allow` | host | `[]` |
| `remote_limits.forward_env_force` | host | `[]` |
| `remote_limits.on_failure` | host | `next` |
| `providers` | host | unset |
| `repos[].match` | host | `required` |
| `repos[].config` | host | unset |
| `repos[].clone_url` | host | unset |
| `repos[].deploy_secret` | host | unset |
| `repos[].deploy_key_file` | host | unset |
| `repos[].root_steps` | host | `false` |
| `compat.legacy_dir` | host | `required` |
| `compat.hold_env` | host | unset |
| `compat.name_prefix` | host | unset |
| `compat.groups` | host | unset |
| `test.procfs_root` | host | unset |
| `test.time_scale` | host | unset |
| `test.http_results` | host | unset |
| `test.agent_url` | host | unset |
| `test.accounting_dir` | host | unset |
| `test.fixed_secrets` | host | unset |
| `test.ssh_exec` | host | unset |
| `test.ssh_gate` | host | unset |
| `test.ssh_root` | host | unset |
| `test.ssh_sshd` | host | unset |
| `test.allow_sprites` | host | unset |
| `remote.forward_env` | repo | unset |
| `remote.forbid_ignored` | repo | unset |
| `remote.allow_ignored` | repo | unset |
| `remote.env` | repo | unset |
| `remote.env_unset` | repo | unset |
| `remote.packages` | repo | unset |
| `remote.tools[].name` | repo | `required` |
| `remote.tools[].arch` | repo | `amd64` |
| `remote.tools[].url` | repo | `required` |
| `remote.tools[].sha256` | repo | `required` |
| `remote.setup[].name` | repo | `required` |
| `remote.setup[].run_as` | repo | `job` |
| `remote.setup[].argv` | repo | `required` |
| `remote.services[].name` | repo | `required` |
| `remote.services[].run_as` | repo | `job` |
| `remote.services[].argv` | repo | `required` |
| `remote.services[].ready` | repo | `required` |
| `remote.services[].ready_timeout` | repo | `120s` |

## Provider sections (`[providers.<type>]`, decoded by core)

| Key | Default |
| --- | --- |
| `providers.<type>.api_key` | unset |
| `providers.<type>.api_key_file` | unset |
| `providers.<type>.name_prefix` | `bitc` |
| `providers.<type>.daily_usd` | `required` |
| `providers.<type>.max_live` | `2` |
| `providers.<type>.launch_wait` | `5m` |
| `providers.<type>.start_timeout` | `20m` |
| `providers.<type>.fallback_until` | `the provider's declaration ("resource" for a plugin)` |
| `providers.<type>.send_env` | `the provider's declaration ("forced" for runpod; "forwarded" for a plugin)` |
| `providers.<type>.breaker` | `{failures = 3, window = "60m"}` |
| `providers.<type>.transport` | `{remote = "origin", ref_prefix = "refs/bitc/jobs/", known_hosts = <built-in>, kind = "git-push", max_bundle = "1GiB"}` |
| `providers.<type>.agent` | `{source = "release"}` |
| `providers.<type>.artifacts` | `{max_compressed = "200MiB", max_unpacked = "2GiB", max_entries = 100000}` |
