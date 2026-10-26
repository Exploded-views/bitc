package plugin

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/Exploded-views/bitc/internal/exit"
	"github.com/Exploded-views/bitc/internal/proc"
	"github.com/Exploded-views/bitc/internal/provider"
	"github.com/Exploded-views/bitc/internal/provider/plugin/pin"
	"github.com/Exploded-views/bitc/internal/state"
)

// realHome is the account's home from the passwd database ("the account's real home", A.9),
// else $HOME.
func realHome() string {
	if u, err := user.Current(); err == nil && u.HomeDir != "" {
		return u.HomeDir
	}
	if h := os.Getenv("HOME"); h != "" {
		return h
	}
	return "/"
}

// dirs are the three plugin directories (A.3.2).
type dirs struct{ state, tmpBase, tmp, shared string }

// prepareDirs creates BITC_PLUGIN_STATE, the per-process TMPDIR (pruning the dirs of dead bitc
// processes) and BITC_PLUGIN_SHARED.
func (p *Provider) prepareDirs() (dirs, error) {
	own := filepath.Join(p.eff.Host.StateDir, "plugins", p.typ)
	d := dirs{state: own, tmpBase: filepath.Join(own, "tmp")}
	acct, err := state.AccountDir(p.eff)
	if err != nil {
		return d, &provider.ErrUnavailable{Why: err.Error(), Local: true}
	}
	d.shared = filepath.Join(acct, "remote", p.typ, "plugin")
	self, err := proc.ReadStat(os.Getpid())
	if err != nil {
		return d, &provider.ErrUnavailable{Why: "cannot read /proc/self/stat: " + err.Error(), Local: true}
	}
	d.tmp = filepath.Join(d.tmpBase, fmt.Sprintf("%d-%d", os.Getpid(), self.Start))
	for _, dir := range []string{d.state, d.tmpBase, d.shared} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return d, &provider.ErrUnavailable{Why: "plugin directory: " + err.Error(), Local: true}
		}
	}
	pruneTmp(d.tmpBase, filepath.Base(d.tmp))
	_ = os.RemoveAll(d.tmp)
	if err := os.Mkdir(d.tmp, 0o700); err != nil {
		return d, &provider.ErrUnavailable{Why: "plugin TMPDIR: " + err.Error(), Local: true}
	}
	return d, nil
}

// pruneTmp removes sibling dirs <pid>-<start> whose process is gone or has another start time,
// and anything else that is not such a name, so several bitc processes never wipe each other.
func pruneTmp(base, keep string) {
	ents, _ := os.ReadDir(base)
	for _, e := range ents {
		if e.Name() == keep {
			continue
		}
		pidS, startS, ok := strings.Cut(e.Name(), "-")
		pid, e1 := strconv.Atoi(pidS)
		start, e2 := strconv.ParseUint(startS, 10, 64)
		if ok && e1 == nil && e2 == nil {
			if st, err := proc.ReadStat(pid); err == nil && st.Start == start {
				continue
			}
		}
		_ = os.RemoveAll(filepath.Join(base, e.Name()))
	}
}

// secretsJSON is the fd 3 payload: the api key and each secret file's bytes (A.3.3). The values
// join the redactor before they leave the process.
func (p *Provider) secretsJSON() ([]byte, error) {
	m := map[string]string{"api_key": p.core.APIKey}
	for name, path := range p.core.Plugin.SecretFiles {
		var st unix.Stat_t
		if err := unix.Stat(path, &st); err != nil {
			return nil, &provider.ErrUnavailable{Why: fmt.Sprintf("plugin secret_files.%s: %v", name, err), Local: true}
		}
		if st.Uid != uint32(os.Getuid()) || st.Mode&0o077 != 0 {
			return nil, &provider.ErrUnavailable{Why: fmt.Sprintf("plugin secret_files.%s: %s must be owned by you and mode 600", name, path), Local: true}
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, &provider.ErrUnavailable{Why: fmt.Sprintf("plugin secret_files.%s: %v", name, err), Local: true}
		}
		m[name] = string(b)
	}
	for _, v := range m {
		p.redactValue(v)
	}
	return json.Marshal(m)
}

// redactValue adds a secret in the forms a plugin could echo it: raw, trimmed, and as JSON
// string content.
func (p *Provider) redactValue(v string) {
	if p.rt.Red == nil || v == "" {
		return
	}
	forms := []string{v, strings.TrimSpace(v)}
	if b, err := json.Marshal(v); err == nil && len(b) > 2 {
		forms = append(forms, string(b[1:len(b)-1]))
	}
	for _, f := range forms {
		p.rt.Red.Add(f)
	}
}

