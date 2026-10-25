package sprites

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Exploded-views/bitc/internal/provider"
)

// SweepRules: one Sprite per job, no self-delete (sprites-provider.md 2.4, decision 8). An own
// Sprite with no live reservation is deleted once it is older than start_timeout.
func (p *Provider) SweepRules() provider.SweepPolicy {
	return provider.SweepPolicy{Noun: "sprite", GoneAfter: p.core.StartTimeout, TimerRequired: true}
}

// DoctorLines are the provider's own `doctor --remote` checks (sprites-provider.md 6.1, 5.2): the
// org guard over every Sprite of the org, Connectors, Sprites outside name_prefix, and more than
// one install holding Sprites (daily_usd is a per-host share).
func (p *Provider) DoctorLines(ctx context.Context) []provider.DoctorLine {
	if p.credentials() != nil {
		return nil
	}
	var out []provider.DoctorLine
	add := func(level, check, detail string) {
		out = append(out, provider.DoctorLine{Level: level, Check: "sprites " + check, Detail: detail})
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	all, err := p.cli.ListSprites(ctx, "")
	var fo provider.ForeignOrg
	switch {
	case errors.As(err, &fo):
		name, org := fo.ForeignOrg()
		add("fail", "org", fmt.Sprintf("sprite %s belongs to organization %q, not %q; use a token of a separate org that holds only bitc Sprites (bitc never touches it: delete it by hand)", name, org, p.set.Org))
	case err != nil:
		add("fail", "org", "cannot list the org's Sprites: "+err.Error())
	default:
		add("ok", "org", fmt.Sprintf("%d Sprites, all in organization %q", len(all), p.set.Org))
		pre := p.core.NamePrefix + "-"
		var outside []string
		installs := map[string]bool{}
		for _, s := range all {
			if !strings.HasPrefix(s.Name, pre) {
				outside = append(outside, s.Name)
			} else if pn, ok := ParseName(p.core.NamePrefix, s.Name); ok {
				installs[pn.Install] = true
			}
		}
		if len(outside) > 0 {
			add("fail", "outside prefix", fmt.Sprintf("%d Sprites do not start with %q: %s; the org must hold only bitc Sprites", len(outside), pre, joinMax(outside, 5)))
		} else {
			add("ok", "outside prefix", "every Sprite starts with "+pre)
		}
		if len(installs) > 1 {
			var ids []string
			for id := range installs {
				ids = append(ids, id)
			}
			sort.Strings(ids)
			add("warn", "installs", fmt.Sprintf("%d installs hold Sprites in this org (%s); with no spend API daily_usd is a per-host share", len(ids), strings.Join(ids, ", ")))
		} else {
			add("ok", "installs", fmt.Sprintf("%d install holds Sprites in this org", len(installs)))
		}
	}
	conns, err := p.cli.ListConnections(ctx)
	switch {
	case err != nil:
		add("warn", "connectors", "cannot list Connectors: "+err.Error())
	case len(conns) > 0:
		add("fail", "connectors", fmt.Sprintf("%d Connector(s): %s; a Connector whose selector matches %s* would hand its credential to hostile jobs: delete them", len(conns), joinMax(conns, 5), p.core.NamePrefix))
	default:
		add("ok", "connectors", "the org has none")
	}
	return out
}

func joinMax(v []string, n int) string {
	if len(v) <= n {
		return strings.Join(v, ", ")
	}
	return strings.Join(v[:n], ", ") + fmt.Sprintf(", and %d more", len(v)-n)
}
