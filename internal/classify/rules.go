package classify

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/Exploded-views/bitc/internal/config"
)

// match reports whether rule r refuses simple command s.
func match(r config.RefuseRule, s Simple, c Context) bool {
	switch {
	case len(r.ArgvPrefix) > 0:
		if len(s.Argv) < len(r.ArgvPrefix) || filepath.Base(s.Argv[0]) != filepath.Base(r.ArgvPrefix[0]) {
			return false
		}
		for i := 1; i < len(r.ArgvPrefix); i++ {
			if s.Argv[i] != r.ArgvPrefix[i] {
				return false
			}
		}
		return true
	case len(r.NpmScript) > 0:
		for _, n := range r.NpmScript {
			if s.Script != "" && s.Script == n {
				return true
			}
		}
		return false
	case r.Tool != "":
		if filepath.Base(s.Argv[0]) != r.Tool {
			return false
		}
		for _, a := range s.Argv[1:] {
			for _, u := range r.UnlessArgs {
				if a == u || strings.HasPrefix(a, u+"=") {
					return false
				}
			}
		}
		if r.When == "always" {
			return true
		}
		return repoWide(s.Argv[1:], r.SkipOptionValues, c)
	}
	return false
}

// repoWide: no path argument, or one that is the cwd, an ancestor, /, the work-tree root or a
// top-level dir of it (spec 6.7).
func repoWide(args, skipValues []string, c Context) bool {
	skip := map[string]bool{}
	for _, s := range skipValues {
		skip[s] = true
	}
	var paths []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			paths = append(paths, args[i+1:]...)
			break
		}
		if strings.HasPrefix(a, "-") && a != "-" {
			if skip[a] {
				i++
			}
			continue
		}
		paths = append(paths, a)
	}
	if len(paths) == 0 {
		return true
	}
	for _, p := range paths {
		if isRepoWidePath(p, c) {
			return true
		}
	}
	return false
}

func isRepoWidePath(p string, c Context) bool {
	if strings.Contains(p, opaqueMarker) {
		return false
	}
	p = strings.ReplaceAll(p, pwdMarker, c.Cwd)
	if !filepath.IsAbs(p) {
		p = filepath.Join(c.Cwd, p)
	}
	if real, err := filepath.EvalSymlinks(p); err == nil {
		p = real
	} else {
		p = filepath.Clean(p)
	}
	switch {
	case p == "/" || p == c.Cwd || p == c.Worktree:
		return true
	case strings.HasPrefix(c.Cwd, p+string(os.PathSeparator)):
		return true // an ancestor of the cwd
	case c.Worktree != "" && filepath.Dir(p) == c.Worktree:
		if st, err := os.Stat(p); err == nil && st.IsDir() {
			return true // a top-level dir of the work tree
		}
	}
	return false
}
