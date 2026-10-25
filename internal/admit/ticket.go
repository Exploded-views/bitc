package admit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/Exploded-views/bitc/internal/lock"
	"github.com/Exploded-views/bitc/internal/proc"
	"github.com/Exploded-views/bitc/internal/state"
)

// Ticket is a waiter's file, flocked by its owner for its whole life (spec 6.2).
type Ticket struct {
	Pid         int              `json:"pid"`
	Start       uint64           `json:"start"` // /proc/<pid>/stat field 22
	Argv        []string         `json:"argv"`
	Cwd         string           `json:"cwd"`
	Job         string           `json:"job"`
	Class       int              `json:"class"` // 0 high, 1 normal
	Nested      bool             `json:"nested"`
	Depth       int              `json:"depth"`
	State       string           `json:"state"` // "waiting" | "running"
	GroupWait   []string         `json:"group_wait,omitempty"`
	Slot        int              `json:"slot,omitempty"`
	Groups      map[string][]int `json:"groups,omitempty"`
	Inherited   []string         `json:"inherited_groups,omitempty"` // groups an ancestor holds; never unlocked here
	Token       string           `json:"token"`
	Leader      int              `json:"leader,omitempty"` // the command's pid
	Pgid        int              `json:"pgid,omitempty"`   // 0: the command shares the caller's group (TTY)
	LeaderStart uint64           `json:"leader_start,omitempty"`
	HoldStarted int64            `json:"hold_started,omitempty"` // unix ns
	WaitStarted int64            `json:"wait_started"`           // unix ns
}

// TicketFile is a ticket read from disk with its name.
type TicketFile struct {
	Name string
	Pool string
	T    Ticket
}

var ticketNameRE = regexp.MustCompile(`^[01]-[0-9]{19}-[0-9a-f]{8}\.json$`)

// own is the caller's ticket: its open, flocked file and its content.
type own struct {
	f    *lock.File
	name string
	pool string
	t    Ticket
}

// newTicket creates tickets/<class>-<unix_ns>-<rand8>.json by temp file + flock + rename.
func newTicket(l state.Layout, pool string, t Ticket) (*own, error) {
	dir := l.Tickets(pool)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	tmp := filepath.Join(dir, ".tmp-"+randHex(8))
	f, err := lock.Open(tmp)
	if err != nil {
		return nil, err
	}
	if ok, err := f.TryLock(); !ok || err != nil {
		f.Close()
		os.Remove(tmp)
		if err == nil {
			err = errors.New("lock busy")
		}
		return nil, fmt.Errorf("cannot lock new ticket %s: %w", tmp, err)
	}
	now := time.Now()
	t.WaitStarted = now.UnixNano()
	o := &own{f: f, pool: pool, t: t,
		name: fmt.Sprintf("%d-%019d-%s.json", t.Class, now.UnixNano(), randHex(4))}
	if err := o.write(); err != nil {
		f.Close()
		os.Remove(tmp)
		return nil, err
	}
	final := filepath.Join(dir, o.name)
	if err := os.Rename(tmp, final); err != nil {
		f.Close()
		os.Remove(tmp)
		return nil, err
	}
	f.Path = final
	return o, nil
}

func (o *own) write() error {
	b, err := json.Marshal(o.t)
	if err != nil {
		return err
	}
	return o.f.Rewrite(b)
}

// drop unlinks then closes the ticket (the close releases its flock).
func (o *own) drop() {
	os.Remove(o.f.Path)
	o.f.Close()
}

