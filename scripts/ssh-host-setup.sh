#!/bin/sh
# ssh-host-setup.sh: set up an ssh host for bitc's `ssh` provider (providers delta C.5, C.6.3, C.8.7).
# It implements docs/ssh-host.md, section "The host", and nothing else. Run it once as
# root after reading it; it is idempotent, prints every change, and with --dry-run changes nothing.
#
# It ends with the firewall self-test (C.5.3): from inside the host, as a slot user, through the
# path jobs use, the LAN router, the controller, a ULA, a link-local, a 100.64/10 and a 198.18/15
# sample must all fail, and github.com:22 and registry.npmjs.org:443 must work. Anything else
# prints FIREWALL NOT ACTIVE or EGRESS BROKEN, exits non-zero and leaves /var/lib/bitc/selftest-failed,
# which makes the gate refuse slot-boot until a later run of this script passes the test.
#
# It touches only these paths (and creates the users bitc and bitc-s1..bitc-sN):
#   /opt/bitc/            /etc/bitc/host.conf       /var/lib/bitc/
#   /etc/sudoers.d/bitc   /etc/ssh/sshd_config.d/bitc.conf        ~bitc/.ssh/authorized_keys
#   /etc/cron.allow       /etc/at.allow (the first run keeps the old file as /var/lib/bitc/*.orig)
#   /etc/nftables.d/bitc.nft                        /etc/systemd/system/bitc-nft.service
#   /sys/fs/cgroup/bitc (created, memory and pids delegated, so check-host can read it)
#   /usr/local/bin/{mac,macctl,open} (only when they link into /opt/orbstack-guest) and
#   the /opt/orbstack-guest mount (unmounted when present), scratch files under $TMPDIR (/tmp).
#   After the unmount, scripts/ssh-host-orbstack-clean.sh removes exactly these OrbStack leftovers:
#   /opt/orbstack-guest/etc/resolv.conf (a regular file), the /mnt/linux symlink, the empty
#   /opt/orbstack-guest/etc, /opt/orbstack-guest, /mnt/mac, /mnt/machines, /Users/<name> and /Users,
#   and replaces /etc/resolv.conf with a regular copy when it links into /opt/orbstack-guest (the
#   old link texts are kept in /var/lib/bitc/resolv.conf.link and mnt-linux.link).
# The self-test also writes /run/bitc-probe-resolv.conf and creates a transient bv0 veth pair and
# network namespace (the veth and namespace go away when the test ends; the resolv file stays).
# It writes no kernel setting except the cgroup delegation above. It only READS
# /proc/sys/net/ipv4/ip_forward and stops when forwarding is off: enabling forwarding is an owner
# step in the runbook (section "Forwarding"). It never touches binfmt_misc.
#
# usage: ssh-host-setup.sh --resolver IPV4 --controller-key 'ssh-ed25519 AAAA...' [options]
#   --slots N             slot users bitc-s1..bitc-sN (default 1)
#   --gateway IPV4        the VM's gateway (default: from `ip route`)
#   --netns true|false    per-job network namespace (default true; false needs --slots 1)
#   --memory-per-slot S   cgroup memory.max per slot, e.g. 16GiB (default: none)
#   --secret-path PATH    a reachability credential to check (repeatable)
#   --no-systemd          do not write or enable the boot unit (load the ruleset now only)
#   --agent PATH          this host's bitc binary (linux, this arch); required unless --dry-run: the
#                         firewall self-test runs it (copied to /opt/bitc/sbin/bitc-selftest)
#   --router IPV4         the LAN router the self-test must not reach on :80 and :443 (default 192.168.1.1)
#   --controller IPV4     the controller-facing LAN address the self-test must not reach on :22
#                         (required unless --dry-run)
#   --allow HOST:PORT     (repeatable) an egress target the firewall self-test must REACH, instead of the
#                         defaults github.com:22 and registry.npmjs.org:443 (an IPv6 host in brackets).
#                         It changes only what the test expects: it never adds or changes a firewall rule,
#                         and the ruleset is byte-identical with and without it. A literal private (RFC1918),
#                         loopback, link-local, CGNAT (100.64/10), 198.18/15 or IPv6 ULA/link-local address
#                         is refused unless --allow-test-targets is also given (host names are fine).
#   --allow-test-targets  let --allow name such a private address (for a simulated LAN; never on a real host)
#   --dry-run             print what would change, change nothing
#   --print NAME          print one managed text and exit (sudoers, sshd, nft, host.conf,
#                         authorized-keys, tunnel-sshd, tunnel-authorized-keys, nft-unit)
#   --uids "1001, 1002"   with --print nft: the slot uids (default: looked up)
#   --tunnel-key KEY, --tunnel-port N   for --print tunnel-authorized-keys / tunnel-sshd (controller side)
set -eu
PATH=/usr/sbin:/usr/bin:/sbin:/bin
export PATH
here=$(cd "$(dirname "$0")" && pwd)

