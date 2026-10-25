package agent_test

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Exploded-views/bitc/internal/agent"
	"github.com/Exploded-views/bitc/internal/agent/agenttest"
)

// runFakePod drives the real binary (stage 1, re-exec, stage 2) as a fake pod: unprivileged,
// TestRoot set, port 0. env is the child's whole environment (production passes no PATH to
// stage 1's env beyond the pod's own; tests choose). It returns the job's out.log and the log.
func runFakePod(t *testing.T, f *agenttest.Fixture, env []string) (string, string) {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "bitc")
	if out, err := exec.Command("go", "build", "-o", bin, "../../cmd/bitc").CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	root := t.TempDir()
	f.Spec.TestRoot, f.Spec.Port = root, 0
	enc, err := f.Spec.Encode()
	if err != nil {
		t.Fatal(err)
	}
	child := exec.Command(bin, "__agent")
	child.Env = append([]string{"BITC_SPEC=" + enc, "BITC_PROVIDER_TYPE=none",
		"BITC_DEADLINE=" + strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10)}, env...)
	stdout, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = child.Process.Kill(); _ = child.Wait() }()
	var log strings.Builder
	done := make(chan struct{})
	go func() {
		defer close(done)
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			log.WriteString(sc.Text() + "\n")
			if strings.HasPrefix(sc.Text(), "__BITC_EXIT__") || strings.HasPrefix(sc.Text(), "__BITC_SETUP_FAILED__") {
				return
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("the fake pod never reached EXIT")
	}
	out, _ := os.ReadFile(filepath.Join(root, "run", f.Spec.Job, "out.log"))
	return string(out), log.String()
}

func TestBinaryJobInheritsOnlyStdFds(t *testing.T) {
	f := agenttest.NewFixture(t, "#!/bin/sh\n")
	// ls lists the shell's fds (its own dir fd is the shell's child's, not listed)
	f.Spec.Argv = []string{"sh", "-c", `ls -l /proc/$$/fd`}
	out, log := runFakePod(t, f, []string{"PATH=/usr/bin:/bin"})
	var fds []string
	for _, l := range strings.Split(out, "\n") {
		if pre, _, ok := strings.Cut(l, " -> "); ok {
			fds = append(fds, pre[strings.LastIndex(pre, " ")+1:]+" -> "+l[strings.Index(l, " -> ")+4:])
		}
	}
	for _, l := range fds {
		if n, _ := strconv.Atoi(strings.Fields(l)[0]); n > 2 {
			t.Fatalf("the job inherited fds %q\n%s", fds, log)
		}
	}
	if len(fds) < 3 {
		t.Fatalf("no fd listing: %q\n%s", out, log)
	}
}

func TestBinaryRunsWithAnEmptyEnvironment(t *testing.T) {
	f := agenttest.NewFixture(t, "#!/bin/sh\necho ran-ok\n")
	f.Spec.Setup = []agent.Step{{Name: "s", RunAs: "job", Argv: []string{"sh", "-c", "echo step-ok >&2"}}}
	out, log := runFakePod(t, f, nil) // no PATH at all, as in a real pod's stage 2
	if !strings.Contains(out, "ran-ok") || !strings.Contains(log, "__BITC_EXIT__ n0nce 0") {
		t.Fatalf("out %q\n%s", out, log)
	}
}
