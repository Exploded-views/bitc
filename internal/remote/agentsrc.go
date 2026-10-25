package remote

import (
	"bytes"
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
	"runtime"
	"strings"
	"time"

	"github.com/Exploded-views/bitc/internal/config"
	"github.com/Exploded-views/bitc/internal/provider"
	"github.com/Exploded-views/bitc/internal/state"
	"github.com/Exploded-views/bitc/internal/version"
)

var shaRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

const releaseBase = "https://github.com/Exploded-views/bitc/releases/download"

// AgentSource is where the pod gets its agent (spec 8.4 step 1): a URL to download, or Build,
// a pinned commit the pod builds itself. SHA256 is what the agent binary must hash to.
type AgentSource struct {
	URL, SHA256 string
	Build       *provider.AgentBuild // Ref is filled in by the transport
	Path        string               // a local file whose bytes are delivered (source url paths, source build with module_dir)
	Commit      string               // module_dir build: the source tree's HEAD
	Arch        string
}

// String is the doctor line: the URL, or "build <commit12> on <image>".
func (a AgentSource) String() string {
	if a.Build != nil {
		return "build " + a.Build.Commit[:12] + " on " + a.Build.Image
	}
	if a.URL == "" && a.Commit != "" {
		return "build " + a.Commit[:12] + " from module_dir, " + a.Arch
	}
	return a.URL
}

// ResolveAgent returns the agent URL and sha256 for linux/amd64 (spec 8.4 step 1); a build
// source has no URL. A source that is delivered as bytes (module_dir) has none either, so it is
// ineligible for a provider that fetches by URL.
func ResolveAgent(e *config.Effective, core *config.Provider) (string, string, error) {
	return ResolveAgentFor(e, core, "amd64")
}

// ResolveAgentFor is ResolveAgent for one architecture.
func ResolveAgentFor(e *config.Effective, core *config.Provider, arch string) (string, string, error) {
	if core.Agent.Source == "build" && core.Agent.ModuleDir != "" && (e.Test == nil || e.Test.AgentURL == "") {
		return "", "", &provider.ErrIneligible{Why: "agent.module_dir: this agent is delivered as bytes over exec or ssh; this provider fetches it by url"}
	}
	a, err := ResolveAgentSourceFor(e, core, arch)
	return a.URL, a.SHA256, err
}

// ResolveAgentSource resolves the provider's agent source for linux/amd64.
func ResolveAgentSource(e *config.Effective, core *config.Provider) (AgentSource, error) {
	return ResolveAgentSourceFor(e, core, "amd64")
}

// ResolveAgentSourceFor resolves the agent source for one architecture (providers delta C.4):
// a url source expands {arch} and hashes paths[arch]; the release source takes bitc_<ver>_linux_<arch>
// and its sha from checksums.txt; a build source with module_dir names the tree to build (the
// build itself is BuildAgent); a build source with commit, image and sha256 is built by the pod.
func ResolveAgentSourceFor(e *config.Effective, core *config.Provider, arch string) (AgentSource, error) {
	if e.Test == nil || e.Test.AgentURL == "" {
		if a := core.Agent; a.Source == "build" {
			if a.ModuleDir != "" {
				commit, err := CheckModuleDir(a.ModuleDir)
				return AgentSource{Arch: arch, Commit: commit}, err
			}
			return AgentSource{SHA256: a.SHA256, Arch: arch, Build: &provider.AgentBuild{Commit: a.Commit, Image: a.Image, SHA256: a.SHA256}}, nil
		}
	}
	url, sha, path, err := resolveAgentURL(e, core, arch)
	return AgentSource{URL: url, SHA256: sha, Path: path, Arch: arch}, err
}

func resolveAgentURL(e *config.Effective, core *config.Provider, arch string) (url, sha, path string, err error) {
	switch {
	case e.Test != nil && e.Test.AgentURL != "":
		self, err := os.Executable()
		if err != nil {
			return "", "", "", err
		}
		sha, err := fileSHA(self)
		return e.Test.AgentURL, sha, self, err
	case core.Agent.Source == "url":
		path := core.Agent.Paths[arch]
		if path == "" && arch == "amd64" {
			path = core.Agent.Path
		}
		if path == "" {
			return "", "", "", &provider.ErrIneligible{Why: "agent paths has no " + arch + " entry"}
		}
		sha, err := fileSHA(path)
		if err != nil {
			return "", "", "", &provider.ErrIneligible{Why: "agent.path: " + err.Error()}
		}
		return strings.ReplaceAll(core.Agent.URL, "{arch}", arch), sha, path, nil
	case version.Release == "true" && runtime.GOOS == "linux" && runtime.GOARCH == arch:
		self, err := os.Executable()
		if err != nil {
			return "", "", "", err
		}
		sha, err := fileSHA(self)
		return fmt.Sprintf("%s/v%s/bitc_%s_linux_%s", releaseBase, version.Current(), version.Current(), arch), sha, self, err
	case version.Release == "true":
		url, sha, err := ResolveRelease(version.Current(), arch, FetchChecksums)
		return url, sha, "", err
	default:
		return "", "", "", &provider.ErrIneligible{Why: `agent source "release" needs a release build of bitc for linux/` + arch + `; set agent = { source = "url", ... }`}
	}
}

