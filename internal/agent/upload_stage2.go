//go:build linux

package agent

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Exploded-views/bitc/internal/exit"
)

// Stage 2 of the ssh upload transport: the bundle that the root side left in
// WorkRoot/.bitc-upload is verified, fetched, fsck'ed and checked out into work/<job>, every git
// command run as the job user through the same System path as a setup step.

var hexShaRE = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

const (
	codeTimeout = 10 * time.Minute
	blockSize   = 4096
	inodeSpare  = 100_000
)

func (o Options) codeTimeout() time.Duration {
	if o.CodeTimeout > 0 {
		return o.CodeTimeout
	}
	return codeTimeout
}

// stderrTail is how much of a command's stderr the root-side agent keeps: the last 64 KiB.
const stderrTail = 64 << 10

// tailBuf keeps only the last stderrTail bytes written to it, so a chatty command cannot grow the
// root agent's memory.
type tailBuf struct{ b []byte }

func (t *tailBuf) Write(p []byte) (int, error) {
	n := len(p)
	if len(p) >= stderrTail {
		t.b = append(t.b[:0], p[len(p)-stderrTail:]...)
		return n, nil
	}
	t.b = append(t.b, p...)
	if len(t.b) > 2*stderrTail {
		t.b = append(t.b[:0], t.b[len(t.b)-stderrTail:]...)
	}
	return n, nil
}

func (t *tailBuf) String() string {
	if len(t.b) > stderrTail {
		return string(t.b[len(t.b)-stderrTail:])
	}
	return string(t.b)
}

// capBuf keeps the first max bytes written to it and drops the rest.
type capBuf struct {
	b   []byte
	max int
}

func (c *capBuf) Write(p []byte) (int, error) {
	if room := c.max - len(c.b); room > 0 {
		c.b = append(c.b, p[:min(room, len(p))]...)
	}
	return len(p), nil
}

// stopper is implemented by an output writer that can ask for its command to be killed.
type stopper interface{ Stopped() <-chan struct{} }

// runCmd runs argv through the System (as root or the job user) in dir with env, stdout to out,
// and returns the tail of what it wrote to stderr. A timeout, or out asking to stop, kills the
// process group.
func (o Options) runCmd(argv []string, root bool, dir string, env []string, uid, gid int, out io.Writer, timeout time.Duration) (string, error) {
	c := o.System.Cmd(argv, root, uid, gid)
	c.Dir, c.Env = dir, env
	o.bind(c, argv[0], env)
	var errb tailBuf
	c.Stdout, c.Stderr = out, &errb
	if timeout <= 0 {
		if _, ok := out.(stopper); !ok {
			err := c.Run()
			return errb.String(), err
		}
	}
	if err := c.Start(); err != nil {
		return errb.String(), err
	}
	done := make(chan error, 1)
	exit.Go(func() { done <- c.Wait() })
	var expire <-chan time.Time
	if timeout > 0 {
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		expire = timer.C
	}
	var stopped <-chan struct{}
	if s, ok := out.(stopper); ok {
		stopped = s.Stopped()
	}
	kill := func() {
		_ = syscall.Kill(-c.Process.Pid, syscall.SIGKILL)
		_ = c.Process.Kill()
		<-done
	}
	select {
	case err := <-done:
		return errb.String(), err
	case <-stopped:
		kill()
		return errb.String(), errors.New("stopped: output over the limit")
	case <-expire:
		kill()
		return errb.String(), fmt.Errorf("timed out after %s", timeout)
	}
}