DRY=0 SLOTS=1 RESOLVER= GATEWAY= NETNS=true MEM= KEY= SECRETS= SYSTEMD=1 PRINT= UIDS= TKEY= TPORT=22022 ALLOWS= TESTTARGETS=0 AGENT= ROUTER=192.168.1.1 CONTROLLER=
die() { echo "ssh-host-setup: $*" >&2; exit 2; }
while [ $# -gt 0 ]; do
  [ $# -ge 2 ] || [ "$1" = --dry-run ] || [ "$1" = --no-systemd ] || [ "$1" = --allow-test-targets ] || die "$1 needs a value"
  case $1 in
    --dry-run) DRY=1; shift ;;
    --no-systemd) SYSTEMD=0; shift ;;
    --allow-test-targets) TESTTARGETS=1; shift ;;
    --allow)
      case $2 in '' | *[[:space:]]*) die "--allow '$2': want HOST:PORT" ;; esac
      ALLOWS="$ALLOWS $2"; shift 2 ;;
    --slots) SLOTS=$2; shift 2 ;;
    --resolver) RESOLVER=$2; shift 2 ;;
    --gateway) GATEWAY=$2; shift 2 ;;
    --netns) NETNS=$2; shift 2 ;;
    --memory-per-slot) MEM=$2; shift 2 ;;
    --controller-key) KEY=$2; shift 2 ;;
    --secret-path) SECRETS="$SECRETS $2"; shift 2 ;;
    --agent) AGENT=$2; shift 2 ;;
    --router) ROUTER=$2; shift 2 ;;
    --controller) CONTROLLER=$2; shift 2 ;;
    --print) PRINT=$2; shift 2 ;;
    --uids) UIDS=$2; shift 2 ;;
    --tunnel-key) TKEY=$2; shift 2 ;;
    --tunnel-port) TPORT=$2; shift 2 ;;
    *) die "unknown option $1" ;;
  esac
done

