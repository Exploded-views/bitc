package ssh

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Exploded-views/bitc/internal/config"
	"github.com/Exploded-views/bitc/internal/provider"
)

// The upload transport (Task H.5): the job's commit goes to the host as a git bundle on the
// gate session, right after the boot frame of `slot-boot --run`. No deploy key, no job ref on a
// forge and no forge reachability for code are involved.

// upload reports whether this provider is configured for the upload transport.
func (p *Provider) upload() bool { return p.core.Transport.Kind == config.TransportUpload }

// bundle is the job's commit as a file in a private directory.
type bundle struct {
	dir, path string
	size      int64
	sha256    string
	ref       string
}

// Close removes the bundle and its directory.
func (b *bundle) Close() {
	if b != nil && b.dir != "" {
		_ = os.RemoveAll(b.dir)
	}
}

// makeBundle writes the bundle of sha under a fresh 0700 directory in dir, as a 0600 file. The
// temporary ref refs/bitc/upload/<job> exists in the work tree only while git builds it (a bundle
// needs a ref) and is deleted again whatever happens. A bundle over max is ineligible.
func makeBundle(ctx context.Context, worktree, dir, job, sha string, max config.Size) (*bundle, error) {
	if sha == "" {
		return nil, &provider.ErrIneligible{Why: "no commit to upload"}
	}
	ref := provider.UploadRef(job)
	git := func(args ...string) ([]byte, error) {
		c := exec.CommandContext(ctx, "git", append([]string{"-C", worktree}, args...)...)
		c.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
		return c.CombinedOutput()
	}
	fail := func(what string, out []byte, err error) error {
		why := oneLine(string(out))
		if why == "" {
			why = oneLine(err.Error())
		}
		return &provider.ErrIneligible{Why: "could not " + what + ": " + why}
	}
	if out, err := git("update-ref", ref, sha); err != nil {
		return nil, fail("pin the commit for upload", out, err)
	}
	defer func() {
		// the delete must outlive a cancelled ctx (Ctrl-C during `git bundle create`)
		dctx, stop := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer stop()
		c := exec.CommandContext(dctx, "git", "-C", worktree, "update-ref", "-d", ref)
		c.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
		_, _ = c.CombinedOutput()
	}()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, &provider.ErrIneligible{Why: "could not make the upload directory: " + oneLine(err.Error())}
	}
	tmp, err := os.MkdirTemp(dir, "upload-")
	if err != nil {
		return nil, &provider.ErrIneligible{Why: "could not make the upload directory: " + oneLine(err.Error())}
	}
	b := &bundle{dir: tmp, path: filepath.Join(tmp, "code.bundle"), ref: ref}
	if out, err := git("bundle", "create", b.path, ref); err != nil {
		b.Close()
		return nil, fail("bundle the commit", out, err)
	}
	if err := os.Chmod(b.path, 0o600); err != nil {
		b.Close()
		return nil, &provider.ErrIneligible{Why: "could not secure the bundle: " + oneLine(err.Error())}
	}
	f, err := os.Open(b.path)
	if err != nil {
		b.Close()
		return nil, &provider.ErrIneligible{Why: "could not read the bundle: " + oneLine(err.Error())}
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		b.Close()
		return nil, &provider.ErrIneligible{Why: "could not read the bundle: " + oneLine(err.Error())}
	}
	b.size, b.sha256 = n, hex.EncodeToString(h.Sum(nil))
	if config.Size(n) > max {
		b.Close()
		return nil, &provider.ErrIneligible{Why: fmt.Sprintf("bundle is %d MiB, over max_bundle %s", (n+(1<<20)-1)>>20, max)}
	}
	return b, nil
}

// The host's git must carry the fix for CVE-2024-32002 (git 2.45.1, or a distribution package
// whose changelog names it), because the host checks out a repository the job's owner controls.
const gitFloor = "2.45.1"

var gitVersionRE = regexp.MustCompile(`(\d+)\.(\d+)(?:\.(\d+))?`)

// GitFloorLine judges the host's `git --version` output and the part of its package changelog
// that names the CVE (empty when the package has none): ok at git >= 2.45.1 or when the
// changelog names CVE-2024-32002, else a fail line `ssh-git`.
func GitFloorLine(version, changelog string) provider.DoctorLine {
	line := func(level, detail string) provider.DoctorLine {
		return provider.DoctorLine{Level: level, Check: "ssh-git", Detail: detail}
	}
	m := gitVersionRE.FindStringSubmatch(version)
	if m == nil {
		return line("fail", "the host did not report its git version; upload needs git "+gitFloor+" or later")
	}
	v := m[0]
	nums := [3]int{}
	for i := range nums {
		nums[i], _ = strconv.Atoi(m[i+1])
	}
	if slices.Compare(nums[:], []int{2, 45, 1}) >= 0 {
		return line("ok", "git "+v)
	}
	if strings.Contains(changelog, "CVE-2024-32002") {
		return line("ok", "git "+v+" (the package changelog names CVE-2024-32002)")
	}
	return line("fail", fmt.Sprintf("git %s is older than %s and its package changelog does not name CVE-2024-32002", v, gitFloor))
}

// uploadLines are the doctor lines of the upload transport: the transport itself, and the git
// rule judged from the two items `check-host --doctor` relays about the host's git
// (ssh-git-version: the `git --version` text; ssh-git-changelog: the changelog's CVE line, or
// nothing). Both items are consumed here; the judged line replaces them.
func (p *Provider) uploadLines(host []provider.DoctorLine) (rest, extra []provider.DoctorLine) {
	extra = append(extra, provider.DoctorLine{Level: "ok", Check: "ssh transport", Detail: "upload (max " + p.core.Transport.MaxBundle.String() + ")"})
	var version, changelog string
	seen := false
	for _, l := range host {
		switch l.Check {
		case "ssh-git-version":
			version, seen = l.Detail, true
		case "ssh-git-changelog":
			changelog, seen = l.Detail, true
		default:
			rest = append(rest, l)
		}
	}
	if seen || !hasFail(rest) { // a host that could not be asked at all has its own fail line
		extra = append(extra, GitFloorLine(version, changelog))
	}
	return rest, extra
}
