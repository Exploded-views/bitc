package plugin

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Exploded-views/bitc/internal/config"
	"github.com/Exploded-views/bitc/internal/provider"
)

func sampleJob() *provider.JobSpec {
	hex32 := func(b byte) (a [32]byte) {
		for i := range a {
			a[i] = b
		}
		return a
	}
	return &provider.JobSpec{Job: "j1", Pool: "heavy", Argv: []string{"npm", "test"}, Cwd: "sub", Env: []string{"CI=1", "A=b=c"},
		CloneURL: "git@example.com:o/r.git", Ref: "refs/bitc/jobs/j1", Sha: strings.Repeat("ab", 20), SizeKey: "cpu8",
		Timeout: 90 * time.Minute, Created: time.Date(2026, 10, 23, 5, 0, 0, 900, time.UTC),
		Deadline: time.Date(2026, 10, 23, 7, 0, 0, 0, time.FixedZone("x", 3600)), Expiry: time.Date(2026, 10, 23, 7, 15, 0, 0, time.UTC),
		Artifacts: []config.Artifact{{Path: "out", Replace: true}}, Packages: []string{"git"},
		Tools:     []config.Tool{{Name: "go", Arch: "amd64", URL: "https://x/go.tgz", SHA256: strings.Repeat("cd", 32)}},
		Setup:     []config.Step{{Name: "deps", RunAs: "job", Argv: []string{"npm", "ci"}}},
		Services:  []config.Service{{Name: "pg", RunAs: "root", Argv: []string{"pg"}, Ready: []string{"pg_isready"}, ReadyTimeout: 30 * time.Second}},
		RootSteps: true, RepoID: "repo1", DeploySecret: "RUNPOD_SECRET_NAME", DeployKey: "-----KEY-----",
		Bearer: hex32(0x0b), MacKey: hex32(0x0c), Nonce: hex32(0x0d), Install: "0123456789abcdef", Name: "bitc-0123456789abcdef-heavy-cpu8-deadbeef-1793000100",
		Owner: strings.Repeat("ef", 32), AgentURL: "https://x/agent", AgentSHA256: strings.Repeat("12", 32)}
}

func canon(t *testing.T, b []byte) string {
	t.Helper()
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, b)
	}
	out, _ := json.MarshalIndent(v, "", "  ") // map keys are sorted
	return string(out)
}

func TestJobWireFormatMatchesTheGoldenFile(t *testing.T) {
	b, err := marshalJob(sampleJob())
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile("testdata/job-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	if got := canon(t, b); got != canon(t, want) {
		t.Fatalf("job object differs from testdata/job-v1.json:\n%s", got)
	}
}

func TestJobWireOmitsTestOutsideTestAndNeverCarriesTheDeployKey(t *testing.T) {
	j := sampleJob()
	b, _ := marshalJob(j)
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("marshalJob: %v", err)
	}
	if _, has := m["test"]; has {
		t.Error("test key present outside [test]")
	}
	for _, banned := range []string{"deploy_secret", "deploy_key", "DeploySecret", "RUNPOD_SECRET_NAME", "-----KEY-----"} {
		if bytes.Contains(b, []byte(banned)) {
			t.Errorf("job object carries %q", banned)
		}
	}
	j.Unprivileged, j.TestRoot = true, "/tmp/r"
	b, _ = marshalJob(j)
	_ = json.Unmarshal(b, &m)
	if string(m["test"]) != `{"unprivileged":true,"test_root":"/tmp/r"}` {
		t.Errorf("test = %s", m["test"])
	}
}

func TestJobWireEmptyListsAreArraysAndAnUnusedBearerIsZeros(t *testing.T) {
	b, _ := marshalJob(&provider.JobSpec{})
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("marshalJob: %v", err)
	}
	for _, k := range []string{"argv", "env", "artifacts", "packages", "tools", "setup", "services"} {
		if string(m[k]) != "[]" {
			t.Errorf("%s = %s, want []", k, m[k])
		}
	}
	if string(m["bearer"]) != `"`+strings.Repeat("0", 64)+`"` {
		t.Errorf("bearer = %s", m["bearer"])
	}
	if string(m["timeout_s"]) != "0" || string(m["root_steps"]) != "false" {
		t.Errorf("timeout_s %s root_steps %s", m["timeout_s"], m["root_steps"])
	}
}

