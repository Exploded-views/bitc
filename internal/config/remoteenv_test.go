package config

import (
	"fmt"
	"strings"
	"testing"
)

func TestSecretLike(t *testing.T) {
	for _, n := range []string{"NPM_TOKEN", "api_key", "MY_SECRET", "DB_PASSWORD", "DB_PASS", "PASSWD", "AWS_CREDENTIALS",
		"GH_AUTH", "SENTRY_DSN", "SSH_PRIVATE_FILE", "COOKIE_JAR", "SESSION_ID", "TLS_CERT", "DATABASE_URL", "direct_url"} {
		if !secretLike(n) {
			t.Errorf("%s should be secret-like", n)
		}
	}
	for _, n := range []string{"CI", "TZ", "NODE_ENV", "HOME", "DEBUG", "LANG", "APP_URL", "DB_HOST"} {
		if secretLike(n) {
			t.Errorf("%s should not be secret-like", n)
		}
	}
}

func TestForwardEnvNamesAreValidated(t *testing.T) {
	for _, bad := range []string{"1BAD", "BAD-NAME", "A=B", "A$(x)", "BITC_X", ""} {
		for _, key := range []string{"remote.forward_env", "remote_limits.forward_env_allow", "remote_limits.forward_env_force"} {
			e := &Effective{HostFile: "/h/bitc.toml", RepoFile: "/r/.bitc.toml"}
			switch key {
			case "remote.forward_env":
				e.Remote = &RepoRemote{ForwardEnv: []string{bad}}
			case "remote_limits.forward_env_allow":
				e.RemoteLimits.ForwardEnvAllow = []string{bad}
			default:
				e.RemoteLimits.ForwardEnvForce = []string{bad}
			}
			var p problems
			checkRemoteEnv(&p, e)
			if len(p.list) != 1 || !strings.Contains(p.list[0], key+": "+fmt.Sprintf("%q", bad)) {
				t.Errorf("%s %q: %q", key, bad, p.list)
			}
		}
	}
	e := &Effective{Remote: &RepoRemote{ForwardEnv: []string{"CI", "DATABASE_URL"}},
		RemoteLimits: RemoteLimits{ForwardEnvAllow: []string{"CI", "DATABASE_URL"}, ForwardEnvForce: []string{"DATABASE_URL"}}}
	var p problems
	checkRemoteEnv(&p, e)
	if len(p.list) != 0 {
		t.Fatalf("valid and forced names refused: %q", p.list)
	}
}
