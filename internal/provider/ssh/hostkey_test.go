package ssh

import (
	"strings"
	"testing"
)

func TestParseHostKey(t *testing.T) {
	cases := []struct {
		name, in, typ, err string
	}{
		{"ed25519", edKey(), "ssh-ed25519", ""},
		{"rsa 3072", rsaKey(3072), "ssh-rsa", ""},
		{"rsa 2048", rsaKey(2048), "", "ssh-rsa key is 2048 bits, 3072 or more required"},
		{"dss", "ssh-dss AAAAB3NzaC1kc3M=", "", "ssh-dss is not accepted"},
		{"empty", "", "", "required"},
		{"garbage", "hello", "", "unparsable"},
		{"bad base64", "ssh-ed25519 !!!", "", "unparsable"},
		{"type mismatch", strings.Replace(edKey(), "ssh-ed25519 ", "ecdsa-sha2-nistp256 ", 1), "", "unparsable"},
		{"comment kept out", edKey() + " owner@host", "ssh-ed25519", ""},
		{"unknown type", "ssh-foo AAAA", "", "unparsable"},
	}
	for _, c := range cases {
		typ, err := ParseHostKey(c.in)
		got := ""
		if err != nil {
			got = err.Error()
		}
		if typ != c.typ || got != c.err {
			t.Errorf("%s: got (%q, %q), want (%q, %q)", c.name, typ, got, c.typ, c.err)
		}
	}
}

func TestKnownHostsLineIsOneAliasedLineWithoutAComment(t *testing.T) {
	got := KnownHostsLine(edKey() + " owner@host")
	if want := "bitc-ssh " + edKey() + "\n"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
