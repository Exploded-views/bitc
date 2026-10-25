// Package agenttest is a fixture that drives agent.Run in-process as a plain user against a
// local git repository, for the agent's own tests and the conformance suite.
package agenttest

import (
	"bytes"
	"context"
	"encoding/hex"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Exploded-views/bitc/internal/agent"
	"github.com/Exploded-views/bitc/internal/exit"
)

// Fixture is one fake job: a repo with run.sh and sub/keep, and the spec that clones it.
type Fixture struct {
	Repo, Root string
	Spec       agent.Spec
	K          []byte
	HTTP       *http.Client
	Deadline   time.Time
	SelfDelete func()
	// NoListener runs the job in no-listener mode (spec port -1): there is no results server and
	// the agent exits by itself after result.json, so Run does not set NoServe.
	NoListener bool
	// System replaces PlainSystem; StateRoot, SelfSHA, KeyRoot and KeepAlive are Options' fields.
	System    agent.System
	StateRoot string
	SelfSHA   func() (string, error)
	KeyRoot   string
	KeepAlive *agent.KeepAliveConf
}

// NewFixture commits script as run.sh in a fresh repo and prepares a spec for it.
func NewFixture(t *testing.T, script string) *Fixture {
	t.Helper()
	repo := t.TempDir()
	_ = os.MkdirAll(filepath.Join(repo, "sub"), 0o750)
	_ = os.WriteFile(filepath.Join(repo, "run.sh"), []byte(script), 0o600) // run as `sh run.sh`
	_ = os.WriteFile(filepath.Join(repo, "sub", "keep"), nil, 0o600)
	for _, a := range [][]string{{"init", "-q"}, {"add", "-A"}, {"-c", "user.name=t", "-c", "user.email=t@x", "commit", "-q", "-m", "i"}, {"update-ref", "refs/bitc/jobs/x", "HEAD"}} {
		c := exec.Command("git", a...)
		c.Dir = repo
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("%v %s", err, out)
		}
	}
	out, _ := exec.Command("git", "-C", repo, "rev-parse", "HEAD").Output()
	k := bytes.Repeat([]byte{5}, 32)
	f := &Fixture{Repo: repo, Root: t.TempDir(), K: k, Deadline: time.Now().Add(time.Hour)}
	f.Spec = agent.Spec{Job: "j1", Pool: "heavy", Argv: []string{"sh", "../run.sh"}, Cwd: "sub", CloneURL: repo,
		Ref: "refs/bitc/jobs/x", Sha: strings.TrimSpace(string(out)), Timeout: agent.Duration(time.Minute), Port: 0,
		Bearer: hex.EncodeToString(bytes.Repeat([]byte{4}, 32)), MacKey: hex.EncodeToString(k),
		Nonce: "n0nce", Unprivileged: true, Env: []string{"A=1"}}
	return f
}

// Run drives agent.Run once and returns the agent's stdout and exit code.
func (f *Fixture) Run(t *testing.T) (string, int) {
	t.Helper()
	var out bytes.Buffer
	if f.NoListener {
		f.Spec.Port = -1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var sys agent.System = agent.PlainSystem{}
	if f.System != nil {
		sys = f.System
	}
	o := agent.Options{Stdout: &out, ResultsRoot: f.Root, ToolsRoot: filepath.Join(f.Root, "tools"),
		WorkRoot: filepath.Join(f.Root, "work"), System: sys, NoServe: !f.NoListener,
		HTTP: f.HTTP, Deadline: f.Deadline, StateRoot: f.StateRoot, SelfSHA: f.SelfSHA, KeyRoot: f.KeyRoot, KeepAlive: f.KeepAlive}
	if f.SelfDelete != nil {
		o.SelfDelete = func(context.Context) error { f.SelfDelete(); return nil }
	}
	done := make(chan int, 1)
	exit.Go(func() { done <- agent.Run(ctx, o, f.Spec, agent.Secrets{}) })
	select {
	case code := <-done:
		return out.String(), code
	case <-time.After(45 * time.Second):
		t.Fatalf("the agent did not exit after its job:\n%s", out.String())
		return "", 0
	}
}