# ---- --allow: validated first (before --print and before any check), the agent re-checks every value
allow_ok() { # allow_ok HOST:PORT: die unless it is a well-formed target (and not private, unless --allow-test-targets)
  v=$1
  printf '%s\n' "$v" | grep -Eq '^[][A-Za-z0-9.:_-]+$' || die "--allow '$v': want HOST:PORT (an IPv6 host in brackets)"
  case $v in *:*) ;; *) die "--allow '$v': want HOST:PORT" ;; esac
  port=${v##*:}
  h=${v%:*}
  case $port in '' | *[!0-9]* | 0*) die "--allow '$v': the port must be 1-65535" ;; esac
  { [ "${#port}" -le 5 ] && [ "$port" -le 65535 ]; } || die "--allow '$v': the port must be 1-65535"
  case $h in
    \[*\])
      h=$(printf '%s' "${h#[}" | tr 'A-F' 'a-f')
      h=${h%]}
      printf '%s\n' "$h" | grep -Eq '^[0-9a-f:.]*:[0-9a-f:.]*$' || die "--allow '$v': not a valid IPv6 address"
      [ "$TESTTARGETS" = 1 ] && return 0
      first=${h%%:*}
      while [ "${#first}" -lt 4 ]; do first=0$first; done
      case $first in 0000 | fe[89ab]? | fc?? | fd?? | ff??) die "--allow '$v': a private, loopback, link-local, multicast, IPv4-mapped or ULA address (only with --allow-test-targets)" ;; esac
      ;;
    *\[* | *\]* | *:*) die "--allow '$v': an IPv6 host goes in brackets" ;;
    *)
      if printf '%s\n' "$h" | grep -Eq '^[0-9.]+$'; then
        printf '%s\n' "$h" | grep -Eq '^([0-9]{1,3}\.){3}[0-9]{1,3}$' || die "--allow '$v': not a valid IPv4 address"
        oldifs=$IFS
        IFS=.
        set -- $h
        IFS=$oldifs
        for o in "$1" "$2" "$3" "$4"; do
          case $o in 0?*) die "--allow '$v': not a valid IPv4 address (leading zero)" ;; esac
          [ "$o" -le 255 ] || die "--allow '$v': not a valid IPv4 address"; done
        [ "$TESTTARGETS" = 1 ] && return 0
        a=$1 b=$2
        bad=0
        case $a in 0 | 10 | 127) bad=1 ;; esac
        [ "$a" -ge 224 ] && bad=1
        { [ "$a" = 172 ] && [ "$b" -ge 16 ] && [ "$b" -le 31 ]; } && bad=1
        { [ "$a" = 192 ] && [ "$b" = 168 ]; } && bad=1
        { [ "$a" = 169 ] && [ "$b" = 254 ]; } && bad=1
        { [ "$a" = 100 ] && [ "$b" -ge 64 ] && [ "$b" -le 127 ]; } && bad=1
        { [ "$a" = 198 ] && [ "$b" -ge 18 ] && [ "$b" -le 19 ]; } && bad=1
        [ "$bad" = 0 ] || die "--allow '$v': a private, loopback, link-local, CGNAT or 198.18/15 address (only with --allow-test-targets)"
      else
        lh=$(printf '%s' "$h" | tr 'A-Z' 'a-z')
        printf '%s\n' "$lh" | grep -Eq '^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*$' || die "--allow '$v': not a valid host name"
        case ${lh##*.} in [a-z]*) ;; *) die "--allow '$v': not a valid host name or IP literal" ;; esac
        case ${lh##*.} in localhost) [ "$TESTTARGETS" = 1 ] || die "--allow '$v': localhost (only with --allow-test-targets)" ;; esac
      fi
      ;;
  esac
}
if [ -n "$ALLOWS" ]; then
  set -f
  # shellcheck disable=SC2086
  for t in $ALLOWS; do allow_ok "$t"; done
  set +f
fi

# ---- the managed texts (the agent's check-host compares the first two byte for byte)
sudoers() {
  cat <<'EOT'
Defaults!/opt/bitc/sbin/bitc-gate !use_pty, !requiretty, !log_output, env_reset
bitc ALL=(root) NOPASSWD: /opt/bitc/sbin/bitc-gate
EOT
}
sshd_conf() {
  cat <<'EOT'
Match User bitc
    AuthenticationMethods publickey
    DisableForwarding yes
    AllowAgentForwarding no
    PermitTTY no
EOT
}
nft_rules() {
  sed -e "s|<uids>|$UIDS|" -e "s|<resolver>|$RESOLVER|g" -e "s|<gateway>|$GATEWAY|g" <<'EOT'
table inet bitc
delete table inet bitc
table inet bitc {
  set slot_uids { typeof meta skuid; elements = { <uids> } }
  set lan4 { type ipv4_addr; flags interval; elements = {
    0.0.0.0/8, 10.0.0.0/8, 100.64.0.0/10, 127.0.0.0/8, 169.254.0.0/16, 172.16.0.0/12,
    192.0.0.0/24, 192.168.0.0/16, 198.18.0.0/15, 224.0.0.0/4, 240.0.0.0/4 } }
  chain input {
    type filter hook input priority 0; policy accept;
    iifname "bv*" drop
  }
  chain forward {
    type filter hook forward priority 0; policy accept;
    iifname "bv*" ip daddr <resolver> meta l4proto { udp, tcp } th dport 53 accept
    iifname "bv*" ip daddr <gateway> drop
    iifname "bv*" ip daddr @lan4 drop
    iifname "bv*" meta nfproto ipv6 drop
    oifname "bv*" ct state established,related accept
    oifname "bv*" drop
  }
  chain output {
    type filter hook output priority 0; policy accept;
    meta skuid != @slot_uids accept
    oifname "lo" accept
    meta skuid @slot_uids meta nfproto ipv6 drop
    ip daddr <resolver> meta l4proto { udp, tcp } th dport 53 accept
    ip daddr <gateway> drop
    ip daddr @lan4 drop
  }
  chain postrouting {
    type nat hook postrouting priority srcnat; policy accept;
    ip saddr 10.203.0.0/16 oifname != "bv*" masquerade
  }
}
EOT
}
host_conf() {
  echo "slots=$SLOTS"
  echo "resolver=$RESOLVER"
  echo "gateway=$GATEWAY"
  echo "netns=$NETNS"
  [ -z "$MEM" ] || echo "memory_per_slot=$MEM"
  [ -z "$SECRETS" ] || echo "secret_paths=${SECRETS# }"
}
authorized_keys() { echo "restrict,command=\"/opt/bitc/sbin/bitc-login\" $KEY bitc-controller"; }
nft_unit() {
  cat <<'EOT'
[Unit]
Description=bitc LAN block for job users
Before=network-pre.target
Wants=network-pre.target

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/usr/sbin/nft -f /etc/nftables.d/bitc.nft

[Install]
WantedBy=multi-user.target
EOT
}
tunnel_sshd() {
  cat <<EOT
Match User bitc-tunnel
    AuthenticationMethods publickey
    AllowTcpForwarding remote
    PermitOpen none
    PermitListen 127.0.0.1:$TPORT
    GatewayPorts no
    AllowStreamLocalForwarding no
    PermitTunnel no
    X11Forwarding no
    AllowAgentForwarding no
    PermitTTY no
    PermitUserRC no
    ForceCommand /bin/false
EOT
}
tunnel_authorized_keys() { echo "restrict,port-forwarding,permitlisten=\"127.0.0.1:$TPORT\",command=\"/bin/false\" $TKEY bitc-tunnel"; }

if [ -n "$PRINT" ]; then
  case $PRINT in
    sudoers) sudoers ;;
    sshd) sshd_conf ;;
    nft) [ -n "$UIDS" ] || UIDS='<uids>'; [ -n "$RESOLVER" ] || RESOLVER='<resolver>'; [ -n "$GATEWAY" ] || GATEWAY='<gateway>'; nft_rules ;;
    host.conf) host_conf ;;
    authorized-keys) authorized_keys ;;
    nft-unit) nft_unit ;;
    tunnel-sshd) tunnel_sshd ;;
    tunnel-authorized-keys) tunnel_authorized_keys ;;
    *) die "--print: unknown text $PRINT" ;;
  esac
  exit 0
