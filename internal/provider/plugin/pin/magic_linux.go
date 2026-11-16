//go:build linux

package pin

// notBinaryReason is the Check reason for a file that is not this OS's executable format.
const notBinaryReason = "not an ELF file"

// isExecutableMagic: Linux pins ELF only.
func isExecutableMagic(m [4]byte) bool { return string(m[:]) == "\x7fELF" }

// quarantined is a darwin concept; Linux has no such attribute.
func quarantined(int, string) error { return nil }
