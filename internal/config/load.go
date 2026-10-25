package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"github.com/pelletier/go-toml/v2"
	"golang.org/x/sys/unix"

	"github.com/Exploded-views/bitc/internal/exit"
	"github.com/Exploded-views/bitc/internal/version"
)

// problems collects every config error so `config check` can list them all.
type problems struct{ list []string }

func (p *problems) addf(format string, a ...any) { p.list = append(p.list, fmt.Sprintf(format, a...)) }
func (p *problems) err() error {
	if len(p.list) == 0 {
		return nil
	}
	lines := make([]string, len(p.list))
	for i, s := range p.list {
		lines[i] = "bitc: " + s
	}
	return &exit.Error{Code: exit.Config, Line: strings.Join(lines, "\n"), Outcome: "config", Reason: p.list[0]}
}

type layer struct {
	file   string
	schema fileSchema
	repo   bool
}

// Load reads built-in defaults, the host file, one repo file and BITC_* env (spec 5.1-5.6).
// File problems are 78 (all listed); malformed BITC_* values are 64.
func Load(o LoadOptions) (*Effective, error) {
	env := envMap(o.Env)
	e := &Effective{Pools: map[string]*Pool{}, Groups: map[string]Group{}, Providers: map[string]*Provider{},
		Vars: Vars{}, Sources: map[string]string{}}
	cwd, err := filepath.EvalSymlinks(o.Cwd)
	if err != nil {
		cwd = filepath.Clean(o.Cwd)
	}
	e.Cwd, e.LogicalCwd = cwd, cwd
	if pwd := env["PWD"]; pwd != "" && filepath.IsAbs(pwd) {
		if real, err := filepath.EvalSymlinks(pwd); err == nil && real == cwd {
			e.LogicalCwd = filepath.Clean(pwd)
		}
	}
	var p problems
	hostPath := firstNonEmpty(o.Flags.ConfigFile, env["BITC_CONFIG"])
	if hostPath == "" {
		xdg := firstNonEmpty(env["XDG_CONFIG_HOME"], filepath.Join(env["HOME"], ".config"))
		hostPath = filepath.Join(xdg, "bitc", "bitc.toml")
	}
	host, hostExists := readLayer(&p, expandHome(hostPath, env), false)
	if hostExists {
		e.HostFile = host.file
	}
	applyHost(&p, e, host, env)
	e.Worktree, e.RepoFile = findRepo(cwd, env, o.RepoFile)
	sum := sha256.Sum256([]byte(e.Worktree))
	e.WT8 = hex.EncodeToString(sum[:])[:8]
	matchGrant(&p, e)
	var repo *layer
	if e.RepoFile == "" && e.Grant != nil && e.Grant.Config != "" {
		e.RepoFile = expandHome(e.Grant.Config, env)
	}
	if e.RepoFile != "" {
		r, ok := readLayer(&p, e.RepoFile, true)
		if ok {
			repo = &r
		}
	}
	mergePools(&p, e, host, repo)
	applyEnv(&p, e, env, o)
	e.GlobalFlags = o.Flags
	if o.Flags.Verbose > 0 && e.LogLevel == "info" { // an explicit BITC_LOG=quiet wins over -v
		e.LogLevel = "debug"
	}
	checkTemplates(&p, e)
	checkRemoteEnv(&p, e)
	checkTestGate(&p, e)
	if err := p.err(); err != nil {
		return nil, err
	}
	if err := usageErr(e); err != nil {
		return nil, err
	}
	return e, nil
}

// maxConfigBytes caps a config file; a larger one is a config error.
const maxConfigBytes = 1 << 20

