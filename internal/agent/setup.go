//go:build linux

package agent

import (
	"archive/tar"
	"compress/bzip2"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
)

// toolNameRE duplicates internal/config's toolNameRE (the agent does not import config); keep them equal.
var toolNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// stepError names the failing step for __BITC_SETUP_FAILED__ and carries its stderr tail.
// Repo marks the repo's own failure (spec 8.4 step 5): a repo setup or root step or a service
// that exited non-zero by itself, or a tool sha256 mismatch. Anything else is infra.
type stepError struct {
	Name  string
	Tail  string
	First string // the first non-empty stderr line ("" when there was none)
	Repo  bool
	Err   error
}

func (e *stepError) Error() string { return e.Name + ": " + e.Err.Error() }

// class is the marker's class field; when in doubt, infra.
func (e *stepError) class() string {
	if e.Repo {
		return "repo"
	}
	return "infra"
}

// byItself: the process exited with a non-zero code on its own, not by a signal (the OOM killer
// and the agent's own kills are signals), so the failure is the repo's (spec 8.4 step 5).
func byItself(err error) bool {
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		return false
	}
	ws, ok := ee.Sys().(syscall.WaitStatus)
	return ok && ws.Exited() && ws.ExitStatus() != 0
}

// firstLine is the first non-empty line of s, carriage returns removed.
func firstLine(s string) string {
	for _, l := range strings.Split(strings.ReplaceAll(s, "\r", ""), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			return l
		}
	}
	return ""
}

// errorLine is the marker's last field: the first stderr line, else the error, cut to 200 bytes.
func errorLine(err error) string {
	var se *stepError
	l := ""
	if errors.As(err, &se) {
		l = se.First
		if l == "" {
			l = firstLine(se.Err.Error())
		}
	} else {
		l = firstLine(err.Error())
	}
	if len(l) > 200 {
		l = l[:200]
	}
	return l
}

