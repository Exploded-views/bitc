package runpod

import (
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Exploded-views/bitc/internal/provider"
)

// EncodeName is provider.ResourceName with an explicit job8 (tests use it to make fixed names).
func EncodeName(prefix, install, pool, size, job8 string, expiry time.Time) string {
	tail := "-" + size + "-" + job8 + "-" + strconv.FormatInt(expiry.Unix(), 10)
	head := prefix + "-" + install + "-"
	room := 63 - len(head) - len(tail)
	if room < 1 { // unreachable with the loader's 10-character cap on name_prefix and size keys
		room = 1
	}
	if len(pool) > room {
		pool = strings.TrimRight(pool[:room], "-")
	}
	return head + pool + tail
}

var nameRE = regexp.MustCompile(`^([a-z0-9]+)-([0-9a-f]{16})-([A-Za-z0-9-]+)-([a-z0-9]+)-([0-9a-f]{8})-([0-9]+)$`)

// ParseName returns the fields of a name with this prefix.
func ParseName(prefix, name string) (provider.Resource, bool) {
	m := nameRE.FindStringSubmatch(name)
	if m == nil || m[1] != prefix {
		return provider.Resource{}, false
	}
	e, err := strconv.ParseInt(m[6], 10, 64)
	if err != nil {
		return provider.Resource{}, false
	}
	return provider.Resource{Name: name, Install: m[2], Pool: m[3], SizeKey: m[4], Job: m[5], Expires: time.Unix(e, 0)}, true
}