const helloOK = `{"protocol":1,"type":"modal","plugin_version":"0.3.1","caps":{"live_logs":true,"artifacts":true,"spend":false,"self_delete":false,"fallback_until":"attach","fixed":false,"multi_job":false},"secrets":["token_secret"],"future":{"x":1}}`

func TestParseHello(t *testing.T) {
	h, err := parseHello([]byte(helloOK), "modal")
	if err != nil {
		t.Fatalf("parseHello: %v", err)
	}
	if h.Version != "0.3.1" || !h.Caps.LiveLogs || !h.Caps.Artifacts || h.Caps.Spend || h.Caps.FallbackUntil != provider.UntilAttach || len(h.Secrets) != 1 || h.Secrets[0] != "token_secret" {
		t.Errorf("hello = %+v", h)
	}
	h, err = parseHello([]byte(strings.Replace(helloOK, `"secrets":["token_secret"],`, "", 1)), "modal")
	if err != nil || len(h.Secrets) != 0 {
		t.Errorf("secrets are optional: %+v %v", h, err)
	}
}

func TestParseHelloRefusals(t *testing.T) {
	caps := `"caps":{"live_logs":true,"artifacts":true,"spend":false,"self_delete":false,"fallback_until":"attach","fixed":false,"multi_job":false}`
	for name, body := range map[string]string{
		"no protocol":       `{"type":"modal","plugin_version":"1",` + caps + `}`,
		"protocol 2":        `{"protocol":2,"type":"modal","plugin_version":"1",` + caps + `}`,
		"other type":        `{"protocol":1,"type":"other","plugin_version":"1",` + caps + `}`,
		"no plugin_version": `{"protocol":1,"type":"modal",` + caps + `}`,
		"no caps":           `{"protocol":1,"type":"modal","plugin_version":"1"}`,
		"caps missing one":  `{"protocol":1,"type":"modal","plugin_version":"1","caps":{"live_logs":true,"artifacts":true,"spend":false,"self_delete":false,"fallback_until":"attach","fixed":false}}`,
		"wrong type":        `{"protocol":"1","type":"modal","plugin_version":"1",` + caps + `}`,
		"bad boundary":      strings.Replace(`{"protocol":1,"type":"modal","plugin_version":"1",`+caps+`}`, `"attach"`, `"later"`, 1),
		"not json":          `[1,2`,
	} {
		_, err := parseHello([]byte(body), "modal")
		var pe *protoErr
		if !errors.As(err, &pe) {
			t.Errorf("%s: got %v, want a protocol error", name, err)
		}
	}
	_, err := parseHello([]byte(strings.Replace(helloOK, `"protocol":1`, `"protocol":2`, 1)), "modal")
	var pe *protoErr
	if !errors.As(err, &pe) || !strings.Contains(pe.Why, "speaks protocol 2, bitc supports [1]") {
		t.Errorf("protocol 2 message: %v", err)
	}
}

