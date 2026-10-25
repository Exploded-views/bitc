package admit

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/Exploded-views/bitc/internal/config"
	"github.com/Exploded-views/bitc/internal/lock"
	"github.com/Exploded-views/bitc/internal/proc"
	"github.com/Exploded-views/bitc/internal/state"
)

// CompatLocks makes bitc and a legacy flock pool exclude each other (spec 12).
type CompatLocks interface {
	TrySlot(pool string, slot int) (release func(), ok bool, err error)
	TryUnit(group string, unit int, supervisorPid int) (release func(), ok bool, err error)
	MirrorHigh(pool string, pid int) (remove func(), err error)
	RemoveMirror(pool string, pid int)
	LegacyHigh(pool string) bool
	VerifiedClaim(env []string) (group string, ok bool)
	HoldEnv(units map[string][]int) string
}

type compat struct {
	c   *config.Compat
	dir string // <legacy_dir>/locks
}

// NewCompat returns nil when compat mode is off.
func NewCompat(c *config.Compat, _ state.Layout) CompatLocks {
	if c == nil {
		return nil
	}
	return &compat{c: c, dir: filepath.Join(c.LegacyDir, "locks")}
}

func (x *compat) lockPath(stem string, i int) string {
	return filepath.Join(x.dir, fmt.Sprintf("%s.%d.lock", stem, i))
}

func (x *compat) take(path string, pid int) (func(), bool, error) {
	if _, err := os.Stat(x.dir); err != nil {
		return nil, false, fmt.Errorf("compat: legacy lock dir %s: %w", x.dir, err)
	}
	f, err := lock.Open(path)
	if err != nil {
		return nil, false, err
	}
	if ok, _ := f.TryLock(); !ok {
		f.Close()
		return nil, false, nil
	}
	if pid > 0 {
		_ = f.Rewrite([]byte(strconv.Itoa(pid) + "\n"))
	}
	return func() { _ = f.Unlock(); f.Close() }, true, nil
}

// TrySlot: bitc slot i also needs exactly locks/<pool>.<i>.lock (same index, spec 12).
func (x *compat) TrySlot(pool string, slot int) (func(), bool, error) {
	return x.take(x.lockPath(pool, slot), 0)
}

// TryUnit: group unit j also needs locks/<stem>.<j>.lock; the supervisor pid goes inside so a
// nested legacy call can verify it. Groups without a legacy stem need nothing.
func (x *compat) TryUnit(group string, unit int, pid int) (func(), bool, error) {
	stem, ok := x.c.Groups[group]
	if !ok {
		return func() {}, true, nil
	}
	return x.take(x.lockPath(stem, unit), pid)
}

func (x *compat) MirrorHigh(pool string, pid int) (func(), error) {
	p := filepath.Join(x.dir, fmt.Sprintf("%s.prio.%d", pool, pid))
	if err := os.WriteFile(p, nil, 0o600); err != nil {
		return func() {}, err
	}
	return func() { _ = os.Remove(p) }, nil
}

// RemoveMirror deletes the priority marker of a dead high caller (its ticket was pruned).
func (x *compat) RemoveMirror(pool string, pid int) {
	_ = os.Remove(filepath.Join(x.dir, fmt.Sprintf("%s.prio.%d", pool, pid)))
}

// LegacyHigh: a live locks/<pool>.prio.<pid> marker; dead pids' markers (gone or zombie) are
// removed on sight.
func (x *compat) LegacyHigh(pool string) bool {
	ents, _ := os.ReadDir(x.dir)
	prefix := pool + ".prio."
	live := false
	for _, e := range ents {
		if !strings.HasPrefix(e.Name(), prefix) {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimPrefix(e.Name(), prefix))
		if err != nil {
			continue
		}
		if pid == os.Getpid() {
			continue // our own mirror
		}
		if st, err := proc.ReadStat(pid); err != nil || st.State == 'Z' {
			_ = os.Remove(filepath.Join(x.dir, e.Name()))
			continue
		}
		live = true
	}
	return live
}

var claimRE = regexp.MustCompile(`^([A-Za-z0-9_-]+)\.([1-9][0-9]*)$`)

// VerifiedClaim believes <hold_env>=<stem>.<j> only if that lock is held now, its file holds a
// decimal pid, and that pid is an ancestor of this process (legacy rules).
func (x *compat) VerifiedClaim(env []string) (string, bool) {
	if x.c.HoldEnv == "" {
		return "", false
	}
	var val string
	for _, kv := range env {
		if strings.HasPrefix(kv, x.c.HoldEnv+"=") {
			val = strings.TrimPrefix(kv, x.c.HoldEnv+"=")
		}
	}
	m := claimRE.FindStringSubmatch(val)
	if m == nil {
		return "", false
	}
	group := ""
	for g, stem := range x.c.Groups {
		if stem == m[1] {
			group = g
		}
	}
	j, _ := strconv.Atoi(m[2])
	path := x.lockPath(m[1], j)
	if group == "" || !lock.Held(path) {
		return "", false
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 1 {
		return "", false
	}
	st, err := proc.ReadStat(pid)
	if err != nil || !proc.IsAncestor(pid, st.Start) {
		return "", false
	}
	return group, true
}

func (x *compat) HoldEnv(units map[string][]int) string {
	if x.c.HoldEnv == "" {
		return ""
	}
	for g, js := range units {
		if stem, ok := x.c.Groups[g]; ok && len(js) > 0 {
			return fmt.Sprintf("%s=%s.%d", x.c.HoldEnv, stem, js[0])
		}
	}
	return ""
}

// LegacyHolders lists held legacy locks for status and doctor. It only observes (HeldRO): a
// legacy `flock -n` racing it never finds a free lock busy.
func LegacyHolders(c *config.Compat) []string {
	if c == nil {
		return nil
	}
	dir := filepath.Join(c.LegacyDir, "locks")
	ents, _ := os.ReadDir(dir)
	var out []string
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".lock") && lock.HeldRO(filepath.Join(dir, e.Name())) {
			b, _ := os.ReadFile(filepath.Join(dir, e.Name()))
			out = append(out, strings.TrimSuffix(e.Name(), ".lock")+" held"+pidSuffix(b))
		}
	}
	return out
}

func pidSuffix(b []byte) string {
	if s := strings.TrimSpace(string(b)); s != "" {
		if _, err := strconv.Atoi(s); err == nil {
			return " by pid " + s
		}
	}
	return ""
}
