# ssh host runbook

How the owner prepares a Linux machine as an `ssh` provider host (providers delta C.5 to C.8).
Everything the host gets is installed by `scripts/ssh-host-setup.sh`; this page says what to run,
what it will change and how to check and undo it. Read the script before running it as root.

## What you need

- A Linux machine you do not mind jobs running on: an OrbStack Ubuntu machine, or any Ubuntu 22.04
  or newer with systemd, sshd, sudo, nftables (the script installs it with apt when missing) and
  cgroup v2.
- A `bitc` binary built for that machine's architecture (`GOOS=linux`, `arm64` on an M-series
  Mac): `--agent` below. The firewall self-test runs it.
- The controller's ed25519 public key (the key without a comment).
- The addresses: the DNS resolver jobs may use (`--resolver`), the LAN router (`--router`, default
  192.168.1.1) and the controller-facing LAN address (`--controller`, required for a real run: the
  self-test must show a job cannot reach it on port 22).
- Swap off (`swapoff -a`, and remove it from `/etc/fstab`), or per-job secrets can reach disk.

### OrbStack Ubuntu machine

Create the machine with `orb create ubuntu bitc-host` and open a root shell with
`orb -m bitc-host -u root`. The setup script unmounts `/opt/orbstack-guest` and removes the
`mac`, `macctl` and `open` bridges; `check-host` fails while they exist. After the unmount it runs
`scripts/ssh-host-orbstack-clean.sh`, which removes exactly the known leftovers and nothing else
(each removal is printed; a second run prints `ok: no OrbStack leftovers`; `--dry-run` changes
nothing): the regular file `/opt/orbstack-guest/etc/resolv.conf`, the symlink `/mnt/linux`, and the
empty directories `/opt/orbstack-guest/etc`, `/opt/orbstack-guest`, `/mnt/mac`, `/mnt/machines`,
`/Users/<name>` and `/Users`. When `/etc/resolv.conf` is a link into `/opt/orbstack-guest` whose
target is a regular file, it is replaced by a regular copy first, in one rename. Anything else there
(an unknown file, a non-empty directory, another link) stays, and `check-host` keeps failing on it.
The old link texts are kept in `/var/lib/bitc/resolv.conf.link` and `/var/lib/bitc/mnt-linux.link`.
An OrbStack restart brings the leftovers back: run the setup script again. OrbStack machines share
one kernel with OrbStack's Docker engine (decision 10): you accepted that residual risk on
2026-10-23. Copy the repo's `scripts/` directory and the agent binary into the machine.

### Generic Ubuntu

Any other Ubuntu host works the same way; run the commands below as root (`sudo -i`). Make sure
`/etc/ssh/sshd_config` has the line `Include /etc/ssh/sshd_config.d/*.conf`; the script stops
otherwise.

## The one sudoers rule

The login account `bitc` may run exactly one program as root. The script installs this file as
`/etc/sudoers.d/bitc` (mode 0440) after `visudo -c` accepts it:

```
Defaults!/opt/bitc/sbin/bitc-gate !use_pty, !requiretty, !log_output, env_reset
bitc ALL=(root) NOPASSWD: /opt/bitc/sbin/bitc-gate
```

Print it any time with `sh scripts/ssh-host-setup.sh --print sudoers`. No other sudoers rule is
added; `check-host` fails if the file differs by a byte.

## Forwarding

Jobs reach the internet through the VM, so the VM must forward IPv4 (design C.8 item 7). The setup
script never writes kernel settings: it reads `/proc/sys/net/ipv4/ip_forward` and stops with a clear
message when it is not `1`. Turn it on yourself, as root, once, before the script:

```
cat /proc/sys/net/ipv4/ip_forward
printf 'net.ipv4.ip_forward = 1\n' >/etc/sysctl.d/60-bitc.conf
sysctl --system
```

(`sysctl --system` applies it now and at every boot the file applies it again.) Check that
`cat /proc/sys/net/ipv4/ip_forward` prints `1`. `check-host` fails `ssh-lan-rules` when it is not.

## The host