// FetchChecksums downloads a release's checksums.txt over https (a variable so tests never touch
// the network; the conformance suite passes its own).
var FetchChecksums = func(url string) ([]byte, error) {
	if !strings.HasPrefix(url, "https://") {
		return nil, errors.New("not an https url")
	}
	c := &http.Client{Timeout: 30 * time.Second}
	resp, err := c.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", url, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 1<<20))
}

// ResolveRelease returns the URL and sha256 of the release agent for arch: the asset
// bitc_<ver>_linux_<arch>, with its sha read from the release's checksums.txt. A failed
// fetch is transient (nothing was created); a missing line makes the job ineligible.
func ResolveRelease(ver, arch string, fetch func(url string) ([]byte, error)) (url, sha string, err error) {
	base := fmt.Sprintf("%s/v%s/", releaseBase, ver)
	name := fmt.Sprintf("bitc_%s_linux_%s", ver, arch)
	b, err := fetch(base + "checksums.txt")
	if err != nil {
		return "", "", &provider.ErrTransient{Why: "agent checksums: " + err.Error()}
	}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && f[1] == name && len(f[0]) == 64 {
			return base + name, f[0], nil
		}
	}
	return "", "", &provider.ErrIneligible{Why: "checksums.txt has no " + name}
}

// Runner runs argv in dir with extra env (added to the process env); BuildAgent's seam for tests.
type Runner func(dir string, env []string, argv ...string) ([]byte, error)

func execRunner(dir string, env []string, argv ...string) ([]byte, error) {
	c := exec.Command(argv[0], argv[1:]...)
	c.Dir, c.Env = dir, append(os.Environ(), env...)
	return c.CombinedOutput()
}

// CheckModuleDir verifies that dir is a git work tree with no uncommitted or untracked changes
// and returns its HEAD (providers delta C.4 item 3).
func CheckModuleDir(dir string) (string, error) {
	bad := func(why string) error { return &provider.ErrIneligible{Why: "agent.module_dir: " + why} }
	head, err := git(dir, "rev-parse", "HEAD")
	if err != nil {
		return "", bad("not a git work tree: " + dir)
	}
	if out, err := git(dir, "status", "--porcelain", "--untracked-files=all"); err != nil || strings.TrimSpace(out) != "" {
		return "", bad("work tree has uncommitted or untracked changes: " + dir)
	}
	return strings.TrimSpace(head), nil
}

// BuildAgent builds the agent for arch from the module_dir tree (providers delta C.4 item 3):
// CGO_ENABLED=0 GOOS=linux GOARCH=<arch> go build -trimpath -ldflags=-buildid= ./cmd/bitc, into
// <account>/agents/<arch>/<sha256>, once per architecture and tree state (HEAD of a clean tree).
// A nil run uses the go on PATH.
func BuildAgent(e *config.Effective, core *config.Provider, arch string, run Runner) (AgentSource, error) {
	if run == nil {
		run = execRunner
	}
	dir := core.Agent.ModuleDir
	commit, err := CheckModuleDir(dir)
	if err != nil {
		return AgentSource{}, err
	}
	acct, err := state.AccountDir(e)
	if err != nil {
		return AgentSource{}, &provider.ErrUnavailable{Why: err.Error(), Local: true}
	}
	out := filepath.Join(acct, "agents", arch)
	if err := os.MkdirAll(out, 0o700); err != nil {
		return AgentSource{}, &provider.ErrUnavailable{Why: err.Error(), Local: true}
	}
	key := filepath.Join(out, "tree-"+commit)
	if b, err := os.ReadFile(key); err == nil {
		if sha := strings.TrimSpace(string(b)); shaRE.MatchString(sha) {
			cached := filepath.Join(out, filepath.Base(sha))
			if _, err := os.Stat(cached); err == nil {
				return AgentSource{SHA256: sha, Path: cached, Commit: commit, Arch: arch}, nil
			}
		}
	}
	tmp, err := os.CreateTemp(out, "build-*")
	if err != nil {
		return AgentSource{}, &provider.ErrUnavailable{Why: err.Error(), Local: true}
	}
	tmp.Close()
	defer os.Remove(tmp.Name())
	env := []string{"CGO_ENABLED=0", "GOOS=linux", "GOARCH=" + arch}
	argv := []string{"go", "build", "-trimpath", "-ldflags=-buildid=", "-o", tmp.Name(), "./cmd/bitc"}
	if msg, err := run(dir, env, argv...); err != nil {
		return AgentSource{}, &provider.ErrIneligible{Why: "agent.module_dir: go build: " + err.Error() + ": " + string(bytes.TrimSpace(msg))}
	}
	sha, err := fileSHA(tmp.Name())
	if err != nil {
		return AgentSource{}, &provider.ErrIneligible{Why: "agent.module_dir: " + err.Error()}
	}
	final := filepath.Join(out, sha)
	if err := os.Chmod(tmp.Name(), 0o700); err != nil { //nolint:gosec // an executable the host sends; owner-only
		return AgentSource{}, err
	}
	if err := os.Rename(tmp.Name(), final); err != nil {
		return AgentSource{}, err
	}
	if err := os.WriteFile(key, []byte(sha+"\n"), 0o600); err != nil {
		return AgentSource{}, err
	}
	return AgentSource{SHA256: sha, Path: final, Commit: commit, Arch: arch}, nil
}

