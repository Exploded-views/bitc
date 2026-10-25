package state

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Exploded-views/bitc/internal/exit"
)

func TestOpenCreatesPrivateDirAndSchema(t *testing.T) {
	root := filepath.Join(t.TempDir(), "a", "state")
	l, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(root)
	if st.Mode().Perm() != 0o700 {
		t.Fatalf("mode %o", st.Mode().Perm())
	}
	b, _ := os.ReadFile(l.Schema())
	if strings.TrimSpace(string(b)) != "1" {
		t.Fatalf("SCHEMA %q", b)
	}
	if l.SlotLock("db", 2) != filepath.Join(root, "pools", "db", "slot.2.lock") {
		t.Fatal(l.SlotLock("db", 2))
	}
	if l.NestLock("db", 1, 2) != filepath.Join(root, "pools", "db", "slot.1.nest.2.lock") {
		t.Fatal(l.NestLock("db", 1, 2))
	}
	if l.GroupUnitLock("bigmem", 1) != filepath.Join(root, "groups", "bigmem", "unit.1.lock") {
		t.Fatal(l.GroupUnitLock("bigmem", 1))
	}
}

func TestOpenRefusesGroupReadableDir(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := Open(root)
	if exit.Code(err) != exit.Config || !strings.Contains(err.Error(), "group- or world-accessible") {
		t.Fatalf("got %v", err)
	}
}

func TestOpenRefusesNewerSchema(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "SCHEMA"), []byte("2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Open(root)
	if exit.Code(err) != exit.Config || !strings.Contains(err.Error(), "schema 2") {
		t.Fatalf("got %v", err)
	}
}

func TestOpenRefusesRelativeRoot(t *testing.T) {
	if _, err := Open("rel/state"); exit.Code(err) != exit.Config {
		t.Fatalf("got %v", err)
	}
}
