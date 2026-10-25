package config

import "testing"

func init() {
	RegisterProviderType("seforced", ProviderType{
		Endpoints: func(map[string]any) ([]string, error) { return nil, nil },
		SendEnv:   func(map[string]any) string { return "forced" },
	})
	RegisterProviderType("sedefault", ProviderType{
		Endpoints: func(map[string]any) ([]string, error) { return nil, nil },
	})
	RegisterProviderType("semulti", ProviderType{
		Endpoints: func(map[string]any) ([]string, error) { return nil, nil },
		SendEnv:   func(map[string]any) string { return "forced" },
		MultiJob:  func(map[string]any) bool { return true },
	})
}

func TestSendEnvDefaultsAndSource(t *testing.T) {
	for _, c := range []struct {
		typ, extra, want, src string
		multi                 bool
	}{
		{"seforced", "", "forced", "default", false},
		{"seforced", "send_env = \"none\"\n", "none", "host", false},
		{"seforced", "send_env = \"forced\"\n", "forced", "host", false},
		{"sedefault", "", "forwarded", "default", false},
		{"sedefault", "send_env = \"forced\"\n", "forced", "host", false},
		{"semulti", "", "forwarded", "default", true},
		{"semulti", "send_env = \"none\"\n", "none", "host", true},
	} {
		e := mustLoad(t, newFixture(t, fbHost(c.typ, c.extra), ""))
		got := e.Providers[c.typ]
		if got.SendEnv != c.want || got.SendEnvSource != c.src || got.MultiJob != c.multi {
			t.Errorf("%s %q: level %q source %q multi %v, want %q %q %v", c.typ, c.extra, got.SendEnv, got.SendEnvSource, got.MultiJob, c.want, c.src, c.multi)
		}
	}
}

func TestSendEnvValidation78(t *testing.T) {
	for _, c := range []struct{ typ, extra, msg string }{
		{"sedefault", "send_env = \"all\"\n", `providers.sedefault.send_env: must be "none", "forwarded" or "forced": "all"`},
		{"sedefault", "send_env = true\n", `providers.sedefault.send_env: must be "none", "forwarded" or "forced": true`},
		{"semulti", "send_env = \"forced\"\n", `providers.semulti.send_env: "forced" is not allowed: semulti reuses resources across jobs`},
	} {
		_, err := newFixture(t, fbHost(c.typ, c.extra), "").load()
		expect78(t, err, c.msg)
	}
}

func TestSendEnvIsHostOnly(t *testing.T) {
	f := newFixture(t, fbHost("sedefault", ""), "version = 1\n[providers.sedefault]\nsend_env = \"none\"\n")
	_, err := f.load()
	expect78(t, err, "providers.sedefault.send_env: host-only key")
}
