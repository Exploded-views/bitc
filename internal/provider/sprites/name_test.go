package sprites

import (
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Exploded-views/bitc/internal/provider"
)

var testSecret = []byte("0123456789abcdef0123456789abcdef")

const testInstall = "0123456789abcdef"

func TestNameHasTheDesignedShapeAndTag(t *testing.T) {
	exp := time.Unix(1_800_000_000, 0)
	n := EncodeName(testSecret, "bitc", testInstall, "cpu8", "deadbeef", exp)
	if want := "bitc-" + testInstall + "-cpu8-deadbeef-1800000000-"; !strings.HasPrefix(n, want) || len(n) != 55 {
		t.Fatalf("%q (%d chars)", n, len(n))
	}
	body := strings.TrimSuffix(n, n[len(n)-9:])
	if tag := n[len(n)-8:]; tag != provider.OwnerTag(testSecret, body)[:8] || !regexp.MustCompile(`^[0-9a-f]{8}$`).MatchString(tag) {
		t.Fatalf("tag %q is not the first 8 hex of HMAC(secret, %q)", tag, body)
	}
}

func TestParseNameRoundTripsAndRejectsOtherShapes(t *testing.T) {
	exp := time.Unix(1_800_000_000, 0)
	n := EncodeName(testSecret, "bitc", testInstall, "cpu8", "deadbeef", exp)
	p, ok := ParseName("bitc", n)
	if !ok || p.Install != testInstall || p.Size != "cpu8" || p.Job8 != "deadbeef" || !p.Expires.Equal(exp) || p.Tag != n[len(n)-8:] {
		t.Fatalf("%+v %v", p, ok)
	}
	for _, bad := range []string{"", "bitc", "other-" + n[5:], n + "-x", n[:len(n)-1], "bitc-" + testInstall + "-cpu8-deadbeef-notanumber-0123abcd",
		"bitc-short-cpu8-deadbeef-1800000000-0123abcd", "bitc-" + testInstall + "-cpu8-DEADBEEF-1800000000-0123abcd"} {
		if _, ok := ParseName("bitc", bad); ok {
			t.Errorf("%q parsed", bad)
		}
	}
	if _, ok := ParseName("other", n); ok {
		t.Error("a name of another prefix parsed")
	}
}

func TestVerifyNameNeedsTheInstallSecret(t *testing.T) {
	exp := time.Unix(1_800_000_000, 0)
	n := EncodeName(testSecret, "bitc", testInstall, "cpu8", "deadbeef", exp)
	if !VerifyName(testSecret, n) {
		t.Fatal("a name made with the secret does not verify")
	}
	if VerifyName([]byte("another-install-secret-0123456789"), n) {
		t.Error("verified under another secret")
	}
	for _, forged := range []string{strings.Replace(n, "deadbeef", "deadbeee", 1), strings.Replace(n, "1800000000", "1900000000", 1), strings.Replace(n, "cpu8", "cpu9", 1), n[:len(n)-1] + "0", "nonsense"} {
		if forged != n && VerifyName(testSecret, forged) {
			t.Errorf("%q verified", forged)
		}
	}
}

func TestLongestAllowedPrefixAndSizeFitIn63(t *testing.T) {
	n := EncodeName(testSecret, "bitcbitcbi", testInstall, "abcdef", "deadbeef", time.Unix(1_800_000_000, 0))
	if len(n) != 63 {
		t.Fatalf("%d chars: %s", len(n), n)
	}
}
