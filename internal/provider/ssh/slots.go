package ssh

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Exploded-views/bitc/internal/lock"
)

// SlotLocks are the controller-side slot flocks, ACCOUNT/remote/ssh/slot.<i>.lock (providers
// delta C.3.1). A job's process holds them for its ssh attempt only; the host-side lease (the
// owner file) is the other half and guards a second controller and a dead one.
type SlotLocks struct {
	Dir string
	N   int
}

// Lease is a set of held slot locks.
type Lease struct {
	Slots []int
	files []*lock.File
}

// Acquire takes the lowest `need` free slots, all or none. busy counts the slots that were held
// by someone else; the lease is nil (and nothing stays held) when fewer than `need` were free.
func (l *SlotLocks) Acquire(need int) (lease *Lease, busy int, err error) {
	got := &Lease{}
	for i := 1; i <= l.N; i++ {
		f, err := lock.Open(filepath.Join(l.Dir, fmt.Sprintf("slot.%d.lock", i)))
		if err != nil {
			got.Release()
			return nil, busy, err
		}
		ok, err := f.TryLock()
		switch {
		case err != nil:
			f.Close()
			got.Release()
			return nil, busy, err
		case !ok:
			f.Close()
			busy++
		case len(got.Slots) < need:
			got.Slots, got.files = append(got.Slots, i), append(got.files, f)
		default: // free, but not needed: look on only to count the busy ones
			_ = f.Unlock()
			f.Close()
		}
	}
	if len(got.Slots) < need {
		got.Release()
		return nil, busy, nil
	}
	return got, busy, nil
}

// Release drops the locks (idempotent).
func (l *Lease) Release() {
	if l == nil {
		return
	}
	for _, f := range l.files {
		_ = f.Unlock()
		f.Close()
	}
	l.files, l.Slots = nil, nil
}

// SlotsArg renders slots as the gate writes them: "1" or "1+2".
func SlotsArg(slots []int) string {
	s := make([]string, len(slots))
	for i, n := range slots {
		s[i] = strconv.Itoa(n)
	}
	return strings.Join(s, "+")
}

// ResourceID is <host>/s<i>[+s<j>...]/<name> (providers delta C.3.5).
func ResourceID(host string, slots []int, name string) string {
	s := make([]string, len(slots))
	for i, n := range slots {
		s[i] = "s" + strconv.Itoa(n)
	}
	return host + "/" + strings.Join(s, "+") + "/" + name
}

// ParseResourceID splits a resource id: the slots must ascend, each in 1..64.
func ParseResourceID(id string) (host string, slots []int, name string, ok bool) {
	i := strings.LastIndexByte(id, '/')
	if i <= 0 || i == len(id)-1 || strings.ContainsRune(id[i+1:], '/') {
		return "", nil, "", false
	}
	name = id[i+1:]
	rest := id[:i]
	j := strings.LastIndexByte(rest, '/')
	if j <= 0 {
		return "", nil, "", false
	}
	host = rest[:j]
	for _, f := range strings.Split(rest[j+1:], "+") {
		n, err := strconv.Atoi(strings.TrimPrefix(f, "s"))
		if !strings.HasPrefix(f, "s") || err != nil || n < 1 || n > 64 || strconv.Itoa(n) != f[1:] || (len(slots) > 0 && n <= slots[len(slots)-1]) {
			return "", nil, "", false
		}
		slots = append(slots, n)
	}
	return host, slots, name, len(slots) > 0
}
