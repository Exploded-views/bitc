package plugin

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/Exploded-views/bitc/conformance/harness"
	"github.com/Exploded-views/bitc/internal/config"
	"github.com/Exploded-views/bitc/internal/provider"
	"github.com/Exploded-views/bitc/internal/provider/plugin"
)

// PL1: unknown fields are ignored everywhere; a missing required field or a wrong type is a
// protocol error (providers delta A.4.6).
func TestPL1_UnknownFieldsAreIgnoredAndMissingOrMistypedOnesAreProtocolErrors(t *testing.T) {
	e := newEnv(t, opts{fault: "extra-fields", helloFault: "extra-fields"}) // the plugin adds a field nobody knows to hello, configure, results, events and errors
	r := e.job()
	if r.Code != 0 || r.Stdout != "hi\n" || e.ranLocally() || strings.Contains(r.Stderr, "protocol error") {
		t.Fatalf("a remote run with extra fields: %+v", r)
	}
	e.api.Behave("create-fails") // an error object with an extra field is still a transient error: the job moves on
	r = e.job()
	if r.Code != 0 || !e.ranLocally() || !strings.Contains(r.Stderr, "running heavy locally: ") || strings.Contains(r.Stderr, "protocol error") {
		t.Fatalf("an error object with extra fields: %+v", r)
	}
	for _, c := range []struct{ fault, hello, want string }{
		{"", "hello-no-caps", "plugin protocol error: hello: "},
		{"configure-no-endpoints", "", "plugin protocol error: configure: "},
		{"list-not-array", "", "plugin protocol error: list: "},
	} {
		e := newEnv(t, opts{fault: c.fault, helloFault: c.hello})
		r := e.job()
		if !strings.Contains(r.Stderr, c.want) || e.creates() != 0 {
			t.Errorf("%s%s: want %q and no create: %+v (creates %d)", c.fault, c.hello, c.want, r, e.creates())
		}
	}
}

// PL2: endpoints, arch and max_name come from configure; under [test] a non-loopback endpoint is 78.
func TestPL2_ConfigureSuppliesEndpointsAndArch(t *testing.T) {
	e := newEnv(t, opts{apiBase: "http://192.0.2.7:9"})
	r := e.h.Run("config", "check")
	if r.Code != 78 || !strings.Contains(r.Stderr, `providers.fakex: [test]: endpoint "192.0.2.7" is not a loopback literal`) {
		t.Errorf("a non-loopback endpoint under [test]: %+v", r)
	}
	e = newEnv(t, opts{})
	if r := e.h.Run("config", "check"); r.Code != 0 {
		t.Errorf("the fake's loopback endpoint: %+v", r)
	}
	e = newEnv(t, opts{extra: "arch = [\"arm64\"]\n"}) // the plugin tells configure it runs arm64 only
	r = e.job()
	if r.Code != 0 || !e.ranLocally() || !strings.Contains(r.Stderr, `arch arm64 not allowed by pool (arch = ["amd64"])`) || e.creates() != 0 {
		t.Errorf("an arm64-only plugin for an amd64 pool: %+v", r)
	}
}

// PL3: an estimate of 0 prices the job at the host price; a negative or non-integer one is over
// the cap and nothing is launched.
func TestPL3_EstimateIsFlooredByTheHostPriceAndRangeChecked(t *testing.T) {
	e := newEnv(t, opts{fault: "estimate-zero", daily: "0.10"}) // $0.24/h for ~77 min does not fit in $0.10
	r := e.job()
	if !e.ranLocally() || !strings.Contains(r.Stderr, "running heavy locally: daily cap $0.10: ") || e.creates() != 0 {
		t.Errorf("estimate 0 under a $0.10 cap: %+v (creates %d)", r, e.creates())
	}
	e = newEnv(t, opts{fault: "estimate-zero"}) // and within a roomy cap it runs
	if r := e.job(); r.Code != 0 || e.ranLocally() || e.creates() != 1 {
		t.Errorf("estimate 0 under a $25 cap: %+v", r)
	}
	for _, f := range []string{"estimate-negative", "estimate-float"} {
		e := newEnv(t, opts{fault: f})
		r := e.job()
		if !e.ranLocally() || !strings.Contains(r.Stderr, "running heavy locally: daily cap $25.00: plugin fakex reported usd_micros out of range") || e.creates() != 0 {
			t.Errorf("%s: %+v (creates %d)", f, r, e.creates())
		}
	}
}

