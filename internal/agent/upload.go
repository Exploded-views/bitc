//go:build linux

package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"syscall"
	"time"

	"github.com/Exploded-views/bitc/internal/exit"
	"golang.org/x/sys/unix"
)

// The ssh upload transport, root side: the job's commit arrives as a git bundle on the gate
// session's stdin, right after the boot payload frame, and is written to
// slots/<n>/work/.bitc-upload/code.bundle. Stage 2 (upload_stage2.go) turns it into work/<job>.

const (
	defaultMaxBundle = 1 << 30
	maxMaxBundle     = 8 << 30
	uploadDirName    = ".bitc-upload"
	uploadBundleName = "code.bundle"
	uploadIdle       = 30 * time.Second
	uploadOverall    = 10 * time.Minute
	uploadSpare      = 1 << 30 // free space kept on top of what the bundle and its checkout need
)

var codeSHA256RE = regexp.MustCompile(`^[0-9a-f]{64}$`)

// UploadEnv holds the seams of the upload paths (root side and stage 2). The zero value is the
// real host.
type UploadEnv struct {
	// Statfs reports the bytes available to the agent and the free inodes under path.
	Statfs func(path string) (avail, filesFree uint64, err error)
	// Now is the clock the overall deadline is computed from.
	Now func() time.Time
	// Idle and Overall override the 30 s read idle timeout and the 10 min overall limit.
	Idle, Overall time.Duration
	// Raced runs between mkdirat and openat of the upload dir (tests swap the dir there).
	Raced func()
	// Fchown replaces fchown(2) on the upload dir and bundle.
	Fchown func(fd, uid, gid int) error
	// OwnerUID is the uid that must own the new dir and that it is chowned to (0 on a host).
	OwnerUID int
}

func (e UploadEnv) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

func (e UploadEnv) statfs(path string) (avail, filesFree uint64, err error) {
	if e.Statfs != nil {
		return e.Statfs(path)
	}
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, 0, err
	}
	return st.Bavail * uint64(st.Bsize), st.Ffree, nil //nolint:gosec // Bsize is positive
}

func (e UploadEnv) fchown(fd, uid, gid int) error {
	if e.Fchown != nil {
		return e.Fchown(fd, uid, gid)
	}
	return unix.Fchown(fd, uid, gid)
}

// uploadRefusal is a refusal the controller is told about (exit 65); any other error is ours (69).
type uploadRefusal string

func (e uploadRefusal) Error() string { return string(e) }

// BundleCap is the largest bundle this host takes.
func (c HostConf) BundleCap() int64 {
	if c.MaxBundle > 0 {
		return c.MaxBundle
	}
	return defaultMaxBundle
}

// validateCode checks the payload's code field with the frame only, before any bundle byte is read.
func validateCode(c *CodeUpload, job string, max int64) error {
	switch {
	case c.Kind != "upload":
		return uploadRefusal("bad code kind")
	case c.Len < 1 || c.Len > max:
		return uploadRefusal("bad code length")
	case !codeSHA256RE.MatchString(c.SHA256):
		return uploadRefusal("bad code sha256")
	case c.Ref != "refs/bitc/upload/"+job:
		return uploadRefusal("bad code ref")
	}
	return nil
}

