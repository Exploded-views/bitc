package remote

import (
	"context"
	"errors"
	"github.com/Exploded-views/bitc/internal/redact"
	"os"
	"path/filepath"
	"time"

	"github.com/Exploded-views/bitc/internal/config"
	"github.com/Exploded-views/bitc/internal/provider"
	"github.com/Exploded-views/bitc/internal/state"
)

type remoteResource struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Pool    string `json:"pool"`
	Expires string `json:"expires"`
}

type slotHold struct {
	Slots []int  `json:"slots"`
	Name  string `json:"name"`
	Own   bool   `json:"own"`
	Until string `json:"until"`
}

// slotsStatus is a fixed-capacity provider's slots: how many it owns, how many are held now by
// any controller and which groups hold them.
type slotsStatus struct {
	Total int        `json:"total"`
	Busy  int        `json:"busy"`
	Free  int        `json:"free"`
	Held  []slotHold `json:"held"`
}

type providerStatus struct {
	Own        []remoteResource `json:"own"`
	Foreign    []remoteResource `json:"foreign"`
	SpendUSD   string           `json:"spend_usd"`             // "" without a spend API or on an error
	LedgerUSD  string           `json:"ledger_usd"`            // today's cost in this host's ledger
	EstUSD     string           `json:"est_usd_today"`         // the same figure for a provider with no Spend API (the cap sees the ledger only)
	Breaker    string           `json:"breaker"`               // "" or why launches are paused
	LastSweep  string           `json:"last_sweep,omitempty"`  // RFC 3339; the last successful non-dry sweep listing
	ForeignOrg string           `json:"foreign_org,omitempty"` // a resource of another organization that fails the listing (never touched)
	Error      string           `json:"error,omitempty"`
	Slots      *slotsStatus     `json:"slots,omitempty"` // a fixed-capacity provider only
}

// Set by internal/remote/rails, which imports this package (so these are hooks, not imports).
var (
	LedgerToday  func(dir string, now time.Time) config.Micros
	BreakerState func(dir string, b config.Breaker, now time.Time) (why string, paused bool)
)

// StatusSection is status's remote part: resources by owner and today's spend.
func StatusSection(e *config.Effective) any {
	out := map[string]providerStatus{}
	acct, err := state.AccountDir(e)
	if err != nil {
		return out
	}
	secret, _ := state.InstallSecret(acct)
	install := state.Install16(secret)
	for typ, core := range e.Providers {
		if _, ok := provider.Lookup(typ, core); !ok || (core.APIKey == "" && typ != "fake" && core.Plugin == nil && !core.Fixed) {
			continue
		}
		p, err := NewProvider(context.Background(), typ, core, e, provider.Runtime{Red: redact.New(), Stderr: os.Stderr, Debug: e.LogLevel == "debug"})
		if err == nil {
			defer CloseProvider(p)
		}
		if err != nil {
			out[typ] = providerStatus{Own: []remoteResource{}, Foreign: []remoteResource{}, Error: err.Error()}
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		ps := providerStatus{Own: []remoteResource{}, Foreign: []remoteResource{}}
		if list, err := p.List(ctx, []string{core.NamePrefix + "-"}); err == nil {
			for _, r := range list {
				rr := remoteResource{ID: r.ID, Name: r.Name, Pool: r.Pool, Expires: r.Expires.UTC().Format(time.RFC3339)}
				if r.Install == install {
					ps.Own = append(ps.Own, rr)
				} else {
					ps.Foreign = append(ps.Foreign, rr)
				}
			}
		} else {
			ps.Error = err.Error()
			var fo provider.ForeignOrg
			if errors.As(err, &fo) {
				ps.ForeignOrg, _ = fo.ForeignOrg()
			}
		}
		if sr, ok := p.(provider.SlotReporter); ok {
			if st, err := sr.SlotStatus(ctx); err != nil {
				if ps.Error == "" {
					ps.Error = err.Error()
				}
			} else {
				ps.Slots = &slotsStatus{Total: st.Total, Held: []slotHold{}}
				for _, h := range st.Held {
					ps.Slots.Busy += len(h.Slots)
					ps.Slots.Held = append(ps.Slots.Held, slotHold{Slots: h.Slots, Name: h.Name, Own: h.Install == install, Until: h.Until.UTC().Format(time.RFC3339)})
				}
				ps.Slots.Free = max(st.Total-ps.Slots.Busy, 0)
			}
		}
		if s, err := p.Spend(ctx); err == nil && p.Info().Caps.Spend {
			ps.SpendUSD = s.Dollars()
		}
		cancel()
		dir := filepath.Join(acct, "remote", typ)
		if LedgerToday != nil {
			ps.LedgerUSD = LedgerToday(dir, time.Now()).Dollars()
			ps.EstUSD = ps.LedgerUSD
		}
		if t, ok := LastSweep(dir); ok {
			ps.LastSweep = t.UTC().Format(time.RFC3339)
		}
		if BreakerState != nil {
			if why, paused := BreakerState(dir, core.Breaker, time.Now()); paused {
				ps.Breaker = why
			}
		}
		out[typ] = ps
	}
	return out
}
