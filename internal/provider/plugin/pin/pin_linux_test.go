//go:build linux

// Linux-only: the capability xattr is a Linux file capability, and Linux pins ELF only.

package pin

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestCheckRefusesFileCapabilities(t *testing.T) {
	old := fgetxattr
	defer func() { fgetxattr = old }()
	fgetxattr = func(fd int, attr string, dest []byte) (int, error) {
		if attr == "security.capability" {
			return 20, nil
		}
		return 0, unix.ENODATA
	}
	p := execCopy(t, t.TempDir(), "bitc-provider-x", 0o755)
	wantKind(t, Check(p), KindPrivs, "")
}

// Linux requires \x7fELF: a Mach-O or fat image is not an executable here.
func TestCheckRefusesMachOAndFatOnLinux(t *testing.T) {
	dir := t.TempDir()
	for name, magic := range map[string][]byte{"macho64": {0xcf, 0xfa, 0xed, 0xfe}, "macho32": {0xce, 0xfa, 0xed, 0xfe}, "fat": {0xca, 0xfe, 0xba, 0xbe}} {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, append(magic, make([]byte, 60)...), 0o755); err != nil {
			t.Fatal(err)
		}
		wantKind(t, Check(p), KindReason, "not an ELF file")
	}
}
