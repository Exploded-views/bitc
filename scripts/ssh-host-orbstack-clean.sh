#!/bin/sh
# ssh-host-orbstack-clean.sh: remove the known OrbStack leftovers that stay on the root file system
# after setup unmounts /opt/orbstack-guest. Called by ssh-host-setup.sh as root with ROOT=/; tests
# call it with a temp dir as ROOT.
#
# usage: ssh-host-orbstack-clean.sh [--dry-run] ROOT
#
# It acts only on these exact paths under ROOT, in this order, and nothing else:
#   1. /etc/resolv.conf, only when it is a symlink into /opt/orbstack-guest/ whose target is a
#      regular file: replaced (one rename) by a regular 0644 copy; the old link text is kept in
#      /var/lib/bitc/resolv.conf.link
#   2. /opt/orbstack-guest/etc/resolv.conf, only when it is a regular file
#   3. /mnt/linux, only when it is a symlink (its text is kept in /var/lib/bitc/mnt-linux.link)
#   4. rmdir (never rm -r) of the empty directories /opt/orbstack-guest/etc, /opt/orbstack-guest,
#      /mnt/mac, /mnt/machines, every direct child of /Users, then /Users
# It never follows a symlink (every parent directory on the way must be a real directory), never
# writes outside ROOT, and a second run finds nothing to do. With --dry-run it changes nothing.
set -eu

DRY=0
case ${1-} in
  --dry-run) DRY=1; shift ;;