// installTool downloads to a root-only dir, verifies sha256 on that copy, extracts to
// <tools>/<name>/ stripping a single top dir, and returns the dir to put on PATH.
func installTool(client *http.Client, toolsRoot string, t Tool) (string, error) {
	// The name becomes a path component removed and created as root: validate before any filesystem call.
	if !toolNameRE.MatchString(t.Name) {
		return "", &stepError{Name: t.Name, Repo: true, Err: fmt.Errorf("tool name %q must match %s", t.Name, toolNameRE)}
	}
	if filepath.Dir(filepath.Join(toolsRoot, t.Name)) != filepath.Clean(toolsRoot) {
		return "", &stepError{Name: t.Name, Repo: true, Err: errors.New("tool name escapes the tools dir")}
	}
	if err := ensureDir(filepath.Dir(toolsRoot), 0o755); err != nil { // e.g. /opt/bitc: also created under the inherited umask
		return "", err
	}
	if err := ensureDir(toolsRoot, 0o755); err != nil { // the job user traverses it to reach the tools
		return "", err
	}
	dl := filepath.Join(toolsRoot, ".dl")
	if err := os.MkdirAll(dl, 0o700); err != nil {
		return "", err
	}
	if !strings.HasPrefix(t.URL, "https://") {
		return "", &stepError{Name: t.Name, Err: errors.New("tool urls must be https")}
	}
	resp, err := client.Get(t.URL)
	if err != nil {
		return "", &stepError{Name: t.Name, Err: err}
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", &stepError{Name: t.Name, Err: fmt.Errorf("HTTP %d", resp.StatusCode)}
	}
	local := filepath.Join(dl, t.Name)
	f, err := os.OpenFile(local, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(f, h), resp.Body)
	f.Close()
	if err != nil {
		return "", &stepError{Name: t.Name, Err: err}
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != t.SHA256 {
		// the pin does not match what the URL serves: the repo's own config, retrying cannot help
		return "", &stepError{Name: t.Name, Repo: true, Err: fmt.Errorf("sha256 %s, want %s", got, t.SHA256)}
	}
	dest := filepath.Join(toolsRoot, t.Name)
	_ = os.RemoveAll(dest)
	if err := ensureDir(dest, 0o755); err != nil {
		return "", err
	}
	err = extractArchive(local, t.URL, dest)
	if err != nil {
		return "", &stepError{Name: t.Name, Repo: errors.Is(err, errUnsafeArchive) || errors.Is(err, errUnsupportedArchive), Err: err}
	}
	return toolPathDir(dest), nil
}

// toolPathDir is the directory of an installed tool to put on PATH: its bin dir when it has one.
func toolPathDir(dest string) string {
	if st, err := os.Stat(filepath.Join(dest, "bin")); err == nil && st.IsDir() {
		return filepath.Join(dest, "bin")
	}
	return dest
}

// reuseTools is installTool's check for a tool a provisioning already installed: the archive
// kept in the root-only download dir must still hash to the pin and the extracted tree must be
// there. A change is infra (nothing of the repo is at fault; the Sprite is not what was made).
func reuseTools(toolsRoot string, tools []Tool) ([]string, error) {
	var dirs []string
	for _, t := range tools {
		if !toolNameRE.MatchString(t.Name) {
			return nil, &stepError{Name: t.Name, Repo: true, Err: fmt.Errorf("tool name %q must match %s", t.Name, toolNameRE)}
		}
		f, err := os.Open(filepath.Join(toolsRoot, ".dl", t.Name))
		if err != nil {
			return nil, &stepError{Name: t.Name, Err: fmt.Errorf("provisioned tool is missing: %w", err)}
		}
		h := sha256.New()
		_, err = io.Copy(h, f)
		f.Close()
		if err != nil {
			return nil, &stepError{Name: t.Name, Err: err}
		}
		if got := hex.EncodeToString(h.Sum(nil)); got != t.SHA256 {
			return nil, &stepError{Name: t.Name, Err: fmt.Errorf("provisioned tool changed: sha256 %s, want %s", got, t.SHA256)}
		}
		dest := filepath.Join(toolsRoot, t.Name)
		if st, err := os.Stat(dest); err != nil || !st.IsDir() {
			return nil, &stepError{Name: t.Name, Err: errors.New("provisioned tool dir is missing")}
		}
		dirs = append(dirs, toolPathDir(dest))
	}
	return dirs, nil
}

// ensureDir creates dir (and any missing parents) and then sets the mode of every directory it
// created explicitly, so no umask can narrow them. Existing parents are left alone.
func ensureDir(dir string, perm os.FileMode) error {
	var made []string
	for d := filepath.Clean(dir); ; d = filepath.Dir(d) {
		if _, err := os.Lstat(d); err == nil || d == filepath.Dir(d) {
			break
		}
		made = append(made, d)
	}
	if err := os.MkdirAll(dir, perm); err != nil {
		return err
	}
	for _, d := range made {
		if err := os.Chmod(d, perm); err != nil { //nolint:gosec // the job user must reach these dirs
			return err
		}
	}
	return os.Chmod(dir, perm) //nolint:gosec // the job user must reach these dirs
}

// ensureTree creates dir below dest and chmods every component from dest down to dir to 0755,
// so intermediate dirs of a nested entry never keep the umask's narrower mode.
func ensureTree(dest, dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // the job user must reach these dirs
		return err
	}
	rel, err := filepath.Rel(dest, dir)
	if err != nil {
		return err
	}
	cur := dest
	if err := os.Chmod(cur, 0o755); err != nil { //nolint:gosec // the job user must reach these dirs
		return err
	}
	if rel == "." {
		return nil
	}
	for _, c := range strings.Split(rel, string(filepath.Separator)) {
		cur = filepath.Join(cur, c)
		if err := os.Chmod(cur, 0o755); err != nil { //nolint:gosec // the job user must reach these dirs
			return err
		}
	}
	return nil
}

var errUnsupportedArchive = errors.New("unsupported tool archive")

// extractArchive unpacks the downloaded tool archive src into dest in-process; the URL's suffix
// picks the decompressor. Nothing here runs the system tar: tool archives are repo-controlled and
// the agent is root, so owners, setuid bits and device nodes must never be honoured.
// .tar.gz/.tgz use compress/gzip, .tar is read directly, .tar.bz2/.tbz2 use compress/bzip2;
// .tar.xz/.txz and .tar.zst/.tzst are piped through `xz -dc` / `zstd -dc`, which only write to stdout.
func extractArchive(src, url, dest string) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	has := func(sfx ...string) bool {
		for _, s := range sfx {
			if strings.HasSuffix(url, s) {
				return true
			}
		}
		return false
	}
	switch {
	case has(".tar.gz", ".tgz"):
		gz, err := gzip.NewReader(f)
		if err != nil {
			return err
		}
		return extractTar(gz, dest)
	case has(".tar"):
		return extractTar(f, dest)
	case has(".tar.bz2", ".tbz2"):
		return extractTar(bzip2.NewReader(f), dest)
	case has(".tar.xz", ".txz"):
		return extractViaFilter(f, dest, "xz", "-dc")
	case has(".tar.zst", ".tzst"):
		return extractViaFilter(f, dest, "zstd", "-dc")
	}
	return fmt.Errorf("%w: %q has no supported suffix (.tar .tar.gz .tgz .tar.bz2 .tbz2 .tar.xz .txz .tar.zst .tzst)", errUnsupportedArchive, url)
}

