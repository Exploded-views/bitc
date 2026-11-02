#!/bin/sh
# retry_test.sh: self-check of retry.sh (bounded retries of a command).
set -eu
rt=$(cd "$(dirname "$0")" && pwd)/retry.sh
t=$(mktemp -d); trap 'rm -rf "$t"' EXIT
fail() { echo "FAIL: $*"; exit 1; }
n() { cat "$t/count" 2>/dev/null || echo 0; }
# a command that fails until its Nth run
cat >"$t/flaky.sh" <<'SCRIPT'
#!/bin/sh
c=$(cat "$1/count" 2>/dev/null || echo 0); c=$((c + 1)); echo "$c" >"$1/count"
[ "$c" -ge "$2" ]
SCRIPT

rc=0; sh "$rt" 3 0 sh "$t/flaky.sh" "$t" 1 >/dev/null 2>&1 || rc=$?
[ "$rc" = 0 ] && [ "$(n)" = 1 ] || fail "a command that passes is run once (rc $rc, runs $(n))"

rm -f "$t/count"; rc=0; sh "$rt" 3 0 sh "$t/flaky.sh" "$t" 3 >/dev/null 2>&1 || rc=$?
[ "$rc" = 0 ] && [ "$(n)" = 3 ] || fail "the third try may pass (rc $rc, runs $(n))"

rm -f "$t/count"; rc=0; sh "$rt" 3 0 sh "$t/flaky.sh" "$t" 4 >"$t/out" 2>&1 || rc=$?
[ "$rc" != 0 ] && [ "$(n)" = 3 ] || fail "three tries is the bound (rc $rc, runs $(n))"

rm -f "$t/count"; rc=0; sh "$rt" 1 0 sh "$t/flaky.sh" "$t" 2 >/dev/null 2>&1 || rc=$?
[ "$rc" != 0 ] && [ "$(n)" = 1 ] || fail "one try means no retry (rc $rc, runs $(n))"

# the failing command's own exit code survives
rc=0; sh "$rt" 2 0 sh -c 'exit 7' >/dev/null 2>&1 || rc=$?
[ "$rc" = 7 ] || fail "exit code is the command's, got $rc"

# bad arguments are a usage error
rc=0; sh "$rt" x 0 true >/dev/null 2>&1 || rc=$?
[ "$rc" = 2 ] || fail "non-numeric tries: exit $rc, want 2"
rc=0; sh "$rt" 3 0 >/dev/null 2>&1 || rc=$?
[ "$rc" = 2 ] || fail "no command: exit $rc, want 2"
echo "retry_test: ok"
