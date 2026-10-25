package ssh

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/Exploded-views/bitc/internal/agent"
	"github.com/Exploded-views/bitc/internal/provider"
)

// jobState is what Watch, Output and Fetch need of a launched job.
type jobState struct {
	spec  *provider.JobSpec
	sha   string
	slots []int
	prov  []string // the provisioning output, printed as `pod:` lines by Watch
}

func (p *Provider) job(id string) *jobState {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.jobs[id]
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

var (
	setupLineRE = regexp.MustCompile(`^setup step (\S+) failed: (.*)$`)
	pkgMissing  = regexp.MustCompile(`^setup step packages failed: package (\S+) not installed on this host$`)
	busyRE      = regexp.MustCompile(`^refused: slot ([0-9]+) busy on host$`)
)

// ensureHost is C.5 step 2: connect, `probe` (the arch must be the configured one), the agent
// installed by its sha when the host lacks it, and the host checks of C.8. Nothing of this job
// exists on the host yet, so every failure here is a pre-resource one.
func (p *Provider) ensureHost(ctx context.Context, sha, path string) error {
	res, out, err := p.output(ctx, "probe", nil)
	if err != nil {
		return err
	}
	if res.Code != 0 {
		return p.failure("probe", res, out)
	}
	var pr struct {
		Arch  string `json:"arch"`
		Slots int    `json:"slots"`
	}
	if err := jsonUnmarshal(out, &pr); err != nil {
		return &provider.ErrTransient{Why: fmt.Sprintf("ssh %s: probe failed: unreadable answer", p.set.Host)}
	}
	if pr.Arch != p.set.Arch {
		return &provider.ErrUnavailable{Why: fmt.Sprintf("host arch %s, configured %s", pr.Arch, p.set.Arch)}
	}
	if pr.Slots < p.set.Slots {
		return &provider.ErrUnavailable{Why: fmt.Sprintf("host has %d slots, configured %d", pr.Slots, p.set.Slots)}
	}
	for try := 0; ; try++ {
		res, out, err = p.output(ctx, "agent "+sha+" check-host", nil)
		if err != nil {
			return err
		}
		switch {
		case res.Code == 0:
			return nil
		case res.Code == 66 && try == 0:
			if err := p.install(ctx, sha, path); err != nil {
				return err
			}
		case res.Code == 66:
			return &provider.ErrTransient{Why: fmt.Sprintf("ssh %s: the agent is still not installed", p.set.Host)}
		case res.Code == 69: // a host check failed: the first failing line is the reason
			return &provider.ErrUnavailable{Why: "host check failed: " + firstFail(out)}
		default:
			return p.failure("check-host", res, out)
		}
	}
}

func firstFail(out string) string {
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "fail ") {
			return oneLine(l)
		}
	}
	return firstNonEmpty(lastLine(out), "no reason given")
}

// install sends the agent's bytes over the session: the gate keeps them only if they hash to sha.
func (p *Provider) install(ctx context.Context, sha, path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return &provider.ErrIneligible{Why: "agent file: " + oneLine(err.Error())}
	}
	if sum := sha256.Sum256(b); hex.EncodeToString(sum[:]) != sha {
		return &provider.ErrIneligible{Why: "the agent file changed while the job started"}
	}
	res, out, err := p.output(ctx, "install "+sha, bytes.NewReader(b))
	if err != nil {
		return err
	}
	if res.Code != 0 {
		return p.failure("install", res, out)
	}
	return nil
}

