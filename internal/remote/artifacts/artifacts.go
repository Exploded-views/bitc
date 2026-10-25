// Package artifacts brings a remote job's declared paths back into the work tree (spec 8.4).
package artifacts

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/Exploded-views/bitc/internal/config"
	"github.com/Exploded-views/bitc/internal/lock"
)

const maxAttempts = 5

var errOverCap = errors.New("the archive is over max_compressed")

// capWriter refuses bytes beyond the cap, so one flooding attempt cannot fill the disk.
type capWriter struct {
	w      io.Writer
	n, max int64
}

func (c *capWriter) Write(p []byte) (int, error) {
	if room := c.max - c.n; int64(len(p)) > room {
		if room > 0 {
			n, _ := c.w.Write(p[:room])
			c.n += int64(n)
		}
		return int(max(room, 0)), errOverCap
	}
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// Download fetches with resume: after a failure it continues from the bytes it has, at most
// five attempts, and never writes more than max bytes.
func Download(ctx context.Context, fetch func(off int64, w io.Writer) error, dst string, max int64) error {
	f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	var last error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		off, err := f.Seek(0, io.SeekEnd)
		if err != nil {
			return err
		}
		cw := &capWriter{w: f, n: off, max: max}
		last = fetch(off, cw)
		if last == nil {
			return nil
		}
		if errors.Is(last, errOverCap) {
			return fmt.Errorf("%w (%d bytes)", errOverCap, max)
		}
		if ctx != nil && ctx.Err() != nil {
			return ctx.Err()
		}
	}
	return last
}

// cleanDeclared validates a declared path from config: relative, no "..".
func cleanDeclared(p string) (string, error) {
	c := path.Clean(strings.TrimSuffix(p, "/"))
	if c == "." || c == ".." || strings.HasPrefix(c, "../") || strings.HasPrefix(c, "/") {
		return "", fmt.Errorf("artifact path %q is not inside the work tree", p)
	}
	return c, nil
}

// match reports how a cleaned entry name relates to the declared paths: under (the path or
// inside it), parent (a directory leading to one), or neither.
func match(name string, declared []string) (under, parent bool) {
	for _, p := range declared {
		switch {
		case name == p || strings.HasPrefix(name, p+"/"):
			return true, false
		case strings.HasPrefix(p, name+"/"):
			parent = true
		}
	}
	return false, parent
}

func declaredList(declared []config.Artifact) ([]string, error) {
	var out []string
	for _, a := range declared {
		c, err := cleanDeclared(a.Path)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

// Validate checks the whole archive before anything is touched (spec 8.4).
func Validate(archive string, declared []config.Artifact, lim config.ArtifactLimits) error {
	decl, err := declaredList(declared)
	if err != nil {
		return err
	}
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	if st, err := f.Stat(); err == nil && st.Size() > int64(lim.MaxCompressed) {
		return fmt.Errorf("the archive is over max_compressed (%d bytes)", int64(lim.MaxCompressed))
	}
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("not a tar.gz: %w", err)
	}
	tr := tar.NewReader(gz)
	var total int64
	n := 0
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		n++
		if n > lim.MaxEntries {
			return fmt.Errorf("more than %d entries", lim.MaxEntries)
		}
		if strings.HasPrefix(h.Name, "/") {
			return fmt.Errorf("absolute entry %q", h.Name)
		}
		if strings.Contains("/"+h.Name+"/", "/../") {
			return fmt.Errorf("entry %q contains a .. element", h.Name)
		}
		if h.Typeflag != tar.TypeReg && h.Typeflag != tar.TypeDir {
			return fmt.Errorf("entry %q is not a regular file or dir", h.Name)
		}
		under, parent := match(path.Clean(h.Name), decl)
		if !under && (!parent || h.Typeflag != tar.TypeDir) {
			return fmt.Errorf("entry %q is outside the declared paths", h.Name)
		}
		if h.Size < 0 {
			return fmt.Errorf("entry %q has a negative size", h.Name)
		}
		total += h.Size
		if total > int64(lim.MaxUnpacked) {
			return fmt.Errorf("unpacked size over max_unpacked (%d bytes)", int64(lim.MaxUnpacked))
		}
	}
}

// symlinkComponent returns the first symlink on the way to rel inside wt, or "".
func symlinkComponent(wt, rel string) string {
	cur := wt
	for _, part := range strings.Split(filepath.Clean(rel), "/") {
		cur = filepath.Join(cur, part)
		st, err := os.Lstat(cur)
		if err != nil {
			return ""
		}
		if st.Mode()&os.ModeSymlink != 0 {
			r, _ := filepath.Rel(wt, cur)
			return r
		}
	}
	return ""
}

func symlinkErr(p, link string) error {
	return fmt.Errorf("artifact path '%s' passes through the symbolic link '%s'", p, link)
}

