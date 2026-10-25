package remote

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/Exploded-views/bitc/internal/redact"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Exploded-views/bitc/internal/config"
	"github.com/Exploded-views/bitc/internal/provider"
	"github.com/Exploded-views/bitc/internal/remote/gitref"
	"github.com/Exploded-views/bitc/internal/state"
	"github.com/Exploded-views/bitc/internal/status"
)

// PruneReservations is set by internal/remote/rails: it drops reservations whose process is gone
// (or that expired) from dir under the launch lock and returns their names; with dry it only
// reports them.
var PruneReservations func(dir string, dry bool) []string

// OrphanNames and DropOrphan are set by internal/remote/rails: the names whose only reservation
// in dir is an orphan (a resource discarded before attach and not confirmed gone, spec 8.3), and
// the removal of one.
var (
	OrphanNames func(dir string) map[string]bool
	DropOrphan  func(dir, name string)
)

// LiveReservations is set by internal/remote/rails: the names with a non-orphan reservation in dir
// (the sweep prunes dead ones first).
var LiveReservations func(dir string) map[string]bool

func init() {
	status.RemoteSweep = Sweep
	status.RemoteDoctor = Doctor
	status.RemoteSection = StatusSection
}

type sweepInput struct {
	prov     provider.Provider
	typ      string
	dir      string // ACCOUNT/remote/<type>, holds the reservations
	prefix   string // "<name_prefix>-"
	install  string
	secret   []byte
	launched map[string]bool // names with a ledger launch line
	foreign  bool
	dry      bool
	// rules: a one-resource-per-job provider (provider.SweepRules); goneAfter is its scaled age limit
	rules     provider.SweepPolicy
	goneAfter time.Duration
	reserved  map[string]bool // names with a live (non-orphan) reservation on this host
	now       time.Time
	out       io.Writer
	errw      io.Writer
}

// sweepProvider tears down own resources past E whose owner tag verifies (or that have a
// ledger launch line); with --foreign also other installs' prefixed resources past E.
func sweepProvider(ctx context.Context, in sweepInput) {
	list, err := in.prov.List(ctx, []string{in.prefix})
	if err != nil {
		var fo provider.ForeignOrg
		if errors.As(err, &fo) { // a Sprite of another org fails the whole list: name it, never touch it
			name, org := fo.ForeignOrg()
			fmt.Fprintf(in.errw, "bitc: sweep: %s: %v; %s belongs to organization %q and is never touched; nothing was swept: delete it by hand\n", in.typ, err, name, org)
			return
		}
		fmt.Fprintf(in.errw, "bitc: sweep: %s: %v\n", in.typ, err)
		return
	}
	if !in.dry && in.dir != "" {
		stampSweep(in.dir, in.now)
	}
	noun := in.typ
	if in.rules.Noun != "" {
		noun = in.rules.Noun
	}
	var orphans map[string]bool
	if OrphanNames != nil && in.dir != "" {
		orphans = OrphanNames(in.dir)
	}
	teardown := func(r provider.Resource) bool {
		if in.dry {
			return false
		}
		if err := in.prov.Teardown(ctx, r.ID); err != nil {
			fmt.Fprintf(in.errw, "bitc: sweep: %s: could not tear down %s: %v\n", in.typ, r.ID, err)
			return false
		}
		return true
	}
	for _, r := range list {
		// An orphan reservation never shields its resource: a verified owner tag is enough, and a
		// confirmed teardown drops the orphan (spec 8.3).
		if orphans[r.Name] && r.Install == in.install {
			if tag, _ := in.prov.Owner(ctx, r.ID); provider.OwnerMatches(in.secret, r.Name, tag) {
				fmt.Fprintf(in.out, "swept %s %s (orphaned by a pre-attach discard)\n", noun, r.Name)
				if teardown(r) && DropOrphan != nil {
					DropOrphan(in.dir, r.Name)
				}
				continue
			}
		}
		pastE := !r.Expires.IsZero() && !in.now.Before(r.Expires)
		if !pastE {
			// one resource per job: an own one that no live reservation of this host accounts for
			// and that is older than start_timeout lost its job process (design 2.4)
			if in.goneAfter > 0 && r.Install == in.install && !in.reserved[r.Name] && !r.Created.IsZero() && in.now.Sub(r.Created) > in.goneAfter {
				if tag, _ := in.prov.Owner(ctx, r.ID); provider.OwnerMatches(in.secret, r.Name, tag) {
					fmt.Fprintf(in.out, "swept %s %s (job process gone)\n", noun, r.Name)
					teardown(r)
				}
			}
			continue
		}
		tear := func(what string) {
			if in.rules.Noun != "" {
				fmt.Fprintf(in.out, "swept %s %s (past its expiry%s)\n", noun, r.Name, map[bool]string{true: "; --foreign"}[what == "foreign resource"])
			} else {
				fmt.Fprintf(in.out, "swept %s %s (%s, past its expiry%s)\n", what, r.ID, r.Name, map[bool]string{true: "; --foreign"}[what == "foreign resource"])
			}
			teardown(r)
		}
		if r.Install == in.install {
			tag, _ := in.prov.Owner(ctx, r.ID)
			if provider.OwnerMatches(in.secret, r.Name, tag) || in.launched[r.Name] {
				tear("resource")
			} else {
				fmt.Fprintf(in.errw, "bitc: sweep: %s: %s has no verified owner tag; kept\n", in.typ, r.ID)
			}
			continue
		}
		if in.foreign {
			tear("foreign resource")
		}
	}
}

