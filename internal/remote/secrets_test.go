package remote

import (
	"bytes"
	"testing"

	"github.com/Exploded-views/bitc/internal/config"
)

func TestJobSecretsAreFixedOnlyUnderTheSeam(t *testing.T) {
	b, k, m := jobSecrets(&config.Effective{Test: &config.Test{FixedSecrets: true}})
	for name, got := range map[string]struct {
		have [32]byte
		fill byte
	}{"B": {b, 0x0b}, "K": {k, 0x0c}, "M": {m, 0x0d}} {
		if !bytes.Equal(got.have[:], bytes.Repeat([]byte{got.fill}, 32)) {
			t.Errorf("%s = %x, want 32 bytes of %#x", name, got.have, got.fill)
		}
	}
	for _, e := range []*config.Effective{{}, {Test: &config.Test{}}} {
		b1, k1, m1 := jobSecrets(e)
		b2, _, _ := jobSecrets(e)
		if b1 == b2 || b1 == k1 || k1 == m1 || b1 == ([32]byte{}) {
			t.Errorf("secrets without the seam are not random: %x %x %x", b1, k1, m1)
		}
	}
}
