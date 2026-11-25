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

// Defence in depth: every pod_env value of 8 bytes or more joins the run's redaction list
// (e.Secrets), next to the API key, so a stray echo of one is masked by the same redactor.
func TestPodEnvValuesJoinTheRedactionList(t *testing.T) {
	pinLinux(t)
	f := newFixture(t, podEnvHost+"pod_env = { HF_HOME = \"s3cr3t-hf-value\", A_TOKEN = \"s3cr3t-token-value\", EMPTY = \"\" }\n", "")
	e := mustLoad(t, f)
	got := map[string]bool{}
	for _, s := range e.Secrets {
		got[s] = true
	}
	for _, want := range []string{"s3cr3t-hf-value", "s3cr3t-token-value"} {
		if !got[want] {
			t.Errorf("Secrets lacks the pod_env value %q: %v", want, e.Secrets)
		}
	}
	if got[""] {
		t.Errorf("an empty value must not be a redaction pattern: %q", e.Secrets)
	}
}

// Only values of 8 bytes or more join the redaction list: a short value such as "1", "true" or "/"
// would otherwise rewrite those strings in every history line and status output on the host.
func TestPodEnvShortValuesStayOutOfTheRedactionList(t *testing.T) {
	pinLinux(t)
	const eight, token = "abcdefgh", "hf_AbCdEfGhIjKlMnOpQrStUvWxYz0123456789"
	f := newFixture(t, podEnvHost+"pod_env = { ONE = \"1\", TRUE = \"true\", SLASH = \"/\", SEVEN = \"abcdefg\", EIGHT = \""+eight+"\", TOKEN = \""+token+"\", EMPTY = \"\" }\n", "")
	e := mustLoad(t, f)
	got := map[string]bool{}
	for _, s := range e.Secrets {
		got[s] = true
	}
	for _, short := range []string{"", "1", "true", "/", "abcdefg"} {
		if got[short] {
			t.Errorf("the value %q is under 8 bytes and must not be a redaction pattern: %q", short, e.Secrets)
		}
	}
	for _, want := range []string{eight, token} {
		if !got[want] {
			t.Errorf("Secrets lacks the pod_env value %q: %q", want, e.Secrets)
		}
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
