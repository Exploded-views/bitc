package container

import (
	"context"
	"errors"
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
	Name   string
	Pool   string
	Job    string // the bitc.job label
	Ticket string
	Live   lock.State // StateFree: the ticket is gone or nobody holds it (an orphan); StateHeld: a run owns it; StateUnknown: this host cannot tell
}

// listFormat prints name|pool|ticket|job per container.
const listFormat = `{{.Names}}|{{.Label "` + LabelPool + `"}}|{{.Label "` + LabelTicket + `"}}|{{.Label "` + LabelJob + `"}}`

// List is every container (running or not) that carries this state dir's label, with its ticket's state.
func List(ctx context.Context, runtime string, l state.Layout, env []string) ([]Found, error) {
	ctx, cancel := context.WithTimeout(ctx, removeLimit)
	defer cancel()
	cmd := exec.CommandContext(ctx, runtime, "ps", "-a", "--filter", "label="+LabelState+"="+StateHash(l.Root), "--format", listFormat)
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
		if len(parts) != 4 || parts[0] == "" || !ticketIDRE.MatchString(parts[2]) {
			continue
		}
		found = append(found, Found{Name: parts[0], Pool: parts[1], Ticket: parts[2], Job: parts[3], Live: TicketState(l, parts[2])})
	}
	return found, nil
}

// TicketState is the state of the lock on the ticket file with this id, in whichever pool holds it.
// A missing ticket is free; a pools dir that cannot be read (anything but "it is not there") says
// nothing, so it is unknown: the sweep never removes on a guess.
func TicketState(l state.Layout, id string) lock.State {
	pools, err := os.ReadDir(filepath.Join(l.Root, "pools"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return lock.StateUnknown
	}
	st := lock.StateFree
	for _, p := range pools {
		switch lock.Inspect(filepath.Join(l.Tickets(p.Name()), id+".json")) {
		case lock.StateHeld:
			return lock.StateHeld
		case lock.StateUnknown:
			st = lock.StateUnknown
		}
	}
	return st
}

// Sweep removes this state dir's orphaned containers (a label match whose ticket is gone or
// unheld) and the cidfiles of dead runs. removed is called for each orphan, after its removal, or
// instead of it when dry; kept for each whose holder this host cannot tell (never removed on a
// guess, #24). A live run's container is never reported. It returns the listing error (docker not
// there, the daemon down); the cidfiles are cleaned either way.
func Sweep(ctx context.Context, runtime string, l state.Layout, env []string, dry bool, removed, kept func(Found)) error {
	if !dry {
		sweepCIDFiles(l)
	}
	found, err := List(ctx, runtime, l, env)
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
				if err := Remove(ctx, runtime, f.Name, env); err != nil {
					continue // still there: the next sweep tries again
				}
			}
			removed(f)
		}
	}
	return nil
}

// sweepCIDFiles removes the cidfile of every ticket that is gone.
func sweepCIDFiles(l state.Layout) {
	ents, _ := os.ReadDir(l.ContainersDir())
	for _, e := range ents {
		id, ok := strings.CutSuffix(e.Name(), ".cid")
		if ok && ticketIDRE.MatchString(id) && TicketState(l, id) == lock.StateFree {
			_ = os.Remove(filepath.Join(l.ContainersDir(), e.Name()))
		}
	}
}
