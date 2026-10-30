// Package state owns bitc's on-disk layout (spec 9).
package state

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/Exploded-views/bitc/internal/exit"
)

// SchemaVersion is the state-dir schema this binary writes and understands.
const SchemaVersion = 1

// statfs is unix.Statfs; tests stand in a network filesystem.
var statfs = unix.Statfs

// Layout names every path in a state dir. Root is absolute.
type Layout struct{ Root string }

func (l Layout) Schema() string          { return filepath.Join(l.Root, "SCHEMA") }
func (l Layout) AdmitLock() string       { return filepath.Join(l.Root, "admit.lock") }
func (l Layout) Reservations() string    { return filepath.Join(l.Root, "admit", "reservations.json") }
func (l Layout) PoolDir(p string) string { return filepath.Join(l.Root, "pools", p) }
func (l Layout) Tickets(p string) string { return filepath.Join(l.PoolDir(p), "tickets") }
func (l Layout) SlotLock(p string, i int) string {
	return filepath.Join(l.PoolDir(p), fmt.Sprintf("slot.%d.lock", i))
}
func (l Layout) NestLock(p string, i, depth int) string {
	return filepath.Join(l.PoolDir(p), fmt.Sprintf("slot.%d.nest.%d.lock", i, depth))
}
func (l Layout) GroupUnitLock(g string, j int) string {
	return filepath.Join(l.Root, "groups", g, fmt.Sprintf("unit.%d.lock", j))
}
func (l Layout) History() string                { return filepath.Join(l.Root, "history.jsonl") }
func (l Layout) JobsDir() string                { return filepath.Join(l.Root, "jobs") }
func (l Layout) JobDir(wt8, name string) string { return filepath.Join(l.Root, "jobs", wt8, name) }

// Open creates the state dir (0700) or checks an existing one, then checks SCHEMA.
// Every refusal is 78 (spec 9).
func Open(root string) (Layout, error) {
	if !filepath.IsAbs(root) {
		return Layout{}, exit.Configf("state dir %q is not an absolute path", root)
	}
	if err := MkdirPrivate(root); err != nil {
		return Layout{}, err
	}
	if err := checkFS(root); err != nil {
		return Layout{}, err
	}
	l := Layout{Root: root}
	b, err := os.ReadFile(l.Schema())
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if err := WriteFileAtomic(l.Schema(), []byte(strconv.Itoa(SchemaVersion)+"\n")); err != nil {
			return Layout{}, exit.Configf("state dir %s: %v", root, err)
		}
	case err != nil:
		return Layout{}, exit.Configf("state dir %s: %v", root, err)
	default:
		if err := checkSchema(root, b); err != nil {
			return Layout{}, err
		}
	}
	return l, nil
}

// Check runs Open's refusals on an existing state dir without creating or writing
// anything; a missing dir passes, since the first command creates it 0700.
func Check(root string) error {
	if !filepath.IsAbs(root) {
		return exit.Configf("state dir %q is not an absolute path", root)
	}
	var st unix.Stat_t
	if err := unix.Stat(root, &st); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			if l := danglingLink(root); l != "" { // MkdirAll fails on a link to nowhere
				return exit.Configf("cannot create %s: dangling symlink %s", root, l)
			}
			return nil
		}
		return exit.Configf("%s: %v", root, err)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR {
		return exit.Configf("state dir %s is not a directory", root)
	}
	if err := checkOwnerMode(root, &st); err != nil {
		return err
	}
	if err := checkFS(root); err != nil {
		return err
	}
	b, err := os.ReadFile(Layout{Root: root}.Schema())
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return exit.Configf("state dir %s: %v", root, err)
	}
	return checkSchema(root, b)
}

// danglingLink returns the path component of the missing root that exists as a symlink to
// nowhere: the one directly under its nearest existing ancestor. It is "" if there is none.
func danglingLink(root string) string {
	for child := root; ; {
		parent := filepath.Dir(child)
		if parent == child {
			return ""
		}
		var st unix.Stat_t
		if unix.Stat(parent, &st) == nil {
			if _, err := os.Lstat(child); err == nil {
				return child
			}
			return ""
		}
		child = parent
	}
}

// checkFS refuses a network filesystem: flock needs a local one.
func checkFS(root string) error {
	var sfs unix.Statfs_t
	if err := statfs(root, &sfs); err == nil {
		if fs := networkFS(&sfs); fs != "" {
			return exit.Configf("state dir %s is on %s; bitc needs a local filesystem for flock", root, fs)
		}
	}
	return nil
}

// checkSchema refuses a SCHEMA body this bitc cannot read.
func checkSchema(root string, b []byte) error {
	v, perr := strconv.Atoi(strings.TrimSpace(string(b)))
	if perr != nil || v > SchemaVersion {
		return exit.Configf("state dir %s has schema %s; this bitc understands schema %d",
			root, strings.TrimSpace(string(b)), SchemaVersion)
	}
	return nil
}

// MkdirPrivate creates path and missing parents with 0700 and checks path itself:
// owned by the user, not group- or world-accessible.
func MkdirPrivate(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return exit.Configf("cannot create %s: %v", path, err)
	}
	var st unix.Stat_t
	if err := unix.Stat(path, &st); err != nil {
		return exit.Configf("%s: %v", path, err)
	}
	return checkOwnerMode(path, &st)
}

// checkOwnerMode refuses a dir another user owns or group or world can reach.
func checkOwnerMode(path string, st *unix.Stat_t) error {
	if st.Uid != uint32(os.Getuid()) {
		return exit.Configf("%s is not owned by you (uid %d)", path, st.Uid)
	}
	if st.Mode&0o077 != 0 {
		return exit.Configf("%s is group- or world-accessible (mode %o); run chmod 700 %s", path, st.Mode&0o777, path)
	}
	return nil
}

// WriteFileAtomic writes b to path (0600) through a temp file and rename.
func WriteFileAtomic(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
