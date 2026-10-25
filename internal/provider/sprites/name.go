package sprites

import (
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Exploded-views/bitc/internal/provider"
)

// Parsed are the fields of a Sprite name (sprites-provider.md 2.1).
type Parsed struct {
	Install, Size, Job8, Tag string
	Expires                  time.Time
}

// EncodeName is <prefix>-<install16>-<size>-<job8>-<E unix>-<tag8>, tag8 being the first 8 hex of
// HMAC(install secret, the name without "-tag8"). The pool is not in the name (no room).
func EncodeName(secret []byte, prefix, install, size, job8 string, expiry time.Time) string {
	body := prefix + "-" + install + "-" + size + "-" + job8 + "-" + strconv.FormatInt(expiry.Unix(), 10)
	return body + "-" + provider.OwnerTag(secret, body)[:8]
}

var spriteNameRE = regexp.MustCompile(`^([a-z0-9]+)-([0-9a-f]{16})-([a-z0-9]+)-([0-9a-f]{8})-([0-9]+)-([0-9a-f]{8})$`)

// ParseName reads a name with this prefix.
func ParseName(prefix, name string) (Parsed, bool) {
	m := spriteNameRE.FindStringSubmatch(name)
	if m == nil || m[1] != prefix {
		return Parsed{}, false
	}
	e, err := strconv.ParseInt(m[5], 10, 64)
	if err != nil {
		return Parsed{}, false
	}
	return Parsed{Install: m[2], Size: m[3], Job8: m[4], Tag: m[6], Expires: time.Unix(e, 0)}, true
}

// VerifyName reports whether the name's tag8 is the one this install's secret makes.
func VerifyName(secret []byte, name string) bool {
	i := strings.LastIndexByte(name, '-')
	if i < 0 || len(name)-i-1 != 8 {
		return false
	}
	body, tag := name[:i], name[i+1:]
	return provider.OwnerMatchesPrefix(secret, body, tag)
}