fi

# ---- checks before anything changes
case $SLOTS in '' | *[!0-9]* | 0) die "--slots must be a number from 1 to 64" ;; esac
[ "$SLOTS" -le 64 ] || die "--slots must be a number from 1 to 64"
ipv4() { printf '%s\n' "$1" | grep -Eq '^([0-9]{1,3}\.){3}[0-9]{1,3}$'; }
ipv4 "$RESOLVER" || die "--resolver IPV4 is required"
if [ -z "$GATEWAY" ]; then GATEWAY=$(ip -4 route show default 2>/dev/null | sed -n 's/^default via \([0-9.]*\).*/\1/p' | head -n1); fi
ipv4 "$GATEWAY" || die "no gateway found; pass --gateway IPV4"
ipv4 "$ROUTER" || die "--router must be an IPv4 address"
if [ -n "$CONTROLLER" ]; then
  ipv4 "$CONTROLLER" || die "--controller must be an IPv4 address"
elif [ "$DRY" = 0 ]; then
  die "--controller IPV4 is required: the self-test must show the controller-facing LAN address is unreachable on :22"
else
  echo "note: --controller IPV4 is required for a real run (the self-test probes it on :22)"
fi
if [ "$DRY" = 0 ]; then
  [ -n "$AGENT" ] || die "--agent PATH (this host's bitc binary) is required: the firewall self-test runs it"
fi
case $NETNS in true) ;; false) [ "$SLOTS" = 1 ] || die "--netns false requires --slots 1" ;; *) die "--netns must be true or false" ;; esac
printf '%s\n' "$KEY" | grep -Eq '^ssh-ed25519 [A-Za-z0-9+/=]+$' || die "--controller-key must be 'ssh-ed25519 <base64>' (no comment)"
# (arguments are validated above, before this root check, so the checks can be tested unprivileged)
[ "$(id -u)" = 0 ] || die "must run as root"
# (after the root check, so a path the caller cannot read never hides the root refusal)
[ "$DRY" = 1 ] || [ -x "$AGENT" ] || die "--agent $AGENT is not an executable file"
# forwarding is the owner's step (runbook, "Forwarding"): this script only reads it
if [ "$(cat /proc/sys/net/ipv4/ip_forward 2>/dev/null)" = 1 ]; then :; elif [ "$DRY" = 1 ]; then
  echo "warning: /proc/sys/net/ipv4/ip_forward is not 1; a real run stops until you enable it (runbook, \"Forwarding\")"