// extractViaFilter pipes f through a decompressor that writes only to stdout and extracts its output.
func extractViaFilter(f *os.File, dest, name string, args ...string) error {
	c := agentCmd(name, args...)
	c.Stdin = f
	out, err := c.StdoutPipe()
	if err != nil {
		return err
	}
	var se strings.Builder
	c.Stderr = &se
	if err := c.Start(); err != nil {
		return err
	}
	xerr := extractTar(out, dest)
	_, _ = io.Copy(io.Discard, out) // let the filter finish if extraction stopped early
	if werr := c.Wait(); xerr == nil && werr != nil {
		xerr = fmt.Errorf("%s: %w: %s", name, werr, firstLine(se.String()))
	}
	return xerr
}

// extractTar writes the tar stream into dest, stripping the first path component of every entry
// (like tar --strip-components=1: entries with nothing below the top dir are skipped). It only
// creates regular files, dirs and in-tree relative symlinks; modes are masked to perm&0o755 (no
// setuid/setgid/sticky, no group/other write); it never chowns, so everything is root-owned and
// the job user reaches it through the other-read/exec bits. Device, fifo and hard-link entries,
// and any other type, are errUnsafeArchive.
func extractTar(r io.Reader, dest string) error {
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		switch hdr.Typeflag {
		case tar.TypeReg, tar.TypeDir, tar.TypeSymlink:
		case tar.TypeXGlobalHeader:
			continue
		default:
			return fmt.Errorf("%w: %s has entry type %q", errUnsafeArchive, hdr.Name, hdr.Typeflag)
		}
		parts := strings.SplitN(strings.TrimPrefix(hdr.Name, "./"), "/", 2)
		if len(parts) < 2 || parts[1] == "" {
			continue // the single top dir
		}
		target := filepath.Join(dest, filepath.Clean("/"+parts[1]))
		// the agent extracts as root: nothing may be written through a link the archive made
		if err := noLinksOnPath(dest, target); err != nil {
			return err
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			_ = ensureTree(dest, target)
		case tar.TypeReg:
			_ = ensureTree(dest, filepath.Dir(target))
			mode := os.FileMode(hdr.Mode).Perm() & 0o755
			out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|syscall.O_NOFOLLOW, mode)
			if err != nil {
				return err
			}
			_, err = io.Copy(out, tr)
			if err == nil {
				err = out.Chmod(mode) // the umask must not narrow it
			}
			if cerr := out.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return err
			}
		case tar.TypeSymlink:
			if filepath.IsAbs(hdr.Linkname) || !within(dest, filepath.Join(filepath.Dir(target), hdr.Linkname)) {
				return fmt.Errorf("%w: %s -> %s leaves the tool dir", errUnsafeArchive, hdr.Name, hdr.Linkname)
			}
			_ = ensureTree(dest, filepath.Dir(target))
			_ = os.Symlink(hdr.Linkname, target)
		}
	}
}

var errUnsafeArchive = errors.New("unsafe tool archive")

// within reports whether p is dest or below it.
func within(dest, p string) bool {
	r, err := filepath.Rel(dest, p)
	return err == nil && r != ".." && !strings.HasPrefix(r, "../")
}

// noLinksOnPath refuses when any existing component from dest down to target is a symlink.
func noLinksOnPath(dest, target string) error {
	rel, err := filepath.Rel(dest, target)
	if err != nil {
		return err
	}
	cur := dest
	for _, c := range strings.Split(rel, string(filepath.Separator)) {
		if c == "." {
			continue
		}
		cur = filepath.Join(cur, c)
		st, err := os.Lstat(cur)
		if err != nil {
			return nil // not there yet: it will be created as a real dir
		}
		if st.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: %s goes through a symlink", errUnsafeArchive, cur)
		}
	}
	return nil
}

