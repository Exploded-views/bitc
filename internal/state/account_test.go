package state

import (
	"os"
	"path/filepath"
	"testing"
)

func TestInstallSecretIsCreatedOnce(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "account")
	a, err := InstallSecret(dir)
	if err != nil || len(a) != 32 {
		t.Fatal(err)
	}
	b, _ := InstallSecret(dir)
	if string(a) != string(b) {
		t.Fatal("secret changed")
	}
	st, _ := os.Stat(filepath.Join(dir, "install-secret"))
	if st.Mode().Perm() != 0o600 || len(Install16(a)) != 16 {
		t.Fatal(st.Mode())
	}
}

func TestInstallSecretRejectsAShortFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "account")
	if err := MkdirPrivate(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "install-secret"), []byte("short"), 0o600); err != nil {
		t.Fatal(err)
	}
	if b, err := InstallSecret(dir); err == nil {
		t.Fatalf("a %d-byte secret was accepted", len(b))
	}
}
