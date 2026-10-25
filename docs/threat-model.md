# Threat model

bitc guards a shared host against **accidents** (an agent that runs ten test suites at once) and
against **untrusted repository content** (a `.bitc.toml` from a branch you have not read). It does
not defend an account against a process that already runs as that account.

## What a repo file cannot do

A repo file (`.bitc.toml`) cannot raise capacities, enable a provider, grant root steps or deploy
keys, widen the caller-env allowlist, lengthen waits or remove host refusals. Such a key in a repo
file is a config error (78). A private `BITC_DIR` or `BITC_CONFIG` still shares the fixed
accounting directory, so a second state dir cannot multiply the daily spend cap. The `[test]`
seams in the host file are accepted only when every configured provider's endpoint is a loopback
address, so a test configuration cannot reach a real service.

Repo hooks (`pre_run`, `post_run`, `env`, `wrap`) run as you, like the command itself, and never
on a remote machine.

## Secrets

Provider credentials come only from the provider's section of the host file (`api_key`, host file
mode 0600) or from a file it names (`api_key_file`: the key alone on one line, mode 0600, owned by
you; anything else is a config error). They are used only by bitc's HTTP client. Secrets never
appear in argv, in a child's environment, logs, history, the ledger, URLs or output. A redacting
writer wraps every log sink, and `bitc config` and `bitc doctor` print `<redacted>`.

## Caller environment on remote providers

The host decides, per provider, how much of the caller's forwarded environment may leave the
machine (`send_env`: `none`, `forwarded` or `forced`). A resource that serves more than one job
never receives forced names, because its disk, files and processes can outlive the job.

## Remote jobs: least privilege

Per-repo deploy keys are granted by the host and shredded before repo code runs. The agent in the
remote machine starts without the caller's environment, listens from boot, and reports through
results that carry a MAC and marker lines checked by bitc. The bearer token is never logged, no
volume is shared between jobs, artifacts are validated before they are unpacked, the agent and
tools are verified by sha256, and a resource is torn down only when its owner tag verifies. Use
the narrowest API key the provider offers.

A repo that the host has granted `root_steps` runs setup and service steps as root on the remote
machine. That repo is trusted with its own results, but it still cannot reach other repos' keys.

## Plugins

A provider plugin is an executable pinned in the host file by absolute path and sha256. bitc checks
the file before it runs (regular file, owner, no setuid, setgid or file capabilities, no group- or
world-writable parent directory) and runs it with `no_new_privs`. A pinned plugin is **fully
trusted host code**: it runs as your account, receives the provider credential and every job's
spec, and can read anything else the account can read, including the install secret that signs
owner tags, the ledger and other providers' key files. bitc's own checks on markers, result MACs
and spend rails catch a *buggy* plugin, not a hostile one. Install only plugins you would run
yourself.

## Out of scope

- A process running as the same account can read the provider key and call the provider itself, or
  edit bitc's state files.
- Members of a provider's account can read the environment a resource was created with.
- Anything bitc does not run: bitc does not sandbox the commands you pass it on the local machine.
- Denial of service by someone with shell access to the host. bitc is cooperative on the local
  machine: it keeps well-behaved callers from hurting each other.

## No shell, no eval

Commands, hooks and setup steps are argument arrays, passed to `execve` as given. Hook output is
parsed, never evaluated.

## Supply chain

Releases are built with `-trimpath` and `CGO_ENABLED=0`, ship a sha256 `checksums.txt`, and CI runs
`govulncheck`; dependencies are updated by Dependabot. Signed checksums come after v1.
