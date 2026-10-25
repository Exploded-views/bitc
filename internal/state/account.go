package state

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"

	"github.com/Exploded-views/bitc/internal/config"
	"github.com/Exploded-views/bitc/internal/exit"
)

// AccountDir is <passwd home>/.local/state/bitc/account (spec 2, 9). No env, flag or key moves
// it, so a private BITC_DIR or BITC_CONFIG cannot multiply the cap; only [test] accounting_dir.
func AccountDir(e *config.Effective) (string, error) {
	if e.Test != nil && e.Test.AccountingDir != "" {
		return e.Test.AccountingDir, MkdirPrivate(e.Test.AccountingDir)
	}
	u, err := user.Current()
	if err != nil || u.HomeDir == "" {
		return "", exit.Unavailablef("cannot find your home directory in the passwd database: %v", err)
	}
	dir := filepath.Join(u.HomeDir, ".local", "state", "bitc", "account")
	return dir, MkdirPrivate(dir)
}

// InstallSecret returns the 32-byte install secret, creating it once. The file appears
// atomically (a full temp file linked into place), so no reader sees a partial secret; a file
// of any other length is an error rather than a key.
func InstallSecret(dir string) ([]byte, error) {
	if err := MkdirPrivate(dir); err != nil {
		return nil, err
	}
	p := filepath.Join(dir, "install-secret")
	b, err := readSecret(p)
	if err == nil || !errors.Is(err, fs.ErrNotExist) {
		return b, err
	}
	b = make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	tmp, err := os.CreateTemp(dir, ".install-secret-*") // mode 0600
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp.Name())
	_, werr := tmp.Write(b)
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return nil, werr
	}
	if err := os.Link(tmp.Name(), p); err != nil && !errors.Is(err, fs.ErrExist) {
		return nil, err
	}
	return readSecret(p) // the winner's secret, ours or another process's
}

func readSecret(p string) ([]byte, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	if len(b) != 32 {
		return nil, fmt.Errorf("%s holds %d bytes, not 32; remove it to make a new install secret", p, len(b))
	}
	return b, nil
}

// Install16 is 16 hex of sha256(secret).
func Install16(secret []byte) string {
	s := sha256.Sum256(secret)
	return hex.EncodeToString(s[:])[:16]
}