// PL4: a list that omits a live resource is still counted by this host's reservation.
func TestPL4_AnOmittedLiveResourceIsStillCountedByItsReservation(t *testing.T) {
	e := newEnv(t, opts{fault: "list-omits", maxLive: "1"})
	a := e.h.Start(e.jobArgs("sleep 4; echo A")...)
	e.h.Until(func() bool { return len(e.api.Live()) == 1 }, 30*time.Second)
	r := e.job() // list shows nothing, yet one reservation fills max_live = 1
	if !e.ranLocally() || !strings.Contains(r.Stderr, "running heavy locally: 1 live of max_live 1") || e.creates() != 1 {
		t.Errorf("second job: %+v (creates %d)", r, e.creates())
	}
	if ra := a.Wait(); ra.Code != 0 || ra.Stdout != "A\n" {
		t.Errorf("first job: %+v", ra)
	}
}

// PL5: a list that names a resource outside the requested prefixes, or prices one below zero, is a
// protocol error that counts as over the cap.
func TestPL5_OutOfRangeListEntriesCountAsOverTheCap(t *testing.T) {
	for _, c := range []struct{ fault, field string }{{"list-foreign-name", "name"}, {"list-negative-price", "usd_per_hour"}} {
		e := newEnv(t, opts{fault: c.fault})
		r := e.job()
		want := "running heavy locally: daily cap $25.00: plugin fakex reported " + c.field + " out of range"
		if !e.ranLocally() || !strings.Contains(r.Stderr, want) || e.creates() != 0 {
			t.Errorf("%s: want %q: %+v (creates %d)", c.fault, want, r, e.creates())
		}
	}
}

// PL6: a spend figure below today's ledger changes nothing: the ledger is what counts.
func TestPL6_ASpendBelowTheLedgerIsIgnored(t *testing.T) {
	// $600 an hour: a job estimates ~$770 and its first billed minute is ~$9.9. A $775 cap takes
	// the first job but not a second one once the ledger holds the first one's cost; the fake
	// reports $0 spend all along.
	e := newEnv(t, opts{sizes: "{ cpu8 = { usd_per_hour = 600 } }", daily: "775"})
	if r := e.job(); r.Code != 0 || e.ranLocally() {
		t.Fatalf("first job: %+v", r)
	}
	if !strings.Contains(slurp(filepath.Join(e.acct, "ledger.jsonl")), `"event":"end"`) {
		t.Fatalf("no ledger end line: %s", slurp(filepath.Join(e.acct, "ledger.jsonl")))
	}
	r := e.job()
	if !e.ranLocally() || !regexp.MustCompile(`running heavy locally: daily cap \$775\.00: spent \$[1-9]`).MatchString(r.Stderr) || e.creates() != 1 {
		t.Errorf("second job: want a cap note whose spent figure is the ledger's: %+v (creates %d)", r, e.creates())
	}
}

