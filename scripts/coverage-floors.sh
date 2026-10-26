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
# A package may carry a tolerance in coverage-tolerances.json (next to FLOORS; optional), one line
# each: "pkg": {"tolerance": 0.4, "why": "..."}. check then passes at actual >= floor - tolerance.
# It is only for a measured environment gap (the same tests cover a bit less on CI than locally),
# and check fails if a tolerance is above 0.5 or names a package with no floor. ratchet ignores
# tolerances: it writes the real current value and never lowers a floor.
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
tolf=$(dirname "$floors")/coverage-tolerances.json
[ -r "$tolf" ] || tolf=
new=$(mktemp); trap 'rm -f "$new"' EXIT

rc=0
awk -v mode="$mode" -v drop="$drop" -v new="$new" -v floorsf="$floors" -v tolf="$tolf" '
  function tenths(s) { return int(s * 10 + 0.000001) }
  function slack(p) { return (mode == "check" && (p in tol)) ? tol[p] : 0 }
  function fmt(t) { return sprintf("%d.%d", t / 10, t % 10) }
  tolf != "" && FILENAME == tolf && /^ *"[^"]+": *\{ *"tolerance": *[0-9.]+/ {
    split($0, q, "\""); v = $0; sub(/^[^:]*: *\{ *"tolerance": */, "", v); sub(/[^0-9.].*$/, "", v)
    tol[q[2]] = tenths(v); next
  }
  FILENAME == floorsf && /^ *"[^"]+": *[0-9.]+,? *$/ {
    split($0, q, "\""); v = $0; sub(/^[^:]*: */, "", v); sub(/[, ]*$/, "", v)
    floor[q[2]] = tenths(v); next
  }
  FILENAME != floorsf && FILENAME != tolf && $1 == "ok" && match($0, /coverage: [0-9.]+% of statements/) {
    v = substr($0, RSTART + 10, RLENGTH - 10); sub(/%.*/, "", v); got[$2] = tenths(v); next
  }
  FILENAME != floorsf && FILENAME != tolf && /^\t[^\t]+\t+coverage: [0-9.]+% of statements$/ { got[$1] = 0; next }
  FILENAME != floorsf && FILENAME != tolf && $1 == "?" && /\[no test files\]/ { got[$2] = 0 }
  END {
    for (p in got) {
      if (!(p in floor)) {
        if (mode == "check") { printf "%s  floor: none (no floor entry)  actual: %s\n", p, fmt(got[p]); bad = 1 }
      } else if (got[p] < floor[p] - slack(p)) { printf "%s  floor: %s  actual: %s\n", p, fmt(floor[p]), fmt(got[p]); bad = 1 }
    }
    if (mode == "check") for (p in tol) {
      if (!(p in floor)) { printf "%s  tolerance: %s  no floor entry\n", p, fmt(tol[p]); bad = 1 }
      else if (tol[p] > 5) { printf "%s  tolerance: %s  above the 0.5 cap\n", p, fmt(tol[p]); bad = 1 }
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
  }' $tolf "$floors" "$run" || rc=$?
[ "$rc" = 0 ] || exit "$rc"
[ "$mode" = check ] || cat "$new" >"$floors"
