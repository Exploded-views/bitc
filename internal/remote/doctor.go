package remote

import (
	"context"
	"errors"
	"fmt"
	"github.com/Exploded-views/bitc/internal/redact"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Exploded-views/bitc/internal/config"
	"github.com/Exploded-views/bitc/internal/exit"
	"github.com/Exploded-views/bitc/internal/provider"
	"github.com/Exploded-views/bitc/internal/remote/gitref"
	"github.com/Exploded-views/bitc/internal/state"
)

// Doctor is `bitc doctor --remote` (spec 3.3).
func Doctor(e *config.Effective, out io.Writer) (failed, unusable bool) {
	line := func(level, check, detail string) {
		fmt.Fprintf(out, "%s %s: %s\n", level, check, detail)
		failed = failed || level == "fail"
	}
	ctx := context.Background()
	var install string
	if acct, err := state.AccountDir(e); err == nil {
		if secret, err := state.InstallSecret(acct); err == nil {
			install = state.Install16(secret)
		}
	}
	var started []provider.Provider
	defer func() {
		for _, p := range started {
			CloseProvider(p)
		}
	}()
	for typ, core := range e.Providers {
		if _, ok := provider.Lookup(typ, core); !ok {
			continue
		}
		if core.APIKey == "" && typ != "fake" && core.Plugin == nil && !core.Fixed {
			line("fail", typ+" credentials", "set api_key or api_key_file")
			continue
		}
		if core.APIKey != "" {
			line("ok", typ+" credentials", "<redacted>")
		}
		if core.CleanupOnly { // jobs refused: list only, through the cleanup-only handle
			doctorCleanup(ctx, typ, core, e, line)
			continue
		}
		p, err := NewProvider(ctx, typ, core, e, provider.Runtime{Red: redact.New(), Stderr: os.Stderr, Debug: e.LogLevel == "debug"})
		if err != nil {
			var ec *provider.ErrConfig
			if errors.As(err, &ec) { // 78: the host file is wrong (providers delta A.4.5, A.8)
				if core.Plugin != nil {
					line("fail", "plugin-"+typ, ec.Why)
				} else {
					line("fail", typ+" config", ec.Why)
				}
				fmt.Fprintln(os.Stderr, exit.Configf("%s: %s", e.HostFile, ec.Why).Line)
				unusable = true
			} else if core.Plugin != nil {
				line("fail", "plugin-"+typ, err.Error())
			} else {
				line("fail", typ+" config", err.Error())
			}
			continue
		}
		started = append(started, p)
		if pl, ok := p.(interface{ DoctorLine() string }); ok { // a plugin (providers delta A.8, B.2)
			line("ok", "plugin-"+typ, pl.DoctorLine())
			if core.FallbackUntil == "attach" {
				line("ok", "plugin-"+typ, "fallback_until = attach (host opt-in; FB15 is the author's claim)")
			}
		}
		if list, err := p.List(ctx, []string{core.NamePrefix + "-"}); err != nil {
			line("fail", typ+" api", err.Error())
		} else {
			line("ok", typ+" api", fmt.Sprintf("%d live %s- resources", len(list), core.NamePrefix))
			for _, r := range list {
				if install != "" && r.Install != install && !r.Expires.IsZero() && r.Expires.Before(time.Now()) {
					line("warn", typ+" foreign", fmt.Sprintf("%s (%s, past its expiry; sweep --foreign tears it down)", r.ID, r.Name))
				}
			}
		}
		if p.Info().Caps.Spend {
			if s, err := p.Spend(ctx); err != nil {
				line("fail", typ+" billing", err.Error())
			} else {
				line("ok", typ+" billing", "spent today "+s.Dollars()+" of daily_usd "+core.DailyUSD.Dollars())
				level := "ok"
				if s >= core.DailyUSD {
					level = "warn"
				}
				line(level, typ+" cap headroom", (core.DailyUSD-s).Dollars()+" left today (live resources and reservations also count at launch)")
			}
		} else {
			line("warn", typ+" billing", "no spend API: the cap sees live resources and this host's ledger only")
		}
		if sr, ok := p.(provider.SweepRules); ok && !sr.SweepRules().TimerRequired && sr.SweepRules().TimerAdvice != "" {
			var dir string
			if acct, err := state.AccountDir(e); err == nil {
				dir = filepath.Join(acct, "remote", typ)
			}
			if last, ok := LastSweep(dir); !ok || time.Since(last) > 30*time.Minute {
				line("warn", sr.SweepRules().TimerAdvice, "no sweep in 30 min")
			} else {
				line("ok", sr.SweepRules().TimerAdvice, "the last sweep was "+strconv.Itoa(int(time.Since(last).Minutes()))+"m ago")
			}
		}
		if sr, ok := p.(provider.SweepRules); ok && sr.SweepRules().TimerRequired {
			var dir string
			if acct, err := state.AccountDir(e); err == nil {
				dir = filepath.Join(acct, "remote", typ)
			}
			if last, ok := LastSweep(dir); !ok {
				line("fail", typ+" sweep timer", "no sweep has run in the last 30 minutes (none recorded); "+typ+" has no self-delete, so the sweep timer is mandatory: docs/sweep-timer.md")
			} else if ago := int(time.Since(last).Minutes()); ago > 30 {
				line("fail", typ+" sweep timer", "no sweep has run in the last 30 minutes (the last sweep was "+strconv.Itoa(ago)+"m ago); "+typ+" has no self-delete, so the sweep timer is mandatory: docs/sweep-timer.md")
			} else {
				line("ok", typ+" sweep timer", "the last sweep was "+strconv.Itoa(ago)+"m ago")
			}
		}
		if d, ok := p.(provider.Doctorer); ok {
			for _, l := range d.DoctorLines(ctx) {
				line(l.Level, l.Check, l.Detail)
			}
		}
		if !p.Info().Caps.SelfDelete {
			line("warn", typ+" self-delete", "resources rely on cleanup and the sweep timer")
		}
		if src, err := ResolveAgentSource(e, core); err != nil {
			line("fail", typ+" agent", err.Error())
			var inel *provider.ErrIneligible
			unusable = unusable || (errors.As(err, &inel) && strings.HasPrefix(inel.Why, "agent.module_dir:"))
		} else {
			line("ok", typ+" agent", src.String())
		}
		if err := exec.Command("git", "-C", e.Worktree, "ls-remote", core.Transport.Remote, core.Transport.RefPrefix+"*").Run(); err != nil {
			line("warn", typ+" refs", "cannot list "+core.Transport.RefPrefix+" on "+core.Transport.Remote+": "+err.Error())
		} else {
			line("ok", typ+" refs", core.Transport.RefPrefix)
		}
		if raw, err := exec.Command("git", "-C", e.Worktree, "remote", "get-url", core.Transport.Remote).Output(); err == nil {
			if clone, err := gitref.CloneURL(strings.TrimSpace(string(raw))); err == nil {
				if gitref.KnownHostsHas(core.Transport.KnownHosts, gitref.HostOf(clone)) {
					line("ok", typ+" known hosts", gitref.HostOf(clone))
				} else {
					line("fail", typ+" known hosts", gitref.HostOf(clone)+" is not in transport.known_hosts")
				}
			}
		}
	}
	return failed, unusable
}

