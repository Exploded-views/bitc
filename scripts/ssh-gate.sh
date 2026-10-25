#!/bin/sh
# bitc-gate: the one command the ssh host's `bitc` login may run as root (providers delta C.5).
# Installed as /opt/bitc/sbin/bitc-gate, called by bitc-login through sudo with the client's
# command line as its only argument. It never reads its environment: $1 is split on single
# spaces and every field is checked against a fixed grammar. Anything else exits 64 with
# nothing on stdout.
#
#   probe
#   install <sha256>                         agent bytes on stdin
#   agent <sha256> check-host [--doctor]
#   agent <sha256> slot-boot --prepare <slots> <name> <nonce> [tag=<hex>] [lease=<secs>] [payload]
#   agent <sha256> slot-boot --run <slots> <name> <nonce>
#   agent <sha256> tail <job> <off> <off> <off>
#   agent <sha256> read <job> <file> <off>
#   agent <sha256> list
#   agent <sha256> teardown <slots> <name>
set -u
PATH=/usr/sbin:/usr/bin:/sbin:/bin
export PATH
umask 077
BASE=/opt/bitc
STATE=/var/lib/bitc
CONF=/etc/bitc/host.conf
VERSION=1
bad() { exit 64; }

[ $# -eq 1 ] || bad
cmd=$1
nl='
'
case $cmd in
  '' | *"$nl"* | ' '* | *' ' | *'  '*) bad ;;
esac
[ -z "$(printf '%s' "$cmd" | LC_ALL=C tr -d 'A-Za-z0-9._=+ -')" ] || bad
set -f
IFS=' '
# shellcheck disable=SC2086
set -- $cmd

re() { printf '%s\n' "$2" | grep -Eq "$1"; }
is_sha() { re '^[0-9a-f]{64}$' "$1"; }
is_nonce() { re '^[0-9a-f]{32}$' "$1"; }
is_name() { re '^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$' "$1"; }
is_off() { re '^[0-9]{1,18}$' "$1"; }
is_slots() { re '^[1-9][0-9]*(\+[1-9][0-9]*)*$' "$1"; }

nslots() {
  n=$(sed -n 's/^slots=//p' "$CONF" 2>/dev/null | head -n1)
  case $n in '' | *[!0-9]*) n=0 ;; esac
  printf '%s' "$n"
}

# slots_ok <slots>: syntax, ascending, each within host.conf's slots
slots_ok() {
  is_slots "$1" || return 1
  max=$(nslots)
  last=0
  for s in $(printf '%s' "$1" | tr '+' ' '); do
    [ "$s" -gt "$last" ] && [ "$s" -le "$max" ] || return 1
    last=$s
  done
}

do_probe() {
  case $(uname -m) in x86_64) a=amd64 ;; aarch64 | arm64) a=arm64 ;; *) a=$(uname -m) ;; esac
  cg=false
  [ -f /sys/fs/cgroup/cgroup.controllers ] && cg=true
  n=$(nslots)
  ok=true
  i=1
  while [ "$i" -le "$n" ]; do
    id -u "bitc-s$i" >/dev/null 2>&1 || ok=false
    i=$((i + 1))
  done
  printf '{"arch":"%s","kernel":"%s","cgroup2":%s,"slots":%s,"users_ok":%s,"gate_version":%s}\n' \
    "$a" "$(uname -r)" "$cg" "$n" "$ok" "$VERSION"
}

# do_install <sha>: the bytes on stdin go in only if they hash to <sha>; the newest 3 stay
do_install() {
  dir=$BASE/agent
  [ -d "$dir" ] || { mkdir -p "$dir" && chmod 0755 "$dir"; } || exit 69
  tmp=$(mktemp "$dir/.install.XXXXXX") || exit 69
  trap 'rm -f "$tmp"' EXIT
  head -c 268435456 >"$tmp"
  got=$(sha256sum "$tmp" | cut -d' ' -f1)
  [ "$got" = "$1" ] || { echo "install: sha256 mismatch" >&2; exit 65; }
  chmod 0755 "$tmp"
  mkdir -p "$dir/$1" && chmod 0755 "$dir/$1" && mv "$tmp" "$dir/$1/bitc" || exit 69
  trap - EXIT
  n=0
  IFS=$nl
  for d in $(ls -1t "$dir"); do
    is_sha "$d" || continue
    n=$((n + 1))
    [ "$n" -le 3 ] || rm -rf "${dir:?}/$d"
  done
  IFS=' '
  echo "installed $1"
}

