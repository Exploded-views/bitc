// Package pin checks and opens a pinned provider plugin (providers delta A.2): an absolute path
// and a sha256, a regular ELF file owned by the user or root, no group or world write on it or
// any parent, no setuid or setgid bit, no file capabilities. Open returns the file descriptor
// that was hashed, so the bytes checked are the bytes run.
package pin

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// Kind classifies a Check failure; the host-file message of A.8 is built from it.
type Kind int

const (
	KindReason   Kind = iota // missing, symlink, not a regular file, not executable, not an ELF file
	KindInsecure             // Detail names the path that is writable or owned by another user
	KindPrivs                // setuid, setgid or a security.capability xattr
)

// Error is a failed pin check.
type Error struct {
	Kind   Kind
	Detail string // KindReason: the reason; KindInsecure: the path
}

func (e *Error) Error() string {
	switch e.Kind {
	case KindInsecure:
		return "insecure: " + e.Detail
	case KindPrivs:
		return "insecure: setuid/setgid/capabilities"
	}
	return e.Detail
}

// Changed is a plugin whose bytes no longer hash to the pinned sha256.
type Changed struct{ Path, Want, Got string }

func (c *Changed) Error() string {
	return fmt.Sprintf("plugin %s changed since load (sha256 %s, now %s)", c.Path, Short(c.Want), Short(c.Got))
}

// Short is the first 12 hex characters of a digest.
func Short(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}

// fgetxattr is swapped by tests: setting security.capability needs privilege.
var fgetxattr = unix.Fgetxattr

func hasCapXattr(fd int) bool {
	n, err := fgetxattr(fd, "security.capability", nil)
	return err == nil && n > 0
}

func ownerOK(uid uint32) bool { return uid == uint32(os.Getuid()) || uid == 0 }

// Check is the load-time test of A.2.3. It never executes or reads more than the ELF magic.
func Check(path string) error {
	var lst unix.Stat_t
	if err := unix.Lstat(path, &lst); err != nil {
		return &Error{KindReason, "missing"}
	}
	switch lst.Mode & unix.S_IFMT {
	case unix.S_IFLNK:
		return &Error{KindReason, "symlink"}
	case unix.S_IFREG:
	default:
		return &Error{KindReason, "not a regular file"}
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, unix.ELOOP) {
			return &Error{KindReason, "symlink"}
		}
		return &Error{KindReason, "cannot open"}
	}
	defer unix.Close(fd)
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil || st.Mode&unix.S_IFMT != unix.S_IFREG {
		return &Error{KindReason, "not a regular file"}
	}
	if unix.Access(path, unix.X_OK) != nil {
		return &Error{KindReason, "not executable"}
	}
	var magic [4]byte
	if n, _ := unix.Pread(fd, magic[:], 0); n < 4 || string(magic[:]) != "\x7fELF" {
		return &Error{KindReason, "not an ELF file"}
	}
	if !ownerOK(st.Uid) || st.Mode&0o022 != 0 {
		return &Error{KindInsecure, path}
	}
	if st.Mode&(unix.S_ISUID|unix.S_ISGID) != 0 || hasCapXattr(fd) {
		return &Error{KindPrivs, path}
	}
	for dir := filepath.Dir(path); ; dir = filepath.Dir(dir) {
		var ds unix.Stat_t
		if err := unix.Stat(dir, &ds); err != nil {
			return &Error{KindInsecure, dir}
		}
		// A sticky directory (/tmp) lets nobody rename or remove another user's entry.
		writable := ds.Mode&0o022 != 0 && ds.Mode&unix.S_ISVTX == 0
		if !ownerOK(ds.Uid) || writable {
			return &Error{KindInsecure, dir}
		}
		if dir == "/" || dir == "." {
			break
		}
	}
	return nil
}

// Open is the spawn-time test of A.2.4: it opens path with O_NOFOLLOW|O_CLOEXEC, re-checks the
// mode bits and the capability xattr on the open fd, hashes the bytes and compares them with
// want. The caller execs that same fd.
func Open(path, want string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("plugin %s: %w", path, err)
	}
	f := os.NewFile(uintptr(fd), path)
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil || st.Mode&unix.S_IFMT != unix.S_IFREG {
		f.Close()
		return nil, fmt.Errorf("plugin %s: not a regular file", path)
	}
	if st.Mode&(unix.S_ISUID|unix.S_ISGID) != 0 || hasCapXattr(fd) {
		f.Close()
		return nil, &Error{KindPrivs, path}
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		f.Close()
		return nil, fmt.Errorf("plugin %s: %w", path, err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		f.Close()
		return nil, &Changed{Path: path, Want: want, Got: got}
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// SumFile is the sha256 of a file, lowercase hex.
func SumFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
