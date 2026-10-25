package agent_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Exploded-views/bitc/internal/agent"
	"github.com/Exploded-views/bitc/internal/agent/agenttest"
)

func TestRunMarkersResultsAndMAC(t *testing.T) {
	f := agenttest.NewFixture(t, "#!/bin/sh\npwd\necho to-err >&2\nexit 3\n")
	log, code := f.Run(t)
	for _, want := range []string{"__BITC_BOOT__ n0nce", "__BITC_BEGIN__ n0nce", "__BITC_EXIT__ n0nce 3"} {
		if !strings.Contains(log, want) {
			t.Fatalf("missing %q in\n%s", want, log)
		}
	}
	if code != 3 {
		t.Fatal(code)
	}
	dir := filepath.Join(f.Root, "j1")
	raw, _ := os.ReadFile(filepath.Join(dir, "result.json"))
	rf, err := agent.VerifyResult(f.K, raw)
	if err != nil || rf.Exit != 3 {
		t.Fatal(rf, err)
	}
	outB, _ := os.ReadFile(filepath.Join(dir, "out.log"))
	errB, _ := os.ReadFile(filepath.Join(dir, "err.log"))
	if !strings.HasSuffix(strings.TrimSpace(string(outB)), "/sub") || string(errB) != "to-err\n" {
		t.Fatalf("%q %q", outB, errB)
	}
}

func TestRefMismatchNeverRunsTheCommand(t *testing.T) {
	f := agenttest.NewFixture(t, "#!/bin/sh\necho RAN\n")
	f.Spec.Sha = strings.Repeat("0", 40)
	log, _ := f.Run(t)
	if !strings.Contains(log, "__BITC_SETUP_FAILED__ n0nce ref-mismatch") || strings.Contains(log, "RAN") {
		t.Fatal(log)
	}
}

func TestEmptyDeployKeyFailsForSSHClones(t *testing.T) {
	f := agenttest.NewFixture(t, "#!/bin/sh\n")
	f.Spec.CloneURL = "git@example.invalid:o/r.git"
	log, _ := f.Run(t)
	if !strings.Contains(log, "__BITC_SETUP_FAILED__ n0nce deploy-key") {
		t.Fatal(log)
	}
}

func TestSecondBootIsLostAndNeverRunsTwice(t *testing.T) {
	f := agenttest.NewFixture(t, "#!/bin/sh\necho RAN\n")
	f.Run(t)
	log, _ := f.Run(t)
	if !strings.Contains(log, "__BITC_BOOT__ n0nce") || !strings.Contains(log, "__BITC_LOST__ n0nce") || strings.Contains(log, "__BITC_BEGIN__") {
		t.Fatal(log)
	}
}

func TestFailingSetupStepPrintsItsStderrTail(t *testing.T) {
	f := agenttest.NewFixture(t, "#!/bin/sh\n")
	f.Spec.Setup = []agent.Step{{Name: "deps", RunAs: "job", Argv: []string{"sh", "-c", "echo line1 >&2; echo boom >&2; exit 2"}}}
	log, _ := f.Run(t)
	i, j := strings.Index(log, "boom"), strings.Index(log, "__BITC_SETUP_FAILED__ n0nce deps repo line1\n")
	if i < 0 || j < 0 || i > j {
		t.Fatal(log) // exited non-zero by itself: repo, with the first stderr line
	}
}

func TestSetupFailureClasses(t *testing.T) {
	for _, c := range []struct {
		name string
		edit func(f *agenttest.Fixture)
		want string
	}{
		{"killed-step", func(f *agenttest.Fixture) {
			f.Spec.Setup = []agent.Step{{Name: "deps", RunAs: "job", Argv: []string{"sh", "-c", "kill -9 $$"}}}
		}, "__BITC_SETUP_FAILED__ n0nce deps infra "},
		{"service-exits", func(f *agenttest.Fixture) {
			f.Spec.RootSteps = true
			f.Spec.Services = []agent.Service{{Name: "db", RunAs: "job", Argv: []string{"sh", "-c", "echo bad config >&2; exit 3"},
				Ready: []string{"false"}, ReadyTimeout: agent.Duration(20 * time.Second)}}
		}, "__BITC_SETUP_FAILED__ n0nce db repo bad config\n"},
		{"service-never-ready", func(f *agenttest.Fixture) {
			f.Spec.RootSteps = true
			f.Spec.Services = []agent.Service{{Name: "db", RunAs: "job", Argv: []string{"sleep", "30"},
				Ready: []string{"false"}, ReadyTimeout: agent.Duration(time.Second)}}
		}, "__BITC_SETUP_FAILED__ n0nce db infra "},
		{"ref-mismatch", func(f *agenttest.Fixture) { f.Spec.Sha = strings.Repeat("0", 40) }, "__BITC_SETUP_FAILED__ n0nce ref-mismatch infra "},
	} {
		f := agenttest.NewFixture(t, "#!/bin/sh\necho RAN\n")
		c.edit(f)
		if log, _ := f.Run(t); !strings.Contains(log, c.want) || strings.Contains(log, "RAN") {
			t.Errorf("%s: %q", c.name, log)
		}
	}
}

