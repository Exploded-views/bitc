#!/bin/sh
# conformance-shards.sh: how CI splits the conformance suite into parallel legs.
#
#   shards              the shard names, one per line
#   pkg SHARD           the package the shard tests
#   run SHARD           the go test -run regex
#   skip SHARD          the go test -skip regex ("" if none)
#   owners PKG TEST     the shards that run TEST of PKG (exactly one is right)
#   verify              reads "PKG TEST" lines on stdin; names each with zero or several owners, exit 1
#
# The table is NAME PKG RUN. A REST row runs everything else in its package (-run . with a -skip of
# the other rows' regexes there), so a new test lands in rest instead of being skipped. The other
# rows are prefixes; the check in conformance-shards_test.sh (make lint) fails on a test that two
# shards claim or that no shard runs. CONFORMANCE_SHARDS_TABLE names a file to use instead (tests).
set -eu

table() {
  if [ -n "${CONFORMANCE_SHARDS_TABLE:-}" ]; then cat "$CONFORMANCE_SHARDS_TABLE"; return; fi
  cat <<'TABLE'
sh ./conformance ^TestSH
fb-sp ./conformance ^Test(FB|SP)
dj-rr-re ./conformance ^Test(DJ|RR|RE)
rest ./conformance REST
plugin ./conformance/plugin .
provider ./conformance/provider .
TABLE
}

row() { table | awk -v n="$1" '$1 == n { print; found = 1 } END { exit !found }' || { echo "conformance-shards: no shard $1" >&2; exit 2; }; }
field() { row "$1" | awk -v i="$2" '{ print $i }'; }

skip_of() {
  [ "$(field "$1" 3)" = REST ] || return 0
  pkg=$(field "$1" 2)
  table | awk -v p="$pkg" -v me="$1" '$2 == p && $1 != me && $3 != "REST" { s = s (s ? "|" : "") $3 } END { print s }'
}

run_of() {
  r=$(field "$1" 3)
  if [ "$r" = REST ]; then echo .; else echo "$r"; fi
}

owners() {
  for s in $(table | awk -v p="$1" '$2 == p { print $1 }'); do
    printf '%s\n' "$2" | grep -Eq "$(run_of "$s")" || continue
    sk=$(skip_of "$s")
    if [ -n "$sk" ] && printf '%s\n' "$2" | grep -Eq "$sk"; then continue; fi
    echo "$s"
  done
}

cmd=${1:-}; [ $# -eq 0 ] || shift
case $cmd in
  shards) table | awk '{ print $1 }' ;;
  pkg) field "${1:?shard}" 2 ;;
  run) run_of "${1:?shard}" ;;
  skip) skip_of "${1:?shard}" ;;
  owners) owners "${1:?pkg}" "${2:?test}" ;;
  verify)
    rc=0
    while read -r pkg name; do
      n=$(owners "$pkg" "$name" | wc -l | tr -d ' ')
      if [ "$n" != 1 ]; then
        echo "conformance-shards: $pkg $name is run by $n shards, want exactly 1" >&2; rc=1
      fi
    done
    exit $rc ;;
  *) echo 'usage: conformance-shards.sh shards | pkg SHARD | run SHARD | skip SHARD | owners PKG TEST | verify' >&2; exit 2 ;;
esac
