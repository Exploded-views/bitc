#!/bin/sh
# repro-check.sh [COMMIT [ARCH]]: prove the agent build is reproducible, host versus container.
# Builds COMMIT (default HEAD) for linux/ARCH (amd64 by default, or arm64) three ways and fails unless all sha256s are identical:
#   1, 2  scripts/agent-build-sha.sh twice (fresh export, fresh caches, two temp paths and two
#         containers), the build the host config's agent.sha256 comes from;
#   3     the normal scripts/dev.sh route (warm dev cache, another path), as the pod's cache-less
#         build must agree with a developer's.
# Run it before a live proof (a maintainer step). NOT part of make ci: it downloads
# modules from the default GOPROXY, so it needs the network.
set -eu
arch=${2-amd64}
case $arch in amd64|arm64) ;; *) echo "repro-check: arch must be amd64 or arm64: \"$arch\"" >&2; exit 64 ;; esac
top=$(git rev-parse --show-toplevel)
commit=$(git -C "$top" rev-parse --verify "${1:-HEAD}^{commit}")
here=$top/scripts
a=$(sh "$here/agent-build-sha.sh" "$commit" "$arch")
b=$(sh "$here/agent-build-sha.sh" "$commit" "$arch")
cache=${BITC_DEV_CACHE:-$HOME/.cache/bitc-build}
mkdir -p "$cache"
tmp=$(mktemp -d "$cache/repro.XXXXXX")
trap 'chmod -R u+w "$tmp" 2>/dev/null || :; rm -rf "$tmp"' EXIT
mkdir "$tmp/src"
git -C "$top" archive "$commit" | tar -x -C "$tmp/src"
cp "$top/internal/agent/buildrecipe.sh" "$tmp/recipe.sh"
git -C "$tmp/src" init -q   # scripts/dev.sh wants a git work tree
(cd "$tmp/src" && sh "$here/dev.sh" sh -c '. "$1" && bitc_build "$2" "$3" "$4"' sh "$tmp/recipe.sh" "$commit" "$tmp/agent" "$arch") >&2
s=$(sha256sum "$tmp/agent"); c=${s%% *}
echo "commit            $commit (linux/$arch)"
echo "export 1 (docker) $a"
echo "export 2 (docker) $b"
echo "dev.sh route      $c"
if [ "$a" = "$b" ] && [ "$a" = "$c" ]; then echo "repro-check: identical"; else echo "repro-check: MISMATCH" >&2; exit 1; fi
