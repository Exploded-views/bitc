package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"time"
)

// SlotOwner is slots/<i>/owner (providers delta C.3.1).
type SlotOwner struct {
	Name       string `json:"name"`
	Nonce      string `json:"nonce"`
	Tag        string `json:"tag,omitempty"`
	Job        string `json:"job,omitempty"`
	Created    int64  `json:"created"`
	LeaseUntil int64  `json:"lease_until"`
}

// SlotSys is every destructive action of a slot's teardown steps (C.5 step 8), so the lease
// logic can be tested without root.
type SlotSys interface {
	UID(slot int) (uid, gid int, err error)
	KillSlot(slot, uid int) error
	DropNet(slot int) error
	ClearScheduling(slot, uid int) error
	RemoveIPC(uid int) error
	RemoveStray(slot, uid int) (int, error)
	UnmountKey(slot int) error
	Recreate(slot, uid, gid int) error
}

// SlotHost is the host's slot state under Root.
type SlotHost struct {
	Root string
	Conf HostConf
	Now  func() time.Time
	Sys  SlotSys
	Log  io.Writer
}

// BusyError: a slot's lease is still ahead.
type BusyError struct{ Slot int }

func (e *BusyError) Error() string { return fmt.Sprintf("slot %d busy on host", e.Slot) }

// NotLeasedError: the slot's owner is not the caller.
type NotLeasedError struct {
	Slot int
	Name string
}

func (e *NotLeasedError) Error() string {
	return fmt.Sprintf("slot %d not leased to %s", e.Slot, e.Name)
}

func (h *SlotHost) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now()
}

// StateDir is /var/lib/bitc under the host's root.
func (h *SlotHost) StateDir() string { return filepath.Join(h.Root, "/var/lib/bitc") }

func (h *SlotHost) slotDir(i int) string { return filepath.Join(h.StateDir(), "slots", fmt.Sprint(i)) }

// ResultsDir is where the agent keeps one directory per job.
func (h *SlotHost) ResultsDir() string { return filepath.Join(h.StateDir(), "results") }

