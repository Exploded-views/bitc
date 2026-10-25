#!/bin/sh
# scrub.sh [--tree] PATTERNS_FILE: fail if the work tree or a commit (contents, message, author
# or committer) matches a pattern (one extended regex per line, case-insensitive). --tree checks
# the commits in main..HEAD; without it, the whole history. Prints only file names and counts, never the matching text. The pattern file lives
# outside this repository. Exit 0 clean, 1 matches, 2 error: it fails closed, so a repo git
# cannot read is never "clean". git's own errors stay on stderr.
set -eu
range=--all
if [ "${1:-}" = --tree ]; then range=main..HEAD; shift; fi
pat=${1:?usage: scrub.sh [--tree] PATTERNS_FILE}
fail() { echo "scrub: $*" >&2; exit 2; }
[ -r "$pat" ] || fail "cannot read the pattern file"
git rev-parse --git-dir >/dev/null || fail "not a readable git repo"
# names [REV]: matching file names in REV (the work tree if none); git grep exits 1 on no match
names() {
  rc=0; out=$(git grep -I -l -i -E -f "$pat" "$@" -- .) || rc=$?
  [ "$rc" -le 1 ] || fail "git grep failed (status $rc)"
  [ -z "$out" ] || printf '%s\n' "$out"
}
hits=0
tree=$(names)
if [ -n "$tree" ]; then
  echo "scrub: work tree files with matches:"; printf '%s\n' "$tree" | sed 's/^/  /'; hits=1
fi
commits=$(git rev-list "$range") || fail "git rev-list $range failed"
for c in $commits; do
  f=$(names "$c")
  if [ -n "$f" ]; then
    echo "scrub: commit $c has matches in:"; printf '%s\n' "$f" | sed "s/^$c:/  /"; hits=1
  fi
done
log=$(git log "$range" --format='%an %ae %cn %ce%n%B') || fail "git log $range failed"
rc=0; n=$(printf '%s\n' "$log" | grep -c -i -E -f "$pat") || rc=$?
[ "$rc" -le 1 ] || fail "grep failed (status $rc)"
if [ "$n" != 0 ]; then echo "scrub: $n commit message or identity lines match"; hits=1; fi
[ "$hits" = 1 ] || echo "scrub: clean"
exit "$hits"
