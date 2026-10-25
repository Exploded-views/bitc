package remote

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/Exploded-views/bitc/internal/config"
	"github.com/Exploded-views/bitc/internal/provider"
)

func init() { Preflight = FullPreflight }

func ineligible(format string, a ...any) error {
	return &provider.ErrIneligible{Why: fmt.Sprintf(format, a...)}
}

// FullPreflight runs the network-free checks in spec order, then the provider's own.
func FullPreflight(ctx context.Context, j *Job) error {
	p, wt := j.P, j.P.Eff.Worktree
	if out, err := git(wt, "rev-parse", "--is-inside-work-tree"); err != nil || strings.TrimSpace(out) != "true" {
		return ineligible("not inside a git work tree")
	}
	if out, _ := git(wt, "status", "--porcelain", "--untracked-files=all"); strings.TrimSpace(out) != "" {
		return ineligible("the work tree has uncommitted or untracked changes (a remote job runs a commit)")
	}
	if !allowed(p.Pool.Remote.Allow, p.Args.Argv) {
		return ineligible("the command matches no allow entry of [pools.%s.remote]", p.Pool.Name)
	}
	if err := argumentPaths(wt, p.Eff.Cwd, p.Args.Argv); err != nil {
		return err
	}
	for _, a := range p.Args.Argv[1:] {
		if a == "--stdin" || a == "-" {
			return ineligible("the command reads stdin (--stdin or -)")
		}
	}
	if ok, why := StdinIdle(j.Inv.Stdin); !ok {
		return ineligible("%s", why)
	}
	if err := forbidIgnored(wt, p.Eff.Remote); err != nil {
		return err
	}
	for _, a := range j.Spec.Artifacts {
		if err := artifactPath(wt, a.Path); err != nil {
			return err
		}
	}
	if needsRoot(p.Eff.Remote) && (p.Eff.Grant == nil || !p.Eff.Grant.RootSteps) {
		return ineligible("the repo's remote setup runs as root but its [[repos]] grant has no root_steps")
	}
	if CloneURLCheck != nil {
		if err := CloneURLCheck(j); err != nil {
			return err
		}
	}
	env, err := RemoteEnv(p, j.Inv.Env)
	if err != nil {
		return ineligible("remote env: %v", err)
	}
	j.Spec.Env = env
	level, _ := provider.ParseEnvLevel(j.Core.SendEnv)
	forced, plain := CallerEnvNames(p, j.Inv.Env)
	if err := SendEnvCheck(j.Type, level, j.Prov.Info().Caps.MultiJob, forced, plain); err != nil {
		return err
	}
	var poolArch []string
	if p.Pool.Remote != nil {
		poolArch = p.Pool.Remote.Arch
	}
	arch, err := SelectArch(j.Prov.Info().Arch, poolArch)
	if err != nil {
		return err
	}
	if j.Spec.Tools, err = ToolsForArch(j.Spec.Tools, arch); err != nil {
		return err
	}
	j.Spec.Arch = arch
	if err := j.Prov.Preflight(ctx, j.Spec); err != nil {
		return err
	}
	if j.Core.APIKey == "" && j.Type != "fake" && j.Core.Plugin == nil && !j.Prov.Info().Caps.Fixed { // a plugin may use secret_files only
		return &provider.ErrUnavailable{Why: "no credentials (set api_key or api_key_file)", Local: true}
	}
	return nil
}

func needsRoot(r *config.RepoRemote) bool {
	if r == nil {
		return false
	}
	for _, s := range r.Setup {
		if s.RunAs == "root" {
			return true
		}
	}
	for _, s := range r.Services {
		if s.RunAs == "root" {
			return true
		}
	}
	return false
}

