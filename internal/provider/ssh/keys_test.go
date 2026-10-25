package ssh

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"math/big"
	"os"
	"path/filepath"
	"testing"
)

func wstr(b []byte) []byte {
	out := make([]byte, 4, 4+len(b))
	binary.BigEndian.PutUint32(out, uint32(len(b)))
	return append(out, b...)
}

// edKey is a syntactically valid ssh-ed25519 public key line (the 32 bytes are not a real point;
// the parser only reads the wire format).
func edKey() string {
	blob := append(wstr([]byte("ssh-ed25519")), wstr(bytes.Repeat([]byte{7}, 32))...)
	return "ssh-ed25519 " + base64.StdEncoding.EncodeToString(blob)
}

// rsaKey is an ssh-rsa key line whose modulus has the given number of bits.
func rsaKey(bits int) string {
	n := new(big.Int).Lsh(big.NewInt(1), uint(bits-1))
	mod := append([]byte{0}, n.Bytes()...) // mpint: a leading zero keeps it positive
	blob := append(wstr([]byte("ssh-rsa")), wstr([]byte{1, 0, 1})...)
	blob = append(blob, wstr(mod)...)
	return "ssh-rsa " + base64.StdEncoding.EncodeToString(blob)
}

// privateKeyFile writes an OpenSSH private key container with the given cipher name (the key
// material itself is not real) and returns its path, mode 0600.
func privateKeyFile(t *testing.T, cipher string) string {
	t.Helper()
	body := append([]byte("openssh-key-v1\x00"), wstr([]byte(cipher))...)
	body = append(body, wstr([]byte("none"))...)
	body = append(body, wstr(nil)...)
	body = append(body, 0, 0, 0, 1)
	body = append(body, wstr([]byte("pub"))...)
	body = append(body, wstr([]byte("priv"))...)
	enc := base64.StdEncoding.EncodeToString(body)
	p := filepath.Join(t.TempDir(), "id_ed25519")
	if err := os.WriteFile(p, []byte("-----BEGIN OPENSSH PRIVATE KEY-----\n"+enc+"\n-----END OPENSSH PRIVATE KEY-----\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}
