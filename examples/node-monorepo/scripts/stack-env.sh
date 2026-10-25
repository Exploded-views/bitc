#!/bin/sh
# stack-env.sh N: print the environment of end-to-end stack slot N (1..40) as KEY=VALUE lines.
n=${1:-}
case $n in ''|*[!0-9]*) echo "stack slot must be a number, got '$n'" >&2; exit 64 ;; esac
if [ "$n" -lt 1 ] || [ "$n" -gt 40 ]; then echo "stack slot $n is outside 1..40" >&2; exit 64; fi
echo "export APP_STACK_PROJECT=demo-env-$n"
echo "APP_STACK_DB_PORT=$((46000 + n))"
echo "DATABASE_URL='postgresql://app:app@localhost:$((46000 + n))/app'"
echo "APP_STACK_SLOT=$n"
