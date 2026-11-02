#!/bin/sh
# coverage-merge_test.sh: self-check of coverage-merge.sh on fake per-leg `go test -cover` output.
set -eu
cm=$(cd "$(dirname "$0")" && pwd)/coverage-merge.sh
t=$(mktemp -d); trap 'rm -rf "$t"' EXIT
fail() { echo "FAIL: $*"; [ ! -f "$t/out" ] || cat "$t/out"; exit 1; }
# want CODE ARGS...: coverage-merge.sh ARGS exits CODE; output goes to $t/out
want() {
  code=$1; shift; rc=0
  sh "$cm" "$@" >"$t/out" 2>&1 || rc=$?
  [ "$rc" = "$code" ] || fail "coverage-merge.sh $*: exit $rc, want $code"
}
has() { grep -q -- "$1" "$t/out" || fail "output lacks $1"; }

printf 'ok  \tex/a\t0.012s\tcoverage: 85.3%% of statements\n' >"$t/test-cover-core.txt"
printf 'ok  \tex/p\t0.5s\tcoverage: 70.0%% of statements\n?   \tex/n\t[no test files]\n' >"$t/test-cover-providers.txt"

# both legs present: merged in the order given
want 0 "$t/merged.txt" "$t" core providers
[ "$(grep -c 'coverage:' "$t/merged.txt")" = 2 ] || fail "merged output lacks a leg's lines"
grep -q 'ex/a' "$t/merged.txt" && grep -q 'ex/p' "$t/merged.txt" || fail "merged output lacks a package"

# a missing leg file is refused, and the leg is named
rm -f "$t/merged.txt"
want 1 "$t/merged.txt" "$t" core providers extra
has extra
[ ! -s "$t/merged.txt" ] || fail "a refused merge left output"

# a leg with no coverage: lines is refused, and named
printf '?   \tex/n\t[no test files]\n' >"$t/test-cover-providers.txt"
want 1 "$t/merged.txt" "$t" core providers
has providers

# an empty leg file likewise
: >"$t/test-cover-providers.txt"
want 1 "$t/merged.txt" "$t" core providers
has providers

# a failed package in a leg is refused (the leg's own job would have failed, but do not trust it)
printf 'ok  \tex/p\t0.5s\tcoverage: 70.0%% of statements\nFAIL\tex/q\t0.1s\n' >"$t/test-cover-providers.txt"
want 1 "$t/merged.txt" "$t" core providers
has providers

# no expected legs at all is a usage error, not an empty pass
want 2 "$t/merged.txt" "$t"
echo "coverage-merge_test: ok"