// acquireUploadLock takes the host-wide upload lock, waiting until the given time.
func acquireUploadLock(path string, until time.Time) (func(), error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return nil, err
	}
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() { f.Close() }, nil // closing drops the flock
		}
		if err != syscall.EWOULDBLOCK || time.Now().After(until) {
			f.Close()
			return nil, uploadRefusal("host busy with another upload")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// removeAt removes name below dirfd without ever following a link: a file or link is unlinked, a
// directory is emptied through its own fd and then removed.
func removeAt(dirfd int, name string) error {
	err := unix.Unlinkat(dirfd, name, 0)
	if err == nil || errors.Is(err, unix.ENOENT) {
		return nil
	}
	err = unix.Unlinkat(dirfd, name, unix.AT_REMOVEDIR)
	if err == nil || errors.Is(err, unix.ENOENT) {
		return nil
	}
	if !errors.Is(err, unix.ENOTEMPTY) {
		return err
	}
	fd, err := unix.Openat(dirfd, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	d := os.NewFile(uintptr(fd), name)
	names, err := d.Readdirnames(-1)
	if err == nil {
		for _, n := range names {
			if err = removeAt(fd, n); err != nil {
				break
			}
		}
	}
	d.Close()
	if err != nil {
		return err
	}
	return unix.Unlinkat(dirfd, name, unix.AT_REMOVEDIR)
}

// removeUploadDir removes work/.bitc-upload through a dirfd on work (itself opened without
// following a link). A missing work or upload dir is fine.
func removeUploadDir(work string) error {
	fd, err := unix.Open(work, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	return removeAt(fd, uploadDirName)
}

// copyExactly copies exactly n bytes of src to dst and returns their sha256. Each read has the
// idle timeout; the whole copy has to end by limit. A short stream or a timeout is ErrTruncated.
// It never reads more than n bytes from src.
func (e UploadEnv) copyExactly(dst io.Writer, src io.Reader, n int64, limit time.Time) (string, error) {
	idle := e.Idle
	if idle <= 0 {
		idle = uploadIdle
	}
	type chunk struct {
		b   []byte
		err error
	}
	ch := make(chan chunk)
	stop := make(chan struct{})
	defer close(stop)
	exit.Go(func() {
		lr := io.LimitReader(src, n)
		for {
			buf := make([]byte, 64<<10)
			k, err := lr.Read(buf)
			select {
			case ch <- chunk{buf[:k], err}:
			case <-stop:
				return
			}
			if err != nil {
				return
			}
		}
	})
	h := sha256.New()
	overall := time.NewTimer(limit.Sub(e.now()))
	defer overall.Stop()
	idleT := time.NewTimer(idle)
	defer idleT.Stop()
	var got int64
	for got < n {
		select {
		case c := <-ch:
			if len(c.b) > 0 {
				if _, err := dst.Write(c.b); err != nil {
					return "", err
				}
				h.Write(c.b)
				got += int64(len(c.b))
			}
			if got < n && c.err != nil {
				return "", ErrTruncated
			}
			if !idleT.Stop() {
				select {
				case <-idleT.C:
				default:
				}
			}
			idleT.Reset(idle)
		case <-idleT.C:
			return "", ErrTruncated
		case <-overall.C:
			return "", ErrTruncated
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ReceiveUpload is the root side after the handoff: under the host-wide upload lock it checks the
// space, kills the slot, creates work/.bitc-upload and streams exactly c.Len bytes of src into
// code.bundle, all fd-relative. Whatever goes wrong, the upload dir is gone when it returns.
func (h *SlotHost) ReceiveUpload(env UploadEnv, slots []int, c CodeUpload, deadline time.Time, src io.Reader) (err error) {
	slot := slots[0]
	limit := env.now().Add(env.Overall)
	if env.Overall <= 0 {
		limit = env.now().Add(uploadOverall)
	}
	if deadline.Before(limit) {
		limit = deadline
	}
	if err := os.MkdirAll(h.StateDir(), 0o700); err != nil {
		return err
	}
	unlock, err := acquireUploadLock(filepath.Join(h.StateDir(), "upload.lock"), limit)
	if err != nil {
		return err
	}
	defer unlock()
	work := filepath.Join(h.slotDir(slot), "work")
	avail, _, err := env.statfs(work)
	if err != nil {
		return fmt.Errorf("statfs %s: %w", work, err)
	}
	if avail < 3*uint64(c.Len)+uploadSpare { //nolint:gosec // Len is within 1..8 GiB
		return uploadRefusal("insufficient disk for bundle")
	}
	uid, gid, err := h.Sys.UID(slot)
	if err != nil {
		return err
	}
	if err := h.Sys.KillSlot(slot, uid); err != nil {
		return fmt.Errorf("slot %d: kill: %w", slot, err)
	}
	wfd, err := unix.Open(work, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open %s: %w", work, err)
	}
	defer unix.Close(wfd)
	defer func() {
		if err != nil {
			if rerr := removeAt(wfd, uploadDirName); rerr != nil {
				err = fmt.Errorf("%w (and the upload dir is still there: %w)", err, rerr)
			}
		}
	}()
	if err := removeAt(wfd, uploadDirName); err != nil {
		return fmt.Errorf("remove the stale upload dir: %w", err)
	}
	if err := unix.Mkdirat(wfd, uploadDirName, 0o750); err != nil {
		return fmt.Errorf("mkdirat: %w", err)
	}
	var made unix.Stat_t
	if err := unix.Fstatat(wfd, uploadDirName, &made, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return uploadRefusal("upload dir raced")
	}
	if env.Raced != nil {
		env.Raced()
	}
	dfd, err := unix.Openat(wfd, uploadDirName, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return uploadRefusal("upload dir raced")
	}
	defer unix.Close(dfd)
	var got unix.Stat_t
	if err := unix.Fstat(dfd, &got); err != nil || int(got.Uid) != env.OwnerUID || got.Dev != made.Dev || got.Ino != made.Ino || got.Mode&unix.S_IFMT != unix.S_IFDIR {
		return uploadRefusal("upload dir raced")
	}
	if err := env.fchown(dfd, env.OwnerUID, gid); err != nil {
		return fmt.Errorf("fchown the upload dir: %w", err)
	}
	if err := unix.Fchmod(dfd, 0o750); err != nil {
		return fmt.Errorf("fchmod the upload dir: %w", err)
	}
	ffd, err := unix.Openat(dfd, uploadBundleName, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o640)
	if err != nil {
		return fmt.Errorf("create the bundle: %w", err)
	}
	f := os.NewFile(uintptr(ffd), uploadBundleName)
	defer f.Close()
	if err := env.fchown(ffd, env.OwnerUID, gid); err != nil {
		return fmt.Errorf("fchown the bundle: %w", err)
	}
	if err := unix.Fchmod(ffd, 0o640); err != nil {
		return fmt.Errorf("fchmod the bundle: %w", err)
	}
	sum, err := env.copyExactly(f, src, c.Len, limit)
	switch {
	case errors.Is(err, ErrTruncated):
		return uploadRefusal("truncated payload")
	case err != nil:
		return fmt.Errorf("write the bundle: %w", err)
	case sum != c.SHA256:
		return uploadRefusal("bundle hash mismatch")
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("write the bundle: %w", err)
	}
	return nil
}

// removeUpload removes slot i's stale work/.bitc-upload (teardown step of C.5 step 8, after the
// slot is killed).
func (h *SlotHost) removeUpload(i int) error {
	return removeUploadDir(filepath.Join(h.slotDir(i), "work"))
}
