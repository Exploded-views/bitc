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
