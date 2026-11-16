package container

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/Exploded-views/bitc/internal/lock"
	"github.com/Exploded-views/bitc/internal/state"
)

var ticketIDRE = regexp.MustCompile(`^[01]-[0-9]{19}-[0-9a-f]{8}$`)

// Found is a container of this state dir and what its ticket says about its run.
type Found struct {
	ID     string // the container id: what a removal goes by
	Name   string
	Pool   string
	Job    string // the bitc.job label
	Ticket string
	Live   lock.State // StateFree: the ticket is gone or nobody holds it (an orphan); StateHeld: a run owns it; StateUnknown: this host cannot tell
	Why    string     // with StateUnknown: why (the error looking for the ticket, or the lock this host cannot inspect)
}

// listFormat prints id|name|pool|ticket|job per container.
const listFormat = `{{.ID}}|{{.Names}}|{{.Label "` + LabelPool + `"}}|{{.Label "` + LabelTicket + `"}}|{{.Label "` + LabelJob + `"}}`

// readPools lists the state dir's pools/. A dir that is not there has no tickets, which is not an
// error; one that cannot be read says nothing about which tickets are held, which is.
func readPools(l state.Layout) ([]os.DirEntry, error) {
	ents, err := os.ReadDir(filepath.Join(l.Root, "pools"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("cannot tell which runs are live: %w", err)
	}
	// bitc makes only directories here; a stray regular file (.DS_Store, an editor's backup) is not a
	// pool, and must not turn every ticket check into "unknown". Anything else, a symlink to a pool dir
	// moved to another disk included, goes through the normal check: dropping it would read a live
	// run's ticket as free, and whatever is not a directory ends as "unknown" (kept).
	var pools []os.DirEntry
	for _, e := range ents {
		if !e.Type().IsRegular() {
			pools = append(pools, e)
		}
	}
	return pools, nil
}

// ticketState is the state of the lock on the ticket file with this id, in whichever pool holds it.
// A ticket that is not there (ENOENT) is free. Any other error looking for it (EACCES, EIO, a path
// component that is not a directory) is unknown, and so is a lock this host cannot inspect: the sweep
// keeps the container. lock.Inspect itself takes any Lstat error for "free"; that is its own
// callers' business, and this sweep checks first rather than rely on it.
func ticketState(l state.Layout, pools []os.DirEntry, id string) (lock.State, string) {
	st, why := lock.StateFree, ""
	for _, p := range pools {
		path := filepath.Join(l.Tickets(p.Name()), id+".json")
		if _, err := os.Lstat(path); err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				st, why = lock.StateUnknown, "its ticket cannot be checked: "+err.Error()
			}
			continue
		}
		switch lock.Inspect(path) {
		case lock.StateHeld:
			return lock.StateHeld, ""
		case lock.StateUnknown:
			st, why = lock.StateUnknown, "its ticket lock cannot be inspected on this host"
		}
	}
	return st, why
}

// list is every container (running or not) that carries this state dir's label, with its ticket's state.
func list(ctx context.Context, runtime string, l state.Layout, pools []os.DirEntry, env []string) ([]Found, error) {
	ctx, cancel := context.WithTimeout(ctx, removeLimit)
	defer cancel()
	cmd := exec.CommandContext(ctx, runtime, "ps", "-a", "--no-trunc", "--filter", "label="+LabelState+"="+StateHash(l.Root), "--format", listFormat)
	cmd.Env = env
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(ee.Stderr) > 0 {
			return nil, errors.New(firstLine(string(ee.Stderr), err))
		}
		return nil, err
	}
	var found []Found
	for _, line := range strings.Split(string(out), "\n") {
		parts := strings.Split(strings.TrimSpace(line), "|")
		// a label that is not a ticket id was not written by bitc: it is not this sweep's to judge
		if len(parts) != 5 || !fullIDRE.MatchString(parts[0]) || !ticketIDRE.MatchString(parts[3]) {
			continue
		}
		live, why := ticketState(l, pools, parts[3])
		found = append(found, Found{ID: parts[0], Name: parts[1], Pool: parts[2], Ticket: parts[3], Job: parts[4], Live: live, Why: why})
	}
	return found, nil
}

// Sweep removes this state dir's orphaned containers (a label match whose ticket is gone or
// unheld), by the id the listing gave, and then the cidfiles and env files of dead runs. removed is
// called for each orphan, after its removal, or instead of it when dry; kept for each whose holder
// this host cannot tell (never removed on a guess, #24). A live run's container is never reported.
//
// Any error aborts the sweep: the tickets cannot be read, the listing fails, a removal fails. It
// returns the error and removes nothing more: containers it removed before a failing removal stay
// removed, and the files of dead runs are touched only after every removal succeeded. A sweep that
// cannot see what it is doing (the tickets, the listing) deletes nothing at all.
func Sweep(ctx context.Context, runtime string, l state.Layout, env []string, dry bool, removed, kept func(Found)) error {
	pools, err := readPools(l)
	if err != nil {
		return err
	}
	found, err := list(ctx, runtime, l, pools, env)
	if err != nil {
		return err
	}
	for _, f := range found {
		switch f.Live {
		case lock.StateHeld:
		case lock.StateUnknown:
			kept(f)
		default:
			if !dry {
				if err := Remove(ctx, runtime, f.ID, env); err != nil {
					return fmt.Errorf("cannot remove %s: %w", f.Name, err)
				}
			}
			removed(f)
		}
	}
	if !dry {
		sweepFiles(l, pools)
	}
	return nil
}

// sweepFiles removes the cidfile and the env file of every ticket that is gone.
func sweepFiles(l state.Layout, pools []os.DirEntry) {
	ents, _ := os.ReadDir(l.ContainersDir())
	for _, e := range ents {
		id, ok := strings.CutSuffix(e.Name(), ".cid")
		if !ok {
			id, ok = strings.CutSuffix(e.Name(), ".env")
		}
		if !ok || !ticketIDRE.MatchString(id) {
			continue
		}
		if st, _ := ticketState(l, pools, id); st == lock.StateFree {
			_ = os.Remove(filepath.Join(l.ContainersDir(), e.Name()))
		}
	}
}
