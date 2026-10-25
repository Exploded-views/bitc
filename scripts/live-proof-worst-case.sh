#!/bin/sh
# live-proof-worst-case.sh PRICE_PER_HOUR START_TIMEOUT_MIN TIMEOUT_MIN RUNS [SWEEP_INTERVAL_MIN]
# live-proof-worst-case.sh --sprites PRICE_PER_HOUR PROVISION_MIN START_TIMEOUT_MIN TIMEOUT_MIN RUNS [SWEEP_INTERVAL_MIN]
# Prints the worst-case spend of RUNS proof jobs. Each can bill until its expiry E, plus the
# 10-minute self-delete grace, plus, when self-delete can only stop the pod (spec appendix: the
# pod-scoped key may lack delete), the wait until the next sweep removes it (default 15 min,
# the sweep timer's interval). Spec 8.3: E = create + start_timeout + timeout + 15m.
#
# --sprites (sprites-provider.md 8.5): a Sprite has no self-delete and the agent exits at D, so a
# job bills its pre-attach Sprite and the one retry (2 x PROVISION_MIN), then up to
# start + timeout + 15 + the sweep interval.
set -eu
if [ "${1:-}" = "--sprites" ]; then
  shift
  price=${1:?price per hour, e.g. 0.7571}; prov=${2:?provision minutes}; start=${3:?start_timeout minutes}; timeout=${4:?timeout minutes}; runs=${5:?number of runs}
  sweep=${6:-15}
  awk -v p="$price" -v v="$prov" -v s="$start" -v t="$timeout" -v n="$runs" -v w="$sweep" 'BEGIN {
    m = 2 * v + s + t + 15 + w
    printf "worst case per job: %d min (2 x %d provisioning + %d + %d + 15 + %d sweep) x $%.4f/h = $%.2f\n", m, v, s, t, w, p, p * m / 60
    printf "worst case for %d jobs: $%.2f\n", n, n * p * m / 60
  }'
  exit 0
fi
price=${1:?price per hour, e.g. 0.24}; start=${2:?start_timeout minutes}; timeout=${3:?timeout minutes}; runs=${4:?number of runs}
sweep=${5:-15}
awk -v p="$price" -v s="$start" -v t="$timeout" -v n="$runs" -v w="$sweep" 'BEGIN {
  m = s + t + 15 + 10 + w
  printf "worst case per job: %d min x $%.4f/h = $%.2f\n", m, p, p * m / 60
  printf "worst case for %d jobs: $%.2f\n", n, n * p * m / 60
}'