else
  die "/proc/sys/net/ipv4/ip_forward is not 1: enable forwarding first (runbook, section \"Forwarding\"); this script never writes kernel settings"
fi
for t in sshd sudo visudo ip nsenter useradd usermod ssh-keygen; do command -v "$t" >/dev/null 2>&1 || die "$t is not installed"; done

tmp=$(mktemp -d "${TMPDIR:-/tmp}/bitc-setup.XXXXXX")
trap 'rm -rf "$tmp"' EXIT

say() { printf '%s\n' "$*"; }
act() { # act <description> <command...>: print, and run unless --dry-run
  d=$1
  shift
  if [ "$DRY" = 1 ]; then say "would: $d"; else say "change: $d"; "$@"; fi
}

# put <path> <mode> <owner:group> <generator>: write the text of <generator> unless it is already there
PUT_WROTE=0
put() {
  p=$1 m=$2 o=$3 g=$4
  "$g" >"$tmp/new"
  cur_m=$(stat -c '%a' "$p" 2>/dev/null || echo -)
  cur_o=$(stat -c '%U:%G' "$p" 2>/dev/null || echo -)
  if [ -f "$p" ] && cmp -s "$tmp/new" "$p" && [ "$cur_m" = "$(printf '%s' "$m" | sed 's/^0*//')" ] && [ "$cur_o" = "$o" ]; then
    say "ok: $p is as wanted"
    PUT_WROTE=0
    return 0
  fi
  PUT_WROTE=1
  act "write $p ($m $o)" install -m "$m" -o "${o%:*}" -g "${o#*:}" "$tmp/new" "$p"
}

mkd() { # mkd <dir> <mode> <owner>
  cm=$(stat -c '%a' "$1" 2>/dev/null || echo -)
  co=$(stat -c '%U' "$1" 2>/dev/null || echo -)
  if [ -d "$1" ] && [ "$cm" = "$(printf '%s' "$2" | sed 's/^0*//')" ] && [ "$co" = "$3" ]; then say "ok: $1 is as wanted"; return 0; fi
  act "directory $1 ($2 $3)" sh -c 'mkdir -p "$1" && chown "$3":"$(id -gn "$3")" "$1" && chmod "$2" "$1"' sh "$1" "$2" "$3"
}

have_user() { id -u "$1" >/dev/null 2>&1; }

# ---- packages
if ! command -v nft >/dev/null 2>&1; then
  command -v apt-get >/dev/null 2>&1 || die "nft is not installed and there is no apt-get; install nftables"
  act "install the nftables package" sh -c 'DEBIAN_FRONTEND=noninteractive apt-get install -y -qq nftables'
fi

# ---- users
if have_user bitc; then say "ok: user bitc exists"; else
  act "create user bitc (shell /bin/sh, home /home/bitc)" useradd --system --user-group --no-create-home --home-dir /home/bitc --shell /bin/sh bitc
fi
if [ "$(getent passwd bitc | cut -d: -f7)" = /bin/sh ] && [ "$(passwd -S bitc | cut -d' ' -f2)" != P ]; then say "ok: bitc has shell /bin/sh and a locked password"; else
  act "lock the password of bitc and set its shell to /bin/sh" usermod -L -s /bin/sh bitc
fi
i=1
while [ "$i" -le "$SLOTS" ]; do
  if have_user "bitc-s$i"; then say "ok: user bitc-s$i exists"; else
    act "create user bitc-s$i (nologin, no sudo, own group)" useradd --system --user-group --no-create-home \
      --home-dir "/var/lib/bitc/slots/$i/home" --shell /usr/sbin/nologin "bitc-s$i"
  fi
  i=$((i + 1))
done

