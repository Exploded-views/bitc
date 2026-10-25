// Package fake is a test-only provider that runs the job as a local process.
package fake

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"

	"github.com/Exploded-views/bitc/internal/config"
	"github.com/Exploded-views/bitc/internal/provider"
)

// Types are the provider types the fake registers: "fake" and a second, identical one so a test
// can walk a pool through two providers with different host settings (one instance per type).
var Types = []string{"fake", "fake2"}

func init() {
	for _, typ := range Types {
		register(typ)
	}
}

func register(typ string) {
	provider.Register(typ, provider.Factory{
		TestOnly:  true,
		Endpoints: func(map[string]any) ([]string, error) { return nil, nil },
		// fallback_until_cap (test only) sets what the fake declares as Caps.FallbackUntil;
		// send_env_default and multi_job (test only) set Caps.SendEnv and Caps.MultiJob.
		FallbackCap: func(raw map[string]any) provider.Boundary { return capOf(raw) },
		SendEnv:     func(raw map[string]any) provider.EnvLevel { return sendEnvOf(raw) },
		MultiJob:    func(raw map[string]any) bool { return multiOf(raw) },
		New: func(core *config.Provider, eff *config.Effective) (provider.Provider, error) {
			mode, _ := core.Raw["mode"].(string)
			if mode == "" {
				mode = "run"
			}
			stateDir, _ := core.Raw["state_dir"].(string)
			f := &Fake{typ: typ, mode: mode, wt: eff.Worktree, stateDir: stateDir, cap: capOf(core.Raw),
				sendEnv: sendEnvOf(core.Raw), multi: multiOf(core.Raw)}
			if n, ok := core.Raw["list_page"].(int64); ok && n > 0 {
				f.listPage, f.track = int(n), true
			}
			if b, ok := core.Raw["track"].(bool); ok && b {
				f.track = true
			}
			f.price, _ = rawNum(core.Raw["usd_per_hour"])
			f.spend, f.hasSpend = rawNum(core.Raw["spend_usd"])
			if d, ok := core.Raw["estimate_delay"].(string); ok {
				f.estimateDelay, _ = time.ParseDuration(d)
			}
			f.gate, _ = core.Raw["gate"].(string)
			f.arch = []string{"amd64"}
			switch a := core.Raw["arch"].(type) {
			case string:
				f.arch = []string{a}
			case []any:
				f.arch = nil
				for _, x := range a {
					s, _ := x.(string)
					f.arch = append(f.arch, s)
				}
			}
			return f, nil
		},
	})
}

// Fake runs one job per process (a bitc run is one process).
type Fake struct {
	typ      string // "fake" or "fake2"
	sendEnv  provider.EnvLevel
	multi    bool
	mode     string
	wt       string
	stateDir string // "" records nothing
	job      atomic.Pointer[provider.JobSpec]

	price         float64       // usd_per_hour: Estimate is price x (E - now)
	spend         float64       // spend_usd: today's account spend
	hasSpend      bool          // spend_usd is set: Caps.Spend
	estimateDelay time.Duration // estimate_delay: Estimate sleeps, widening the cap check
	gate          string        // gate: Watch waits after BEGIN until this file exists
	arch          []string      // arch: the architectures Info reports (default amd64)
	cap           provider.Boundary
	track         bool // track (or list_page): every launch is listed in resources.json until torn down
	listPage      int  // list_page: List reads the resources in pages of this size
}

// sendEnvOf reads send_env_default (test only): the level the fake declares in Caps.SendEnv.
func sendEnvOf(raw map[string]any) provider.EnvLevel {
	s, _ := raw["send_env_default"].(string)
	l, _ := provider.ParseEnvLevel(s)
	return l
}

func multiOf(raw map[string]any) bool { b, _ := raw["multi_job"].(bool); return b }

func capOf(raw map[string]any) provider.Boundary {
	if s, _ := raw["fallback_until_cap"].(string); s == "attach" {
		return provider.UntilAttach
	}
	return provider.UntilResource
}

// preAttachModes create a resource and then fail in a stated way; "attached-fails" fails after
// the fake received job input.
var failModes = map[string]bool{"preattach-fails": true, "preattach-fails-once": true, "preattach-teardown-fails": true,
	"preattach-repo-setup": true, "attached-fails": true, "preattach-then-transient": true, "transient-then-preattach": true}

// rawNum reads a TOML number (an integer or a float).
func rawNum(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int64:
		return float64(n), true
	}
	return 0, false
}

// fakeRes is one entry of state_dir/resources.json, what List and Owner report.
type fakeRes struct {
	ID, Name, Owner  string
	Created, Expires time.Time
	USDPerHour       float64 `json:"usd_per_hour"`
}

func (f *Fake) loadRes() []fakeRes {
	var rs []fakeRes
	if f.stateDir == "" {
		return nil
	}
	if b, err := os.ReadFile(filepath.Join(f.stateDir, "resources.json")); err == nil {
		_ = json.Unmarshal(b, &rs)
	}
	return rs
}