func TestParseConfigure(t *testing.T) {
	c, err := parseConfigure([]byte(`{"endpoints":["api.eu.example"],"arch":["amd64","arm64"],"max_name":63,"extra":true}`), "modal")
	if err != nil || len(c.Endpoints) != 1 || c.Endpoints[0] != "api.eu.example" || len(c.Arch) != 2 || c.MaxName != 63 {
		t.Fatalf("configure = %+v, %v", c, err)
	}
	if c, err := parseConfigure([]byte(`{"endpoints":[],"arch":["amd64"],"max_name":60}`), "modal"); err != nil || c.MaxName != 60 {
		t.Errorf("max_name 60 is allowed: %+v %v", c, err)
	}
	for name, body := range map[string]string{
		"no endpoints": `{"arch":["amd64"],"max_name":63}`, "no arch": `{"endpoints":[],"max_name":63}`, "no max_name": `{"endpoints":[],"arch":["amd64"]}`,
		"string max_name": `{"endpoints":[],"arch":["amd64"],"max_name":"63"}`, "not json": `{`,
	} {
		var pe *protoErr
		if _, err := parseConfigure([]byte(body), "modal"); !errors.As(err, &pe) {
			t.Errorf("%s: got %v, want a protocol error", name, err)
		}
	}
	var ec *provider.ErrConfig
	_, err = parseConfigure([]byte(`{"endpoints":[],"arch":["amd64"],"max_name":59}`), "modal")
	if !errors.As(err, &ec) || ec.Why != "providers.modal.plugin: max_name 59 is below 60, the shortest name bitc builds" {
		t.Errorf("max_name 59: %v", err)
	}
}

func TestMapErrorTable(t *testing.T) {
	for _, c := range []struct {
		eo   string
		want string // type and text
	}{
		{`{"class":"ineligible","why":"too big"}`, "*provider.ErrIneligible:too big"},
		{`{"class":"transient","why":"429"}`, "*provider.ErrTransient:429"},
		{`{"class":"unavailable","why":"401"}`, "*provider.ErrUnavailable:401:pause"},
		{`{"class":"unavailable_local","why":"no key"}`, "*provider.ErrUnavailable:no key:local"},
		{`{"class":"setup","why":"apt failed","step":"packages","setup_class":"infra"}`, "*provider.ErrSetup:packages:infra:apt failed"},
		{`{"class":"setup","why":"npm ERR","step":"deps","setup_class":"repo"}`, "*provider.ErrSetup:deps:repo:npm ERR"},
		{`{"class":"config","why":"bad region"}`, "*provider.ErrConfig:bad region"},
		{`{"class":"bogus","why":"x"}`, "*plugin.protoErr:plugin protocol error: unknown error class \"bogus\""},
		{`{"class":"pre_attach","why":"x","id":"r1","inner":{"class":"transient","why":"y"}}`, "*plugin.protoErr:plugin protocol error: error class \"pre_attach\" outside launch"},
		{`{"class":"attached","why":"x","id":"r1"}`, "*plugin.protoErr:plugin protocol error: error class \"attached\" outside launch"},
	} {
		eo, err := parseErrObj([]byte(c.eo))
		if err != nil {
			t.Errorf("%s: parseErrObj: %v", c.eo, err)
			continue
		}
		err = mapError("watch", eo)
		var got string
		var ine *provider.ErrIneligible
		var tr *provider.ErrTransient
		var una *provider.ErrUnavailable
		var se *provider.ErrSetup
		var ec *provider.ErrConfig
		var pe *protoErr
		switch {
		case errors.As(err, &ine):
			got = "*provider.ErrIneligible:" + ine.Why
		case errors.As(err, &tr):
			got = "*provider.ErrTransient:" + tr.Why
		case errors.As(err, &una):
			got = "*provider.ErrUnavailable:" + una.Why
			if una.Local {
				got += ":local"
			} else {
				got += ":pause"
			}
		case errors.As(err, &se):
			got = "*provider.ErrSetup:" + se.Step + ":" + se.Class + ":" + se.Line
		case errors.As(err, &ec):
			got = "*provider.ErrConfig:" + ec.Why
		case errors.As(err, &pe):
			got = "*plugin.protoErr:" + pe.Error()
		default:
			got = "nil/other"
		}
		if got != c.want {
			t.Errorf("%s:\n got  %s\n want %s", c.eo, got, c.want)
		}
	}
	if _, err := parseErrObj([]byte(`{"why":"no class"}`)); err == nil {
		t.Error("an error object without a class is a protocol error")
	}
	if _, err := parseErrObj([]byte(`"just a string"`)); err == nil {
		t.Error("an error object that is not an object is a protocol error")
	}
}