func fileSHA(p string) (string, error) {
	f, err := os.Open(p)
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

// FetchAgent downloads a release agent (a variable so tests never touch the network). The size
// is bounded by the caller's hash check, not trusted.
var FetchAgent = func(url string) ([]byte, error) {
	if !strings.HasPrefix(url, "https://") {
		return nil, errors.New("not an https url")
	}
	c := &http.Client{Timeout: 5 * time.Minute}
	resp, err := c.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", url, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 256<<20))
}

// ResolveAgentFile returns the sha256 and the local path of the agent binary for arch whose bytes
// a fixed-capacity provider sends over its session (providers delta C.4 item 4), whatever the
// source: a url source's paths[arch], a module_dir build, a release asset (downloaded once into
// the account's agents directory and checked against checksums.txt), or, under [test] agent_url,
// this binary. Nothing is fetched by the host itself.
func ResolveAgentFile(e *config.Effective, core *config.Provider, arch string) (sha, path string, err error) {
	a := core.Agent
	switch {
	case e.Test != nil && e.Test.AgentURL != "":
		self, err := os.Executable()
		if err != nil {
			return "", "", err
		}
		sha, err := fileSHA(self)
		return sha, self, err
	case a.Source == "build" && a.ModuleDir != "":
		src, err := BuildAgent(e, core, arch, nil)
		return src.SHA256, src.Path, err
	case a.Source == "build":
		return "", "", &provider.ErrIneligible{Why: `agent source "build" with a pinned commit is built in the pod; a fixed host takes agent.module_dir`}
	case a.Source == "url":
		path := a.Paths[arch]
		if path == "" {
			return "", "", &provider.ErrIneligible{Why: "agent paths has no " + arch + " entry"}
		}
		sha, err := fileSHA(path)
		if err != nil {
			return "", "", &provider.ErrIneligible{Why: "agent.paths." + arch + ": " + err.Error()}
		}
		return sha, path, nil
	case version.Release == "true" && runtime.GOOS == "linux" && runtime.GOARCH == arch:
		self, err := os.Executable()
		if err != nil {
			return "", "", err
		}
		sha, err := fileSHA(self)
		return sha, self, err
	case version.Release == "true":
		url, want, err := ResolveRelease(version.Version, arch, FetchChecksums)
		if err != nil {
			return "", "", err
		}
		acct, err := state.AccountDir(e)
		if err != nil {
			return "", "", &provider.ErrUnavailable{Why: err.Error(), Local: true}
		}
		dir := filepath.Join(acct, "agents", arch)
		cached := filepath.Join(dir, want)
		if got, err := fileSHA(cached); err == nil && got == want {
			return want, cached, nil
		}
		b, err := FetchAgent(url)
		if err != nil {
			return "", "", &provider.ErrTransient{Why: "agent download: " + err.Error()}
		}
		if sum := sha256.Sum256(b); hex.EncodeToString(sum[:]) != want {
			return "", "", &provider.ErrIneligible{Why: "agent download does not match checksums.txt"}
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return "", "", &provider.ErrUnavailable{Why: err.Error(), Local: true}
		}
		if err := state.WriteFileAtomic(cached, b); err != nil {
			return "", "", &provider.ErrUnavailable{Why: err.Error(), Local: true}
		}
		return want, cached, nil
	}
	return "", "", &provider.ErrIneligible{Why: `agent source "release" needs a release build of bitc; set agent = { source = "url", ... } or module_dir`}
}