# the ruleset needs the uids; a dry run before the users exist shows placeholders
UIDS=
i=1
while [ "$i" -le "$SLOTS" ]; do
  u=$(id -u "bitc-s$i" 2>/dev/null || echo "<uid of bitc-s$i>")
  UIDS="${UIDS:+$UIDS, }$u"
  i=$((i + 1))
done

# ---- layout
mkd /opt/bitc 755 root
mkd /opt/bitc/sbin 755 root
mkd /opt/bitc/agent 755 root
mkd /etc/bitc 755 root
mkd /var/lib/bitc 700 root
mkd /var/lib/bitc/slots 700 root
# fail closed from here on: the gate refuses slot-boot until the self-test at the end passes
# (transient state, not managed configuration: written silently on every real run, removed by a passing
# self-test, so a second run reports no change for it)
if [ "$DRY" = 1 ]; then
  [ -e /var/lib/bitc/selftest-failed ] || say "would: mark the firewall self-test as not passed (/var/lib/bitc/selftest-failed)"
else
  echo "setup not finished" >/var/lib/bitc/selftest-failed && chmod 600 /var/lib/bitc/selftest-failed
  say "note: slot-boot is refused until the firewall self-test below passes"
fi
mkd /var/lib/bitc/results 700 root
mkd /var/lib/bitc/tools 700 root
i=1
while [ "$i" -le "$SLOTS" ]; do
  mkd "/var/lib/bitc/slots/$i" 700 root
  for d in work home tmp vartmp runlock; do mkd "/var/lib/bitc/slots/$i/$d" 700 "bitc-s$i"; done
  i=$((i + 1))
done
mkd /home/bitc 755 root
mkd /home/bitc/.ssh 755 root

# ---- gate, login wrapper, host.conf
cp_gate() { cat "$here/ssh-gate.sh"; }
cp_login() { cat "$here/ssh-login.sh"; }
[ -f "$here/ssh-gate.sh" ] && [ -f "$here/ssh-login.sh" ] || die "ssh-gate.sh and ssh-login.sh must sit beside this script"
put /opt/bitc/sbin/bitc-gate 755 root:root cp_gate
put /opt/bitc/sbin/bitc-login 755 root:root cp_login
put /etc/bitc/host.conf 644 root:root host_conf

# ---- sudoers: validated before it is installed
sudoers >"$tmp/sudoers"
visudo -cf "$tmp/sudoers" >/dev/null || die "the sudoers text does not validate"
put /etc/sudoers.d/bitc 440 root:root sudoers

# ---- sshd
grep -Eq '^[[:space:]]*Include[[:space:]]+/etc/ssh/sshd_config\.d/\*\.conf' /etc/ssh/sshd_config 2>/dev/null ||
  die "/etc/ssh/sshd_config does not Include /etc/ssh/sshd_config.d/*.conf; add that line first"
mkd /etc/ssh/sshd_config.d 755 root
put /etc/ssh/sshd_config.d/bitc.conf 644 root:root sshd_conf
if [ "$DRY" = 0 ]; then
  sshd -t || die "sshd rejects its configuration"
  if [ -d /run/systemd/system ]; then
    systemctl reload ssh 2>/dev/null || systemctl reload sshd 2>/dev/null || say "note: reload sshd yourself"
  else
    say "note: no systemd; reload sshd yourself"
  fi
fi
put /home/bitc/.ssh/authorized_keys 644 root:root authorized_keys

# ---- cron and at: nobody but root
root_only() { echo root; }
# keep what was there (once), so the runbook's Undo can restore it
keep_orig() { # keep_orig <path>
  b=/var/lib/bitc/$(basename "$1")
  [ -e "$b.orig" ] || [ -e "$b.absent" ] && return 0
  if [ -e "$1" ]; then act "keep the old $1 as $b.orig" sh -c 'cp -p "$1" "$2" && chmod 600 "$2"' sh "$1" "$b.orig"
  else act "note that $1 did not exist ($b.absent)" sh -c ': >"$1" && chmod 600 "$1"' sh "$b.absent"; fi
}
keep_orig /etc/cron.allow
keep_orig /etc/at.allow
put /etc/cron.allow 644 root:root root_only
put /etc/at.allow 644 root:root root_only

