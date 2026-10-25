package ssh

import "github.com/Exploded-views/bitc/internal/provider"

// SweepRules: a slot group is one resource per job (providers delta C.3.6). An own group with no
// live reservation on this host is freed once its lease is older than start_timeout, an own group
// whose only reservation is an orphan at once, and any own group past E; the owner tag must verify
// (the shared sweep checks it). The agent frees its own slot at D + 10m, so the timer is advised
// (doctor warns), not mandatory.
func (p *Provider) SweepRules() provider.SweepPolicy {
	return provider.SweepPolicy{Noun: "slot", GoneAfter: p.core.StartTimeout, TimerAdvice: "ssh-sweep"}
}
