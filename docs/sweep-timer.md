# Sweep timer runbook

`bitc sweep` cleans local state (dead tickets, lost jobs, history rotation, stale artifact
staging dirs) and, when remote providers are configured, tears down this install's resources that
are past their expiry and deletes its job refs older than 3h. A job that dies hard (the host loses
power, bitc is SIGKILLed) leaves its resource behind until a sweep removes it, so run the sweep on
a timer. A systemd **user** timer that runs `bitc sweep` every 15 minutes needs these two unit
files.

Remote providers are off by default in this release: with a `[providers.<type>]` table but no
`experimental_remote = true` under `[host]`, every command, `bitc sweep` included, exits 78 (spec
5.8). To clean up resources an earlier version left, set the flag and run `bitc sweep`. Sprites
jobs are refused in this release, but with the flag the sweep (and this timer) still cleans up
Sprites left by 0.1.0: it only lists them, checks their owner tag and deletes this install's.

```ini
# ~/.config/systemd/user/bitc-sweep.service
[Unit]
Description=bitc sweep (local state and leaked remote resources)

[Service]
Type=oneshot
ExecStart=%h/go/bin/bitc sweep
```

```ini
# ~/.config/systemd/user/bitc-sweep.timer
[Unit]
Description=Run bitc sweep every 15 minutes

[Timer]
OnBootSec=5min
OnUnitActiveSec=15min
Persistent=true

[Install]
WantedBy=timers.target
```

Then enable the timer:

```sh
systemctl --user daemon-reload && systemctl --user enable --now bitc-sweep.timer
loginctl enable-linger $USER
```

`enable-linger` lets the timer run without a login session. Adjust `ExecStart` to wherever `bitc`
is installed.

## Reading the output

```sh
journalctl --user -u bitc-sweep
systemctl --user list-timers bitc-sweep.timer
```

Each torn-down item is one `swept ...` line on stdout; problems are `bitc: sweep: <provider>: ...`
lines on stderr. Sweep exits 0 unless its own arguments are wrong (64) or the config is invalid
(78), so a provider outage never fails the unit: read the journal for the stderr lines.

Run `bitc sweep --dry-run` to see what a sweep would remove without removing it. Only resources
carrying this install's verified owner tag are torn down; another install's resources are touched
only with `--foreign`.

## Sprites: the timer is mandatory

A Sprite has no self-delete, so for the `sprites` provider the timer is not optional, including in
this release, where Sprites jobs are refused and the sweep only cleans up (spec 5.8). The sweep
records each successful listing, and `bitc doctor --remote` reports `fail sprites sweep timer` (exit
69) when no sweep ran in the last 30 minutes; a `--dry-run` does not count. Besides Sprites past
their expiry, the sweep deletes this install's Sprites that no live reservation on the host
accounts for once they are older than `start_timeout` (`job process gone`) and orphaned ones
(`orphaned by a pre-attach discard`). A Sprite that belongs to another organization fails the whole
listing: the sweep names it on stderr, never touches it, and sweeps nothing until it is deleted by
hand.

## Warning

A stopped resource keeps billing for its disk until it is swept or deleted. If the timer is not
running, expired resources keep costing money. Check `bitc status` for the remote section and
`bitc doctor --remote` for the provider checks.
