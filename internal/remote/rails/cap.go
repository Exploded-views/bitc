package rails

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/Exploded-views/bitc/internal/config"
	"github.com/Exploded-views/bitc/internal/provider"
)

// estimate is price per hour (micro-dollars) x seconds / 3600.
func estimate(perHour config.Micros, seconds int64) config.Micros {
	return config.Micros(int64(perHour) * seconds / 3600)
}

// railError keeps a provider's ErrUnavailable (401/402/403: Walk pauses the provider) and makes
// anything else a transient fallback; an error always counts as over the cap (spec 8.3).
func railError(err error) error {
	var una *provider.ErrUnavailable
	if errors.As(err, &una) {
		return err
	}
	return &provider.ErrTransient{Why: err.Error()}
}

// CapNote is the fallback text (spec 8.3), two decimals.
func CapNote(daily, spent, committed, job config.Micros) string {
	return fmt.Sprintf("daily cap %s: spent %s + committed %s + this job %s", daily.Dollars(), spent.Dollars(), committed.Dollars(), job.Dollars())
}

type capInput struct {
	core      *config.Provider
	prov      provider.Provider
	ownPrefix string // "<name_prefix>-"
	compatPfx string // "" or the compat prefix
	worstSpan time.Duration
	highest   config.Micros
	ledger    config.Micros // ledger_today
	resv      []reservation
	secret    []byte
	thisJob   config.Micros
	now       time.Time
	note      func(string)
	debug     func(string)
	// tornOrphans (output): orphan reservations whose resource this check tore down.
	tornOrphans []string
}

// checkCap returns nil to launch, or *provider.ErrTransient with the note.
func checkCap(ctx context.Context, in *capInput) error {
	prefixes := []string{in.ownPrefix}
	if in.compatPfx != "" {
		prefixes = append(prefixes, in.compatPfx)
	}
	list, err := in.prov.List(ctx, prefixes)
	if err != nil {
		return railError(err)
	}
	var spent config.Micros
	if in.prov.Info().Caps.Spend {
		if spent, err = in.prov.Spend(ctx); err != nil {
			return railError(err)
		}
	}
	if in.ledger > spent {
		spent = in.ledger
	}
	orphans := orphanOnly(in.resv)
	listed := map[string]bool{}
	var committed config.Micros
	live := 0
	for _, r := range list {
		own := len(r.Name) >= len(in.ownPrefix) && r.Name[:len(in.ownPrefix)] == in.ownPrefix
		// A resource whose only reservation is an orphan is torn down at once, whatever its expiry
		// (spec 8.3): the orphan never shields it. A confirmed teardown drops the orphan.
		if own && orphans[r.Name] {
			if tag, err := in.prov.Owner(ctx, r.ID); err == nil && provider.OwnerMatches(in.secret, r.Name, tag) {
				if in.prov.Teardown(ctx, r.ID) == nil {
					if in.debug != nil {
						in.debug("tore down orphan " + r.Name)
					}
					in.tornOrphans = append(in.tornOrphans, r.Name)
					continue
				}
			}
		}
		if own && !r.Expires.IsZero() && in.now.After(r.Expires) {
			if tag, err := in.prov.Owner(ctx, r.ID); err == nil && provider.OwnerMatches(in.secret, r.Name, tag) {
				if in.prov.Teardown(ctx, r.ID) == nil {
					in.note(fmt.Sprintf("tore down %s (%s, past its expiry)", r.ID, r.Name))
					continue
				}
			}
		}
		listed[r.Name] = true
		live++
		switch {
		case own && !r.Expires.IsZero() && !r.Created.IsZero():
			committed += estimate(r.USDPerHour, int64(r.Expires.Sub(r.Created).Seconds()))
		default: // compat-prefix or unparsable: the worst case
			committed += estimate(in.highest, int64(in.worstSpan.Seconds()))
		}
	}
	for _, rv := range in.resv {
		if !listed[rv.Name] && !slices.Contains(in.tornOrphans, rv.Name) {
			committed += config.Micros(rv.Estimate)
			live++
		}
	}
	// An unpriced fixed-capacity provider has no daily_usd: only this comparison is skipped
	// (providers delta C.3.3); the breaker, List, reservations and max_live still apply.
	unpriced := in.prov.Info().Caps.Fixed && in.core.DailyUSD == 0
	if !unpriced && spent+committed+in.thisJob > in.core.DailyUSD {
		return &provider.ErrTransient{Why: CapNote(in.core.DailyUSD, spent, committed, in.thisJob)}
	}
	if live >= in.core.MaxLive {
		return &provider.ErrTransient{Why: fmt.Sprintf("%d live of max_live %d", live, in.core.MaxLive)}
	}
	return nil
}

// orphanOnly: names whose only reservation is an orphan.
func orphanOnly(rs []reservation) map[string]bool {
	out, live := map[string]bool{}, map[string]bool{}
	for _, r := range rs {
		if r.Orphan {
			out[r.Name] = true
		} else {
			live[r.Name] = true
		}
	}
	for n := range live {
		delete(out, n)
	}
	return out
}
