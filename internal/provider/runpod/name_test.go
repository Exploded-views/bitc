package runpod

import (
	"strings"
	"testing"
	"time"
)

func TestNameRoundTrip(t *testing.T) {
	e := time.Unix(1_800_000_000, 0)
	for _, pool := range []string{"heavy", "release-tag", "a-b-c", strings.Repeat("p", 80)} {
		n := EncodeName("bitc", "0123456789abcdef", pool, "cpu8", "deadbeef", e)
		if len(n) > 63 {
			t.Fatalf("%d chars: %s", len(n), n)
		}
		r, ok := ParseName("bitc", n)
		if !ok || r.Install != "0123456789abcdef" || r.SizeKey != "cpu8" || r.Job != "deadbeef" || !r.Expires.Equal(e) {
			t.Fatalf("%s -> %+v %v", n, r, ok)
		}
		if !strings.HasPrefix(pool, r.Pool) {
			t.Fatalf("pool %q from %q", r.Pool, pool)
		}
	}
	for _, bad := range []string{"other-0123456789abcdef-heavy-cpu8-deadbeef-1800000000", "bitc-xyz-heavy-cpu8-deadbeef-1", "bitc-0123456789abcdef-heavy-cpu8-deadbeef-notanumber"} {
		if _, ok := ParseName("bitc", bad); ok {
			t.Errorf("%s parsed", bad)
		}
	}
}
