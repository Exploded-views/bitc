package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// recSys records the destructive calls in order.
type recSys struct {
	mu    sync.Mutex
	calls []string
}

func (r *recSys) rec(f string, a ...any) {
	r.mu.Lock()
	r.calls = append(r.calls, fmt.Sprintf(f, a...))
	r.mu.Unlock()
}
func (r *recSys) UID(s int) (int, int, error)       { return 1000 + s, 1000 + s, nil }
func (r *recSys) KillSlot(s, u int) error           { r.rec("kill %d uid %d", s, u); return nil }
func (r *recSys) DropNet(s int) error               { r.rec("net %d", s); return nil }
func (r *recSys) ClearScheduling(s, u int) error    { r.rec("sched %d", s); return nil }
func (r *recSys) RemoveIPC(u int) error             { r.rec("ipc uid %d", u); return nil }
func (r *recSys) RemoveStray(s, u int) (int, error) { r.rec("stray %d", s); return 0, nil }
func (r *recSys) UnmountKey(s int) error            { r.rec("unmount %d", s); return nil }
func (r *recSys) Recreate(s, u, g int) error        { r.rec("recreate %d", s); return nil }

func newSlotHost(t *testing.T, slots int) (*SlotHost, *recSys, *time.Time) {
	t.Helper()
	root := t.TempDir()
	now := time.Unix(1_800_000_000, 0)
	sys := &recSys{}
	h := &SlotHost{Root: root, Conf: HostConf{Slots: slots, Netns: true}, Now: func() time.Time { return now }, Sys: sys}
	for i := 1; i <= slots; i++ {
		if err := os.MkdirAll(filepath.Join(root, "var/lib/bitc/slots", fmt.Sprint(i)), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return h, sys, &now
}

func ownerOf(t *testing.T, h *SlotHost, slot int) *SlotOwner {
	t.Helper()
	m, err := h.Owners()
	if err != nil {
		t.Fatal(err)
	}
	return m[slot]
}

const n1 = "0123456789abcdef0123456789abcdef"
const n2 = "fedcba9876543210fedcba9876543210"

func TestSlotPrepareWritesOwnerAndCleansResidue(t *testing.T) {
	h, sys, now := newSlotHost(t, 2)
	if err := h.Prepare([]int{1, 2}, "job-a", n1, "abcd", 10*time.Minute); err != nil {
		t.Fatal(err)
	}
	for _, s := range []int{1, 2} {
		o := ownerOf(t, h, s)
		if o == nil || o.Name != "job-a" || o.Nonce != n1 || o.Tag != "abcd" || o.Created != now.Unix() || o.LeaseUntil != now.Add(10*time.Minute).Unix() {
			t.Errorf("slot %d owner = %+v", s, o)
		}
	}
	want := "kill 1 uid 1001,net 1,sched 1,ipc uid 1001,stray 1,unmount 1,recreate 1,kill 2 uid 1002,net 2,sched 2,ipc uid 1002,stray 2,unmount 2,recreate 2"
	if got := strings.Join(sys.calls, ","); got != want {
		t.Errorf("residue pass:\n got %s\nwant %s", got, want)
	}
	b, err := os.ReadFile(filepath.Join(h.Root, "var/lib/bitc/slots/1/owner"))
	if err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(filepath.Join(h.Root, "var/lib/bitc/slots/1/owner")); fi.Mode().Perm() != 0o600 || !json.Valid(b) {
		t.Errorf("owner file mode %v, json valid %v", fi.Mode().Perm(), json.Valid(b))
	}
}

func TestSlotPrepareIsAllOrNoneAndRefusesABusySlot(t *testing.T) {
	h, sys, now := newSlotHost(t, 2)
	if err := h.Prepare([]int{2}, "job-a", n1, "", time.Minute); err != nil {
		t.Fatal(err)
	}
	sys.calls = nil
	*now = now.Add(10 * time.Second)
	err := h.Prepare([]int{1, 2}, "job-b", n2, "", time.Minute)
	var busy *BusyError
	if !errors.As(err, &busy) || busy.Slot != 2 {
		t.Fatalf("error = %v, want a BusyError for slot 2", err)
	}
	if busy.Error() != "slot 2 busy on host" {
		t.Errorf("message = %q, want %q", busy.Error(), "slot 2 busy on host")
	}
	if o := ownerOf(t, h, 1); o != nil {
		t.Errorf("slot 1 was leased although slot 2 was busy: %+v", o)
	}
	if len(sys.calls) != 0 {
		t.Errorf("a refused prepare cleaned slots: %v", sys.calls)
	}
	if o := ownerOf(t, h, 2); o == nil || o.Name != "job-a" {
		t.Errorf("the busy slot's owner changed: %+v", o)
	}
}

func TestSlotPrepareTakesAnExpiredLease(t *testing.T) {
	h, _, now := newSlotHost(t, 1)
	if err := h.Prepare([]int{1}, "job-a", n1, "", time.Minute); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(2 * time.Minute)
	if err := h.Prepare([]int{1}, "job-b", n2, "", time.Minute); err != nil {
		t.Fatalf("prepare over an expired lease: %v", err)
	}
	if o := ownerOf(t, h, 1); o == nil || o.Name != "job-b" || o.Nonce != n2 {
		t.Errorf("owner after the takeover = %+v", o)
	}
}

func TestSlotHandoffChecksNameNonceAndLease(t *testing.T) {
	h, _, now := newSlotHost(t, 2)
	if err := h.Prepare([]int{1, 2}, "job-a", n1, "", time.Minute); err != nil {
		t.Fatal(err)
	}
	read := func() (string, time.Time, error) { return "j1", now.Add(time.Hour), nil }
	var nl *NotLeasedError
	if err := h.Handoff([]int{1, 2}, "job-a", n2, read); !errors.As(err, &nl) || nl.Slot != 1 {
		t.Errorf("stale nonce: %v, want NotLeasedError for slot 1", err)
	}
	if err := h.Handoff([]int{1, 2}, "job-x", n1, read); !errors.As(err, &nl) {
		t.Errorf("wrong name: %v, want NotLeasedError", err)
	}
	if nl != nil && nl.Error() != "slot 1 not leased to job-x" {
		t.Errorf("message = %q", nl.Error())
	}
	called := false
	probe := func() (string, time.Time, error) { called = true; return "j1", now.Add(time.Hour), nil }
	_ = h.Handoff([]int{1}, "job-a", n2, probe)
	if called {
		t.Error("the payload was read although the lease check failed")
	}
	if err := h.Handoff([]int{1, 2}, "job-a", n1, read); err != nil {
		t.Fatal(err)
	}
	for _, s := range []int{1, 2} {
		if o := ownerOf(t, h, s); o == nil || o.LeaseUntil != now.Add(time.Hour).Unix() || o.Job != "j1" {
			t.Errorf("slot %d after the handoff = %+v, want lease to the deadline and job j1", s, o)
		}
	}
	*now = now.Add(2 * time.Hour)
	if err := h.Handoff([]int{1}, "job-a", n1, read); !errors.As(err, &nl) {
		t.Errorf("after the lease ran out: %v, want NotLeasedError", err)
	}
}

func TestSlotHandoffPayloadErrorKeepsTheLease(t *testing.T) {
	h, _, now := newSlotHost(t, 1)
	_ = h.Prepare([]int{1}, "job-a", n1, "", time.Minute)
	boom := errors.New("truncated payload")
	if err := h.Handoff([]int{1}, "job-a", n1, func() (string, time.Time, error) { return "", time.Time{}, boom }); !errors.Is(err, boom) {
		t.Errorf("error = %v, want the payload error", err)
	}
	if o := ownerOf(t, h, 1); o == nil || o.LeaseUntil != now.Add(time.Minute).Unix() {
		t.Errorf("lease after a failed payload = %+v, want it unchanged", o)
	}
}

func TestSlotTeardownIgnoresAnotherJobsSlot(t *testing.T) {
	h, sys, _ := newSlotHost(t, 1)
	_ = h.Prepare([]int{1}, "job-b", n2, "", time.Minute)
	sys.calls = nil
	got, err := h.Teardown([]int{1}, "job-a")
	if err != nil || got != "gone" {
		t.Errorf("teardown of another job's slot = %q, %v; want gone", got, err)
	}
	if len(sys.calls) != 0 {
		t.Errorf("a stale teardown acted: %v", sys.calls)
	}
	if o := ownerOf(t, h, 1); o == nil || o.Name != "job-b" {
		t.Errorf("owner = %+v, want job-b untouched", o)
	}
	if got, _ := h.Teardown([]int{1}, "job-b"); got != "torn" {
		t.Errorf("own teardown = %q, want torn", got)
	}
	if got, _ := h.Teardown([]int{1}, "job-b"); got != "gone" {
		t.Errorf("second teardown = %q, want gone", got)
	}
}

func TestSlotTeardownRunsEveryStepAndFreesTheSlot(t *testing.T) {
	h, sys, _ := newSlotHost(t, 1)
	_ = h.Prepare([]int{1}, "job-a", n1, "", time.Minute)
	_ = h.Handoff([]int{1}, "job-a", n1, func() (string, time.Time, error) { return "j1", time.Unix(1_800_003_600, 0), nil })
	res := filepath.Join(h.Root, "var/lib/bitc/results/j1")
	if err := os.MkdirAll(res, 0o700); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(res, "result.json"), []byte("{}"), 0o600)
	sys.calls = nil
	if got, err := h.Teardown([]int{1}, "job-a"); err != nil || got != "torn" {
		t.Fatalf("teardown = %q, %v", got, err)
	}
	if want := "kill 1 uid 1001,net 1,sched 1,ipc uid 1001,stray 1,unmount 1,recreate 1"; strings.Join(sys.calls, ",") != want {
		t.Errorf("steps:\n got %s\nwant %s", strings.Join(sys.calls, ","), want)
	}
	if o := ownerOf(t, h, 1); o != nil {
		t.Errorf("owner left after teardown: %+v", o)
	}
	if _, err := os.Stat(res); err == nil {
		t.Error("the job's results directory is still there")
	}
}

func TestSlotReleaseOnlyActsForTheCurrentOwner(t *testing.T) {
	h, sys, _ := newSlotHost(t, 1)
	_ = h.Prepare([]int{1}, "job-b", n2, "", time.Minute)
	sys.calls = nil
	if err := h.Release([]int{1}, "job-a"); err != nil || len(sys.calls) != 0 {
		t.Errorf("release by a stale agent: %v, calls %v; want a no-op", err, sys.calls)
	}
	if err := h.Release([]int{1}, "job-b"); err != nil || ownerOf(t, h, 1) != nil || len(sys.calls) == 0 {
		t.Errorf("release by the owner: %v, owner %+v, steps %v; want the steps run and the slot freed", err, ownerOf(t, h, 1), sys.calls)
	}
}

func TestSlotOwnersListsFreeAndLeasedSlots(t *testing.T) {
	h, _, _ := newSlotHost(t, 3)
	_ = h.Prepare([]int{2}, "job-a", n1, "tg", time.Minute)
	m, err := h.Owners()
	if err != nil || len(m) != 3 || m[1] != nil || m[3] != nil || m[2] == nil || m[2].Tag != "tg" {
		t.Errorf("owners = %v, %v; want only slot 2 held, tag tg", m, err)
	}
}

func TestSlotMutexSerialisesPrepares(t *testing.T) {
	h, _, _ := newSlotHost(t, 1)
	var wg sync.WaitGroup
	results := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results <- h.Prepare([]int{1}, fmt.Sprintf("job-%d", i), n1, "", time.Minute)
		}(i)
	}
	wg.Wait()
	close(results)
	won := 0
	for err := range results {
		var busy *BusyError
		switch {
		case err == nil:
			won++
		case !errors.As(err, &busy):
			t.Errorf("unexpected error %v", err)
		}
	}
	if won != 1 {
		t.Errorf("%d prepares won the one slot, want 1", won)
	}
}
