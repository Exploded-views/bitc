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

	"golang.org/x/sys/unix"

	"github.com/Exploded-views/bitc/internal/config"
	"github.com/Exploded-views/bitc/internal/exit"
)

// accountPath is the accounting dir's path, without creating it. config.accountDir (experimental.go)
// computes the same path for its read-only leftovers scan: the two must agree.
func accountPath(e *config.Effective) (string, error) {
	if e.Test != nil && e.Test.AccountingDir != "" {
		return e.Test.AccountingDir, nil
	}
	u, err := user.Current()
	if err != nil || u.HomeDir == "" {
		return "", exit.Unavailablef("cannot find your home directory in the passwd database: %v", err)
	}
	return filepath.Join(u.HomeDir, ".local", "state", "bitc", "account"), nil
}

// AccountDir is <passwd home>/.local/state/bitc/account (spec 2, 9). No env, flag or key moves
// it, so a private BITC_DIR or BITC_CONFIG cannot multiply the cap; only [test] accounting_dir.
// Like the state dir it is refused (78) on a network filesystem: the launch lock, the ledger and
// the reservations that hold the spend cap need a reliable flock.
func AccountDir(e *config.Effective) (string, error) {
	dir, err := accountPath(e)
	if err != nil {
		return "", err
	}
	if err := MkdirPrivate(dir); err != nil {
		return dir, err
	}
	return dir, checkFS("accounting dir", dir)
}

// AccountDirOnNetworkFS is AccountDir for sweep, which still has work to do on a network
// filesystem: it returns the dir and, instead of refusing, the name of the network filesystem
// ("" for a local one). The caller must then treat the dir as read-only and its locks as untrusted;
// a dir that does not exist yet is not created there.
func AccountDirOnNetworkFS(e *config.Effective) (dir, netfs string, err error) {
	dir, err = accountPath(e)
	if err != nil {
		return "", "", err
	}
	var sfs unix.Statfs_t
	if statfs(nearestExisting(dir), &sfs) == nil {
		netfs = networkFS(&sfs)
	}
	if _, serr := os.Stat(dir); netfs != "" && serr != nil {
		return dir, netfs, nil // missing on a network filesystem: creating it would be a write
	}
	return dir, netfs, MkdirPrivate(dir) // an existing dir is only checked
}

// ReadInstallSecret is InstallSecret without creating anything: a missing secret is an error.
func ReadInstallSecret(dir string) ([]byte, error) {
	return readSecret(filepath.Join(dir, "install-secret"))
}

// nearestExisting is path, or for a path not yet created its nearest existing parent.
func nearestExisting(path string) string {
	at := path
	for {
		if _, err := os.Stat(at); err == nil {
			return at
		}
		parent := filepath.Dir(at)
		if parent == at {
			return at
		}
		at = parent
	}
}

// CheckAccountDir is AccountDir's filesystem refusal without creating anything. Its callers are
// config check and status. A missing dir is judged by its nearest existing parent, so a fresh
// network-filesystem home is refused before the first remote command creates the dir there.
func CheckAccountDir(e *config.Effective) error {
	dir, err := accountPath(e)
	if err != nil {
		return nil // no home: the remote commands say so; config check never needed one
	}
	return checkFSAt("accounting dir", dir, nearestExisting(dir))
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
