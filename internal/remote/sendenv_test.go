package remote

import (
	"errors"
	"testing"

	"github.com/Exploded-views/bitc/internal/provider"
)

func TestSendEnvCheck(t *testing.T) {
	none, fwd, frc := provider.EnvNone, provider.EnvForwarded, provider.EnvForced
	for _, c := range []struct {
		name          string
		level         provider.EnvLevel
		multi         bool
		forced, plain []string
		why           string // "" = eligible
	}{
		{"no names, none", none, false, nil, nil, ""},
		{"plain, none", none, false, nil, []string{"B_CI", "A_CI"},
			`forwarded env not sent to ssh (providers.ssh.send_env = "none"): A_CI, B_CI`},
		{"forced and plain, none: every name", none, false, []string{"Z_TOKEN"}, []string{"A_CI"},
			`forwarded env not sent to ssh (providers.ssh.send_env = "none"): A_CI, Z_TOKEN`},
		{"plain, forwarded", fwd, false, nil, []string{"A_CI"}, ""},
		{"forced, forwarded: forced names only", fwd, false, []string{"Z_TOKEN", "M_TOKEN"}, []string{"A_CI"},
			`forced env not sent to ssh (providers.ssh.send_env = "forwarded"): M_TOKEN, Z_TOKEN`},
		{"forced, forced", frc, false, []string{"Z_TOKEN"}, []string{"A_CI"}, ""},
		{"forced on a multi-job resource is capped at forwarded", frc, true, []string{"Z_TOKEN"}, nil,
			`forced env not sent to ssh (providers.ssh.send_env = "forwarded"): Z_TOKEN`},
		{"plain on a multi-job resource", frc, true, nil, []string{"A_CI"}, ""},
	} {
		err := SendEnvCheck("ssh", c.level, c.multi, c.forced, c.plain)
		var ine *provider.ErrIneligible
		switch {
		case c.why == "" && err != nil:
			t.Errorf("%s: unexpected %v", c.name, err)
		case c.why != "" && !errors.As(err, &ine):
			t.Errorf("%s: got %v, want ErrIneligible", c.name, err)
		case c.why != "" && ine.Why != c.why:
			t.Errorf("%s: why %q, want %q", c.name, ine.Why, c.why)
		}
	}
}
