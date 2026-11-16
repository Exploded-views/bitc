//go:build darwin

// darwin: pin accepts thin and fat Mach-O (and refuses an ELF), and refuses a quarantined plugin.

package pin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// magicFile is a file of mode 0755 that starts with magic, as Check reads nothing more.
func magicFile(t *testing.T, dir, name string, magic []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, append(append([]byte{}, magic...), make([]byte, 60)...), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCheckAcceptsThinAndFatMachO(t *testing.T) {
	dir := t.TempDir()
	for name, magic := range map[string][]byte{
		"MH_MAGIC_64":  {0xcf, 0xfa, 0xed, 0xfe}, // 0xfeedfacf, little-endian on disk
		"MH_CIGAM_64":  {0xfe, 0xed, 0xfa, 0xcf}, // byte-swapped
		"MH_MAGIC":     {0xce, 0xfa, 0xed, 0xfe}, // 0xfeedface
		"MH_CIGAM":     {0xfe, 0xed, 0xfa, 0xce},
		"FAT_MAGIC":    {0xca, 0xfe, 0xba, 0xbe}, // 0xcafebabe
		"FAT_CIGAM":    {0xbe, 0xba, 0xfe, 0xca},
		"FAT_MAGIC_64": {0xca, 0xfe, 0xba, 0xbf}, // 0xcafebabf
		"FAT_CIGAM_64": {0xbf, 0xba, 0xfe, 0xca},
	} {
		if err := Check(magicFile(t, dir, name, magic)); err != nil {
			t.Errorf("%s: Check: %v", name, err)
		}
	}
}

func TestCheckRefusesAnELFAndAScriptOnDarwin(t *testing.T) {
	dir := t.TempDir()
	wantKind(t, Check(magicFile(t, dir, "elf", []byte("\x7fELF"))), KindReason, "not a Mach-O file")
	wantKind(t, Check(magicFile(t, dir, "script", []byte("#!/b"))), KindReason, "not a Mach-O file")
}

func setQuarantine(t *testing.T, p string) {
	t.Helper()
	if err := unix.Setxattr(p, "com.apple.quarantine", []byte("0083;00000000;Safari;"), 0); err != nil {
		t.Fatalf("cannot set com.apple.quarantine: %v", err)
	}
}

// A quarantined plugin is refused, and the message says how to clear the attribute.
func TestCheckRefusesAQuarantinedPluginAndSaysHowToClearIt(t *testing.T) {
	p := execCopy(t, t.TempDir(), "bitc-provider-x", 0o755)
	setQuarantine(t, p)
	err := Check(p)
	if err == nil || !strings.Contains(err.Error(), "xattr -d com.apple.quarantine") {
		t.Fatalf("got %v, want a refusal that says `xattr -d com.apple.quarantine`", err)
	}
}

func TestOpenRefusesAPluginQuarantinedAfterLoad(t *testing.T) {
	p := execCopy(t, t.TempDir(), "bitc-provider-x", 0o755)
	sum, _ := SumFile(p)
	setQuarantine(t, p)
	f, err := Open(p, sum)
	if err == nil || !strings.Contains(err.Error(), "com.apple.quarantine") {
		if f != nil {
			f.Close()
		}
		t.Fatalf("got %v, want a refusal naming com.apple.quarantine", err)
	}
}
