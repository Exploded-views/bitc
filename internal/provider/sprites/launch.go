package sprites

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/Exploded-views/bitc/internal/agent"
	"github.com/Exploded-views/bitc/internal/exit"
	"github.com/Exploded-views/bitc/internal/provider"
)

// jobState is what Watch, Output and Fetch need of a launched job: the spec (nonce, K, deadline),
// the agent's sha, the provisioning output and the attach exec, still open.
type jobState struct {
	spec *provider.JobSpec
	sha  string
	prov []string // the provisioning output, printed as `pod:` lines by Watch

	sess *Session
	done chan struct{} // closed when the attach exec's stdout is drained
	mu   sync.Mutex
	tail []byte // the last bytes of the attach exec's stdout (a refusal prints there)
	exit int
	werr error
}

func (p *Provider) job(id string) *jobState {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.jobs[id]
}

func (p *Provider) forget(id string) {
	p.mu.Lock()
	st := p.jobs[id]
	delete(p.jobs, id)
	p.mu.Unlock()
	if st != nil && st.sess != nil {
		_ = st.sess.Close()
	}
}

// drain reads the attach exec's stdout so the agent never blocks on a full pipe, keeping the end
// of it for diagnostics (the agent's own log is agent.log, read by Watch).
func (st *jobState) drain() {
	defer close(st.done)
	buf := make([]byte, 4096)
	for {
		n, err := st.sess.Stdout().Read(buf)
		if n > 0 {
			st.mu.Lock()
			st.tail = append(st.tail, buf[:n]...)
			if len(st.tail) > 4096 {
				st.tail = st.tail[len(st.tail)-4096:]
			}
			st.mu.Unlock()
		}
		if err != nil {
			break
		}
	}
	code, werr := st.sess.Wait()
	st.mu.Lock()
	st.exit, st.werr = code, werr
	st.mu.Unlock()
}

// agentEnded reports that the attach exec is over: with the agent's exit status, or (werr set)
// because the connection ended without one (a cut, a cold drop).
func (st *jobState) agentEnded() (code int, werr error, ended bool) {
	select {
	case <-st.done:
	default:
		return 0, nil, false
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.exit, st.werr, true
}

// attachOutput is what the attach exec printed last, one line, for an error message.
func (st *jobState) attachOutput() string {
	st.mu.Lock()
	defer st.mu.Unlock()
	lines := strings.Split(strings.TrimSpace(string(st.tail)), "\n")
	return oneLine(lines[len(lines)-1])
}

// readDeployKey reads the repo's deploy key from its 0600 file (checked again here: the file may
// have changed since the load) and joins it to the redactor.
func (p *Provider) readDeployKey(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	f, err := os.Open(path)
	if err != nil {
		return "", &provider.ErrIneligible{Why: "[[repos]] deploy_key_file cannot be read: " + oneLine(err.Error())}
	}
	defer f.Close()
	var st unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &st); err != nil || st.Mode&unix.S_IFMT != unix.S_IFREG || st.Uid != uint32(os.Getuid()) || st.Mode&0o077 != 0 {
		return "", &provider.ErrIneligible{Why: "[[repos]] deploy_key_file must be a regular file owned by you and mode 600"}
	}
	b, err := io.ReadAll(io.LimitReader(f, 16<<10+1))
	if err != nil || len(b) > 16<<10 || len(strings.TrimSpace(string(b))) == 0 {
		return "", &provider.ErrIneligible{Why: "[[repos]] deploy_key_file is empty or over 16 KiB"}
	}
	key := strings.TrimSpace(string(b))
	if p.rt.Red != nil {
		p.rt.Red.Add(key)
	}
	return key, nil
}

var setupLineRE = regexp.MustCompile(`^setup step (\S+) failed: (.*)$`)

// provisionError maps the exit and last line of `__agent provision` to a pre-attach error: the
// repo's own failure is an ErrSetup `repo` (69), anything else is infrastructure (transient).
func provisionError(name string, code int, out []string) error {
	last := ""
	if len(out) > 0 {
		last = out[len(out)-1]
	}
	switch {
	case code == agent.ProvisionExitRepo:
		step, line := "provision", last
		if m := setupLineRE.FindStringSubmatch(last); m != nil {
			step, line = m[1], m[2]
		}
		return PreAttach(name, &provider.ErrSetup{Step: step, Class: "repo", Line: line})
	case setupLineRE.MatchString(last):
		return PreAttach(name, &provider.ErrTransient{Why: last})
	}
	return PreAttach(name, &provider.ErrTransient{Why: fmt.Sprintf("setup step provision failed: agent exited %d", code)})
}

// keep at most this much provisioning output for the pod: lines
const maxProvLines = 200

func splitLines(b []byte) []string {
	var out []string
	for _, l := range strings.Split(strings.ReplaceAll(strings.TrimRight(string(b), "\n"), "\r", ""), "\n") {
		if l = strings.TrimRight(l, " \t"); l != "" {
			out = append(out, oneLine(l))
		}
	}
	if len(out) > maxProvLines {
		out = out[len(out)-maxProvLines:]
	}
	return out
}

// agentTools converts the job's tools for the provisioning payload.
func agentTools(j *provider.JobSpec) []agent.Tool {
	var out []agent.Tool
	for _, t := range j.Tools {
		out = append(out, agent.Tool{Name: t.Name, URL: t.URL, SHA256: t.SHA256})
	}
	return out
}