// fetchCode clones exactly the ref with the deploy key and pinned known hosts, checks the tip
// is the sha, then shreds the key (spec 8.4 step 4). A local path is allowed only unprivileged.
func fetchCode(s Spec, work, keyDir, deployKey string, mount func(dir string) (func() error, error)) error {
	run := func(name string, args ...string) error {
		c := agentCmd("git", args...)
		c.Dir = work
		c.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "HOME=" + keyDir, "GIT_TERMINAL_PROMPT=0"}
		if out, err := c.CombinedOutput(); err != nil {
			return &stepError{Name: name, Tail: tail(string(out), 20), First: firstLine(string(out)), Err: err} // infra
		}
		return nil
	}
	// the job user must reach its work tree: explicit modes, whatever the umask is
	if err := ensureDir(filepath.Dir(work), 0o755); err != nil {
		return err
	}
	if err := ensureDir(work, 0o755); err != nil {
		return err
	}
	if err := run("code", "init", "-q"); err != nil {
		return err
	}
	ssh := !strings.HasPrefix(s.CloneURL, "/") || !s.Unprivileged
	fetch := []string{"fetch", "-q", "--depth=1", s.CloneURL, s.Ref}
	if ssh {
		if deployKey == "" {
			return &stepError{Name: "deploy-key", Err: errors.New("no deploy key")}
		}
		if err := os.MkdirAll(keyDir, 0o700); err != nil {
			return err
		}
		var unmount func() error
		if mount != nil { // exec delivery: the key lives only on a tmpfs, never on the Sprite's disk
			var err error
			if unmount, err = mount(keyDir); err != nil {
				_ = os.RemoveAll(keyDir)
				return &stepError{Name: "key-mount", Err: err}
			}
		}
		key := filepath.Join(keyDir, "key")
		kh := filepath.Join(keyDir, "known_hosts")
		_ = os.WriteFile(key, []byte(strings.TrimSpace(deployKey)+"\n"), 0o600)
		_ = os.WriteFile(kh, []byte(s.KnownHosts), 0o600)
		cmd := fmt.Sprintf("ssh -i %s -o IdentitiesOnly=yes -o UserKnownHostsFile=%s -o StrictHostKeyChecking=yes -o BatchMode=yes", key, kh)
		fetch = append([]string{"-c", "core.sshCommand=" + cmd}, fetch...)
		defer shred(key, keyDir, unmount)
	}
	if err := run("code", fetch...); err != nil {
		return err
	}
	out, err := agentCmd("git", "-C", work, "rev-parse", "FETCH_HEAD").Output()
	if err != nil || strings.TrimSpace(string(out)) != s.Sha {
		return &stepError{Name: "ref-mismatch", Err: fmt.Errorf("the ref's tip is %s, want %s", strings.TrimSpace(string(out)), s.Sha)}
	}
	return run("code", "checkout", "-q", "--detach", s.Sha)
}

// shred overwrites the key, removes it, unmounts the key tmpfs (when there is one), then removes
// the key dir and ~/.ssh.
func shred(key, dir string, unmount func() error) {
	if f, err := os.OpenFile(key, os.O_WRONLY, 0); err == nil {
		st, _ := f.Stat()
		_, _ = f.Write(make([]byte, st.Size()))
		_ = f.Sync()
		f.Close()
	}
	_ = os.Remove(key)
	if unmount != nil {
		_ = unmount()
	}
	_ = os.RemoveAll(dir)
	removeSSH(agentHome())
}

// agentHome is the passwd home of the agent's uid (ssh uses it, not $HOME, and stage 2 has no
// $HOME), /root for uid 0, else $HOME.
func agentHome() string {
	if h, ok := homeFromPasswd("/etc/passwd", os.Getuid()); ok {
		return h
	}
	if os.Getuid() == 0 {
		return "/root"
	}
	return os.Getenv("HOME")
}

// homeFromPasswd is the home of uid in a passwd-format file.
func homeFromPasswd(path string, uid int) (string, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	for _, l := range strings.Split(string(b), "\n") {
		f := strings.Split(l, ":")
		if len(f) >= 6 && f[2] == strconv.Itoa(uid) && f[5] != "" {
			return f[5], true
		}
	}
	return "", false
}

func removeSSH(home string) {
	if home != "" && home != "/" {
		_ = os.RemoveAll(filepath.Join(home, ".ssh"))
	}
}

func tail(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
