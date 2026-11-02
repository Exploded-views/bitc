#!/bin/sh
# retry.sh TRIES DELAY COMMAND [ARG...]
#
# Runs COMMAND up to TRIES times, DELAY seconds apart, and stops at the first success. The exit
# code is the last run's. For a flaky network step (the CI tool install), never for a test.
set -eu
usage='usage: retry.sh TRIES DELAY COMMAND [ARG...]'
[ $# -ge 3 ] || { echo "$usage" >&2; exit 2; }
tries=$1; delay=$2; shift 2
case $tries$delay in ''|*[!0-9]*) echo "$usage" >&2; exit 2 ;; esac
[ "$tries" -ge 1 ] || { echo "$usage" >&2; exit 2; }
n=1
while :; do
  rc=0
  "$@" || rc=$?
  [ "$rc" -ne 0 ] || exit 0
  [ "$n" -lt "$tries" ] || { echo "retry: '$1' failed $n time(s); giving up" >&2; exit "$rc"; }
  echo "retry: '$1' failed (exit $rc), try $n of $tries; again in ${delay}s" >&2
  sleep "$delay"
  n=$((n + 1))
done