// allowed: argv prefixes; argv[0] matched by basename; a plain entry matches exactly; one ending
// in "--" also allows "-- <args>"; one ending in "*" allows any trailing args (spec 8.4).
func allowed(allow [][]string, argv []string) bool {
	if len(argv) == 0 {
		return false
	}
	a := append([]string{filepath.Base(argv[0])}, argv[1:]...)
	for _, e := range allow {
		pre, tail := e, ""
		if n := len(e); n > 0 && (e[n-1] == "--" || e[n-1] == "*") {
			pre, tail = e[:n-1], e[n-1]
		}
		if len(a) < len(pre) {
			continue
		}
		match := true
		for i := range pre {
			if a[i] != pre[i] {
				match = false
				break
			}
		}
		if !match {
			continue
		}
		rest := a[len(pre):]
		switch {
		case len(rest) == 0, tail == "*", tail == "--" && rest[0] == "--":
			return true
		}
	}
	return false
}

// argumentPaths: no argument names a host path outside the tree or a git-ignored path.
func argumentPaths(wt, cwd string, argv []string) error {
	for _, a := range argv[1:] {
		if strings.HasPrefix(a, "-") {
			continue
		}
		p := a
		if !filepath.IsAbs(p) {
			if !strings.Contains(p, "/") && !strings.HasPrefix(p, ".") {
				if _, err := os.Lstat(filepath.Join(cwd, p)); err != nil {
					continue // a plain word, not a path
				}
			}
			p = filepath.Join(cwd, p)
		}
		p = filepath.Clean(p)
		if p != wt && !strings.HasPrefix(p, wt+"/") {
			if filepath.IsAbs(a) || strings.HasPrefix(a, "..") {
				return ineligible("the command names '%s', a host path outside the work tree", a)
			}
			continue
		}
		rel, _ := filepath.Rel(wt, p)
		if _, err := git(wt, "check-ignore", "-q", "--", rel); err == nil {
			return ineligible("the command names '%s', which git ignores (a remote job has only tracked files)", a)
		}
	}
	return nil
}

// forbidIgnored: present git-ignored files matching forbid_ignored (minus allow_ignored).
func forbidIgnored(wt string, r *config.RepoRemote) error {
	if r == nil || len(r.ForbidIgnored) == 0 {
		return nil
	}
	out, _ := git(wt, "ls-files", "--others", "--ignored", "--exclude-standard", "--directory")
	var hits []string
	for _, f := range strings.Split(strings.TrimSpace(out), "\n") {
		base := filepath.Base(strings.TrimSuffix(f, "/"))
		if f == "" || matchAny(r.AllowIgnored, base) {
			continue
		}
		if matchAny(r.ForbidIgnored, base) {
			hits = append(hits, f)
		}
	}
	if len(hits) > 0 {
		return ineligible("the work tree has git-ignored files a remote job would not have (%s)", strings.Join(hits, ", "))
	}
	return nil
}

func matchAny(globs []string, name string) bool {
	for _, g := range globs {
		if ok, _ := filepath.Match(g, name); ok {
			return true
		}
	}
	return false
}

// artifactPath: relative, inside, untracked, not .git, no symlink component (spec 8.4).
func artifactPath(wt, p string) error {
	clean := filepath.Clean(p)
	if filepath.IsAbs(p) || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return ineligible("artifact path '%s' must be relative and inside the work tree", p)
	}
	if clean == ".git" || strings.HasPrefix(clean, ".git/") {
		return ineligible("artifact path '%s' is under .git", p)
	}
	cur := wt
	for _, part := range strings.Split(clean, "/") {
		cur = filepath.Join(cur, part)
		if st, err := os.Lstat(cur); err == nil && st.Mode()&os.ModeSymlink != 0 {
			rel, _ := filepath.Rel(wt, cur)
			return ineligible("artifact path '%s' passes through the symbolic link '%s'", p, rel)
		}
	}
	if out, _ := git(wt, "ls-files", "--", clean); strings.TrimSpace(out) != "" {
		return ineligible("artifact path '%s' holds tracked files", p)
	}
	return nil
}

func git(dir string, args ...string) (string, error) {
	c := exec.Command("git", append([]string{"-C", dir}, args...)...)
	c.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0")
	out, err := c.Output()
	return string(out), err
}