// PL7: two bitc processes with one plugin: neither removes the other's TMPDIR; a dead process's
// directory is pruned at the next spawn.
func TestPL7_PerProcessTmpdirsSurviveEachOtherAndDeadOnesArePruned(t *testing.T) {
	e := newEnv(t, opts{})
	tmp := filepath.Join(e.own, "tmp")
	stale := filepath.Join(tmp, "2000000000-1") // no such process
	if err := os.MkdirAll(stale, 0o700); err != nil {
		t.Fatal(err)
	}
	entries := func() []string {
		ents, _ := os.ReadDir(tmp)
		var out []string
		for _, d := range ents {
			out = append(out, d.Name())
		}
		return out
	}
	a := e.h.Start(e.jobArgs("sleep 6")...)
	e.h.Until(func() bool { return len(e.api.Live()) == 1 }, 30*time.Second)
	if got := entries(); len(got) != 1 || got[0] == "2000000000-1" {
		t.Fatalf("after the first spawn: %v, want only the first process's directory (the dead one pruned)", got)
	}
	b := e.h.Start(e.jobArgs("sleep 1")...)
	e.h.Until(func() bool { return len(e.api.Live()) == 2 }, 30*time.Second)
	if got := entries(); len(got) != 2 {
		t.Errorf("with two processes: %v, want both directories", got)
	}
	if rb := b.Wait(); rb.Code != 0 {
		t.Errorf("second job: %+v", rb)
	}
	if got := entries(); len(got) != 1 {
		t.Errorf("after the second process ended: %v, want the first one's directory only", got)
	}
	if ra := a.Wait(); ra.Code != 0 {
		t.Errorf("first job: %+v", ra)
	}
	if got := entries(); len(got) != 0 {
		t.Errorf("after both ended: %v", got)
	}
}

// diag is what the reference plugin's `diag` fault records about its own process.
type diag struct {
	Env        []string          `json:"env"`
	Args       []string          `json:"args"`
	Cmdline    string            `json:"cmdline"`
	NoNewPrivs string            `json:"nonewprivs"`
	Secrets    map[string]string `json:"secrets"`
}

func readDiag(t *testing.T, e *env) diag {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(e.own, "diag-*.json"))
	if len(files) != 1 {
		t.Fatalf("diag files: %v", files)
	}
	var d diag
	if err := json.Unmarshal([]byte(slurp(files[0])), &d); err != nil {
		t.Fatal(err)
	}
	return d
}

// PL8: secret_files values arrive on fd 3 and nowhere else; config and doctor never show them; a
// declared secret given inline is 78.
func TestPL8_SecretsArriveOnFd3AndAreRedactedEverywhere(t *testing.T) {
	const tok, apiKey = "tok-secret-value-777", "sk-test-0123456789"
	e := newEnv(t, opts{fault: "diag", declare: "token_secret", secrets: map[string]string{"token_secret": tok}})
	r := e.job()
	if r.Code != 0 || r.Stdout != "hi\n" {
		t.Fatalf("run: %+v", r)
	}
	d := readDiag(t, e)
	if d.Secrets["token_secret"] != tok || d.Secrets["api_key"] != apiKey {
		t.Errorf("fd 3 carried %v, want api_key and token_secret", d.Secrets)
	}
	hay := strings.Join(d.Env, "\n") + "\n" + d.Cmdline + "\n" + strings.Join(d.Args, "\n")
	if strings.Contains(hay, tok) || strings.Contains(hay, apiKey) || !strings.Contains(hay, "BITC_SECRETS_FD=3") {
		t.Errorf("the plugin's argv or env holds a secret (or lacks BITC_SECRETS_FD=3): %s", hay)
	}
	for _, args := range [][]string{{"config", "check"}, {"config", "explain"}, {"doctor", "--remote"}} {
		out := e.h.Run(args...)
		if strings.Contains(out.Stdout+out.Stderr, tok) || strings.Contains(out.Stdout+out.Stderr, apiKey) {
			t.Errorf("%v printed a secret:\n%s%s", args, out.Stdout, out.Stderr)
		}
	}
	bad := newEnv(t, opts{declare: "token_secret", secrets: map[string]string{"token_secret": tok}, extra: "token_secret = \"inline\"\n"})
	if r := bad.h.Run("config", "check"); r.Code != 78 || !strings.Contains(r.Stderr, "providers.fakex.token_secret: secret; set it in plugin.secret_files") {
		t.Errorf("inline declared secret: %+v", r)
	}
}

