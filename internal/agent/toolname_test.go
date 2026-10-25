package agent

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

func TestInstallToolRejectsEscapingNamesBeforeAnyFilesystemCall(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "tools")
	victim := filepath.Join(base, "escape")
	if err := os.MkdirAll(victim, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(victim, "keep"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"../escape", "..", ".", "a/b", ""} {
		_, err := installTool(http.DefaultClient, root, Tool{Name: name, URL: "https://127.0.0.1:1/x.tgz", SHA256: "00"})
		var se *stepError
		if !errors.As(err, &se) || !se.Repo {
			t.Errorf("name %q: want repo-class stepError, got %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(victim, "keep")); err != nil {
		t.Errorf("file outside toolsRoot touched: %v", err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Errorf("toolsRoot was created before validation: %v", err)
	}
}
