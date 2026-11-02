#!/bin/sh
# coverage-floors_test.sh: self-check of coverage-floors.sh on fake `go test -cover` output.
set -eu
cf=$(cd "$(dirname "$0")" && pwd)/coverage-floors.sh
t=$(mktemp -d); trap 'rm -rf "$t"' EXIT
fail() { echo "FAIL: $*"; [ ! -f "$t/out" ] || cat "$t/out"; exit 1; }
# want CODE ARGS...: coverage-floors.sh ARGS exits CODE; output goes to $t/out
want() {
  code=$1; shift; rc=0
  sh "$cf" "$@" >"$t/out" 2>&1 || rc=$?
  [ "$rc" = "$code" ] || fail "coverage-floors.sh $*: exit $rc, want $code"
}
has() { grep -q -- "$1" "$t/out" || fail "output lacks $1"; }
lacks() { if grep -q -- "$1" "$t/out"; then fail "output has $1"; fi; }
floors() { printf '%s\n' "$1" >"$t/floors.json"; }
run() { printf '%s\n' "$@" >"$t/run.txt"; }
same() { [ "$(cat "$t/floors.json")" = "$1" ] || { echo "FAIL: floors file is:"; cat "$t/floors.json"; exit 1; }; }

A='ok  	ex/a	0.012s	coverage: 85.3% of statements'
B='ok  	ex/b	(cached)	coverage: 50.0% of statements'
N='?   	ex/n	[no test files]'
S='ok  	ex/s	0.003s	coverage: [no statements]'

# --- check ---
floors '{
  "ex/a": 85.3,
  "ex/b": 40.0,
  "ex/n": 0.0
}'
run "$A" "$B" "$N" "$S"
want 0 check "$t/run.txt" "$t/floors.json"                  # at or above every floor; no-statements ignored
lacks 'ex/s'

run 'ok  	ex/a	0.012s	coverage: 85.2% of statements' "$B" "$N"
want 1 check "$t/run.txt" "$t/floors.json"; has 'ex/a'; has '85.3'; has '85.2'   # below its floor
lacks 'ex/b'                                                # only the failing package is printed

run "$A" "$B" "$N" 'ok  	ex/new	0.1s	coverage: 10.0% of statements'
want 1 check "$t/run.txt" "$t/floors.json"; has 'ex/new'; has 'no floor'          # tested package, no floor

run "$A" "$B" '?   	ex/n	[no test files]' '?   	ex/untested	[no test files]'
want 1 check "$t/run.txt" "$t/floors.json"; has 'ex/untested'                      # no test files still needs an entry

run "$A" "$B"
want 1 check "$t/run.txt" "$t/floors.json"; has 'ex/n'; has 'not in run'          # stale floor

run "$A" "$B" 'ok  	ex/n	0.1s	coverage: 0.0% of statements'
want 0 check "$t/run.txt" "$t/floors.json"                  # a package that gained tests at 0.0 still passes
run "$A" "$B" 'FAIL	ex/n	0.1s'
want 1 check "$t/run.txt" "$t/floors.json"                  # a failed package has no coverage: stale or missing

# --- ratchet ---
floors '{
  "ex/a": 80.0,
  "ex/b": 40.0,
  "ex/gone": 12.5,
  "ex/n": 0.0
}'
run "$A" "$B" "$N" "$S" 'ok  	ex/new	0.1s	coverage: 10.0% of statements' '?   	ex/newn	[no test files]'
want 0 ratchet "$t/run.txt" "$t/floors.json"
same '{
  "ex/a": 85.3,
  "ex/b": 50.0,
  "ex/gone": 12.5,
  "ex/n": 0.0,
  "ex/new": 10.0,
  "ex/newn": 0.0
}'                                                          # raised, new added, stale kept, no-statements not added
want 1 check "$t/run.txt" "$t/floors.json"; has 'ex/gone'   # the stale entry still fails check until dropped

want 0 ratchet --drop-removed "$t/run.txt" "$t/floors.json"
same '{
  "ex/a": 85.3,
  "ex/b": 50.0,
  "ex/n": 0.0,
  "ex/new": 10.0,
  "ex/newn": 0.0
}'
want 0 check "$t/run.txt" "$t/floors.json"

