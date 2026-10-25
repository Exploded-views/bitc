package remote

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Exploded-views/bitc/internal/config"
	"github.com/Exploded-views/bitc/internal/provider"
)

type doctorStub struct {
	stubBase
	lines []provider.DoctorLine
}

func (doctorStub) SweepRules() provider.SweepPolicy {
	return provider.SweepPolicy{Noun: "thing", TimerRequired: true}
}
func (d doctorStub) DoctorLines(context.Context) []provider.DoctorLine { return d.lines }

func doctorEnv(t *testing.T, lines ...provider.DoctorLine) (*config.Effective, string) {
	t.Helper()
	provider.Register("doctorstub", provider.Factory{
		New: func(*config.Provider, *config.Effective) (provider.Provider, error) {
			return doctorStub{lines: lines}, nil
		},
	})
	acct := filepath.Join(t.TempDir(), "acct")
	e := &config.Effective{Test: &config.Test{AccountingDir: acct}, Worktree: t.TempDir(),
		Providers: map[string]*config.Provider{"doctorstub": {APIKey: "k", NamePrefix: "bitc"}}}
	return e, filepath.Join(acct, "remote", "doctorstub")
}

func TestDoctorSweepTimerIsMandatoryForOneResourcePerJobProviders(t *testing.T) {
	e, dir := doctorEnv(t)
	run := func() (string, bool) {
		var out bytes.Buffer
		failed, _ := Doctor(e, &out)
		return out.String(), failed
	}
	out, failed := run()
	if !failed || !strings.Contains(out, "fail doctorstub sweep timer: no sweep has run in the last 30 minutes") {
		t.Fatalf("no stamp: %q", out)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	stamp := func(ago time.Duration) {
		if err := os.WriteFile(filepath.Join(dir, "last-sweep"), []byte(strconv.FormatInt(time.Now().Add(-ago).Unix(), 10)+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	stamp(45 * time.Minute)
	if out, failed := run(); !failed || !strings.Contains(out, "fail doctorstub sweep timer: no sweep has run in the last 30 minutes (the last sweep was 45m ago)") {
		t.Fatalf("old stamp: %q", out)
	}
	stamp(5 * time.Minute)
	if out, failed := run(); strings.Contains(out, "fail doctorstub sweep timer") || !strings.Contains(out, "ok doctorstub sweep timer: the last sweep was 5m ago") {
		t.Fatalf("fresh stamp: %q failed=%v", out, failed)
	}
}

func TestDoctorPrintsTheProvidersOwnLines(t *testing.T) {
	e, dir := doctorEnv(t, provider.DoctorLine{Level: "fail", Check: "thing org", Detail: "thing x is foreign"},
		provider.DoctorLine{Level: "warn", Check: "thing installs", Detail: "two"})
	_ = os.MkdirAll(dir, 0o700)
	_ = os.WriteFile(filepath.Join(dir, "last-sweep"), []byte(strconv.FormatInt(time.Now().Unix(), 10)), 0o600)
	var out bytes.Buffer
	failed, _ := Doctor(e, &out)
	if !failed || !strings.Contains(out.String(), "fail thing org: thing x is foreign\n") || !strings.Contains(out.String(), "warn thing installs: two\n") {
		t.Fatalf("%q", out.String())
	}
}