// readLayer reads, version-checks, permission-checks and strictly decodes one file.
func readLayer(p *problems, path string, repo bool) (layer, bool) {
	l := layer{file: path, repo: repo}
	// O_NONBLOCK: opening a FIFO or device must not block; the fd is then checked with fstat
	// (regular file, owner, mode) and read through a size cap, never by path (audit #18).
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if errors.Is(err, fs.ErrNotExist) {
		if repo {
			p.addf("%s: no such repo file", path)
		}
		return l, false
	}
	if err != nil {
		p.addf("%s: %v", path, err)
		return l, false
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		p.addf("%s: %v", path, err)
		return l, false
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		p.addf("%s is not a regular file", path)
		return l, false
	}
	b, err := io.ReadAll(io.LimitReader(file, maxConfigBytes+1))
	if err != nil {
		p.addf("%s: %v", path, err)
		return l, false
	}
	if len(b) > maxConfigBytes {
		p.addf("%s is larger than %d bytes", path, maxConfigBytes)
		return l, false
	}
	var head struct {
		Version    *int64  `toml:"version"`
		MinVersion *string `toml:"min_version"`
	}
	if err := toml.Unmarshal(b, &head); err != nil {
		p.addf("%s: %s", path, tomlErr(err))
		return l, false
	}
	if head.MinVersion != nil && !versionAtLeast(version.Current(), *head.MinVersion) {
		p.addf("%s needs bitc >= %s (this is %s)", path, *head.MinVersion, version.Current())
		return l, false
	}
	if head.Version == nil || *head.Version != 1 {
		p.addf("%s: version = 1 is required as the file's first key", path)
		return l, false
	}
	dec := toml.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	decErr := dec.Decode(&l.schema)
	credential := false // spec 5.1: decided from the decoded providers, never from the bytes
	for _, prov := range l.schema.Providers {
		_, inline := prov["api_key"]
		credential = credential || inline
	}
	checkFileMode(p, path, &st, credential)
	if err := decErr; err != nil {
		var sm *toml.StrictMissingError
		if errors.As(err, &sm) {
			for _, de := range sm.Errors {
				row, _ := de.Position()
				p.addf("%s:%d: unknown key %s", path, row, strings.Join(de.Key(), "."))
			}
		} else {
			p.addf("%s: %s", path, tomlErr(err))
		}
		return l, false
	}
	checkClasses(p, path, reflect.ValueOf(l.schema), "", "", repo)
	return l, true
}

func tomlErr(err error) string {
	var de *toml.DecodeError
	if errors.As(err, &de) {
		row, col := de.Position()
		return fmt.Sprintf("line %d column %d: %s", row, col, de.Error())
	}
	return err.Error()
}

// checkFileMode: not group/world-writable, owned by the user or root, 0600 with a credential.
func checkFileMode(p *problems, path string, st *unix.Stat_t, credential bool) {
	if st.Uid != uint32(os.Getuid()) && st.Uid != 0 {
		p.addf("%s is owned by uid %d, not by you or root", path, st.Uid)
	}
	if st.Mode&0o022 != 0 {
		// a checkout under umask 002 leaves files 0664 (git records no mode but the exec bit)
		p.addf("%s is group- or world-writable (mode %o; chmod go-w it)", path, st.Mode&0o777)
	}
	if credential && st.Mode&0o077 != 0 {
		p.addf("%s holds a credential and must be mode 600 (it is %o)", path, st.Mode&0o777)
	}
}