# boot_allowed: fail closed. A failed or missing firewall self-test (ssh-host-setup.sh) refuses
# every boot. It runs only after the whole command line is validated, so a malformed boot is
# still 64 with nothing on stdout.
boot_allowed() {
  if [ -e "$STATE/selftest-failed" ]; then
    echo "refused: firewall self-test has not passed; run ssh-host-setup.sh again on the host"
    exit 65
  fi
}

# do_agent <sha> <verb> args...: re-hash the installed agent and run it with a checked verb
do_agent() {
  sha=$1
  verb=$2
  shift 2
  bin=$BASE/agent/$sha/bitc
  [ -f "$bin" ] || { echo "agent $sha is not installed" >&2; exit 66; }
  [ "$(sha256sum "$bin" | cut -d' ' -f1)" = "$sha" ] || { echo "agent $sha: sha256 mismatch" >&2; exit 65; }
  case $verb in
    check-host)
      case $# in 0) ;; 1) [ "$1" = --doctor ] || bad ;; *) bad ;; esac
      exec "$bin" __agent check-host "$@"
      ;;
    slot-boot)
      [ $# -ge 4 ] || bad
      mode=$1
      slots=$2
      name=$3
      nonce=$4
      shift 4
      slots_ok "$slots" && is_name "$name" && is_nonce "$nonce" || bad
      case $mode in
        --prepare)
          ex=
          for x in tag lease payload; do
            [ $# -gt 0 ] || break
            case $x:$1 in
              tag:tag=*) re '^[0-9a-f]{1,64}$' "${1#tag=}" || bad; ex="$ex $1"; shift ;;
              lease:lease=*) re '^[0-9]{1,4}$' "${1#lease=}" || bad; ex="$ex $1"; shift ;;
              payload:payload) ex="$ex $1"; shift ;;
            esac
          done
          [ $# -eq 0 ] || bad
          boot_allowed
          # shellcheck disable=SC2086
          exec "$bin" __agent slot-boot --prepare "$slots" "$name" "$nonce" $ex
          ;;
        --run)
          [ $# -eq 0 ] || bad
          boot_allowed
          log=$STATE/slots/${slots%%+*}/agent.log
          exec "$bin" __agent slot-boot --run "$slots" "$name" "$nonce" 2>>"$log"
          ;;
        *) bad ;;
      esac
      ;;
    tail)
      [ $# -eq 4 ] && is_name "$1" && is_off "$2" && is_off "$3" && is_off "$4" || bad
      exec "$bin" __agent --root "$STATE/results" tail "$@"
      ;;
    read)
      [ $# -eq 3 ] && is_name "$1" && is_off "$3" || bad
      case $2 in agent.log | out.log | err.log | result.json | artifacts.tar.gz | state.json) ;; *) bad ;; esac
      exec "$bin" __agent --root "$STATE/results" read "$@"
      ;;
    list)
      [ $# -eq 0 ] || bad
      exec "$bin" __agent slot-list
      ;;
    teardown)
      [ $# -eq 2 ] && slots_ok "$1" && is_name "$2" || bad
      exec "$bin" __agent slot-teardown "$@"
      ;;
    *) bad ;;
  esac
}

case $1 in
  probe) [ $# -eq 1 ] || bad; do_probe ;;
  install) [ $# -eq 2 ] && is_sha "$2" || bad; do_install "$2" ;;
  agent) [ $# -ge 3 ] && is_sha "$2" || bad; shift; do_agent "$@" ;;
  *) bad ;;
esac