// fetchUpload is the code step of an upload job. On an error the caller removes work.
func (o Options) fetchUpload(s Spec, work string, uid, gid int) error {
	// git before 2.48 has no --end-of-options for `checkout --detach`; its one operand is the
	// tip that was just checked against a 40 or 64 digit hex sha, so it cannot read as an option.
	if !hexShaRE.MatchString(s.Sha) {
		return &stepError{Name: "code", Err: errors.New("the spec's sha is not a git object name")}
	}
	home := filepath.Join(o.WorkRoot, ".bitc-home-"+s.Job)
	defer os.RemoveAll(home)
	if err := os.Mkdir(home, 0o700); err != nil {
		return &stepError{Name: "code", Err: err}
	}
	if err := o.System.Chown(home, uid, gid); err != nil {
		return &stepError{Name: "job-user", Err: err}
	}
	bundle := filepath.Join(o.WorkRoot, uploadDirName, uploadBundleName)
	base := []string{"PATH=" + agentPath, "HOME=" + home, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0"}
	gitTo := func(dir string, extra []string, out io.Writer, args ...string) error {
		env := append(append([]string{}, base...), extra...)
		errTxt, err := o.runCmd(append([]string{"git"}, args...), false, dir, env, uid, gid, out, o.codeTimeout())
		if err != nil {
			return &stepError{Name: "code", Tail: tail(errTxt, 20), First: firstLine(errTxt), Err: err}
		}
		return nil
	}
	// Everything git prints is bounded or dropped here: this agent is root and exempt from the
	// OOM killer, and git is not trusted to print little.
	git := func(dir string, extra []string, args ...string) error {
		return gitTo(dir, extra, io.Discard, args...)
	}
	if err := git(o.WorkRoot, nil, "init", "-q", "--template=", "--end-of-options", work); err != nil {
		return err
	}
	if err := git(work, nil, "bundle", "verify", "--end-of-options", bundle); err != nil {
		return err
	}
	if err := git(work, nil, "-c", "transfer.fsckObjects=true", "-c", "fetch.fsckObjects=true", "-c", "core.symlinks=true",
		"-c", "submodule.recurse=false", "-c", "core.hooksPath=/dev/null", "fetch", "-q", "--end-of-options", bundle, o.Code.Ref); err != nil {
		return err
	}
	tipBuf := &capBuf{max: 256}
	if err := gitTo(work, nil, tipBuf, "rev-parse", "FETCH_HEAD"); err != nil {
		return err
	}
	tip := string(tipBuf.b)
	if strings.TrimSpace(tip) != s.Sha {
		return &stepError{Name: "ref-mismatch", Err: fmt.Errorf("the ref's tip is %s, want %s", strings.TrimSpace(tip), s.Sha)}
	}
	// Git before 2.48.0 does not fsck objects that come from a bundle, whatever
	// transfer.fsckObjects says: refuse .git, .. and other malformed tree entries explicitly.
	if err := git(work, nil, "fsck", "--strict", "--no-dangling", "--end-of-options", s.Sha); err != nil {
		return err
	}
	unlock, err := acquireUploadLock(o.UploadLock, time.Now().Add(o.codeTimeout()))
	if err != nil {
		return &stepError{Name: "code", Err: err}
	}
	defer unlock()
	avail, ffree, err := o.Upload.statfs(o.WorkRoot)
	if err != nil {
		return &stepError{Name: "code", Err: fmt.Errorf("statfs: %w", err)}
	}
	lw := newListingWriter(avail, ffree)
	if err := gitTo(work, nil, lw, "ls-tree", "-r", "-t", "-l", "-z", "--end-of-options", s.Sha); err != nil {
		if gerr := lw.refusal(); gerr != nil {
			return &stepError{Name: "code", Err: gerr}
		}
		return err
	}
	if err := lw.finish(); err != nil {
		return &stepError{Name: "code", Err: err}
	}
	return git(work, []string{"GIT_LFS_SKIP_SMUDGE=1"}, "-c", "filter.lfs.smudge=", "-c", "filter.lfs.process=", "-c", "filter.lfs.required=false",
		"checkout", "-q", "--detach", s.Sha)
}

// maxListingRecord bounds one `ls-tree -z` record held while it is parsed (a path is a few KiB
// on a real tree; a longer one is hostile).
const maxListingRecord = 1 << 20

func satAdd(a, b uint64) uint64 {
	if s := a + b; s >= a {
		return s
	}
	return ^uint64(0)
}

// listingWriter consumes a `git ls-tree -r -t -l -z` listing as it streams in and keeps only
// running totals of what the checkout would take (each blob rounded up to 4 KiB, plus 4 KiB per
// entry; gitlinks are skipped), with saturating adds. It refuses the checkout when it does not fit
// in the free space (less 1 GiB) or when its entries come within 100000 of the free inodes, and as
// soon as a limit is passed it asks for the git process group to be killed.
type listingWriter struct {
	limitBytes, limitEntries uint64
	diskFull, noInodes       bool // the statfs numbers leave no room at all
	partial                  []byte
	entries, bytesNeeded     uint64
	err                      error // a malformed listing
	over                     bool
	stop                     chan struct{}
}

func newListingWriter(avail, ffree uint64) *listingWriter {
	w := &listingWriter{stop: make(chan struct{})}
	if avail <= uploadSpare {
		w.diskFull = true
	} else {
		w.limitBytes = avail - uploadSpare
	}
	if ffree <= inodeSpare {
		w.noInodes = true
	} else {
		w.limitEntries = ffree - inodeSpare
	}
	return w
}

func (w *listingWriter) Stopped() <-chan struct{} { return w.stop }

func (w *listingWriter) halt() {
	if !w.over {
		w.over = true
		close(w.stop)
	}
}

func (w *listingWriter) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 && !w.over {
		i := bytes.IndexByte(p, 0)
		if i < 0 {
			if len(w.partial)+len(p) > maxListingRecord {
				w.err = errors.New("unreadable tree listing: a record is too long")
				w.halt()
				break
			}
			w.partial = append(w.partial, p...)
			break
		}
		rec := p[:i]
		if len(w.partial) > 0 {
			if len(w.partial)+len(rec) > maxListingRecord {
				w.err = errors.New("unreadable tree listing: a record is too long")
				w.halt()
				break
			}
			w.partial = append(w.partial, rec...)
			rec = w.partial
		}
		w.record(string(rec))
		w.partial = w.partial[:0]
		p = p[i+1:]
	}
	return n, nil
}