// PL8 (echo-launch): the plugin copies each launch request to stderr; under BITC_LOG=debug, and in
// a protocol-error tail, none of B, K, M (raw or hex) or a forced value appears.
func TestPL8_EchoLaunchLeaksNoJobSecretInDebugOrAProtocolErrorTail(t *testing.T) {
	const forced = "forced-canary-value-91"
	for _, fault := range []string{"echo-launch", "echo-launch,launch-malformed"} {
		e := newEnv(t, opts{fault: fault, extra: "send_env = \"forced\"\n",
			limits:     "forward_env_allow = [\"DEPLOY_TOKEN\"]\nforward_env_force = [\"DEPLOY_TOKEN\"]\n",
			repoRemote: "forward_env = [\"DEPLOY_TOKEN\"]\n"})
		r := e.h.RunWith([]string{"BITC_LOG=debug", "DEPLOY_TOKEN=" + forced}, e.jobArgs("echo hi")...)
		all := r.Stdout + r.Stderr
		for _, b := range []byte{0x0b, 0x0c, 0x0d} {
			raw := strings.Repeat(string([]byte{b}), 32)
			for _, form := range []string{raw, hex.EncodeToString([]byte(raw)), forced} {
				if strings.Contains(all, form) {
					t.Errorf("%s: %q leaked into bitc's output:\n%s", fault, form, all)
				}
			}
		}
		if !strings.Contains(r.Stderr, "bitc: fakex: plugin: ") {
			t.Errorf("%s: no debug plugin lines (the check is vacuous):\n%s", fault, r.Stderr)
		}
		if fault != "echo-launch" && !strings.Contains(r.Stderr, "plugin protocol error") {
			t.Errorf("%s: no protocol error tail:\n%s", fault, r.Stderr)
		}
	}
}

// PL9: max_name 59 is 78; 60 keeps names at 60 characters with job8 and E intact, for a
// 10-character prefix and size key.
func TestPL9_MaxNameBelow60Is78AndNamesFitAt60(t *testing.T) {
	e := newEnv(t, opts{extra: "max_name = 59\n"})
	if r := e.h.Run("config", "check"); r.Code != 78 || !strings.Contains(r.Stderr, "providers.fakex.plugin: max_name 59 is below 60, the shortest name bitc builds") {
		t.Errorf("max_name 59: %+v", r)
	}
	e = newEnv(t, opts{extra: "max_name = 60\nname_prefix = \"abcdefghij\"\n", sizes: "{ sizeabcdef = { usd_per_hour = 0.24 } }",
		size: "sizeabcdef", pool: "a-pool-name-that-is-much-longer-than-any-limit-allows"})
	if r := e.job(); r.Code != 0 || e.ranLocally() {
		t.Fatalf("max_name 60: %+v", r)
	}
	for _, c := range e.api.Calls() {
		if c.Method != "POST" || c.Path != "/v1/instances" {
			continue
		}
		var body struct{ Name string }
		_ = json.Unmarshal([]byte(c.Body), &body)
		if len(body.Name) > 60 || !strings.HasPrefix(body.Name, "abcdefghij-") || !regexp.MustCompile(`-sizeabcdef-[0-9a-f]{8}-[0-9]{10}$`).MatchString(body.Name) {
			t.Errorf("name %q (%d characters): want at most 60 with job8 and E intact", body.Name, len(body.Name))
		}
		return
	}
	t.Error("no create call")
}

// adapter builds the host's adapter in-process over the pinned reference plugin: the scripted bitc
// side of the rows that need to hand it a fixed JobSpec or to move a file between load and spawn.
func adapter(t *testing.T, e *env) *plugin.Provider {
	t.Helper()
	core := &config.Provider{Type: "fakex", NamePrefix: "bitc", APIKey: "sk-test-0123456789", DailyUSD: 25_000_000, MaxLive: 6,
		LaunchWait: time.Minute, StartTimeout: 2 * time.Minute, Breaker: config.Breaker{Failures: 3, Window: time.Hour},
		FallbackUntil: "attach", FallbackCap: "attach", SendEnv: "forwarded",
		Plugin:   &config.PluginPin{Path: e.path, SHA256: e.sum, SecretFiles: map[string]string{}},
		Sizes:    map[string]config.Micros{"cpu8": 240_000},
		SizesRaw: map[string]any{"cpu8": map[string]any{"usd_per_hour": 0.24}},
		Raw:      map[string]any{"api_base": e.api.URL, "fault": "diag"}}
	eff := &config.Effective{Host: config.Host{StateDir: e.h.State}, Test: &config.Test{AccountingDir: filepath.Join(e.h.Root, "account"), TimeScale: 1}}
	p, err := plugin.New(core, eff)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	return p
}