// updateRes rewrites resources.json under a lock: several bitc processes may share a state_dir.
func (f *Fake) updateRes(fn func([]fakeRes) []fakeRes) {
	if f.stateDir == "" {
		return
	}
	fd, err := unix.Open(filepath.Join(f.stateDir, "resources.lock"), unix.O_RDWR|unix.O_CREAT|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return
	}
	defer unix.Close(fd)
	if unix.Flock(fd, unix.LOCK_EX) != nil {
		return
	}
	b, _ := json.Marshal(fn(f.loadRes()))
	_ = os.WriteFile(filepath.Join(f.stateDir, "resources.json"), b, 0o600)
}

// call records one API call of the fake's model (calls.jsonl, what a real API would have seen).
// create carries no job input; attach is the call that does and is marked (spec B.1, row FB15).
func (f *Fake) call(op string, attach bool, data string) {
	if f.stateDir == "" {
		return
	}
	b, _ := json.Marshal(map[string]any{"op": op, "attach": attach, "data": data})
	_ = appendLine(filepath.Join(f.stateDir, "calls.jsonl"), string(b))
}

// attachData is everything job-specific the fake receives.
func attachData(j *provider.JobSpec) string {
	return fmt.Sprintf("argv=%q env=%q ref=%s sha=%s bearer=%x mac=%x nonce=%x", j.Argv, j.Env, j.Ref, j.Sha, j.Bearer, j.MacKey, j.Nonce)
}

// appendLine appends s and a newline to p (O_APPEND|O_CREATE, 0600).
func appendLine(p, s string) error {
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(s + "\n")
	return err
}

func (f *Fake) Info() provider.Info {
	return provider.Info{Type: f.typ, Caps: provider.Caps{LiveLogs: true, Spend: f.hasSpend, FallbackUntil: f.cap,
		SendEnv: f.sendEnv, MultiJob: f.multi}, Arch: f.arch}
}

func (f *Fake) Preflight(_ context.Context, _ *provider.JobSpec) error {
	if f.mode == "ineligible" {
		return &provider.ErrIneligible{Why: "fake: request too large"}
	}
	return nil
}

func (f *Fake) Estimate(_ context.Context, j *provider.JobSpec) (config.Micros, error) {
	time.Sleep(f.estimateDelay)
	return config.Micros(f.price * 1e6 * time.Until(j.Expiry).Hours()), nil
}

func (f *Fake) Launch(_ context.Context, j *provider.JobSpec) (provider.Handle, error) {
	switch f.mode {
	case "transient":
		return provider.Handle{}, &provider.ErrTransient{Why: "fake: no capacity"}
	case "unavailable":
		return provider.Handle{}, &provider.ErrUnavailable{Why: "fake: account disabled"}
	}
	f.job.Store(j)
	id := "fake-1"
	if f.track || failModes[f.mode] {
		id = "fake-" + j.Name
		if f.track && !failModes[f.mode] {
			id = "fake-" + j.Job
		}
	}
	attempt := 1
	if f.stateDir != "" {
		_ = appendLine(filepath.Join(f.stateDir, "canaries.log"), fmt.Sprintf("%x %x %x", j.Bearer, j.MacKey, j.Nonce))
		attempt = len(strings.Fields(slurp(filepath.Join(f.stateDir, "attempts")))) + 1
		_ = appendLine(filepath.Join(f.stateDir, "attempts"), j.Name)
	}
	f.call("create", false, "name="+j.Name+" size="+j.SizeKey)
	// A create that leaves nothing (a capacity answer): the first of transient-then-preattach, the
	// second and later of preattach-then-transient. The retry order tests (B.3.3.4) use them.
	if (f.mode == "transient-then-preattach" && attempt == 1) || (f.mode == "preattach-then-transient" && attempt > 1) {
		return provider.Handle{}, &provider.ErrTransient{Why: "fake: no capacity"}
	}
	if f.track || failModes[f.mode] {
		f.updateRes(func(rs []fakeRes) []fakeRes {
			return append(rs, fakeRes{ID: id, Name: j.Name, Owner: j.Owner, Created: time.Now(), Expires: j.Expiry, USDPerHour: f.price})
		})
	}
	switch {
	case f.mode == "preattach-fails" || f.mode == "preattach-teardown-fails" || f.mode == "preattach-then-transient" ||
		(f.mode == "transient-then-preattach" && attempt > 1) || (f.mode == "preattach-fails-once" && attempt == 1):
		return provider.Handle{}, &provider.ErrPreAttach{ID: id, Err: &provider.ErrTransient{Why: "setup step packages failed: apt timeout"}}
	case f.mode == "preattach-repo-setup":
		return provider.Handle{}, &provider.ErrPreAttach{ID: id, Err: &provider.ErrSetup{Step: "deps", Class: "repo", Line: "npm ERR! missing script: ci"}}
	}
	f.call("attach", true, attachData(j))
	if f.stateDir != "" {
		_ = appendLine(filepath.Join(f.stateDir, "launches.log"), j.Name+" "+j.Ref+" "+j.Sha)
		line := j.Arch
		for _, t := range j.Tools {
			line += " " + t.Name + "/" + t.Arch
		}
		_ = appendLine(filepath.Join(f.stateDir, "arch.log"), line)
	}
	if f.mode == "attached-fails" {
		return provider.Handle{}, &provider.ErrAttached{ID: id, Err: &provider.ErrTransient{Why: "fake: lost after attach"}}
	}
	return provider.Handle{ID: id, Name: "fake-" + j.Job}, nil
}