floors '{
  "ex/a": 90.0,
  "ex/b": 40.0
}'
run "$A" "$B"
want 1 ratchet "$t/run.txt" "$t/floors.json"; has 'ex/a'; has '90.0'; has '85.3'     # never lowers
lacks 'ex/b'
same '{
  "ex/a": 90.0,
  "ex/b": 40.0
}'                                                          # and writes nothing
want 1 ratchet --drop-removed "$t/run.txt" "$t/floors.json"                          # the flag does not lift the refusal

floors '{
  "ex/a": 85.3
}'
run 'ok  	ex/a	0.1s	coverage: 85.37% of statements' 'ok  	ex/b	0.1s	coverage: 99.99% of statements' 'ok  	ex/c	0.1s	coverage: 100% of statements'
want 0 ratchet "$t/run.txt" "$t/floors.json"
same '{
  "ex/a": 85.3,
  "ex/b": 99.9,
  "ex/c": 100.0
}'                                                          # rounded down to 0.1, never up
# --- real `go test -cover` output for a package without test files ---
# Go prints it with a leading tab and no "ok"; the coverage is 0.0% of its statements.
T=$(printf '\tex/nt\t\tcoverage: 0.0%% of statements')
floors '{
  "ex/a": 85.3,
  "ex/nt": 0.0
}'
run "$A" "$T"
want 0 check "$t/run.txt" "$t/floors.json"
floors '{
  "ex/a": 85.3
}'
want 1 check "$t/run.txt" "$t/floors.json"; has 'ex/nt'; has 'no floor'            # still needs an entry
want 0 ratchet "$t/run.txt" "$t/floors.json"
same '{
  "ex/a": 85.3,
  "ex/nt": 0.0
}'
# --- tolerances: coverage-tolerances.json next to the floors file ---
floors '{
  "ex/a": 85.3,
  "ex/b": 40.0
}'
tol() { printf '{\n%s\n}\n' "$1" >"$t/coverage-tolerances.json"; }
tol '  "ex/a": {"tolerance": 0.4, "why": "runner measures lower"}'
B2='ok  	ex/b	0.1s	coverage: 40.0% of statements'
run 'ok  	ex/a	0.1s	coverage: 84.9% of statements' "$B2"
want 0 check "$t/run.txt" "$t/floors.json"                  # 85.3 - 0.4 = 84.9: within tolerance
run 'ok  	ex/a	0.1s	coverage: 84.8% of statements' "$B2"
want 1 check "$t/run.txt" "$t/floors.json"; has 'ex/a'; has '85.3'; has '84.8'      # beyond it
run 'ok  	ex/a	0.1s	coverage: 85.3% of statements' 'ok  	ex/b	0.1s	coverage: 39.9% of statements'
want 1 check "$t/run.txt" "$t/floors.json"; has 'ex/b'                               # no tolerance for other packages
tol '  "ex/a": {"tolerance": 0.6, "why": "too generous"}'
run "$A" "$B2"
want 1 check "$t/run.txt" "$t/floors.json"; has 'ex/a'; has 'tolerance'              # over 0.5 is refused even when passing
tol '  "ex/a": {"tolerance": 0.5, "why": "at the cap"}'
want 0 check "$t/run.txt" "$t/floors.json"
tol '  "ex/zzz": {"tolerance": 0.1, "why": "no such floor"}'
want 1 check "$t/run.txt" "$t/floors.json"; has 'ex/zzz'; has 'no floor'             # tolerance without a floor is refused
tol '  "ex/a": {"tolerance": 0.4, "why": "runner measures lower"}'
run 'ok  	ex/a	0.1s	coverage: 85.0% of statements' "$B2"
want 1 ratchet "$t/run.txt" "$t/floors.json"; has 'ex/a'                              # ratchet ignores tolerance: still never lowers
same '{
  "ex/a": 85.3,
  "ex/b": 40.0
}'
run 'ok  	ex/a	0.1s	coverage: 86.0% of statements' "$B2"
want 0 ratchet "$t/run.txt" "$t/floors.json"
same '{
  "ex/a": 86.0,
  "ex/b": 40.0
}'                                                          # writes the real value, not value minus tolerance
rm -f "$t/coverage-tolerances.json"
echo "coverage-floors_test: ok"
