#!/bin/sh
# conformance-shards_test.sh: every test of every conformance package belongs to exactly one shard
# of conformance-shards.sh (a new test can never be skipped silently), and the table is sound.
# Lists the tests with `go test -list`, so it needs the Go toolchain; it runs in make lint.
set -eu
cd "$(dirname "$0")/.."
cs=scripts/conformance-shards.sh
t=$(mktemp -d); trap 'rm -rf "$t"' EXIT
fail() { echo "FAIL: $*"; exit 1; }

shards=$(sh "$cs" shards)
[ -n "$shards" ] || fail "no shards"
for s in $shards; do
  [ -n "$(sh "$cs" pkg "$s")" ] || fail "shard $s has no package"
  [ -n "$(sh "$cs" run "$s")" ] || fail "shard $s has no -run regex"
done

mod=$(go list -m)
go test -list . ./conformance/... >"$t/list" 2>&1 || { cat "$t/list"; fail "go test -list"; }
# "Name" lines, then "ok  <pkg>  <time>" per package -> "./rel/pkg Name"
awk -v mod="$mod" '
  /^Test/ || /^Fuzz/ || /^Example/ || /^Benchmark/ { names[++n] = $1; next }
  $1 == "ok" { pkg = $2; sub("^" mod, ".", pkg); for (i = 1; i <= n; i++) print pkg, names[i]; n = 0; next }
  $1 == "?" { n = 0 }
' "$t/list" >"$t/tests"
[ "$(wc -l <"$t/tests")" -gt 100 ] || { cat "$t/list"; fail "implausibly few tests listed"; }

sh "$cs" verify <"$t/tests" || fail "tests with not exactly one shard (above)"
echo "conformance-shards_test: ok ($(wc -l <"$t/tests" | tr -d ' ') tests, $(echo "$shards" | wc -l | tr -d ' ') shards)"
