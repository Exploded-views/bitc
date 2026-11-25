package config

import (
	"strings"
	"testing"
)

const podEnvHost = "version = 1\n[providers.runpod]\ndaily_usd = 1\nimage = \"x\"\nsizes = {}\n"

// bitc config explain shows each pod_env name with <redacted> in place of its value (#47).
func TestExplainRedactsPodEnvValues(t *testing.T) {
	pinLinux(t)
	f := newFixture(t, podEnvHost+"pod_env = { HF_HOME = \"s3cr3t-hf-value\", A_TOKEN = \"s3cr3t-token-value\" }\n", "")
	e := mustLoad(t, f)
	for _, key := range []string{"", "providers", "providers.runpod", "providers.runpod.pod_env"} {
		out := strings.Join(Explain(e, key), "\n")
		for _, want := range []string{"providers.runpod.pod_env.A_TOKEN = <redacted>", "providers.runpod.pod_env.HF_HOME = <redacted>"} {
			if !strings.Contains(out, want) {
				t.Errorf("explain %q lacks %q:\n%s", key, want, out)
			}
		}
		if strings.Contains(out, "s3cr3t") {
			t.Errorf("explain %q shows a pod_env value:\n%s", key, out)
		}
	}
	if out := strings.Join(Explain(e, "providers.runpod.pod_env.HF_HOME"), "\n"); out != "providers.runpod.pod_env.HF_HOME = <redacted>  # "+f.host+" or default" {
		t.Errorf("one name: %q", out)
	}
}

// Defence in depth: pod_env values that pass the redaction rule (below) join the run's redaction
// list (e.Secrets), next to the API key, so a stray echo of one is masked by the same redactor.
func TestPodEnvValuesJoinTheRedactionList(t *testing.T) {
	pinLinux(t)
	f := newFixture(t, podEnvHost+"pod_env = { HF_HOME = \"s3cr3t-hf-value-long\", A_TOKEN = \"s3cr3t-token-value\", EMPTY = \"\" }\n", "")
	e := mustLoad(t, f)
	got := map[string]bool{}
	for _, s := range e.Secrets {
		got[s] = true
	}
	for _, want := range []string{"s3cr3t-hf-value-long", "s3cr3t-token-value"} {
		if !got[want] {
			t.Errorf("Secrets lacks the pod_env value %q: %v", want, e.Secrets)
		}
	}
	if got[""] {
		t.Errorf("an empty value must not be a redaction pattern: %q", e.Secrets)
	}
}

// The redaction rule: a non-empty value under a credential-like name (TOKEN, KEY, SECRET, PASSWORD,
// PASS, AUTH, CRED anywhere in the name) always joins the list, whatever its length; any other value
// joins only at 16 bytes or more and when it does not start with "/". The empty value never joins.
func TestPodEnvRedactionRule(t *testing.T) {
	pinLinux(t)
	for _, c := range []struct {
		name, value string
		want        bool
	}{
		{"HF_TOKEN", "ab", true},
		{"API_KEY", "x1", true},
		{"MY_SECRET", "s", true},
		{"DB_PASSWORD", "pw", true},
		{"SOME_PASS", "pw", true},
		{"AUTH_HEADER", "b", true},
		{"CREDENTIALS", "c", true},
		{"HF_TOKEN", "", false},
		{"EMPTY_PASSWORD", "", false},
		{"HF_HOME", "/cache/hf", false},
		{"WORKDIR", "/workspace/some/long/path/here", false},
		{"DTYPE", "bfloat16", false},
		{"LANG_X", "en_US.UTF-8", false},
		{"FIFTEEN", "abcdefghijklmno", false},
		{"SIXTEEN", "abcdefghijklmnop", true},
		{"MODEL", "meta-llama/Llama-3-8B-Instruct", true},
		{"ONE", "1", false},
		{"SLASH", "/", false},
	} {
		t.Run(c.name+"="+c.value, func(t *testing.T) {
			f := newFixture(t, podEnvHost+"pod_env = { "+c.name+" = \""+c.value+"\" }\n", "")
			e := mustLoad(t, f)
			got := false
			for _, s := range e.Secrets {
				if s == c.value {
					got = true
				}
				if s == "" {
					t.Errorf("an empty value must not be a redaction pattern: %q", e.Secrets)
				}
			}
			if got != c.want {
				t.Errorf("%s=%q in Secrets = %v, want %v: %q", c.name, c.value, got, c.want, e.Secrets)
			}
		})
	}
}

// Regression guard: a repo (non-host) file that sets pod_env is refused, by key, with no value.
func TestRepoFileCannotSetPodEnv(t *testing.T) {
	pinLinux(t)
	repo := "version = 1\n[providers.runpod]\npod_env = { HF_HOME = \"s3cr3t-repo-value\" }\n"
	_, err := newFixture(t, podEnvHost, repo).load()
	expect78(t, err, "providers.runpod.pod_env: host-only key")
	if strings.Contains(err.Error(), "s3cr3t") {
		t.Fatalf("the value is printed: %v", err)
	}
}
