package ssh

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Exploded-views/bitc/internal/provider"
	"github.com/Exploded-views/bitc/internal/state"
)

// Result is one finished call of the host's gate.
type Result struct {
	Code   int
	Stderr string // the last 64 KiB: everything the ssh client itself printed
}

const stderrKeep = 64 << 10

// tailBuf keeps the last stderrKeep bytes written to it.
type tailBuf struct{ b []byte }

func (t *tailBuf) Write(p []byte) (int, error) {
	t.b = append(t.b, p...)
	if len(t.b) > stderrKeep {
		t.b = t.b[len(t.b)-stderrKeep:]
	}
	return len(p), nil
}

// command builds the process that sends verb to the host: the system ssh client with the pinned
// argv of providers delta C.2.2, or, under `[test] ssh_exec = "local"`, the fake gate (C.11).
func (p *Provider) command(ctx context.Context, verb string) (*exec.Cmd, error) {
	if t := p.eff.Test; t != nil && t.SSHExec == "local" {
		if t.SSHGate == "" || t.SSHRoot == "" {
			return nil, &provider.ErrUnavailable{Why: "[test] ssh_exec = \"local\" needs ssh_gate and ssh_root", Local: true}
		}
		args := []string{"--root", t.SSHRoot, "--", verb}
		return exec.CommandContext(ctx, t.SSHGate, args...), nil
	}
	bin, err := exec.LookPath("ssh")
	if err != nil {
		return nil, &provider.ErrUnavailable{Why: "the ssh client is not on PATH", Local: true}
	}
	acct, err := state.AccountDir(p.eff)
	if err != nil {
		return nil, &provider.ErrUnavailable{Why: err.Error(), Local: true}
	}
	kh := filepath.Join(acct, "ssh", "known_hosts")
	// exactly one line, written at every spawn: the pinned key is never learned, only given
	if err := state.WriteFileAtomic(kh, []byte(KnownHostsLine(p.set.HostKey))); err != nil {
		return nil, &provider.ErrUnavailable{Why: err.Error(), Local: true}
	}
	return exec.CommandContext(ctx, bin, p.set.Argv(kh, p.set.IdentityFile, verb)...), nil
}

// run sends one verb and waits. stdin may be nil; stdout may be nil (discarded). The error is
// non-nil only when the process could not be run or ctx ended; a nonzero exit is in Result.
func (p *Provider) run(ctx context.Context, verb string, stdin io.Reader, stdout io.Writer) (Result, error) {
	cmd, err := p.command(ctx, verb)
	if err != nil {
		return Result{}, err
	}
	var eb tailBuf
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, &eb
	cmd.Env = cleanEnv()
	cmd.WaitDelay = 5 * time.Second
	err = cmd.Run()
	res := Result{Stderr: string(eb.b)}
	var ee *exec.ExitError
	switch {
	case ctx.Err() != nil:
		res.Code = -1
		return res, ctx.Err()
	case errors.As(err, &ee):
		res.Code = ee.ExitCode()
	case err != nil:
		return res, &provider.ErrUnavailable{Why: "cannot run ssh: " + oneLine(err.Error()), Local: true}
	}
	return res, nil
}

// output runs verb and returns its stdout as well.
func (p *Provider) output(ctx context.Context, verb string, stdin io.Reader) (Result, string, error) {
	var out bytes.Buffer
	res, err := p.run(ctx, verb, stdin, &out)
	return res, out.String(), err
}

// cleanEnv is the client's environment: no agent socket, no askpass, no proxy settings.
func cleanEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		switch k {
		case "SSH_AUTH_SOCK", "SSH_ASKPASS", "SSH_ASKPASS_REQUIRE", "DISPLAY", "SSH_CONNECTION":
			continue
		}
		env = append(env, kv)
	}
	return env
}

// oneLine is the first line of s, printable, at most 200 bytes.
func oneLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	s = strings.Map(func(r rune) rune {
		if r < ' ' || r == 0x7f {
			return -1
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	if len(s) > 200 {
		s = strings.ToValidUTF8(s[:200], "")
	}
	return s
}

// lastLine is the last non-empty line of the texts, tried in order.
func lastLine(texts ...string) string {
	for _, t := range texts {
		lines := strings.Split(strings.TrimSpace(t), "\n")
		for i := len(lines) - 1; i >= 0; i-- {
			if l := oneLine(lines[i]); l != "" {
				return l
			}
		}
	}
	return ""
}
