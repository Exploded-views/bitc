#!/bin/sh
# coverage-floors.sh check RUN [FLOORS]
# coverage-floors.sh ratchet [--drop-removed] RUN [FLOORS]
#
# FLOORS defaults to coverage-floors.json at the repo root. RUN is the captured output of
# `go test -cover ...`. Lines read:
#   ok  <pkg> <time> coverage: 85.3% of statements   -> that percentage
#   <tab><pkg><tab><tab>coverage: 0.0% of statements -> 0.0 (go prints a package with statements but
#                                                       no tests like this), and it needs a floor entry
#   ?   <pkg> [no test files]                        -> 0.0, likewise (a package with no statements)
#   ok  <pkg> <time> coverage: [no statements]       -> ignored (nothing to cover)
# Anything else (FAIL lines, test logs) is ignored, so a failed package counts as missing.
# Percentages are compared in whole tenths, so no float rounding is involved.
#
# check: fail (exit 1) if a package is below its floor, has no floor, or a floor names a package
# that is not in the run. Prints only package, floor and actual of each offender.
#
# ratchet: rewrite FLOORS with each floor raised to the current value, rounded DOWN to 0.1, and
# new packages added. It never lowers a floor: if any package is below its floor it prints the
# package, floor and actual, writes nothing and exits 1. Entries for packages missing from the
# run are kept unless --drop-removed is given.
set -eu
usage='usage: coverage-floors.sh check RUN [FLOORS] | ratchet [--drop-removed] RUN [FLOORS]'
mode=${1:-}; [ $# -eq 0 ] || shift
drop=0
if [ "$mode" = ratchet ] && [ "${1:-}" = --drop-removed ]; then drop=1; shift; fi
case $mode in check|ratchet) ;; *) echo "$usage" >&2; exit 2 ;; esac
run=${1:-}
[ -n "$run" ] || { echo "$usage" >&2; exit 2; }
floors=${2:-$(cd "$(dirname "$0")/.." && pwd)/coverage-floors.json}
[ -r "$run" ] && [ -r "$floors" ] || { echo "coverage-floors: cannot read $run or $floors" >&2; exit 2; }
new=$(mktemp); trap 'rm -f "$new"' EXIT

rc=0
awk -v mode="$mode" -v drop="$drop" -v new="$new" '
  function tenths(s) { return int(s * 10 + 0.000001) }
  function fmt(t) { return sprintf("%d.%d", t / 10, t % 10) }
  FNR == 1 { file++ }
  file == 1 && /^ *"[^"]+": *[0-9.]+,? *$/ {
    split($0, q, "\""); v = $0; sub(/^[^:]*: */, "", v); sub(/[, ]*$/, "", v)
    floor[q[2]] = tenths(v); next
  }
  file == 2 && $1 == "ok" && match($0, /coverage: [0-9.]+% of statements/) {
    v = substr($0, RSTART + 10, RLENGTH - 10); sub(/%.*/, "", v); got[$2] = tenths(v); next
  }
  file == 2 && /^\t[^\t]+\t+coverage: [0-9.]+% of statements$/ { got[$1] = 0; next }
  file == 2 && $1 == "?" && /\[no test files\]/ { got[$2] = 0 }
  END {
    for (p in got) {
      if (!(p in floor)) {
        if (mode == "check") { printf "%s  floor: none (no floor entry)  actual: %s\n", p, fmt(got[p]); bad = 1 }
      } else if (got[p] < floor[p]) { printf "%s  floor: %s  actual: %s\n", p, fmt(floor[p]), fmt(got[p]); bad = 1 }
    }
    if (mode == "check") for (p in floor) if (!(p in got)) { printf "%s  floor: %s  actual: not in run\n", p, fmt(floor[p]); bad = 1 }
    if (bad) exit 1
    if (mode == "check") exit 0
    for (p in got) out[p] = got[p]
    if (!drop) for (p in floor) if (!(p in got)) out[p] = floor[p]
    n = 0; for (p in out) key[++n] = p
    for (i = 2; i <= n; i++) { k = key[i]; for (j = i - 1; j >= 1 && key[j] > k; j--) key[j + 1] = key[j]; key[j + 1] = k }
    print "{" > new
    for (i = 1; i <= n; i++) printf "  \"%s\": %s%s\n", key[i], fmt(out[key[i]]), (i < n ? "," : "") > new
    print "}" > new
  }' "$floors" "$run" || rc=$?
[ "$rc" = 0 ] || exit "$rc"
[ "$mode" = check ] || cat "$new" >"$floors"