// Launch leases slots, checks the host, prepares the slots and attaches (providers delta C.5):
//
//	slot locks; probe; install; check-host; `slot-boot --prepare` (the resource point); then
//	`slot-boot --run`, the one call that carries job input (the attach point).
//
// Everything before --prepare leaves nothing on the host (provider.Unmade). A --prepare that
// never got past session setup is the same; any other --prepare failure is an ErrPreAttach naming
// the slot group. A --run that never got past session setup is an ErrPreAttach too; every other
// --run outcome without the `started` ack is attached, and nothing falls back after it.
func (p *Provider) Launch(ctx context.Context, j *provider.JobSpec) (provider.Handle, error) {
	if AgentSpec == nil {
		return provider.Handle{}, errors.New("ssh: no agent spec builder")
	}
	if err := p.credentials(); err != nil {
		return provider.Handle{}, provider.Unmade(err)
	}
	size, ok := p.set.Sizes[j.SizeKey]
	if !ok {
		return provider.Handle{}, provider.Unmade(&provider.ErrIneligible{Why: fmt.Sprintf("size %q is not in [providers.ssh] sizes", j.SizeKey)})
	}
	// all job input is gathered before the first byte is sent, so an input problem creates nothing
	sha, path, err := p.agentFile()
	if err != nil {
		return provider.Handle{}, provider.Unmade(err)
	}
	key, code, err := "", (*agent.CodeUpload)(nil), error(nil)
	var bun *bundle
	var bunFile *os.File
	if p.upload() {
		// the commit goes up on this session: no deploy key, no ref on a forge
		acct, err := p.accountDir()
		if err != nil {
			return provider.Handle{}, provider.Unmade(err)
		}
		if bun, err = makeBundle(ctx, p.eff.Worktree, filepath.Join(acct, "remote", "ssh", "upload"), j.Job, j.Sha, p.core.Transport.MaxBundle); err != nil {
			return provider.Handle{}, provider.Unmade(err)
		}
		defer bun.Close()
		// open it now, before any slot lock or --prepare: a failure here creates nothing
		if bunFile, err = os.Open(bun.path); err != nil {
			return provider.Handle{}, provider.Unmade(&provider.ErrIneligible{Why: "could not read the bundle: " + oneLine(err.Error())})
		}
		defer bunFile.Close()
		code = &agent.CodeUpload{Kind: "upload", Len: bun.size, SHA256: bun.sha256, Ref: bun.ref}
	} else if key, err = p.readDeployKey(j.DeployKeyFile); err != nil {
		return provider.Handle{}, provider.Unmade(err)
	}
	as := AgentSpec(j)
	as.Port, as.Bearer = -1, "" // no listener, no results server and no bearer: the session is the authorization
	enc, err := as.Encode()
	if err != nil {
		return provider.Handle{}, provider.Unmade(err)
	}
	frame, err := agent.BootPayload{Spec: enc, Deadline: j.Deadline.Unix(), DeployKey: key, Provider: "ssh", ResourceID: j.Name, Code: code}.Frame()
	if err != nil {
		return provider.Handle{}, provider.Unmade(&provider.ErrIneligible{Why: "the boot payload is over 8 MiB"})
	}
	var provFrame []byte
	if len(j.Packages) > 0 || len(j.Tools) > 0 {
		var tools []agent.Tool
		for _, t := range j.Tools {
			tools = append(tools, agent.Tool{Name: t.Name, URL: t.URL, SHA256: t.SHA256})
		}
		if provFrame, err = (agent.ProvisionSpec{Packages: j.Packages, Tools: tools}).Frame(); err != nil {
			return provider.Handle{}, provider.Unmade(&provider.ErrIneligible{Why: "the provisioning payload is over 8 MiB"})
		}
	}

	// C.3.1: the controller-side slot locks, the lowest free ones, all or none
	locks, err := p.locks()
	if err != nil {
		return provider.Handle{}, provider.Unmade(err)
	}
	lease, busy, err := locks.Acquire(size.Slots)
	if err != nil {
		return provider.Handle{}, provider.Unmade(&provider.ErrTransient{Why: "slot locks: " + oneLine(err.Error())})
	}
	if lease == nil {
		return provider.Handle{}, provider.Unmade(&provider.ErrTransient{Why: fmt.Sprintf("%d of %d slots busy", busy, p.set.Slots)})
	}
	id := ResourceID(p.set.Host, lease.Slots, j.Name)
	p.mu.Lock()
	p.leases[j.Name] = lease
	p.mu.Unlock()
	// nothing exists on the host (or the signal ended the launch): the locks go at once
	nothing := func(err error) (provider.Handle, error) {
		p.release(id, j.Name)
		if ctx.Err() != nil {
			return provider.Handle{}, ctx.Err()
		}
		return provider.Handle{}, provider.Unmade(err)
	}

	if err := p.ensureHost(ctx, sha, path); err != nil {
		return nothing(err)
	}

	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return nothing(err)
	}
	attempt := hex.EncodeToString(nonce) // an attempt id, not a secret
	slots := SlotsArg(lease.Slots)
	leaseSecs := min(max(int(p.core.StartTimeout/time.Second), 1), 3600)
	verb := fmt.Sprintf("agent %s slot-boot --prepare %s %s %s tag=%s lease=%d", sha, slots, j.Name, attempt, j.Owner, leaseSecs)
	var stdin io.Reader
	if provFrame != nil {
		verb += " payload"
		stdin = bytes.NewReader(provFrame)
	}
	res, out, err := p.output(ctx, verb, stdin)
	if err != nil { // the signal ended it: the core reaps the name
		p.release(id, j.Name)
		return provider.Handle{Created: time.Now()}, err
	}
	if perr := p.prepareError(id, j.Name, res, out); perr != nil {
		var pa *provider.ErrPreAttach
		if errors.As(perr, &pa) {
			return provider.Handle{}, perr // the core tears the slots down by id, which releases the locks
		}
		return nothing(perr)
	}
	lines := splitLines(out)

	// C.5 step 5: the attach. From the moment the exec request is written the job is attached.
	var runIn io.Reader = bytes.NewReader(frame)
	if bun != nil { // exactly len bytes follow the frame, streamed from the file and never held in memory
		runIn = io.MultiReader(runIn, io.LimitReader(bunFile, bun.size))
	}
	res, out, err = p.output(ctx, fmt.Sprintf("agent %s slot-boot --run %s %s %s", sha, slots, j.Name, attempt), runIn)
	if err != nil {
		p.release(id, j.Name)
		return provider.Handle{Created: time.Now()}, err
	}
	if aerr := p.attachError(id, j.Name, res, out); aerr != nil {
		return provider.Handle{}, aerr
	}
	p.mu.Lock()
	p.jobs[id] = &jobState{spec: j, sha: sha, slots: lease.Slots, prov: lines}
	p.mu.Unlock()
	return provider.Handle{ID: id, Name: j.Name}, nil
}