// Launch makes the job's Sprite, provisions it and attaches (sprites-provider.md 4, J2-J4):
//
//	create; build and resources policies, each read back; the agent installed by exec;
//	`__agent provision` (packages and tools); the job policy, read back; then the attach exec,
//	the first and only thing that carries job input.
//
// Everything before the attach is job-neutral, so every failure up to it is an ErrPreAttach
// naming the Sprite (the core deletes it and applies the fallback boundary); a failure to create
// that is not ambiguous is a plain error class. Once the attach was sent, nothing falls back.
func (p *Provider) Launch(ctx context.Context, j *provider.JobSpec) (provider.Handle, error) {
	if err := p.credentials(); err != nil {
		return provider.Handle{}, err
	}
	if AgentSpec == nil {
		return provider.Handle{}, errors.New("sprites: no agent spec builder")
	}
	size, ok := p.set.Sizes[j.SizeKey]
	if !ok {
		return provider.Handle{}, &provider.ErrIneligible{Why: fmt.Sprintf("size %q is not in [providers.sprites] sizes", j.SizeKey)}
	}
	// all job input is gathered before the first request, so an input problem creates nothing
	key, err := p.readDeployKey(j.DeployKeyFile)
	if err != nil {
		return provider.Handle{}, err
	}
	as := AgentSpec(j)
	as.Port, as.Bearer = -1, "" // no listener, no results server and no bearer: the exec channel is the authorization
	enc, err := as.Encode()
	if err != nil {
		return provider.Handle{}, err
	}
	scale := p.eff.Scale
	frame, err := agent.BootPayload{Spec: enc, Deadline: j.Deadline.Unix(), DeployKey: key, Provider: "sprites", ResourceID: j.Name,
		KeepAlive: &agent.KeepAlive{Expire: agent.Duration(scale(5 * time.Minute)), Renew: agent.Duration(scale(60 * time.Second))}}.Frame()
	if err != nil {
		return provider.Handle{}, &provider.ErrIneligible{Why: "the boot payload is over 8 MiB"}
	}
	provFrame, err := agent.ProvisionSpec{Packages: j.Packages, Tools: agentTools(j)}.Frame()
	if err != nil {
		return provider.Handle{}, &provider.ErrIneligible{Why: "the provisioning payload is over 8 MiB"}
	}
	build, err := AllowDomains(p.buildDomains(j)...)
	if err != nil {
		return provider.Handle{}, &provider.ErrIneligible{Why: err.Error()}
	}
	job, err := AllowDomains(p.set.EgressAllow...)
	if err != nil {
		return provider.Handle{}, &provider.ErrIneligible{Why: err.Error()}
	}

	// J2: create. A plain error here means nothing was created.
	if _, err := p.cli.CreateSprite(ctx, j.Name, map[string]string{"bitc": "1"}); err != nil {
		return provider.Handle{}, err
	}
	createdAt := time.Now() // the Sprite bills from here, through provisioning
	pre := func(err error) (provider.Handle, error) {
		if ctx.Err() != nil { // a signal: the core reaps the Sprite by name; Created prices its minutes
			return provider.Handle{Created: createdAt}, ctx.Err()
		}
		return provider.Handle{}, PreAttach(j.Name, err)
	}
	if err := p.cli.ApplyBuildPolicies(ctx, j.Name, build, Resources{LimitMB: int(size.Memory >> 20)}); err != nil {
		return pre(err)
	}
	sudo := p.cli.Sudo()
	install := []string{"sh", "-c", agent.StubInstall(j.AgentURL, j.AgentSHA256)}
	if sudo {
		install = append([]string{"sudo", "-n"}, install...)
	}
	res, err := p.cli.Run(ctx, j.Name, ExecRequest{Cmd: install}, nil)
	if err != nil {
		return pre(err)
	}
	if res.Exit != 0 {
		return pre(&provider.ErrTransient{Why: fmt.Sprintf("agent install failed (exit %d): %s", res.Exit, lastLine(res.Stderr))})
	}
	pcmd, err := AgentCmd(j.AgentSHA256, sudo, "provision")
	if err != nil {
		return pre(&provider.ErrIneligible{Why: err.Error()})
	}
	res, err = p.cli.Run(ctx, j.Name, ExecRequest{Cmd: pcmd, Stdin: true}, provFrame)
	if err != nil {
		return pre(err)
	}
	lines := splitLines(res.Stdout)
	if res.Exit != 0 {
		if ctx.Err() != nil {
			return pre(ctx.Err())
		}
		return provider.Handle{}, provisionError(j.Name, res.Exit, lines)
	}
	if err := p.cli.ApplyJobPolicy(ctx, j.Name, job); err != nil {
		return pre(err)
	}

	// J4: the attach. From the moment its request is written the job is attached.
	sess, err := p.cli.Attach(ctx, j.Name, AttachOptions{AgentSHA: j.AgentSHA256, Sudo: sudo, Deadline: j.Deadline, Payload: frame})
	if err != nil {
		var att *provider.ErrAttached
		var pa *provider.ErrPreAttach
		var in *provider.ErrIneligible
		switch {
		case errors.As(err, &att):
			return provider.Handle{}, err
		case errors.As(err, &pa) && errors.As(pa.Err, &in):
			// an HTTP error before 101 is a transient refusal (1.2): one retry on a fresh Sprite
			return provider.Handle{}, PreAttach(j.Name, &provider.ErrTransient{Why: in.Why})
		case errors.As(err, &pa):
			return provider.Handle{}, err
		}
		return pre(err) // not provisioned: a bug, nothing was sent
	}
	st := &jobState{spec: j, sha: j.AgentSHA256, prov: lines, sess: sess, done: make(chan struct{})}
	exit.Go(st.drain)
	p.mu.Lock()
	p.jobs[j.Name] = st
	p.mu.Unlock()
	return provider.Handle{ID: j.Name, Name: j.Name, Created: createdAt}, nil
}

func lastLine(b []byte) string {
	s := strings.TrimSpace(string(b))
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		s = s[i+1:]
	}
	return oneLine(s)
}