// spawn starts one plugin process through the __plugin-exec shim (A.2.4, A.3.1).
func (p *Provider) spawn() (*session, error) {
	// The TMPDIR path is the same for every spawn in this process: a later spawn takes ownership
	// of it, so an earlier session's waiter (which can return late) must not remove it.
	p.tmpMu.Lock()
	p.tmpGen++
	gen := p.tmpGen
	p.tmpMu.Unlock()
	d, err := p.prepareDirs()
	if err != nil {
		return nil, err
	}
	secrets, err := p.secretsJSON()
	if err != nil {
		return nil, err
	}
	f, err := pin.Open(p.core.Plugin.Path, p.core.Plugin.SHA256)
	if err != nil {
		var ch *pin.Changed
		var pe *pin.Error
		switch {
		case errors.As(err, &ch):
			return nil, &provider.ErrUnavailable{Why: ch.Error(), Local: true}
		case errors.As(err, &pe):
			return nil, &provider.ErrUnavailable{Why: fmt.Sprintf("plugin %s: %s", p.core.Plugin.Path, pe.Error()), Local: true}
		}
		return nil, &provider.ErrUnavailable{Why: err.Error(), Local: true}
	}
	defer f.Close()
	inR, inW, err := os.Pipe()
	if err != nil {
		return nil, &provider.ErrUnavailable{Why: err.Error(), Local: true}
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		inR.Close()
		inW.Close()
		return nil, &provider.ErrUnavailable{Why: err.Error(), Local: true}
	}
	secR, secW, err := os.Pipe()
	if err != nil {
		inR.Close()
		inW.Close()
		outR.Close()
		outW.Close()
		return nil, &provider.ErrUnavailable{Why: err.Error(), Local: true}
	}
	s := &session{typ: p.typ, scale: p.eff.Scale, calls: map[uint64]*call{}, abandon: map[uint64]bool{}, dead: make(chan struct{}),
		exited: make(chan struct{}), stdin: inW, stderr: &ring{}}
	s.stderr.onLine = p.stderrLine
	cmd := exec.Command("/proc/self/exe", "__plugin-exec", p.core.Plugin.Path)
	cmd.Args = []string{"bitc", "__plugin-exec", p.core.Plugin.Path}
	cmd.Env = p.pluginEnv(d)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = inR, outW, s.stderr
	cmd.ExtraFiles = []*os.File{secR, f} // fd 3: secrets, fd 4: the pinned plugin (the shim execs it)
	cmd.SysProcAttr = pluginAttr()
	s.cmd = cmd
	startErr := make(chan error, 1)
	exit.Go(func() {
		// Pdeathsig belongs to the thread that forked the child: stay on it until Wait returns.
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		err := cmd.Start()
		startErr <- err
		if err != nil {
			return
		}
		werr := cmd.Wait()
		s.mu.Lock()
		s.waitErr = werr
		s.mu.Unlock()
		p.tmpMu.Lock()
		if p.tmpGen == gen {
			_ = os.RemoveAll(d.tmp)
		}
		p.tmpMu.Unlock()
		close(s.exited)
	})
	err = <-startErr
	inR.Close()
	outW.Close()
	secR.Close()
	if err != nil {
		inW.Close()
		outR.Close()
		secW.Close()
		_ = os.RemoveAll(d.tmp)
		return nil, &provider.ErrUnavailable{Why: "plugin " + p.core.Plugin.Path + ": " + err.Error(), Local: true}
	}
	exit.Go(func() { // the secrets, one JSON object then EOF
		_, _ = secW.Write(secrets)
		secW.Close()
	})
	exit.Go(func() { s.readLoop(io.Reader(outR)); outR.Close() })
	return s, nil
}

// pluginEnv is the environment built from empty (A.3.2).
func (p *Provider) pluginEnv(d dirs) []string {
	env := []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "HOME=" + realHome(), "LANG=C.UTF-8", "TMPDIR=" + d.tmp,
		"BITC_PLUGIN_STATE=" + d.state, "BITC_PLUGIN_SHARED=" + d.shared, "BITC_PLUGIN_PROTOCOL=1", "BITC_SECRETS_FD=3"}
	for _, n := range p.core.Plugin.EnvPass {
		if v, ok := os.LookupEnv(n); ok {
			env = append(env, n+"="+v)
		}
	}
	return env
}