esac
[ $# -eq 1 ] || { echo "usage: ssh-host-orbstack-clean.sh [--dry-run] ROOT" >&2; exit 2; }
ROOT=$1
case $ROOT in /*) ;; *) echo "ssh-host-orbstack-clean: ROOT must be an absolute path" >&2; exit 2 ;; esac
R=${ROOT%/} # empty for "/"
GUEST=/opt/orbstack-guest
STATE=/var/lib/bitc
PRINTED=0
GONE= # dry run only: paths a real run would already have removed

say() { PRINTED=1; printf '%s\n' "$*"; }
# change <what>: print "change: ..." (or "would: ..." with --dry-run)
change() { if [ "$DRY" = 1 ]; then say "would: $*"; else say "change: $*"; fi; }

# chain_ok <dir path>: every component from ROOT down is a real directory, not a link
chain_ok() (
  set -f
  IFS=/
  p=
  for c in $1; do
    [ -n "$c" ] || continue
    p=$p/$c
    if [ -L "$R$p" ] || [ ! -d "$R$p" ]; then return 1; fi
  done
  return 0
)

# norm <absolute path>: collapse . and .. by string, never by following anything
norm() (
  set -f
  IFS=/
  out=
  for c in $1; do
    case $c in
      '' | .) ;;
      ..) out=${out%/*} ;;
      *) out=$out/$c ;;
    esac
  done
  printf '%s' "${out:-/}"
)

under_guest() { case $1 in "$GUEST"/*) return 0 ;; esac; return 1; }

# regular <logical path>: a regular file (not a link) whose parent chain holds no link
regular() {
  chain_ok "$(dirname "$1")" && [ -f "$R$1" ] && [ ! -L "$R$1" ]
}

# save_link <state file name> <link text>: keep the old text (0600) under $STATE; false when it cannot be saved
save_link() {
  for d in /var /var/lib; do
    if [ -L "$R$d" ]; then return 1; fi
    if [ ! -d "$R$d" ] && [ "$DRY" != 1 ]; then mkdir "$R$d" || return 1; fi
  done
  if [ -L "$R$STATE" ]; then return 1; fi
  if [ "$DRY" = 1 ]; then return 0; fi
  if [ ! -d "$R$STATE" ]; then mkdir -m 700 "$R$STATE" || return 1; fi
  rm -f "$R$STATE/$1"
  (umask 077; set -C; printf '%s\n' "$2" >"$R$STATE/$1") || return 1
}

# ---- 1. /etc/resolv.conf
RESOLV_POINTS_IN=0
if chain_ok /etc && [ -L "$R/etc/resolv.conf" ]; then
  text=$(readlink "$R/etc/resolv.conf")
  case $text in /*) abs=$(norm "$text") ;; *) abs=$(norm "/etc/$text") ;; esac
  if under_guest "$abs"; then
    RESOLV_POINTS_IN=1
    if regular "$abs" && save_link resolv.conf.link "$text"; then
      if [ "$DRY" = 1 ]; then
        change "replace the /etc/resolv.conf link into /opt/orbstack-guest with a regular copy"
        RESOLV_POINTS_IN=0
      else
        tmp=$R/etc/resolv.conf.bitc-new
        rm -f "$tmp"
        if (umask 077; set -C; cat "$R$abs" >"$tmp") && chmod 644 "$tmp" && mv -f "$tmp" "$R/etc/resolv.conf"; then
          change "replace the /etc/resolv.conf link into /opt/orbstack-guest with a regular copy"
          RESOLV_POINTS_IN=0
        else
          rm -f "$tmp"
        fi
      fi
    fi
    if [ "$RESOLV_POINTS_IN" = 1 ]; then
      say "warning: /etc/resolv.conf points into /opt/orbstack-guest but its target is not a regular file; fix DNS by hand"
    fi
  fi
fi

# ---- 2. the guest copy of resolv.conf
if regular "$GUEST/etc/resolv.conf"; then
  if [ "$RESOLV_POINTS_IN" = 1 ]; then
    say "note: $GUEST/etc/resolv.conf kept: /etc/resolv.conf still points into /opt/orbstack-guest"
  else
    change "remove OrbStack leftover $GUEST/etc/resolv.conf"
    if [ "$DRY" = 1 ]; then GONE="$GONE
$GUEST/etc/resolv.conf"; else rm -f "$R$GUEST/etc/resolv.conf"; fi
  fi
fi

# ---- 3. /mnt/linux
if chain_ok /mnt && { [ -e "$R/mnt/linux" ] || [ -L "$R/mnt/linux" ]; }; then
  if [ -L "$R/mnt/linux" ]; then
    if save_link mnt-linux.link "$(readlink "$R/mnt/linux")"; then
      change "remove OrbStack leftover /mnt/linux"
      if [ "$DRY" != 1 ]; then rm -f "$R/mnt/linux"; fi
    else
      say "note: could not save the /mnt/linux link text under $STATE; left in place"
    fi
  else
    say "note: /mnt/linux is not a symlink; left in place (check-host will fail)"
  fi
fi

# ---- 4. empty directories, rmdir only
# empty_after <logical dir>: no entry is left once the dry run's removals are applied
empty_after() {
  for e in "$R$1"/* "$R$1"/.[!.]* "$R$1"/..?*; do
    [ -e "$e" ] || [ -L "$e" ] || continue
    case "$GONE
" in *"
${e#"$R"}
"*) continue ;; esac
    return 1
  done
  return 0
}

# rm_dir <logical dir>: rmdir it when it is a real, empty directory
rm_dir() {
  chain_ok "$1" || return 0
  if [ "$DRY" = 1 ]; then
    if empty_after "$1"; then
      change "remove OrbStack leftover $1"
      GONE="$GONE
$1"
    else
      say "note: $1 is not empty; left in place"
    fi
  elif rmdir "$R$1" 2>/dev/null; then
    change "remove OrbStack leftover $1"
  else
    say "note: $1 is not empty; left in place"
  fi
}

rm_dir "$GUEST/etc"
rm_dir "$GUEST"
rm_dir /mnt/mac
rm_dir /mnt/machines
if chain_ok /Users; then
  for u in "$R"/Users/* "$R"/Users/.[!.]* "$R"/Users/..?*; do
    [ -d "$u" ] && [ ! -L "$u" ] || continue
    rm_dir "/Users/${u##*/}"
  done
  rm_dir /Users
fi

[ "$PRINTED" = 1 ] || say "ok: no OrbStack leftovers"