1. Look first. A dry run changes nothing and prints every change as `would: ...`:

   ```
   sh scripts/ssh-host-setup.sh --dry-run --resolver 9.9.9.9 --slots 2 \
     --controller-key 'ssh-ed25519 AAAA...' --router 192.168.1.1 --controller 10.20.0.50
   ```

   `--print sudoers|sshd|nft|host.conf|authorized-keys|nft-unit` shows one managed text.
2. Verify the agent binary, then run the script for real (as root). The script installs `--agent`
   as `/opt/bitc/sbin/bitc-selftest` and runs it as root without any hash check, so compare it with
   what you built or downloaded first:

   ```
   sha256sum /path/to/bitc    # must equal the release's published hash, or your own local build's
   ```


   ```
   sh scripts/ssh-host-setup.sh --resolver 9.9.9.9 --slots 2 \
     --controller-key 'ssh-ed25519 AAAA...' --router 192.168.1.1 --controller 10.20.0.50 \
     --agent /path/to/bitc
   ```

   It is idempotent: a second run prints `ok: ... is as wanted` for everything in place. It
   creates the users `bitc` and `bitc-s1..bitc-sN`, the layout under `/opt/bitc`, `/etc/bitc` and
   `/var/lib/bitc`, the sudoers rule, the `Match User bitc` sshd snippet, `cron.allow` and
   `at.allow` holding only root (the first run keeps your old files as
   `/var/lib/bitc/cron.allow.orig` and `at.allow.orig`, or an empty `.absent` marker when there was
   none), the `memory` and `pids` cgroup controllers delegated to `/sys/fs/cgroup/bitc`, and the nftables LAN block, loaded now and at boot. Jobs get all
   of the internet except the LAN (design C.8.7): the block covers the VM's gateway, RFC 1918,
   100.64/10, 198.18/15, link-local, multicast and every IPv6 address.
3. Read the self-test, which ends the script.

## The firewall self-test

The last step runs from inside the host as `bitc-s1`, through the same namespace and firewall path
jobs use. Every one of these must fail:

- the router on ports 80 and 443 (`--router`, default 192.168.1.1);
- the controller-facing LAN address on port 22 (`--controller`);
- the VM's own addresses and the job namespace's host end (`10.203.0.1:22`): with the firewall off,
  the VM's own sshd answers there, so a test run against a router that does not exist on your LAN
  cannot pass by accident;
- the VM's gateway, a ULA (`fd00::1`), a link-local (`fe80::1`), a 100.64/10 address and a 198.18/15
  address.

`github.com:22` and `registry.npmjs.org:443` (and DNS) must work. Output looks like
`blocked 192.168.1.1:80` and `egress ok github.com:22`, then `firewall self-test: ok` and the
script's exit status is 0.

Any other result prints `FIREWALL NOT ACTIVE` (something on the LAN side was reached) or
`EGRESS BROKEN` (an internet target failed), the script exits 69, and `/var/lib/bitc/selftest-failed`
(root-only) stays. While that file exists the gate refuses every `slot-boot` with
`refused: firewall self-test has not passed`; reads, `list`, `teardown` and `probe` still work.
Fix the cause (firewall not loaded, wrong `--router`, resolver or egress blocked on the network)
and run the script again; a passing self-test removes the marker. The script also creates the
marker before it changes anything, so an interrupted setup stays closed. To re-run only the test:
`/opt/bitc/sbin/bitc-selftest __agent firewall-selftest --router 192.168.1.1 --controller 10.20.0.50`.

On a network that cannot reach `github.com:22` or `registry.npmjs.org:443` (an air-gapped test rig),
`--allow HOST:PORT` (repeatable, on the script or the verb) names the targets the test must reach
instead. It changes only what the test expects to reach: it adds, opens or changes no firewall rule
(the ruleset is byte-identical with and without it). A literal private (RFC1918), loopback,
link-local, CGNAT (100.64/10), 198.18/15 or IPv6 ULA/link-local address is refused unless
`--allow-test-targets` is also given, which is for simulated networks only; host names are accepted.

### With `--netns false`

`--netns false` (one slot only) skips the per-job network namespace, and two things change:

- The self-test is weaker. The VM's own addresses and `10.203.0.1:22` are not probed, because the
  job shares the VM's network. Only the gateway, router and controller targets give a non-vacuous
  signal, so make sure `--router` and `--controller` are real addresses on your LAN.
