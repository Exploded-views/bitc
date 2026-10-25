package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// Stub is the start script (spec 8.4 step 1): fetch the agent with curl or wget (else install
// curl), verify its sha256, exec it. It keeps the spec env for stage 1 but runs its own tools
// under a fixed PATH. BITC_STUB_DIR is for the stub's own unit test only.
func Stub(url, sum string) string {
	return fmt.Sprintf(`set -eu
P=/usr/sbin:/usr/bin:/sbin:/bin
d=${BITC_STUB_DIR:-/root/.bitc-agent}; mkdir -p "$d"; chmod 700 "$d"; f=$d/bitc
get() { if command -v curl >/dev/null 2>&1; then OUT=$f curl -fsSL --proto =https -o "$f" %[1]q
 elif command -v wget >/dev/null 2>&1; then wget -q -O "$f" %[1]q; else return 1; fi; }
get || { if command -v apt-get >/dev/null 2>&1; then env -i PATH=$P apt-get update -qq && env -i PATH=$P apt-get install -y -qq curl ca-certificates
 elif command -v apk >/dev/null 2>&1; then env -i PATH=$P apk add -q --no-cache curl ca-certificates; fi; get; }
s=$(sha256sum "$f" 2>/dev/null || busybox sha256sum "$f"); s=${s%%%% *}
[ "$s" = %[2]q ] || { echo "bitc agent: sha256 mismatch" >&2; exit 1; }
chmod 700 "$f"; exec "$f" __agent
`, url, sum)
}

func sha256hex(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

// StubInstall is the provisioning script of the exec delivery (sprites-provider.md 3 step 1):
// fetch the agent over https, verify its sha256 and install it root 0755 at
// $BITC_INSTALL_DIR/bitc (default /opt/bitc/bin; the variable is for the unit test only). It does
// not run it: the host starts every agent exec itself, through the sha check (J4). Nothing job
// specific is in it.
func StubInstall(url, sum string) string {
	return fmt.Sprintf(`set -eu
umask 077
P=/usr/sbin:/usr/bin:/sbin:/bin
d=$(mktemp -d); trap 'rm -rf "$d"' EXIT; f=$d/bitc
get() { if command -v curl >/dev/null 2>&1; then curl -fsSL --proto =https -o "$f" %[1]q
 elif command -v wget >/dev/null 2>&1; then wget -q -O "$f" %[1]q; else return 1; fi; }
get || { if command -v apt-get >/dev/null 2>&1; then env -i PATH=$P apt-get update -qq && env -i PATH=$P apt-get install -y -qq curl ca-certificates
 elif command -v apk >/dev/null 2>&1; then env -i PATH=$P apk add -q --no-cache curl ca-certificates; fi; get; }
s=$(sha256sum "$f" 2>/dev/null || busybox sha256sum "$f"); s=${s%%%% *}
[ "$s" = %[2]q ] || { echo "bitc agent: sha256 mismatch" >&2; exit 1; }
t=${BITC_INSTALL_DIR:-/opt/bitc/bin}; mkdir -p "$t"; chmod 755 "$t"
chmod 755 "$f"; mv "$f" "$t/bitc"
`, url, sum)
}
