#!/bin/sh
# agent-build-sha.sh COMMIT [ARCH]: print the sha256 of the linux/ARCH (amd64 by default) bitc agent built from COMMIT (spec 8.4 step 1,
# agent = { source = "build" }). Put the printed value in the host config's agent.sha256: the pod
# builds the same commit with the same recipe and runs the result only if it hashes to that value.
#
# Host side; needs only docker and git. NOT part of make ci: it downloads the modules from the
# default GOPROXY (go.sum verified), so it needs the network. It does not use the dev cache: a
# fresh `git archive COMMIT` export, a fresh module and build cache, in the pinned image from
# scripts/dev.sh. The build recipe is internal/agent/buildrecipe.sh, the file the pod stub embeds.
set -eu
[ $# -ge 1 ] && [ $# -le 2 ] || { echo "usage: agent-build-sha.sh COMMIT [ARCH]   (ARCH: amd64, the default, or arm64)" >&2; exit 64; }
arch=${2-amd64}
case $arch in amd64|arm64) ;; *) echo "agent-build-sha: arch must be amd64 or arm64: \"$arch\"" >&2; exit 64 ;; esac
top=$(git rev-parse --show-toplevel)
commit=$(git -C "$top" rev-parse --verify "$1^{commit}")
img=${BITC_DEV_IMAGE:-$(sed -n 's/^BITC_DEV_IMAGE=\${BITC_DEV_IMAGE:-\(.*\)}$/\1/p' "$top/scripts/dev.sh")}
[ -n "$img" ] || { echo "agent-build-sha: no image pinned in scripts/dev.sh" >&2; exit 2; }
rt=$(command -v docker || command -v podman) || { echo "agent-build-sha: no docker or podman" >&2; exit 2; }
tmp=$(mktemp -d "${TMPDIR:-/tmp}/bitc-agent-build.XXXXXX")
trap 'chmod -R u+w "$tmp" 2>/dev/null || :; rm -rf "$tmp"' EXIT
mkdir "$tmp/src" "$tmp/home" "$tmp/gocache" "$tmp/gomod"
git -C "$top" archive "$commit" | tar -x -C "$tmp/src"
cp "$top/internal/agent/buildrecipe.sh" "$tmp/recipe.sh"
"$rt" run --rm --init --user "$(id -u):$(id -g)" \
  -e HOME=/work/home -e GOCACHE=/work/gocache -e GOMODCACHE=/work/gomod -e GOTOOLCHAIN=local \
  -v "$tmp:/work" -w /work/src "$img" \
  sh -c '. /work/recipe.sh && bitc_build "$1" /work/agent "$2"' sh "$commit" "$arch" >&2
s=$(sha256sum "$tmp/agent"); echo "${s%% *}"
