package spritesfake

import (
	"testing"
	"time"
)

// An open exec session keeps a running Sprite active, unless SessionsDontHold; a live Task always does.
func TestActiveCountsOpenSessionsUnlessSessionsDontHold(t *testing.T) {
	now := time.Now()
	f := &Fake{}
	s := &sprite{sessions: 1, tasks: map[string]task{}}
	if !f.active(s, now) {
		t.Fatal("an open session did not hold the Sprite")
	}
	f.o.SessionsDontHold = true
	if f.active(s, now) {
		t.Fatal("an open session held the Sprite with SessionsDontHold")
	}
	s.tasks["t"] = task{expires: now.Add(time.Minute)}
	if !f.active(s, now) {
		t.Fatal("a live Task did not hold the Sprite")
	}
}
