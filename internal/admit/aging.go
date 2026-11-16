package admit

import (
	"strconv"
	"time"
)

// ticketNS is the creation time recorded in a ticket's name (unix ns), which is fixed for the
// ticket's life: every caller reads the same value from the same file name, and no process keeps
// its own clock for it.
func ticketNS(name string) int64 {
	if len(name) < 21 {
		return 0
	}
	n, _ := strconv.ParseInt(name[2:21], 10, 64)
	return n
}

// Aged reports whether t is a normal-lane ticket that has waited at least after (priority.age_after;
// 0 turns aging off) and so is ordered as a high one. A ticket stamped in the future of now, as
// after a backward clock step, is not aged: the old order stands until time catches up.
func Aged(t TicketFile, now time.Time, after time.Duration) bool {
	return after > 0 && t.T.Class == 1 && now.UnixNano()-ticketNS(t.Name) >= int64(after)
}

func lane(t TicketFile, now time.Time, after time.Duration) int {
	if t.T.Class == 0 || Aged(t, now, after) {
		return 0
	}
	return 1
}

// Ahead reports whether a is ahead of b in admission order: the high lane (high and aged tickets)
// before the normal lane, and by ticket age, oldest first, within a lane.
func Ahead(a, b TicketFile, now time.Time, after time.Duration) bool {
	if la, lb := lane(a, now, after), lane(b, now, after); la != lb {
		return la < lb
	}
	return a.Name[2:] < b.Name[2:]
}