func (w *listingWriter) record(rec string) {
	meta, _, ok := strings.Cut(rec, "\t")
	if !ok {
		return
	}
	f := strings.Fields(meta)
	if len(f) != 4 {
		w.err = fmt.Errorf("unreadable tree listing line %q", rec)
		w.halt()
		return
	}
	if f[0] == "160000" { // a gitlink: no submodule is fetched
		return
	}
	w.entries = satAdd(w.entries, 1)
	w.bytesNeeded = satAdd(w.bytesNeeded, blockSize)
	if f[1] == "blob" {
		n, err := strconv.ParseUint(f[3], 10, 62)
		if err != nil {
			w.err = fmt.Errorf("unreadable blob size in %q", rec)
			w.halt()
			return
		}
		w.bytesNeeded = satAdd(w.bytesNeeded, satAdd(n, blockSize-1)/blockSize*blockSize)
	}
	if w.diskFull || w.bytesNeeded >= w.limitBytes || w.noInodes || w.entries >= w.limitEntries {
		w.halt()
	}
}

// refusal is why the listing was refused (or cut short), or nil.
func (w *listingWriter) refusal() error {
	if w.err != nil {
		return w.err
	}
	if w.diskFull || w.bytesNeeded >= w.limitBytes {
		return errors.New("checkout would exceed disk")
	}
	if w.noInodes || w.entries >= w.limitEntries {
		return errors.New("checkout would exceed inodes")
	}
	return nil
}

// finish judges a complete listing.
func (w *listingWriter) finish() error {
	if len(w.partial) > 0 && w.err == nil {
		w.err = errors.New("unreadable tree listing: it ends inside a record")
	}
	return w.refusal()
}