func slurp(p string) string { b, _ := os.ReadFile(p); return string(b) }

func (f *Fake) Watch(ctx context.Context, h provider.Handle, out provider.Sink) (provider.Result, error) {
	j := f.job.Load()
	if j == nil {
		return provider.Result{}, errors.New("fake: no job")
	}
	out.Setup("fake setup step")
	if f.mode == "setup-fails" { // created, never began: the host tears down and exits 75
		return provider.Result{}, fmt.Errorf("%s's setup failed at 'tools'", h.ID)
	}
	if f.mode == "setup-fails-repo" { // the repo's own step failed by itself: the host tears down and exits 69
		return provider.Result{}, &provider.ErrSetup{Step: "deps", Class: "repo", Line: "npm ERR! missing script: ci"}
	}
	out.Phase(provider.Begun, "")
	if f.mode == "lost" {
		return provider.Result{Began: true}, errors.New("fake-1 was lost")
	}
	for f.gate != "" { // held open until the test creates the gate file (K6)
		if _, err := os.Stat(f.gate); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			return provider.Result{Began: true}, ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
	cmd := exec.CommandContext(ctx, j.Argv[0], j.Argv[1:]...)
	cmd.Dir = filepath.Join(f.wt, j.Cwd)
	cmd.Env = j.Env
	cmd.Stdout = sinkWriter{out, provider.Stdout}
	cmd.Stderr = sinkWriter{out, provider.Stderr}
	err := cmd.Run()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return provider.Result{Exit: 0, Began: true}, nil
	case errors.As(err, &ee):
		return provider.Result{Exit: ee.ExitCode(), Began: true}, nil
	default:
		return provider.Result{Began: true}, fmt.Errorf("fake: %w", err)
	}
}

type sinkWriter struct {
	s  provider.Sink
	st provider.Stream
}

func (w sinkWriter) Write(b []byte) (int, error) {
	w.s.Out(w.st, append([]byte(nil), b...))
	return len(b), nil
}

func (f *Fake) Output(context.Context, provider.Handle, provider.Stream, int64, io.Writer) error {
	return nil
}
func (f *Fake) Fetch(context.Context, provider.Handle, provider.ArtifactSpec, io.Writer) error {
	return errors.New("fake: no artifacts")
}
func (f *Fake) Owner(_ context.Context, id string) (string, error) {
	for _, r := range f.loadRes() {
		if r.ID == id {
			return r.Owner, nil
		}
	}
	return "", nil
}

func (f *Fake) Teardown(_ context.Context, id string) error {
	if f.stateDir == "" {
		return nil
	}
	if f.mode == "preattach-teardown-fails" {
		return errors.New("fake: teardown refused")
	}
	f.updateRes(func(rs []fakeRes) []fakeRes {
		var kept []fakeRes
		for _, r := range rs {
			if r.ID != id {
				kept = append(kept, r)
			}
		}
		return kept
	})
	return appendLine(filepath.Join(f.stateDir, "torn.log"), id)
}

func (f *Fake) List(_ context.Context, prefixes []string) ([]provider.Resource, error) {
	var out []provider.Resource
	all := f.loadRes()
	page := f.listPage
	if page <= 0 {
		page = len(all) + 1
	}
	for start := 0; start < len(all); start += page { // follow every page, as a real client must
		end := min(start+page, len(all))
		for _, r := range all[start:end] {
			for _, p := range prefixes {
				if strings.HasPrefix(r.Name, p) {
					out = append(out, provider.Resource{ID: r.ID, Name: r.Name, Install: installOf(r.Name), Created: r.Created, Expires: r.Expires,
						USDPerHour: config.Micros(r.USDPerHour * 1e6)})
					break
				}
			}
		}
	}
	return out, nil
}

func (f *Fake) Spend(context.Context) (config.Micros, error) {
	return config.Micros(f.spend * 1e6), nil
}

// installOf is the install16 field of a resource name <prefix>-<install16>-... ("" if absent).
func installOf(name string) string {
	parts := strings.SplitN(name, "-", 3)
	if len(parts) < 3 {
		return ""
	}
	return parts[1]
}