// checkClasses walks the decoded schema: a host key in a repo file or a repo key in a host
// file is a problem (spec 5.3).
func checkClasses(p *problems, file string, v reflect.Value, prefix, parentClass string, repo bool) {
	if v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return
		}
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return
	}
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		name := strings.Split(f.Tag.Get("toml"), ",")[0]
		class := strings.Split(f.Tag.Get("bitc"), ",")[0]
		if class == "" {
			class = parentClass
		}
		fv := v.Field(i)
		if fv.IsZero() {
			continue
		}
		key := name
		if prefix != "" {
			key = prefix + "." + name
		}
		switch {
		case repo && class == "host":
			p.addf("%s: %s is a host-only key (repo files may not set it)", file, key)
			if key == "providers" && fv.Kind() == reflect.Map { // name the boundary key too (spec 8.2)
				for _, k := range fv.MapKeys() {
					if m, ok := fv.MapIndex(k).Interface().(map[string]any); ok {
						for _, hk := range []string{"fallback_until", "send_env", "plugin"} {
							if _, has := m[hk]; has {
								p.addf("%s: providers.%s.%s: host-only key", file, k.String(), hk)
							}
						}
					}
				}
			}
			continue
		case !repo && class == "repo":
			p.addf("%s: %s is a repo-only key (the host file may not set it)", file, key)
			continue
		}
		switch fv.Kind() {
		case reflect.Map:
			for _, k := range fv.MapKeys() {
				checkClasses(p, file, fv.MapIndex(k), key+"."+k.String(), class, repo)
			}
		case reflect.Pointer, reflect.Struct:
			checkClasses(p, file, fv, key, class, repo)
		}
	}
}

// versionAtLeast compares SemVer "X.Y.Z"; a non-SemVer build (dev) satisfies everything.
func versionAtLeast(have, need string) bool {
	h, okh := semver(have)
	n, okn := semver(need)
	if !okh || !okn {
		return !okh // dev builds pass; a malformed min_version fails
	}
	for i := 0; i < 3; i++ {
		if h[i] != n[i] {
			return h[i] > n[i]
		}
	}
	return true
}

func semver(s string) ([3]int, bool) {
	var v [3]int
	parts := strings.SplitN(strings.TrimPrefix(s, "v"), "-", 2)
	f := strings.Split(parts[0], ".")
	if len(f) != 3 {
		return v, false
	}
	for i := range f {
		n, err := strconv.Atoi(f[i])
		if err != nil {
			return v, false
		}
		v[i] = n
	}
	return v, true
}

// findRepo returns the work tree and its repo file (spec 5.1).
func findRepo(cwd string, env map[string]string, override string) (wt, repoFile string) {
	if out, err := exec.Command("git", "-C", cwd, "rev-parse", "--show-toplevel").Output(); err == nil {
		top := strings.TrimSpace(string(out))
		if real, err := filepath.EvalSymlinks(top); err == nil {
			top = real
		}
		wt = top
		if _, err := os.Stat(filepath.Join(top, ".bitc.toml")); err == nil {
			repoFile = filepath.Join(top, ".bitc.toml")
		}
	} else {
		home, _ := filepath.EvalSymlinks(env["HOME"])
		for dir := cwd; ; dir = filepath.Dir(dir) {
			if _, err := os.Stat(filepath.Join(dir, ".bitc.toml")); err == nil {
				wt, repoFile = dir, filepath.Join(dir, ".bitc.toml")
				break
			}
			if dir == home || dir == filepath.Dir(dir) {
				break
			}
		}
		if wt == "" {
			wt = cwd
		}
	}
	if override != "" {
		repoFile = override
	}
	return wt, repoFile
}

func matchGrant(p *problems, e *Effective) {
	for i := range e.Repos {
		ok, err := Match(e.Repos[i].Match, e.Worktree)
		if err != nil {
			p.addf("%s: repos[%d].match %q: %v", e.HostFile, i, e.Repos[i].Match, err)
			continue
		}
		if ok {
			e.Grant = &e.Repos[i]
			return
		}
	}
}

func expandHome(p string, env map[string]string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		return filepath.Join(env["HOME"], strings.TrimPrefix(p, "~"))
	}
	return p
}

func envMap(env []string) map[string]string {
	m := map[string]string{}
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok {
			m[k] = v
		}
	}
	if m["HOME"] == "" {
		m["HOME"] = os.Getenv("HOME")
	}
	return m
}

func firstNonEmpty(a ...string) string {
	for _, s := range a {
		if s != "" {
			return s
		}
	}
	return ""
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