func TestMacKeyNeverReachesTheJob(t *testing.T) {
	f := agenttest.NewFixture(t, "#!/bin/sh\nenv\ncat /proc/$PPID/cmdline\n")
	f.Run(t)
	b, _ := os.ReadFile(filepath.Join(f.Root, "j1", "out.log"))
	if !strings.Contains(string(b), "A=1") || strings.Contains(string(b), f.Spec.MacKey) || strings.Contains(string(b), f.Spec.Bearer) {
		t.Fatal("K or B reached the job, or the job did not run")
	}
}

// A step or service name with whitespace would shift the marker's class field (spec 8.4 step 5).
func TestUnsafeStepAndServiceNamesFailAsRepoBeforeRunning(t *testing.T) {
	for _, c := range []struct {
		name string
		edit func(f *agenttest.Fixture)
	}{
		{"setup", func(f *agenttest.Fixture) {
			f.Spec.Setup = []agent.Step{{Name: "x infra", RunAs: "job", Argv: []string{"true"}}}
		}},
		{"service", func(f *agenttest.Fixture) {
			f.Spec.RootSteps = true
			f.Spec.Services = []agent.Service{{Name: "db repo", RunAs: "job", Argv: []string{"true"}, Ready: []string{"true"}}}
		}},
	} {
		f := agenttest.NewFixture(t, "#!/bin/sh\necho RAN\n")
		c.edit(f)
		log, _ := f.Run(t)
		if !strings.Contains(log, "__BITC_SETUP_FAILED__ n0nce step-name repo ") || strings.Contains(log, "RAN") {
			t.Errorf("%s: %q", c.name, log)
		}
	}
}

// Spec 8.4: the job gets exactly the remote env (the declared vars plus HOME and PATH), nothing
// from the agent's own environment. /proc/self/environ is read through cat because a shell
// redirect would read the shell's own (dash-emptied) copy; PWD is added by the job's shell.
func TestJobEnvIsExactlyTheRemoteEnv(t *testing.T) {
	t.Setenv("BITC_AGENT_ONLY", "leak")
	f := agenttest.NewFixture(t, "#!/bin/sh\ncat /proc/self/environ | tr '\\0' '\\n' | cut -d= -f1 | grep -v '^PWD$' | sort\n")
	if _, code := f.Run(t); code != 0 {
		t.Fatal(code)
	}
	b, _ := os.ReadFile(filepath.Join(f.Root, "j1", "out.log"))
	if string(b) != "A\nHOME\nPATH\n" {
		t.Fatalf("job env names %q, want only A, HOME, PATH", b)
	}
}

func TestCommandThatCannotStartEnds127WithTheReason(t *testing.T) {
	f := agenttest.NewFixture(t, "#!/bin/sh\n")
	f.Spec.Argv = []string{"/nonexistent/bitc-no-such-cmd", "secret-looking-arg"}
	f.Spec.Env = []string{"A=1", "SECRET_ENV=hunter2"}
	log, code := f.Run(t)
	want := "bitc agent: cannot start /nonexistent/bitc-no-such-cmd: "
	errB, _ := os.ReadFile(filepath.Join(f.Root, "j1", "err.log"))
	if code != 127 || !strings.HasPrefix(string(errB), want) || !strings.Contains(log, want) {
		t.Fatalf("code %d, err.log %q, agent log:\n%s", code, errB, log)
	}
	if strings.Contains(string(errB)+log, "hunter2") || strings.Contains(string(errB), "secret-looking-arg") {
		t.Fatalf("the message leaked env or args: %q", errB)
	}
}
