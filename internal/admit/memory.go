package admit

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Exploded-views/bitc/internal/state"
)

// MemoryGate is the memory step of an attempt (spec 6.2); it runs under admit.lock.
type MemoryGate struct {
	ProcfsRoot       string // stands in for "/" ("" = "/")
	Floor            int64  // bytes; 0 disables
	Window           time.Duration
	ReservationsPath string
	Warn             io.Writer
	warned           bool
}

type reservation struct {
	Ticket string `json:"ticket"`
	Bytes  int64  `json:"bytes"`
	At     int64  `json:"at"` // unix ns
}

// ReadMemory returns min(MemAvailable, tightest cgroup v2 headroom) in bytes.
func ReadMemory(root string) (avail int64, readable bool, source string) {
	if root == "" {
		root = "/"
	}
	meminfo := filepath.Join(root, "proc", "meminfo")
	f, err := os.Open(meminfo)
	if err != nil {
		return 0, false, meminfo
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) >= 2 && fields[0] == "MemAvailable:" {
			kb, err := strconv.ParseInt(fields[1], 10, 64)
			if err != nil {
				return 0, false, meminfo
			}
			avail, readable, source = kb*1024, true, "MemAvailable"
		}
	}
	if !readable {
		return 0, false, meminfo
	}
	if head, ok := cgroupHeadroom(root); ok && head < avail {
		return head, true, "cgroup"
	}
	return avail, true, source
}

// cgroupHeadroom walks the cgroup v2 path of this process up to the root and returns the
// smallest memory.max - memory.current.
func cgroupHeadroom(root string) (int64, bool) {
	b, err := os.ReadFile(filepath.Join(root, "proc", "self", "cgroup"))
	if err != nil {
		return 0, false
	}
	var rel string
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "0::") {
			rel = strings.TrimPrefix(line, "0::")
		}
	}
	if rel == "" {
		return 0, false
	}
	// os.Root keeps every read inside the cgroup tree, whatever the cgroup file says.
	cg, err := os.OpenRoot(filepath.Join(root, "sys", "fs", "cgroup"))
	if err != nil {
		return 0, false
	}
	defer cg.Close()
	best, found := int64(0), false
	for dir := path.Clean("." + rel); ; dir = path.Dir(dir) {
		maxB, err1 := cg.ReadFile(path.Join(dir, "memory.max"))
		curB, err2 := cg.ReadFile(path.Join(dir, "memory.current"))
		if err1 == nil && err2 == nil && strings.TrimSpace(string(maxB)) != "max" {
			mx, e1 := strconv.ParseInt(strings.TrimSpace(string(maxB)), 10, 64)
			cur, e2 := strconv.ParseInt(strings.TrimSpace(string(curB)), 10, 64)
			if e1 == nil && e2 == nil && (!found || mx-cur < best) {
				best, found = mx-cur, true
			}
		}
		if dir == "." {
			break
		}
	}
	return best, found
}

func (g *MemoryGate) load(now time.Time) []reservation {
	var rs []reservation
	b, err := os.ReadFile(g.ReservationsPath)
	if err == nil {
		_ = json.Unmarshal(b, &rs)
	}
	var live []reservation
	for _, r := range rs {
		if now.Sub(time.Unix(0, r.At)) < g.Window {
			live = append(live, r)
		}
	}
	return live
}

func (g *MemoryGate) save(rs []reservation) error {
	b, err := json.Marshal(rs)
	if err != nil {
		return err
	}
	return state.WriteFileAtomic(g.ReservationsPath, b)
}

// Check: admit iff available - reservations within the window >= floor (spec 6.2).
func (g *MemoryGate) Check(now time.Time) (bool, string, error) {
	if g.Floor <= 0 {
		return true, "", nil
	}
	avail, ok, src := ReadMemory(g.ProcfsRoot)
	if !ok {
		if !g.warned && g.Warn != nil {
			fmt.Fprintf(g.Warn, "bitc: cannot read MemAvailable from %s; memory admission is off for this call\n", src)
			g.warned = true
		}
		return true, "", nil
	}
	var reserved int64
	for _, r := range g.load(now) {
		reserved += r.Bytes
	}
	if free := avail - reserved; free < g.Floor {
		return false, fmt.Sprintf("memory: %d MiB available, floor %d MiB", free>>20, g.Floor>>20), nil
	}
	return true, "", nil
}

// Reserve records bytes for ticket at admission.
func (g *MemoryGate) Reserve(now time.Time, ticket string, bytes int64) error {
	rs := append(g.load(now), reservation{Ticket: ticket, Bytes: bytes, At: now.UnixNano()})
	return g.save(rs)
}

// Drop removes ticket's reservation at the end of its hold.
func (g *MemoryGate) Drop(ticket string) error {
	var kept []reservation
	found := false
	for _, r := range g.load(time.Now()) {
		if r.Ticket == ticket {
			found = true
		} else {
			kept = append(kept, r)
		}
	}
	if !found {
		return nil
	}
	return g.save(kept)
}