// ReadTickets returns the live tickets of pool in (class, time) order, pruning dead ones is
// not done here (callers hold admit.lock and call prune first).
func ReadTickets(l state.Layout, pool string) []TicketFile {
	ents, _ := os.ReadDir(l.Tickets(pool))
	var out []TicketFile
	for _, e := range ents {
		if !ticketNameRE.MatchString(e.Name()) {
			continue
		}
		b, err := os.ReadFile(filepath.Join(l.Tickets(pool), e.Name()))
		if err != nil {
			continue
		}
		var t Ticket
		if json.Unmarshal(b, &t) != nil {
			continue
		}
		out = append(out, TicketFile{Name: e.Name(), Pool: pool, T: t})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// prune removes tickets whose owner is dead (their flock is free). A dead running ticket's
// process group is killed first (spec 6.2). It runs under admit.lock.
func prune(l state.Layout, pool string, notes func(note, pool, job string), removed func(Ticket)) {
	dir := l.Tickets(pool)
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		name := e.Name()
		path := filepath.Join(dir, name)
		if strings.HasPrefix(name, ".tmp-") {
			if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > 10*time.Second && !lock.Held(path) {
				os.Remove(path)
			}
			continue
		}
		if !ticketNameRE.MatchString(name) {
			continue
		}
		f, err := lock.Open(path)
		if err != nil {
			continue
		}
		ok, _ := f.TryLock()
		if !ok {
			f.Close()
			continue
		}
		b, _ := os.ReadFile(path)
		var t Ticket
		_ = json.Unmarshal(b, &t)
		if t.State == "running" && (t.Pgid > 0 || t.Leader > 0) {
			reap(t)
			notes(fmt.Sprintf("bitc: reaped orphaned %s job %s (supervisor died)", pool, t.Job), pool, t.Job)
		}
		os.Remove(path)
		f.Close()
		if removed != nil {
			removed(t)
		}
	}
}

// reap kills a dead hold's command. With a group of its own (pgid > 0): the whole group if its
// leader still matches leader_start; if the leader is gone but /proc/<pgid> is absent, every
// process of that pgid started since the hold; if the id now belongs to another process, nothing.
// Without one (a TTY run in the caller's group, pgid 0): only the command, if its pid and start
// still match, and its descendants; never the rest of the caller's group, which can hold
// unrelated processes (sibling make jobs, later members of a pipeline).
func reap(t Ticket) {
	if t.Pgid > 0 {
		if st, err := proc.ReadStat(t.Pgid); err == nil {
			// /proc/<pgid> exists: the leader is still there, or the id was reused by a new
			// process (the old group is empty). Only the former is ours to kill.
			if st.Start == t.LeaderStart {
				_ = syscall.Kill(-t.Pgid, syscall.SIGKILL)
			}
			return
		}
		for _, pid := range proc.Pids() {
			st, err := proc.ReadStat(pid)
			if err == nil && st.Pgrp == t.Pgid && st.Start >= t.LeaderStart {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
		return
	}
	if t.Leader <= 0 {
		return
	}
	if st, err := proc.ReadStat(t.Leader); err != nil || st.Start != t.LeaderStart {
		return // the command is gone; its orphans were reparented and cannot be told apart safely
	}
	stats := map[int]proc.Stat{}
	for _, pid := range proc.Pids() {
		if st, err := proc.ReadStat(pid); err == nil {
			stats[pid] = st
		}
	}
	tree := map[int]bool{t.Leader: true}
	for grew := true; grew; {
		grew = false
		for pid, st := range stats {
			if !tree[pid] && tree[st.PPid] && st.Start >= t.LeaderStart {
				tree[pid], grew = true, true
			}
		}
	}
	for pid := range tree {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
}

// PruneDead prunes pool's dead tickets under admit.lock. It calls reaped(job) for each dead
// hold whose process group prune killed (spec 3.3) and ticket(name) for each removed ticket.
func PruneDead(l state.Layout, pool string, ticket func(name string), reaped func(job string)) {
	al, err := lock.Open(l.AdmitLock())
	if err != nil {
		return
	}
	defer al.Close()
	if al.LockWait(context.Background(), 30*time.Second) != nil {
		return
	}
	defer al.Unlock()
	before := map[string]bool{}
	for _, t := range ReadTickets(l, pool) {
		before[t.Name] = true
	}
	prune(l, pool, func(_, _, job string) { reaped(job) }, nil)
	for _, t := range ReadTickets(l, pool) {
		delete(before, t.Name)
	}
	names := make([]string, 0, len(before))
	for name := range before {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		ticket(name)
	}
}