func (h *SlotHost) readOwner(i int) (*SlotOwner, error) {
	b, err := os.ReadFile(filepath.Join(h.slotDir(i), "owner"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var o SlotOwner
	if json.Unmarshal(b, &o) != nil || o.Name == "" {
		return nil, nil // a torn or empty file holds nobody
	}
	return &o, nil
}

func (h *SlotHost) writeOwner(i int, o SlotOwner) error {
	b, err := json.Marshal(o)
	if err != nil {
		return err
	}
	p := filepath.Join(h.slotDir(i), "owner")
	if err := os.WriteFile(p+".part", b, 0o600); err != nil {
		return err
	}
	return os.Rename(p+".part", p)
}

// lock takes every slot's mutex in ascending order (so two callers cannot deadlock) and returns
// the release. The mutex is held only while a verb reads or changes owner, never across ssh calls.
func (h *SlotHost) lock(slots []int) (func(), error) {
	order := append([]int(nil), slots...)
	sort.Ints(order)
	var files []*os.File
	unlock := func() {
		for _, f := range files {
			f.Close() // closing drops the flock
		}
	}
	for _, i := range order {
		if err := os.MkdirAll(h.slotDir(i), 0o700); err != nil {
			unlock()
			return nil, err
		}
		f, err := os.OpenFile(filepath.Join(h.slotDir(i), "lock"), os.O_RDWR|os.O_CREATE, 0o600)
		if err != nil {
			unlock()
			return nil, err
		}
		files = append(files, f)
		deadline := time.Now().Add(15 * time.Second)
		for {
			err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
			if err == nil {
				break
			}
			if err != syscall.EWOULDBLOCK || time.Now().After(deadline) {
				unlock()
				return nil, fmt.Errorf("slot %d mutex: %w", i, err)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	return unlock, nil
}

// clean is the teardown steps of C.5 step 8 against one slot, without freeing it.
func (h *SlotHost) clean(i int) error {
	uid, gid, err := h.Sys.UID(i)
	if err != nil {
		return err
	}
	if err := h.Sys.KillSlot(i, uid); err != nil {
		return fmt.Errorf("slot %d: kill: %w", i, err)
	}
	if err := h.removeUpload(i); err != nil {
		return fmt.Errorf("slot %d: upload dir: %w", i, err)
	}
	if err := h.Sys.DropNet(i); err != nil {
		return fmt.Errorf("slot %d: network: %w", i, err)
	}
	if err := h.Sys.ClearScheduling(i, uid); err != nil {
		return fmt.Errorf("slot %d: cron, at, linger: %w", i, err)
	}
	if err := h.Sys.RemoveIPC(uid); err != nil {
		return fmt.Errorf("slot %d: ipc: %w", i, err)
	}
	if n, err := h.Sys.RemoveStray(i, uid); err != nil {
		return fmt.Errorf("slot %d: stray files: %w", i, err)
	} else if n > 0 && h.Log != nil {
		fmt.Fprintf(h.Log, "pod: removed %d stray files of bitc-s%d\n", n, i)
	}
	if err := h.Sys.UnmountKey(i); err != nil {
		return fmt.Errorf("slot %d: key tmpfs: %w", i, err)
	}
	if err := h.Sys.Recreate(i, uid, gid); err != nil {
		return fmt.Errorf("slot %d: dirs: %w", i, err)
	}
	return nil
}

// Prepare leases the slots to name (all or none) and cleans each (C.5 step 3).
func (h *SlotHost) Prepare(slots []int, name, nonce, tag string, lease time.Duration) error {
	unlock, err := h.lock(slots)
	if err != nil {
		return err
	}
	defer unlock()
	now := h.now()
	for _, i := range slots {
		o, err := h.readOwner(i)
		if err != nil {
			return err
		}
		if o != nil && o.LeaseUntil > now.Unix() {
			return &BusyError{Slot: i}
		}
	}
	for _, i := range slots {
		if err := h.writeOwner(i, SlotOwner{Name: name, Nonce: nonce, Tag: tag, Created: now.Unix(), LeaseUntil: now.Add(lease).Unix()}); err != nil {
			return err
		}
		if err := h.clean(i); err != nil {
			return err
		}
	}
	return nil
}

// Handoff checks under the slots' mutexes that they are leased to name with nonce, calls read
// for the job's id and deadline, then extends the lease to that deadline (C.5 step 5).
func (h *SlotHost) Handoff(slots []int, name, nonce string, read func() (job string, deadline time.Time, err error)) error {
	unlock, err := h.lock(slots)
	if err != nil {
		return err
	}
	defer unlock()
	now := h.now()
	owners := map[int]*SlotOwner{}
	for _, i := range slots {
		o, err := h.readOwner(i)
		if err != nil {
			return err
		}
		if o == nil || o.Name != name || o.Nonce != nonce || o.LeaseUntil <= now.Unix() {
			return &NotLeasedError{Slot: i, Name: name}
		}
		owners[i] = o
	}
	job, deadline, err := read()
	if err != nil {
		return err
	}
	for _, i := range slots {
		o := *owners[i]
		o.Job, o.LeaseUntil = job, deadline.Unix()
		if err := h.writeOwner(i, o); err != nil {
			return err
		}
	}
	return nil
}

// Teardown frees the slots owned by name; it returns "torn", or "gone" when none was (C.3.6: a
// stale cleanup never touches a later job that leased the same slot).
func (h *SlotHost) Teardown(slots []int, name string) (string, error) {
	unlock, err := h.lock(slots)
	if err != nil {
		return "", err
	}
	defer unlock()
	torn := false
	for _, i := range slots {
		o, err := h.readOwner(i)
		if err != nil {
			return "", err
		}
		if o == nil || o.Name != name {
			continue
		}
		torn = true
		if o.Job != "" && jobIDRE.MatchString(o.Job) { // the agent first, so it cannot write on
			if err := Teardown(h.ResultsDir(), o.Job, io.Discard); err != nil {
				return "", err
			}
		}
		if err := h.clean(i); err != nil {
			return "", err
		}
		if err := os.Remove(filepath.Join(h.slotDir(i), "owner")); err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
	}
	if torn {
		return "torn", nil
	}
	return "gone", nil
}

// Release is Teardown for the agent's own end of life: only if name still owns the slots.
func (h *SlotHost) Release(slots []int, name string) error {
	_, err := h.Teardown(slots, name)
	return err
}

// Owners returns the owner of every slot (nil for a free one).
func (h *SlotHost) Owners() (map[int]*SlotOwner, error) {
	m := map[int]*SlotOwner{}
	for i := 1; i <= h.Conf.Slots; i++ {
		o, err := h.readOwner(i)
		if err != nil {
			return nil, err
		}
		m[i] = o
	}
	return m, nil
}
