//go:build darwin

package pin

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// notBinaryReason is the Check reason for a file that is not this OS's executable format.
const notBinaryReason = "not a Mach-O file"

// isExecutableMagic accepts thin Mach-O (32 and 64 bit, either byte order) and fat (universal)
// images, whose magics are 0xcafebabe and 0xcafebabf in either byte order.
func isExecutableMagic(m [4]byte) bool {
	switch m {
	case [4]byte{0xcf, 0xfa, 0xed, 0xfe}, [4]byte{0xce, 0xfa, 0xed, 0xfe},
		[4]byte{0xfe, 0xed, 0xfa, 0xcf}, [4]byte{0xfe, 0xed, 0xfa, 0xce},
		[4]byte{0xca, 0xfe, 0xba, 0xbe}, [4]byte{0xca, 0xfe, 0xba, 0xbf},
		[4]byte{0xbe, 0xba, 0xfe, 0xca}, [4]byte{0xbf, 0xba, 0xfe, 0xca}:
		return true
	}
	return false
}

// quarantined refuses a file that carries com.apple.quarantine: Gatekeeper would stop it, or ask
// the user, when it runs. The message says how to clear the attribute.
func quarantined(fd int, path string) error {
	if n, err := unix.Fgetxattr(fd, "com.apple.quarantine", nil); err == nil && n >= 0 {
		return &Error{KindReason, fmt.Sprintf("carries com.apple.quarantine (downloaded, not yet approved); clear it with: xattr -d com.apple.quarantine %s", path)}
	}
	return nil
}