// prepareError classifies the result of `slot-boot --prepare` (providers delta C.5.1 item 2).
func (p *Provider) prepareError(id, name string, res Result, stdout string) error {
	host := p.set.Host
	if c, why := ClassifySession(res.Code, res.Stderr, p.set.User, host); c != ClassOther {
		return p.sessionError(c, why) // it never reached the gate: nothing ran on the host
	}
	last := lastLine(stdout)
	if m := busyRE.FindStringSubmatch(last); res.Code == 65 && m != nil {
		return &provider.ErrTransient{Why: "slot " + m[1] + " busy on host"}
	}
	if res.Code == 0 && last == "prepared "+name {
		return nil
	}
	// the owner file and a wiped slot may exist: the slot group is a resource now
	switch {
	case res.Code == agent.ProvisionExitRepo:
		step, line := "provision", last
		if m := setupLineRE.FindStringSubmatch(last); m != nil {
			step, line = m[1], m[2]
		}
		return &provider.ErrPreAttach{ID: id, Err: &provider.ErrSetup{Step: step, Class: "repo", Line: line}}
	case pkgMissing.MatchString(last):
		return &provider.ErrPreAttach{ID: id, Err: &provider.ErrIneligible{Why: fmt.Sprintf("package %s not installed on %s", pkgMissing.FindStringSubmatch(last)[1], host)}}
	case setupLineRE.MatchString(last):
		return &provider.ErrPreAttach{ID: id, Err: &provider.ErrTransient{Why: last}}
	}
	why := firstNonEmpty(lastLine(res.Stderr, stdout), fmt.Sprintf("exit %d", res.Code))
	return &provider.ErrPreAttach{ID: id, Err: &provider.ErrTransient{Why: fmt.Sprintf("ssh %s: slot prepare failed: %s", host, why)}}
}

// attachError classifies the result of `slot-boot --run` (providers delta C.5.1 item 3): only exit
// 255 with exactly one pre-session line proves the request never existed; everything else that
// is not exit 0 with the `started` ack is attached.
func (p *Provider) attachError(id, name string, res Result, stdout string) error {
	if c, why := ClassifySession(res.Code, res.Stderr, p.set.User, p.set.Host); c != ClassOther {
		inner := p.sessionError(c, why)
		if c == ClassConnect { // transient: the slot group is torn down, then one retry may follow
			inner = &provider.ErrTransient{Why: inner.Error()}
		}
		return &provider.ErrPreAttach{ID: id, Err: inner}
	}
	if res.Code == 0 && hasLine(stdout, "started "+name) {
		return nil
	}
	var why string
	switch l := lastLine(stdout); {
	case res.Code == 0:
		why = "no start acknowledgement"
	case strings.HasPrefix(l, "refused:"):
		why = l
	case res.Code == 255 || res.Code < 0:
		why = firstNonEmpty(lastLine(res.Stderr), "the connection ended")
	default:
		why = firstNonEmpty(l, lastLine(res.Stderr), fmt.Sprintf("exit %d", res.Code))
	}
	return &provider.ErrAttached{ID: id, Err: &provider.ErrTransient{Why: why + " after attach"}}
}

func hasLine(out, want string) bool {
	for _, l := range strings.Split(out, "\n") {
		if strings.TrimRight(l, "\r") == want {
			return true
		}
	}
	return false
}

// maxProvLines keeps at most this much provisioning output for the pod: lines.
const maxProvLines = 200

func splitLines(s string) []string {
	var out []string
	for _, l := range strings.Split(strings.ReplaceAll(strings.TrimRight(s, "\n"), "\r", ""), "\n") {
		if l = strings.TrimRight(l, " \t"); l != "" {
			out = append(out, oneLine(l))
		}
	}
	if len(out) > maxProvLines {
		out = out[len(out)-maxProvLines:]
	}
	return out
}
