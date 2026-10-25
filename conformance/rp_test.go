package conformance

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Exploded-views/bitc/conformance/harness"
	"github.com/Exploded-views/bitc/internal/agent"
	"github.com/Exploded-views/bitc/internal/config"
	"github.com/Exploded-views/bitc/internal/provider"
	"github.com/Exploded-views/bitc/internal/provider/runpod"
	"github.com/Exploded-views/bitc/internal/provider/runpod/runpodfake"
)

func rpProvider(t *testing.T, f *runpodfake.Fake) *runpod.Provider {
	t.Helper()
	raw := map[string]any{"image": "debian", "api_base": f.URL(), "proxy_domain": f.ProxyAddr(),
		"sizes": map[string]any{"cpu8": map[string]any{"cpu_type": "cpu3c", "vcpu": int64(8), "usd_per_hour": 0.24},
			"cpu16": map[string]any{"cpu_type": "cpu3c", "vcpu": int64(16), "usd_per_hour": 0.48}}}
	core := &config.Provider{Type: "runpod", NamePrefix: "bitc", APIKey: "sk-test-key-0123", Raw: raw}
	p, err := runpod.New(core, &config.Effective{Test: &config.Test{HTTPResults: true}})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestK2_NameEncodesAndParses(t *testing.T) {
	if n := runpod.EncodeName("abcdefghij", "0123456789abcdef", strings.Repeat("p", 80), "abcdefghij", "deadbeef", time.Unix(1_800_000_000, 0)); len(n) > 63 {
		t.Fatalf("%d chars with the longest prefix and size key: %s", len(n), n)
	}
	n := runpod.EncodeName("bitc", "0123456789abcdef", "release-tag", "cpu8", "deadbeef", time.Unix(1_800_000_000, 0))
	r, ok := runpod.ParseName("bitc", n)
	if !ok || r.Pool != "release-tag" || r.SizeKey != "cpu8" || r.Job != "deadbeef" {
		t.Fatalf("%s %+v", n, r)
	}
}

func TestK3_CreateBodyShape(t *testing.T) {
	f := runpodfake.New(t, runpodfake.Options{Bin: harness.Bin, Secrets: map[string]string{"app_key": "PRIVATE KEY"}})
	p := rpProvider(t, f)
	runpod.AgentSpec = func(j *provider.JobSpec) agent.Spec {
		return agent.Spec{Job: j.Job, Nonce: "nn", Unprivileged: true, TestRoot: t.TempDir()}
	}
	j := &provider.JobSpec{Job: "j1", Pool: "heavy", SizeKey: "cpu8", Name: "bitc-x", Owner: "tag", DeploySecret: "app_key",
		AgentURL: "https://example.invalid/bitc", AgentSHA256: strings.Repeat("a", 64), Deadline: time.Now().Add(time.Hour)}
	if _, err := p.Launch(context.Background(), j); err != nil {
		t.Fatal(err)
	}
	if len(f.Bodies()) == 0 {
		t.Fatal("no create request reached the API")
	}
	b := f.Bodies()[0]
	env := b["env"].(map[string]any)
	var keys []string
	for k := range env {
		keys = append(keys, k)
	}
	want := "BITC_AGENT_SHA256 BITC_AGENT_URL BITC_API_BASE BITC_DEADLINE BITC_DEPLOY_KEY BITC_OWNER BITC_PROVIDER_TYPE BITC_SPEC"
	if strings.Join(sortStrings(keys), " ") != want || env["BITC_DEPLOY_KEY"] != "{{ RUNPOD_SECRET_app_key }}" || env["BITC_API_BASE"] != f.URL() {
		t.Fatalf("%v", env)
	}
	cpu, _ := b["cpu"].(map[string]any)
	if b["image"] != "debian" || b["cloud"] != "SECURE" || b["disk"] != 40.0 || cpu["id"] != "cpu3c" || cpu["vcpuCount"] != 8.0 {
		t.Fatalf("%v", b)
	}
	var top []string
	for k := range b {
		top = append(top, k)
	}
	if got := strings.Join(sortStrings(top), " "); got != "cloud cmd cpu disk entrypoint env image name ports" {
		t.Fatalf("the create body is not the REST v2 shape: %s", got)
	}
	if ep, _ := b["entrypoint"].([]any); len(ep) != 2 || ep[0] != "/bin/sh" || ep[1] != "-c" {
		t.Fatalf("entrypoint %v", b["entrypoint"])
	}
	if ports, _ := b["ports"].([]any); len(ports) != 1 || ports[0] != "8090/http" {
		t.Fatalf("ports %v", b["ports"])
	}
	runpod.AgentSpec = func(j *provider.JobSpec) agent.Spec {
		return agent.Spec{Job: j.Job, Unprivileged: true, TestRoot: t.TempDir(), Env: []string{"X=" + strings.Repeat("y", 101<<10)}}
	}
	_, err := p.Launch(context.Background(), j)
	var ine *provider.ErrIneligible
	if !errors.As(err, &ine) || len(f.Bodies()) != 1 {
		t.Fatalf("an oversized body was sent: %v", err)
	}
}

func TestRL1_Defaults(t *testing.T) {
	s, err := runpod.DecodeSettings(map[string]any{"image": "x", "sizes": map[string]any{}})
	if err != nil || s.APIBase != "https://api.runpod.io" || s.ProxyDomain != "proxy.runpod.net" || s.Port != 8090 {
		t.Fatal(s, err)
	}
}

func TestRL2_UnknownSizePricedHighest(t *testing.T) {
	f := runpodfake.New(t, runpodfake.Options{Bin: harness.Bin})
	f.AddPod("p1", runpod.EncodeName("bitc", "0123456789abcdef", "heavy", "nosuch", "deadbeef", time.Now().Add(time.Hour)), time.Now(), nil)
	list, err := rpProvider(t, f).List(context.Background(), []string{"bitc-"})
	if err != nil || len(list) != 1 || list[0].USDPerHour != 480_000 {
		t.Fatal(list, err)
	}
}

func TestRL5_KeyOnlyInTheAuthorizationHeader(t *testing.T) {
	f := runpodfake.New(t, runpodfake.Options{Bin: harness.Bin})
	p := rpProvider(t, f)
	_, _ = p.Spend(context.Background())
	_, _ = p.List(context.Background(), []string{"bitc-"})
	for _, c := range f.Calls() {
		if strings.Contains(c, "sk-test-key") {
			t.Fatal("key in a URL")
		}
	}
	if len(f.Auth()) != 2 {
		t.Fatalf("expected 2 authorised calls, saw %v", f.Auth())
	}
	for _, a := range f.Auth() {
		if a != "Bearer sk-test-key-0123" {
			t.Fatalf("%q", a)
		}
	}
}

func TestRL7_MethodBodyStatus(t *testing.T) {
	f := runpodfake.New(t, runpodfake.Options{Bin: harness.Bin})
	f.SetSpend(1.5)
	m, err := rpProvider(t, f).Spend(context.Background())
	if err != nil || m != 1_500_000 {
		t.Fatal(m, err)
	}
}

func TestRL8_HTTPMapping(t *testing.T) {
	f := runpodfake.New(t, runpodfake.Options{Bin: harness.Bin})
	p := rpProvider(t, f)
	runpod.AgentSpec = func(j *provider.JobSpec) agent.Spec {
		return agent.Spec{Job: j.Job, Unprivileged: true, TestRoot: t.TempDir()}
	}
	j := &provider.JobSpec{Job: "j", SizeKey: "cpu8", Name: "bitc-y", Deadline: time.Now().Add(time.Hour)}
	f.Behave("transient-create")
	_, err := p.Launch(context.Background(), j)
	var tr *provider.ErrTransient
	if !errors.As(err, &tr) {
		t.Errorf("capacity 400: %v", err)
	}
	f.Behave("unavailable")
	_, err = p.Launch(context.Background(), j)
	var un *provider.ErrUnavailable
	if !errors.As(err, &un) {
		t.Errorf("401: %v", err)
	}
	f.Behave("billing-500")
	if _, err := p.Spend(context.Background()); err == nil || !strings.Contains(err.Error(), "HTTP 500") {
		t.Fatal(err)
	}
}

func TestRL9_ListOnlyConfiguredPrefixes(t *testing.T) {
	f := runpodfake.New(t, runpodfake.Options{Bin: harness.Bin})
	e := time.Now().Add(time.Hour).Truncate(time.Second)
	f.AddPod("a", runpod.EncodeName("bitc", "0123456789abcdef", "heavy", "cpu8", "deadbeef", e), time.Now(), nil)
	f.AddPod("b", "someone-else-pod", time.Now(), nil)
	list, _ := rpProvider(t, f).List(context.Background(), []string{"bitc-"})
	if len(list) != 1 || list[0].ID != "a" || !list[0].Expires.Equal(e) || list[0].Pool != "heavy" {
		t.Fatalf("%+v", list)
	}
}

func TestRL10_UnparsableNameCountedHigh(t *testing.T) {
	f := runpodfake.New(t, runpodfake.Options{Bin: harness.Bin})
	f.AddPod("a", "bitc-garbled", time.Now(), nil)
	list, _ := rpProvider(t, f).List(context.Background(), []string{"bitc-"})
	if len(list) != 1 || list[0].USDPerHour != 480_000 || !list[0].Expires.IsZero() {
		t.Fatalf("%+v", list)
	}
}

func sortStrings(a []string) []string {
	for i := 1; i < len(a); i++ {
		for j := i; j > 0 && a[j] < a[j-1]; j-- {
			a[j], a[j-1] = a[j-1], a[j]
		}
	}
	return a
}

// A 4xx create answer puts the API's message (first line, never the env) in the error, so a live
// 422 is diagnosable; the API key never appears either.
func TestK3_CreateRefusalCarriesTheAPIMessage(t *testing.T) {
	f := runpodfake.New(t, runpodfake.Options{Bin: harness.Bin})
	p := rpProvider(t, f)
	runpod.AgentSpec = func(j *provider.JobSpec) agent.Spec {
		return agent.Spec{Job: j.Job, Nonce: "nn", Unprivileged: true, TestRoot: t.TempDir()}
	}
	f.Behave("refuse-create")
	j := &provider.JobSpec{Job: "j1", Pool: "heavy", SizeKey: "cpu8", Name: "bitc-x", Owner: "tag",
		AgentURL: "https://example.invalid/bitc", AgentSHA256: strings.Repeat("a", 64), Deadline: time.Now().Add(time.Hour)}
	_, err := p.Launch(context.Background(), j)
	var ine *provider.ErrIneligible
	if !errors.As(err, &ine) || !strings.Contains(err.Error(), "HTTP 422: unknown field zzz") {
		t.Fatalf("%v", err)
	}
	if strings.Contains(err.Error(), "second line") || strings.Contains(err.Error(), "BITC_SPEC") || strings.Contains(err.Error(), "sk-test-key") {
		t.Fatalf("more than the first line of the message: %v", err)
	}
}

// Pod status is the v2 `status` field; a pod the API reports EXITED ends the watch as lost.
func TestK3_ListReadsTheV2StatusField(t *testing.T) {
	f := runpodfake.New(t, runpodfake.Options{Bin: harness.Bin})
	f.AddPod("p1", runpod.EncodeName("bitc", "0123456789abcdef", "heavy", "cpu8", "deadbeef", time.Now().Add(time.Hour)), time.Now(), nil)
	list, err := rpProvider(t, f).List(context.Background(), []string{"bitc-"})
	if err != nil || len(list) != 1 || list[0].State != "running" {
		t.Fatal(list, err)
	}
}

// The v2 Pod schema names the per-hour price `cost`; spec 8.7 takes max(size price, API price).
func TestK3_ListAppliesTheV2CostWhenAboveTheSizePrice(t *testing.T) {
	f := runpodfake.New(t, runpodfake.Options{Bin: harness.Bin})
	name := runpod.EncodeName("bitc", "0123456789abcdef", "heavy", "cpu8", "deadbeef", time.Now().Add(time.Hour))
	f.AddPod("p1", name, time.Now(), nil)
	f.SetCost("p1", 0.9)
	f.AddPod("p2", runpod.EncodeName("bitc", "0123456789abcdef", "heavy", "cpu8", "deadbeee", time.Now().Add(time.Hour)), time.Now(), nil)
	f.SetCost("p2", 0.1) // below the size price: the size price stays
	list, err := rpProvider(t, f).List(context.Background(), []string{"bitc-"})
	if err != nil || len(list) != 2 {
		t.Fatal(list, err)
	}
	got := map[string]config.Micros{}
	for _, r := range list {
		got[r.ID] = r.USDPerHour
	}
	if got["p1"] != 900_000 || got["p2"] != 240_000 {
		t.Fatalf("prices %v", got)
	}
}