# ---- the LAN block and boot (forwarding is read above and never written here)
mkd /etc/nftables.d 755 root
put /etc/nftables.d/bitc.nft 644 root:root nft_rules
nft_written=$PUT_WROTE
say "ok: net.ipv4.ip_forward is 1"
if [ "$nft_written" = 0 ] && nft list table inet bitc >/dev/null 2>&1; then say "ok: table inet bitc is loaded"; else act "load /etc/nftables.d/bitc.nft now" nft -f /etc/nftables.d/bitc.nft; fi
if [ "$SYSTEMD" = 1 ] && [ -d /run/systemd/system ]; then
  put /etc/systemd/system/bitc-nft.service 644 root:root nft_unit
  act "enable bitc-nft.service at boot" sh -c 'systemctl daemon-reload && systemctl enable bitc-nft.service'
else
  say "note: the ruleset is loaded now; load /etc/nftables.d/bitc.nft at every boot yourself (runbook)"
fi

# ---- cgroup: the bitc subtree with memory and pids, which check-host only reads
if [ -f /sys/fs/cgroup/bitc/cgroup.controllers ] && grep -q memory /sys/fs/cgroup/bitc/cgroup.controllers && grep -q pids /sys/fs/cgroup/bitc/cgroup.controllers; then
  say "ok: /sys/fs/cgroup/bitc has memory and pids"
else
  act "delegate memory and pids to /sys/fs/cgroup/bitc" sh -c 'mkdir -p /sys/fs/cgroup/bitc; echo "+memory +pids" >/sys/fs/cgroup/cgroup.subtree_control 2>/dev/null || true; echo "+memory +pids" >/sys/fs/cgroup/bitc/cgroup.subtree_control 2>/dev/null || true'
fi

# ---- host-integration helpers
for n in mac macctl open; do
  p=/usr/local/bin/$n
  if [ -L "$p" ]; then
    case $(readlink "$p") in /opt/orbstack-guest/*) act "remove the Mac command bridge $p" rm -f "$p" ;; esac
  fi
done
if grep -q ' /opt/orbstack-guest ' /proc/self/mountinfo 2>/dev/null; then
  act "unmount /opt/orbstack-guest" sh -c 'umount /opt/orbstack-guest || echo "note: could not unmount /opt/orbstack-guest; doctor will fail until it is gone"'
fi
# the known OrbStack leftovers can only be seen once the share is no longer mounted over them
if grep -q ' /opt/orbstack-guest ' /proc/self/mountinfo 2>/dev/null; then
  if [ "$DRY" = 1 ]; then
    say "would: remove the known OrbStack leftovers (/opt/orbstack-guest/etc/resolv.conf, /mnt/linux, empty /opt/orbstack-guest, /mnt/mac, /mnt/machines, /Users dirs) after the unmount"
  else
    say "note: /opt/orbstack-guest is still mounted; leftovers not removed"
  fi
elif [ "$DRY" = 1 ]; then
  sh "$here/ssh-host-orbstack-clean.sh" --dry-run /
else
  sh "$here/ssh-host-orbstack-clean.sh" /
fi
if [ -s /proc/swaps ] && [ "$(wc -l </proc/swaps)" -gt 1 ]; then
  say "warning: swap is active; run swapoff -a and remove it from /etc/fstab (runbook), or per-job secrets may reach disk"
fi

# ---- the firewall self-test (mandatory; its failure is this script's failure)
if [ "$DRY" = 1 ]; then
  say "would: copy $AGENT to /opt/bitc/sbin/bitc-selftest and run the firewall self-test as bitc-s1 through the job network path${ALLOWS:+ (must reach:$ALLOWS)}"
  say "dry run complete; nothing was changed"
  exit 0
fi
install -m 755 -o root -g root "$AGENT" /opt/bitc/sbin/bitc-selftest
set -- __agent firewall-selftest --router "$ROUTER" --controller "$CONTROLLER"
# --allow only changes which targets the test expects to reach; no rule is derived from it
set -f
for t in $ALLOWS; do set -- "$@" --allow "$t"; done
set +f
[ "$TESTTARGETS" = 0 ] || set -- "$@" --allow-test-targets
if ! /opt/bitc/sbin/bitc-selftest "$@"; then
  echo "ssh-host-setup: the firewall self-test did not pass; slot-boot stays refused (see above)" >&2
  exit 69
fi
say "done. Next: bitc doctor --remote from the controller"