- Loopback is reachable. The output chain's `oifname "lo" accept` lets the slot user reach every
  loopback and VM-local service.

Separately, `net.ipv4.ip_forward=1` (from "Forwarding") together with the forward chain's
`policy accept` makes the VM forward traffic that does not belong to a job. Anything else on the
VM's networks that can route through it is forwarded; keep the VM off networks you do not trust.

## Verify from the controller

Add the host to `bitc.toml` as the `ssh` provider (see the config reference), with
`experimental_remote = true` under `[host]` (remote providers are off by default in this release,
spec 5.8), then:

```
bitc doctor --remote
```

Every `ssh-*` line must be `ok`. Fix a `fail` line before the first job: each names what is wrong
(for example `fail ssh-host-mounts` for a Mac share, `fail ssh-rosetta` for an x86-64 binfmt
handler, `fail ssh-reach-key` for a credential a slot user can read). `check-host` only reads
`/proc/sys/fs/binfmt_misc`; it never changes it.

A `warn` never changes the exit code and never stops a job from starting. `ssh-host-mounts` warns
for leftovers that are not shares: a host path (`/Users`, `/Volumes`, `/mnt/mac`, `/private`,
`/opt/orbstack-guest`) or an entry under `/mnt` that holds only empty directories (empty
placeholders left by an OrbStack isolated machine), and a virtio-fs or 9p device or tag that is
exported to the VM but mounted nowhere in this machine (the shared VM's devices, accepted as
shared-kernel risk in decision 10). It still fails for every real share: a mount at or under a
host path in any mount namespace, a virtiofs, 9p, nfs, cifs or fuse mount anywhere, any file,
symlink, socket, fifo or device under a host path (at any depth; symlinks are never followed), a
tree over 10000 entries or 16 levels deep (too large to verify), and a virtio device or tag while
any such share mount exists.

## Undo

As root on the host:

```
rm -f /etc/sudoers.d/bitc /etc/ssh/sshd_config.d/bitc.conf \
      /etc/nftables.d/bitc.nft /etc/bitc/host.conf /run/bitc-probe-resolv.conf
systemctl disable --now bitc-nft.service; rm -f /etc/systemd/system/bitc-nft.service
nft delete table inet bitc
systemctl reload ssh
for u in $(getent passwd | cut -d: -f1 | grep '^bitc-s'); do userdel "$u"; done; userdel bitc
rmdir /sys/fs/cgroup/bitc/slot-* /sys/fs/cgroup/bitc
```

Restore `cron.allow` and `at.allow` before removing `/var/lib/bitc`, from what setup kept:

```
for f in cron.allow at.allow; do
  if [ -e /var/lib/bitc/$f.orig ]; then cp -p /var/lib/bitc/$f.orig /etc/$f   # it existed before
  elif [ -e /var/lib/bitc/$f.absent ]; then rm -f /etc/$f                      # it did not (cron and at allow everyone again)
  fi
done
rm -rf /opt/bitc /var/lib/bitc /home/bitc
```

To put the OrbStack links back, before the `rm -rf /var/lib/bitc` above: after an `orb restart` (so the
link targets exist again), run `ln -sf "$(cat /var/lib/bitc/resolv.conf.link)" /etc/resolv.conf` when
that file exists, and `ln -s "$(cat /var/lib/bitc/mnt-linux.link)" /mnt/linux` when that file exists.

If you removed the keep-files, or ran an older version of the script, check `/etc/cron.allow` and
`/etc/at.allow` by hand: they hold only `root` after setup. To turn forwarding off, delete `/etc/sysctl.d/60-bitc.conf` and then run
`sysctl -w net.ipv4.ip_forward=0` yourself (or reboot): `sysctl --system` alone does not set the
value back, because no file sets it to 0, so it stays 1 until the next boot. This is an owner step;
the script never changed forwarding (you enabled it under "Forwarding"). `rmdir` fails harmlessly while a job's cgroup still has processes; the kernel
removes the directories at the next boot. On OrbStack, `orb delete bitc-host` removes everything at
once.