// PL10: the job object of a scripted launch equals the golden file byte for byte after key sorting.
func TestPL10_TheJobObjectMatchesTheGoldenFile(t *testing.T) {
	e := newEnv(t, opts{})
	p := adapter(t, e)
	if err := p.Start(context.Background(), provider.Runtime{}); err != nil {
		t.Fatal(err)
	}
	j := scriptedJob()
	_, _ = p.Launch(context.Background(), j) // the fake refuses a non-loopback clone URL after recording the call: the outcome is beside the point
	var attach *harness.APICall
	for _, c := range e.api.Calls() {
		if c.Attach {
			c := c
			attach = &c
		}
	}
	if attach == nil {
		t.Fatalf("no attach call: %+v", e.api.Calls())
	}
	var body struct {
		Job       json.RawMessage `json:"job"`
		DeployKey string          `json:"deploy_key"`
	}
	if err := json.Unmarshal([]byte(attach.Body), &body); err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile("testdata/job-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	if got := canon(t, body.Job); got != canon(t, want) {
		t.Errorf("the job object differs from testdata/job-v1.json:\n%s", got)
	}
	for _, banned := range []string{"deploy_secret", "deploy_key", "DeploySecret", "RUNPOD_SECRET_NAME", "-----KEY-----"} {
		if strings.Contains(string(body.Job), banned) {
			t.Errorf("the job object carries %q", banned)
		}
	}
	var keys map[string]json.RawMessage
	_ = json.Unmarshal(body.Job, &keys)
	if _, has := keys["test"]; has {
		t.Error("the job object has a test key outside [test]")
	}
	if body.DeployKey != j.DeployKey {
		t.Errorf("the deploy key travels in secrets only: got %q", body.DeployKey)
	}
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

func scriptedJob() *provider.JobSpec {
	fill := func(b byte) (a [32]byte) {
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
		Bearer: fill(0x0b), MacKey: fill(0x0c), Nonce: fill(0x0d), Install: "0123456789abcdef", Name: "bitc-0123456789abcdef-heavy-cpu8-deadbeef-1793000100",
		Owner: strings.Repeat("ef", 32), AgentURL: "https://x/agent", AgentSHA256: strings.Repeat("12", 32)}
}

// PL11: a plugin that creates the resource and answers transient without an id is caught by the
// name check; with a failing list it is reaped as ambiguous; a true id-less transient retries once.
func TestPL11_CreateThenTransientWithoutAnIDIsCaughtByTheNameCheck(t *testing.T) {
	e := newEnv(t, opts{fault: "create-then-transient-no-id"})
	r := e.job()
	if r.Code != 75 || !regexp.MustCompile(`STARVED heavy \(fakex: launch said transient but bitc-[0-9a-f]{16}-heavy-cpu8-[0-9a-f]{8}-[0-9]+ exists\)`).MatchString(r.Stderr) {
		t.Fatalf("want 75 and `launch said transient but <name> exists`: %+v", r)
	}
	if e.ranLocally() || strings.Contains(r.Stderr, "trying") || strings.Contains(r.Stderr, "locally") {
		t.Errorf("another provider was tried: %s", r.Stderr)
	}
	if e.creates() != 1 || len(e.api.Live()) != 0 || len(e.api.Torn()) != 1 || e.failures() != 1 {
		t.Errorf("creates %d, live %v, torn %v, breaker failures %d: want one create, torn down, one failure", e.creates(), e.api.Live(), e.api.Torn(), e.failures())
	}
	e = newEnv(t, opts{fault: "create-then-transient-no-id,list-fails-after-create"})
	r = e.job()
	if r.Code != 75 || !strings.Contains(r.Stderr, "STARVED heavy (fakex: launch ambiguous: ") || e.ranLocally() || strings.Contains(r.Stderr, "trying") {
		t.Errorf("a failing list: want 75 and `launch ambiguous`, no other provider: %+v", r)
	}
	if e.creates() != 1 || len(e.api.Live()) != 0 || len(e.api.Torn()) != 1 || e.failures() != 1 {
		t.Errorf("a failing list: creates %d, live %v, torn %v, breaker failures %d: want one create, reaped by name on the respawned plugin, one failure", e.creates(), e.api.Live(), e.api.Torn(), e.failures())
	}
	e = newEnv(t, opts{})
	e.api.Behave("create-fails") // a true id-less transient: nothing was created
	r = e.job()
	if r.Code != 0 || !e.ranLocally() || !strings.Contains(r.Stderr, "running heavy locally: ") || e.creates() != 2 {
		t.Errorf("nothing created: want one retry (two creates), then the next provider: %+v (creates %d)", r, e.creates())
	}
}

// PL12: setuid, setgid and file-capability plugins are 78 at load; the bit set between load and
// spawn is `insecure` at spawn with no exec; the plugin runs under no-new-privs.
func TestPL12_InsecurePluginFilesAreRefusedAndThePluginRunsNoNewPrivs(t *testing.T) {
	e := newEnv(t, opts{fault: "diag"})
	for name, bit := range map[string]os.FileMode{"setuid": os.ModeSetuid, "setgid": os.ModeSetgid} {
		if err := os.Chmod(e.path, 0o755|bit); err != nil {
			t.Fatal(err)
		}
		r := e.h.Run("config", "check")
		if r.Code != 78 || !strings.Contains(r.Stderr, `providers.fakex.plugin.path: insecure: setuid/setgid/capabilities: "`+e.path+`"`) {
			t.Errorf("%s at load: %+v", name, r)
		}
	}
	if err := os.Chmod(e.path, 0o755); err != nil {
		t.Fatal(err)
	}
	cap := []byte{0x01, 0x00, 0x00, 0x02, 0, 0, 0, 0, 0, 0, 0, 0} // a vfs_cap_data revision 2 blob
	if err := unix.Setxattr(e.path, "security.capability", cap, 0); err != nil {
		t.Logf("file capabilities cannot be set here (%v): that case is skipped", err)
	} else {
		if r := e.h.Run("config", "check"); r.Code != 78 || !strings.Contains(r.Stderr, "insecure: setuid/setgid/capabilities") {
			t.Errorf("security.capability at load: %+v", r)
		}
		_ = unix.Removexattr(e.path, "security.capability")
	}
	// the bit set after load, before the spawn: refused at spawn, the plugin never runs
	p := adapter(t, e)
	if err := os.Chmod(e.path, 0o755|os.ModeSetuid); err != nil {
		t.Fatal(err)
	}
	err := p.Start(context.Background(), provider.Runtime{})
	if err == nil || !strings.Contains(err.Error(), "insecure: setuid/setgid/capabilities") {
		t.Errorf("Start with the bit set: %v, want `insecure: setuid/setgid/capabilities`", err)
	}
	if files, _ := filepath.Glob(filepath.Join(e.own, "diag-*.json")); len(files) != 0 || len(e.api.Calls()) != 0 {
		t.Errorf("the plugin ran (diag %v, calls %v)", files, e.api.Calls())
	}
	if err := os.Chmod(e.path, 0o755); err != nil {
		t.Fatal(err)
	}
	if r := e.job(); r.Code != 0 || r.Stdout != "hi\n" {
		t.Fatalf("run: %+v", r)
	}
	if d := readDiag(t, e); d.NoNewPrivs != "1" {
		t.Errorf("NoNewPrivs = %q, want 1", d.NoNewPrivs)
	}
}
