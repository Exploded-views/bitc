package agent

import (
	_ "embed"
	"fmt"
	"strings"
)

//go:embed buildrecipe.sh
var buildRecipe string

// shq single-quotes s for sh.
func shq(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// StubBuild is the start script for agent = { source = "build" } (spec 8.4 step 1). In the pinned
// golang image, as root, it fetches the one agent ref from cloneURL with the deploy key (the
// BITC_DEPLOY_KEY env var, pinned hosts from BITC_KNOWN_HOSTS, the same set the agent uses),
// shreds the key at once, checks out and verifies commit, builds with the recipe in
// buildrecipe.sh, and execs the result only if its sha256 equals sum, under umask 022 (the stub
// itself runs under 077 for the key; the agent must create 0755 dirs the job user can traverse). BITC_STUB_DIR is for the
// stub's own unit test only.
func StubBuild(cloneURL, ref, commit, sum string) string {
	return fmt.Sprintf(`set -eu
PATH=$PATH:/usr/local/go/bin
%[1]s
d=${BITC_STUB_DIR:-/root/.bitc-agent}; mkdir -p "$d"; chmod 700 "$d"
k=$d/k; src=$d/src; rm -rf "$k" "$src"; umask 077; mkdir -m 700 "$k" "$src"
wipe() { { shred -u "$k/key" 2>/dev/null || rm -f "$k/key"; } || :; }
trap wipe EXIT
printf '%%s\n' "$BITC_DEPLOY_KEY" > "$k/key"; chmod 600 "$k/key"
printf '%%s\n' "$BITC_KNOWN_HOSTS" > "$k/known_hosts"
cd "$src"; git init -q .
git -c core.sshCommand="ssh -i $k/key -o IdentitiesOnly=yes -o StrictHostKeyChecking=yes -o UserKnownHostsFile=$k/known_hosts -o BatchMode=yes" fetch -q --depth=1 %[2]s %[3]s
wipe
git -c advice.detachedHead=false checkout -q FETCH_HEAD
[ "$(git rev-parse HEAD)" = %[4]s ] || { echo "bitc agent: fetched commit is not the pinned one" >&2; exit 1; }
export HOME=$d/home GOCACHE=$d/gocache GOMODCACHE=$d/gomod; mkdir -p "$HOME" "$GOCACHE" "$GOMODCACHE"
bitc_build %[4]s "$d/bitc.new"
s=$(sha256sum "$d/bitc.new"); s=${s%%%% *}
[ "$s" = %[5]s ] || { echo "bitc agent: build sha256 mismatch" >&2; exit 1; }
cd "$d"; chmod -R u+w "$src" "$HOME" "$GOCACHE" "$GOMODCACHE" 2>/dev/null || :
rm -rf "$src" "$HOME" "$GOCACHE" "$GOMODCACHE" "$k"
mv "$d/bitc.new" "$d/bitc"; chmod 700 "$d/bitc"; umask 022; exec "$d/bitc" __agent
`, buildRecipe, shq(cloneURL), shq(ref), shq(commit), shq(sum))
}