// doctorCleanup is doctor --remote for a type whose jobs are refused (config.Provider.CleanupOnly):
// it says so, lists through the cleanup-only handle (no other call) and checks the sweep timer.
func doctorCleanup(ctx context.Context, typ string, core *config.Provider, e *config.Effective, line func(level, check, detail string)) {
	line("warn", typ+" jobs", "refused in this release; bitc sweep only lists, owner-checks and deletes Sprites")
	c, err := sweepHandle(ctx, typ, core, e)
	if err != nil {
		line("fail", typ+" config", err.Error())
		return
	}
	if list, err := c.List(ctx, []string{core.NamePrefix + "-"}); err != nil {
		line("fail", typ+" api", err.Error())
	} else {
		line("ok", typ+" api", fmt.Sprintf("%d live %s- resources", len(list), core.NamePrefix))
	}
	if sr, ok := c.(provider.SweepRules); ok && sr.SweepRules().TimerRequired {
		var dir string
		if acct, err := state.AccountDir(e); err == nil {
			dir = filepath.Join(acct, "remote", typ)
		}
		if last, ok := LastSweep(dir); !ok || time.Since(last) > 30*time.Minute {
			line("fail", typ+" sweep timer", "no sweep has run in the last 30 minutes; "+typ+" has no self-delete, so the sweep timer is mandatory: docs/sweep-timer.md")
		} else {
			line("ok", typ+" sweep timer", "the last sweep was "+strconv.Itoa(int(time.Since(last).Minutes()))+"m ago")
		}
	}
}
