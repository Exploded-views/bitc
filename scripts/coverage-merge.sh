#!/bin/sh
# coverage-merge.sh OUT DIR LEG...
#
# Merges the per-leg unit-test outputs DIR/test-cover-LEG.txt (`go test -race -cover`, one file per
# make test-unit LEG=...) into OUT, in the order given, for coverage-floors.sh. Coverage is only
# meaningful on the whole run (a package missing from it is a failure there), so the legs the caller
# expects are named: a leg whose file is missing or unreadable, has no `coverage:` line, or has a
# FAIL line is refused by name and OUT is not written. Exit 1 if refused, 2 on a usage error.
set -eu
usage='usage: coverage-merge.sh OUT DIR LEG...'
[ $# -ge 3 ] || { echo "$usage" >&2; exit 2; }
out=$1; dir=$2; shift 2
tmp=$(mktemp); trap 'rm -f "$tmp"' EXIT
bad=0
for leg in "$@"; do
  f=$dir/test-cover-$leg.txt
  if [ ! -r "$f" ]; then
    echo "coverage-merge: leg $leg: $f is missing" >&2; bad=1; continue
  fi
  if ! grep -q 'coverage: ' "$f"; then
    echo "coverage-merge: leg $leg: $f has no coverage: line" >&2; bad=1; continue
  fi
  if grep -Eq '^(FAIL|--- FAIL|panic:)' "$f"; then
    echo "coverage-merge: leg $leg: $f has a failure" >&2; bad=1; continue
  fi
  cat "$f" >>"$tmp"
done
[ "$bad" -eq 0 ] || exit 1
cat "$tmp" >"$out"
echo "coverage-merge: $# legs ($*) merged into $out"
