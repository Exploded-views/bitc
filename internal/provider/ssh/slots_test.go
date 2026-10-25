package ssh

import (
	"slices"
	"testing"
)

func TestAcquireTakesTheLowestFreeSlots(t *testing.T) {
	l := &SlotLocks{Dir: t.TempDir(), N: 3}
	a, busy, err := l.Acquire(1)
	if err != nil || a == nil || !slices.Equal(a.Slots, []int{1}) || busy != 0 {
		t.Fatalf("first: %v %d %v", a, busy, err)
	}
	b, _, err := l.Acquire(1)
	if err != nil || b == nil || !slices.Equal(b.Slots, []int{2}) {
		t.Fatalf("second takes slot 2: %v %v", b, err)
	}
	a.Release()
	c, _, err := l.Acquire(2)
	if err != nil || c == nil || !slices.Equal(c.Slots, []int{1, 3}) {
		t.Fatalf("a freed slot 1 is the lowest again, then 3: %v %v", c, err)
	}
}

func TestAcquireIsAllOrNone(t *testing.T) {
	l := &SlotLocks{Dir: t.TempDir(), N: 2}
	a, _, _ := l.Acquire(1)
	if a == nil {
		t.Fatal("no first lease")
	}
	b, busy, err := l.Acquire(2)
	if err != nil || b != nil || busy != 1 {
		t.Fatalf("two slots with one busy: lease %v, busy %d, err %v", b, busy, err)
	}
	c, _, _ := l.Acquire(1) // nothing was left held by the failed attempt
	if c == nil || !slices.Equal(c.Slots, []int{2}) {
		t.Fatalf("slot 2 must still be free: %v", c)
	}
}

func TestAcquireWhenAllAreBusyReportsTheCount(t *testing.T) {
	l := &SlotLocks{Dir: t.TempDir(), N: 2}
	for i := 0; i < 2; i++ {
		held, _, err := l.Acquire(1)
		if err != nil || held == nil {
			t.Fatal(err)
		}
		defer held.Release()
	}
	b, busy, err := l.Acquire(1)
	if err != nil || b != nil || busy != 2 {
		t.Fatalf("lease %v, busy %d, err %v", b, busy, err)
	}
}

func TestAnotherLockerSeesTheHeldSlot(t *testing.T) { // a second process opens the same files
	dir := t.TempDir()
	a := &SlotLocks{Dir: dir, N: 2}
	b := &SlotLocks{Dir: dir, N: 2}
	la, _, _ := a.Acquire(1)
	lb, _, _ := b.Acquire(1)
	if la == nil || lb == nil || slices.Equal(la.Slots, lb.Slots) {
		t.Fatalf("two lockers took %v and %v", la, lb)
	}
}

func TestResourceIDRoundTrips(t *testing.T) {
	id := ResourceID("127.0.0.1", []int{1, 3}, "bitc-0123456789abcdef-heavy-s1-aabbccdd-1700000000")
	if id != "127.0.0.1/s1+s3/bitc-0123456789abcdef-heavy-s1-aabbccdd-1700000000" {
		t.Fatalf("id %q", id)
	}
	host, slots, name, ok := ParseResourceID(id)
	if !ok || host != "127.0.0.1" || !slices.Equal(slots, []int{1, 3}) || name != "bitc-0123456789abcdef-heavy-s1-aabbccdd-1700000000" {
		t.Fatalf("parsed %q %v %q %v", host, slots, name, ok)
	}
	for _, bad := range []string{"", "x", "h/s1", "h/1/n", "h/s0/n", "h/s2+s1/n", "h/s1/n/m", "h/s65/n", "h/s1+s1/n"} {
		if _, _, _, ok := ParseResourceID(bad); ok {
			t.Errorf("%q parsed", bad)
		}
	}
}

func TestSlotsArg(t *testing.T) {
	if SlotsArg([]int{2}) != "2" || SlotsArg([]int{1, 2, 4}) != "1+2+4" {
		t.Fatalf("%q %q", SlotsArg([]int{2}), SlotsArg([]int{1, 2, 4}))
	}
}