// stampSweep records that a sweep listed this provider's resources just now.
func stampSweep(dir string, now time.Time) {
	if state.MkdirPrivate(dir) == nil {
		_ = state.WriteFileAtomic(filepath.Join(dir, "last-sweep"), []byte(strconv.FormatInt(now.Unix(), 10)+"\n"))
	}
}

// LastSweep is when a sweep last listed this provider's resources successfully (not a dry run).
func LastSweep(dir string) (time.Time, bool) {
	b, err := os.ReadFile(filepath.Join(dir, "last-sweep"))
	if err != nil {
		return time.Time{}, false
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	return time.Unix(n, 0), true
}

// Sweep is the remote part of `bitc sweep` (spec 8.3). It always returns; problems are noted.
func Sweep(ctx context.Context, e *config.Effective, dry bool, only string, foreign bool, out io.Writer) {
	acct, err := state.AccountDir(e)
	if err != nil {
		fmt.Fprintln(os.Stderr, "bitc: sweep:", err)
		return
	}
	secret, err := state.InstallSecret(acct)
	if err != nil {
		fmt.Fprintln(os.Stderr, "bitc: sweep:", err)
		return
	}
	install := state.Install16(secret)
	for typ, core := range e.Providers {
		if only != "" && only != typ {
			continue
		}
		if _, ok := provider.Lookup(typ, core); !ok {
			continue
		}
		dir := filepath.Join(acct, "remote", typ)
		if PruneReservations != nil {
			for _, name := range PruneReservations(dir, dry) {
				fmt.Fprintf(out, "swept reservation %s (its process is gone)\n", name)
			}
		}
		if core.APIKey == "" && typ != "fake" && core.Plugin == nil && !core.Fixed {
			fmt.Fprintf(os.Stderr, "bitc: sweep: %s: no credentials; skipped resources\n", typ)
		} else if p, err := NewProvider(ctx, typ, core, e, provider.Runtime{Red: redact.New(), Stderr: os.Stderr, Debug: e.LogLevel == "debug"}); err != nil {
			fmt.Fprintf(os.Stderr, "bitc: sweep: %s: %v\n", typ, err)
		} else {
			defer CloseProvider(p)
			in := sweepInput{prov: p, typ: typ, dir: dir, prefix: core.NamePrefix + "-", install: install, secret: secret,
				launched: launchedNames(dir), foreign: foreign, dry: dry, now: time.Now(), out: out, errw: os.Stderr}
			if sr, ok := p.(provider.SweepRules); ok {
				in.rules = sr.SweepRules()
				in.goneAfter = e.Scale(in.rules.GoneAfter)
				if LiveReservations != nil {
					in.reserved = LiveReservations(dir)
				}
			}
			sweepProvider(ctx, in)
		}
		for _, prefix := range []string{core.Transport.RefPrefix, gitref.AgentRefPrefix} {
			refs, err := gitref.SweepRefs(ctx, e.Worktree, core.Transport.Remote, prefix, install, 3*time.Hour, dry)
			if err != nil {
				fmt.Fprintf(os.Stderr, "bitc: sweep: %s: refs: %v\n", typ, err)
			}
			for _, r := range refs {
				fmt.Fprintf(out, "swept ref %s (older than 3h)\n", r)
			}
		}
	}
}

func launchedNames(dir string) map[string]bool {
	names := map[string]bool{}
	f, err := os.Open(filepath.Join(dir, "ledger.jsonl"))
	if err != nil {
		return names
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var l struct{ Event, Name string }
		if json.Unmarshal(sc.Bytes(), &l) == nil && l.Event == "launch" {
			names[l.Name] = true
		}
	}
	return names
}
