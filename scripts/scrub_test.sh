#!/bin/sh
# scrub_test.sh: self-check of scrub.sh in throwaway repos: both modes and the fail-closed exit 2.
set -eu
scrub=$(cd "$(dirname "$0")" && pwd)/scrub.sh
t=$(mktemp -d); trap 'rm -rf "$t"' EXIT
printf 'sekrit\n' >"$t/pat"
g() { git -C "$t/r" -c user.name=t -c user.email=t@example.invalid -c commit.gpgsign=false "$@"; }
# want CODE DIR ARGS...: scrub.sh ARGS, run in DIR, exits CODE
want() {
  code=$1 dir=$2; shift 2; rc=0
  (cd "$dir" && sh "$scrub" "$@") >"$t/out" 2>&1 || rc=$?
  [ "$rc" = "$code" ] || { echo "FAIL: scrub.sh $* in $dir: exit $rc, want $code"; cat "$t/out"; exit 1; }
}
has() { grep -q -- "$1" "$t/out" || { echo "FAIL: output lacks $1"; cat "$t/out"; exit 1; }; }
mkdir "$t/r" "$t/plain"
g init -q -b main; g commit -q --allow-empty -m init; g checkout -q -b task
want 0 "$t/r" --tree "$t/pat"; has '^scrub: clean$'
want 0 "$t/r" "$t/pat"; has '^scrub: clean$'
printf 'a sekrit\n' >"$t/r/f"; g add f; g commit -q -m 'add f'
want 1 "$t/r" --tree "$t/pat"; has '^  f$'          # work tree and a task commit
g rm -q f; g commit -q -m 'drop f'
want 1 "$t/r" --tree "$t/pat"; has 'has matches in' # still in a main..HEAD commit
g checkout -q main; g merge -q --ff-only task; g checkout -q task
want 0 "$t/r" --tree "$t/pat"                       # now on main: outside --tree's range
want 1 "$t/r" "$t/pat"                              # the full-history mode finds it
g commit -q --allow-empty -m 'mentions SEKRIT'
want 1 "$t/r" --tree "$t/pat"; has 'commit message or identity lines match'
want 2 "$t/plain" --tree "$t/pat"; has '^scrub: not a readable git repo$'
want 2 "$t/r" --tree "$t/missing"                   # unreadable pattern file
g branch -q -m main trunk
want 2 "$t/r" --tree "$t/pat"                       # no main: the range cannot be read
echo "scrub_test: ok"