// Apply stages the archive and swaps declared paths in under the work tree's artifacts lock.
func Apply(acctDir, wt, job, archive string, declared []config.Artifact) error {
	decl, err := declaredList(declared)
	if err != nil {
		return err
	}
	if job == "" || job != filepath.Base(job) || job == "." || job == ".." {
		return fmt.Errorf("invalid job id %q", job)
	}
	// The whole archive is checked before anything is staged or removed, even when a caller
	// skipped Validate (names, types, declared paths; the size limits are Validate's job).
	open := config.ArtifactLimits{MaxCompressed: math.MaxInt64, MaxUnpacked: math.MaxInt64, MaxEntries: math.MaxInt}
	if err := Validate(archive, declared, open); err != nil {
		return err
	}
	bitcDir := filepath.Join(wt, ".bitc")
	if link := symlinkComponent(wt, ".bitc"); link != "" {
		return symlinkErr(".bitc", link)
	}
	if err := os.MkdirAll(filepath.Join(bitcDir, "staging"), 0o700); err != nil {
		return err
	}
	if link := symlinkComponent(wt, ".bitc/staging"); link != "" {
		return symlinkErr(".bitc/staging", link)
	}
	if err := writeGitignore(filepath.Join(bitcDir, ".gitignore")); err != nil {
		return err
	}
	staging := filepath.Join(bitcDir, "staging", job)
	_ = os.RemoveAll(staging)
	defer os.RemoveAll(staging) // removed on every exit path
	if err := extract(archive, staging); err != nil {
		return err
	}
	sum := sha256.Sum256([]byte(wt))
	l, err := lock.Open(filepath.Join(acctDir, "locks", "artifacts."+hex.EncodeToString(sum[:])[:16]+".lock"))
	if err != nil {
		return err
	}
	defer l.Close()
	if err := l.LockWait(context.Background(), 10*time.Minute); err != nil {
		return err
	}
	defer l.Unlock()
	for _, p := range decl {
		if link := symlinkComponent(wt, p); link != "" {
			return symlinkErr(p, link)
		}
	}
	for i, a := range declared {
		p := decl[i]
		dst := filepath.Join(wt, p)
		src := filepath.Join(staging, p)
		if a.Replace {
			if err := os.RemoveAll(dst); err != nil {
				return err
			}
		}
		if _, err := os.Lstat(src); err != nil {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
			return err
		}
		if link := symlinkComponent(wt, filepath.Dir(p)); link != "" && filepath.Dir(p) != "." {
			return symlinkErr(p, link)
		}
		if a.Replace {
			if err := os.Rename(src, dst); err != nil {
				return err
			}
			continue
		}
		if err := merge(wt, p, src); err != nil {
			return err
		}
	}
	return nil
}

// extract writes the validated archive with O_NOFOLLOW at every level.
func extract(archive, dest string) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	if err := os.MkdirAll(dest, 0o700); err != nil {
		return err
	}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		target := filepath.Join(dest, filepath.Clean("/"+h.Name))
		if h.Typeflag == tar.TypeDir {
			if err := os.MkdirAll(target, 0o750); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
			return err
		}
		fd, err := unix.Open(target, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, uint32(h.Mode&0o755)|0o600)
		if err != nil {
			return err
		}
		out := os.NewFile(uintptr(fd), target)
		_, err = io.Copy(out, tr)
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
	}
}

// merge (replace = false) moves staged src over wt/rel, archive files overwriting. Every
// destination is checked for a symlink component first, so nothing is written through one.
func merge(wt, rel, src string) error {
	return filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		r, _ := filepath.Rel(src, p)
		sub := filepath.Join(rel, r)
		if link := symlinkComponent(wt, sub); link != "" {
			return symlinkErr(sub, link)
		}
		target := filepath.Join(wt, sub)
		if info.IsDir() {
			return os.MkdirAll(target, 0o750)
		}
		if st, err := os.Lstat(target); err == nil && st.IsDir() {
			return fmt.Errorf("artifact file %q would replace a directory", sub)
		}
		_ = os.Remove(target)
		return os.Rename(p, target) //nolint:gosec // p is in bitc's own staging dir, entries were extracted O_NOFOLLOW, and every wt component was symlink-checked under the artifacts lock
	})
}

// writeGitignore never follows a link: a tracked .bitc/.gitignore symlink must not redirect the write.
func writeGitignore(p string) error {
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0o600)
	if err != nil {
		if errors.Is(err, unix.ELOOP) {
			return symlinkErr(".bitc/.gitignore", p)
		}
		return err
	}
	defer f.Close()
	if st, err := f.Stat(); err != nil || !st.Mode().IsRegular() {
		return fmt.Errorf(".bitc/.gitignore is not a regular file")
	}
	_, err = f.WriteString("*\n")
	return err
}
