#!/bin/sh
# dev.sh CMD...: run CMD in the pinned Go toolchain container (the host needs only docker).
set -eu
BITC_DEV_IMAGE=${BITC_DEV_IMAGE:-golang:1.27.3-bookworm@sha256:972b4110ba0f47539a975c31e52c64765e1ff19f2d74469ae4a595c6676e80a5}
top=$(git rev-parse --show-toplevel)
common=$(cd "$(git rev-parse --git-common-dir)" && pwd)   # main repo's .git, so worktrees resolve
cache=${BITC_DEV_CACHE:-$HOME/.cache/bitc-build}
mkdir -p "$cache/gocache" "$cache/gomod" "$cache/home"
set -- docker run --rm --init --user "$(id -u):$(id -g)" \
  -e HOME="$cache/home" -e GOCACHE="$cache/gocache" -e GOMODCACHE="$cache/gomod" \
  -e GOPATH="$cache/home/go" -e PATH="$cache/home/go/bin:/usr/local/go/bin:/usr/sbin:/usr/bin:/sbin:/bin" \
  -e GOTOOLCHAIN=local -e GOMAXPROCS="${GOMAXPROCS:-4}" -e GOFLAGS="${GOFLAGS:--p=4}" \
  -e FUZZTIME -e SCRUB_PATTERNS -e GITLEAKS_EXTRA -e GITHUB_TOKEN \
  -v "$top:$top" -v "$common:$common" -v "$cache:$cache" \
  ${SCRUB_PATTERNS:+-v "$SCRUB_PATTERNS:$SCRUB_PATTERNS:ro"} \
  ${GITLEAKS_EXTRA:+-v "$GITLEAKS_EXTRA:$GITLEAKS_EXTRA:ro"} \
  --tmpfs /tmp:exec,mode=1777 -w "$PWD" "$BITC_DEV_IMAGE" "$@"
exec ${BITC_DEV_WRAP:-} "$@"
