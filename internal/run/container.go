package run

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/Exploded-views/bitc/internal/admit"
	"github.com/Exploded-views/bitc/internal/config"
	"github.com/Exploded-views/bitc/internal/container"
	"github.com/Exploded-views/bitc/internal/exit"
	"github.com/Exploded-views/bitc/internal/state"
)

// hostPassthrough are the only variables of the caller's environment a container gets: the
// terminal and locale settings a command's output depends on. Everything else (tokens, HOME,
// PATH, SSH_AUTH_SOCK, ...) stays on the host; the pool's env, env_default and env hook say
// what else the container sees (docs/containers.md).
var hostPassthrough = []string{"TERM", "LANG", "LC_ALL", "LC_CTYPE", "TZ"}

// guardCommand is how the guard process is started: bitc's own binary with a hidden subcommand.
// A seam, because a test binary has no such subcommand.
var guardCommand = func() (string, []string, error) {
	self, err := os.Executable()
	return self, []string{"__container-guard"}, err
}

// containerEnv is the environment the command sees inside the container, as NAME=VALUE sorted by
// name: the passthrough variables, the pool's env_default (for keys not passed) and env, the env
// hook's output and BITC_POOL, BITC_SLOT and BITC_JOB. BITC_HOLD and BITC_DIR name host paths and
// a hold the container's bitc could not verify, and the compat hold_env is a host lock: none of
// them is passed.
func containerEnv(in EnvInput, kv []KV) ([]string, error) {
	var caller []string
	for _, e := range in.Caller {
		k, _, _ := strings.Cut(e, "=")
		for _, p := range hostPassthrough {
			if k == p {
				caller = append(caller, e)
			}
		}
	}
	in.Caller = caller
	env, err := BaseEnv(in)
	if err != nil {
		return nil, err
	}
	env = ApplyHookEnv(env, kv, BitcVars(in))
	drop := []string{"BITC_HOLD", "BITC_DIR"}
	for _, x := range in.Hold.ExtraEnv {
		k, _, _ := strings.Cut(x, "=")
		drop = append(drop, k)
	}
	env = withoutKeys(env, drop...)
	sort.Slice(env, func(i, j int) bool {
		ki, _, _ := strings.Cut(env[i], "=")
		kj, _, _ := strings.Cut(env[j], "=")
		return ki < kj
	})
	return env, nil
}

// workTreeMount is the work tree mounted at the same path; with no work tree, the cwd. (The work
// tree is derived from the cwd, so the cwd is inside it; the prefix test is a guard, not a case.)
func workTreeMount(eff *config.Effective) config.Mount {
	src := eff.Worktree
	if src == "" || (eff.Cwd != src && !strings.HasPrefix(eff.Cwd, strings.TrimSuffix(src, "/")+"/")) {
		src = eff.Cwd
	}
	return config.Mount{Source: src, Target: src}
}

// containerRun is the docker side of one containerized run.
type containerRun struct {
	spec    container.Spec
	env     []string // the host's environment: what the docker CLI, the removals and the guard run with
	guard   *container.Guard
	pending sync.WaitGroup
	once    sync.Once
	created bool
}

// containerRuntime finds the pool's runtime CLI, or is the 69 for its absence.
func containerRuntime(pool *config.Pool, env []string) (string, error) {
	rt, err := container.Lookup(pool.Container.Runtime, env)
	if err != nil {
		return "", exit.Unavailablef("%s: container runtime %q not found on PATH (install it, or remove pools.%s.container)", pool.Name, pool.Container.Runtime, pool.Name)
	}
	return rt, nil
}

// newContainerRun builds the docker run argv for the admitted command and starts the guard that
// removes the container if bitc is killed. The argv replaces the command: Exec runs the docker CLI
// as the run's child, so the limit, the signals and the exit status work as for any local run.
func newContainerRun(inv Invocation, p *Prepared, hold *admit.Hold, rt string, in EnvInput, kv []KV, hostEnv []string, stderr io.Writer) (*containerRun, []string, error) {
	eff, pool := p.Eff, p.Pool
	cenv, err := containerEnv(in, kv)
	if err != nil {
		return nil, nil, err
	}
	for _, e := range cenv { // one NAME=VALUE per line of the env file: a value cannot hold a line break
		if k, _, _ := strings.Cut(e, "="); strings.ContainsAny(e, "\n\r\x00") {
			return nil, nil, exit.Configf("%s: the container env %s has a line break or a NUL in its value, which docker's env file cannot hold", pool.Name, k)
		}
	}
	wt := workTreeMount(eff)
	if strings.ContainsAny(wt.Source, ",\"\n") {
		return nil, nil, exit.Configf("%s: the work tree path %q has a comma, a double quote or a newline, which docker's --mount cannot take", pool.Name, wt.Source)
	}
	id := container.TicketID(hold.Ticket)
	cid := filepath.Join(p.Layout.ContainersDir(), id+".cid")
	if err := os.MkdirAll(p.Layout.ContainersDir(), 0o700); err != nil {
		return nil, nil, exit.Unavailablef("%s: cannot create %s: %v", pool.Name, p.Layout.ContainersDir(), err)
	}
	_ = os.Remove(cid) // docker refuses an existing --cidfile
	// The container's environment goes to docker in a 0600 file in the state dir (0700), never on
	// argv, which every local user can read for the whole run. It is removed when the run ends:
	// finish, and the guard if bitc is killed.
	envFile := filepath.Join(p.Layout.ContainersDir(), id+".env")
	if err := container.WriteEnvFile(envFile, cenv); err != nil {
		return nil, nil, exit.Unavailablef("%s: cannot write the container env file: %v", pool.Name, err)
	}
	c := pool.Container
	cr := &containerRun{env: hostEnv, spec: container.Spec{
		Runtime: rt, Image: c.Image, Job: p.Job, Pool: pool.Name, State: container.StateHash(p.Layout.Root), Ticket: id, CID: cid, EnvFile: envFile,
		Mounts: append([]config.Mount{wt}, c.Mounts...), Dir: eff.Cwd, CPUs: c.CPUs, Memory: c.Memory,
		TTY: isTTY(inv.Stdin), Argv: p.Args.Argv}}
	exe, args, err := guardCommand()
	if err == nil {
		cr.guard, err = container.StartGuard(exe, args, rt, cid, envFile, hostEnv)
	}
	if err != nil {
		fmt.Fprintf(stderr, "bitc: %s: cannot start the container guard (%v); if bitc is killed the container stays until the next run or bitc sweep removes it\n", pool.Name, err)
	}
	return cr, container.RunArgv(cr.spec), nil
}

// kill force-removes the container without holding up the run's own teardown (Exec's SIGKILL
// step). It removes by the id docker wrote to the cidfile and by nothing else: no id, no container
// this run created (yet; finish looks again), and a name could be another run's.
func (cr *containerRun) kill() {
	id := container.ReadID(cr.spec.CID)
	if id == "" {
		return
	}
	cr.pending.Add(1)
	exit.Go(func() {
		defer cr.pending.Done()
		_ = container.Remove(context.Background(), cr.spec.Runtime, id, cr.env)
	})
}

// finish runs once the docker CLI has ended, however it ended: the container with the id in the
// cidfile is force-removed (--rm already removed one that ended by itself), the cidfile and the env
// file are removed, and the guard is told it can go. With no id in the cidfile docker never created
// a container for this run: nothing is removed, and a CLI that exited 125 failed before the
// command started. It reports whether docker created the container.
func (cr *containerRun) finish() bool {
	cr.once.Do(func() {
		cr.pending.Wait()
		id := container.ReadID(cr.spec.CID)
		cr.created = id != ""
		if cr.created {
			_ = container.Remove(context.Background(), cr.spec.Runtime, id, cr.env)
		}
		_ = os.Remove(cr.spec.CID)
		_ = os.Remove(cr.spec.EnvFile)
		if cr.guard != nil {
			cr.guard.Done()
		}
	})
	return cr.created
}

// reapOrphans removes this state dir's containers whose run is gone (a bitc killed twice over:
// the guard did not get to them). It runs before admission, so the next run sweeps what the last
// one left; a failure is not the run's.
func reapOrphans(ctx context.Context, p *Prepared, rt string, env []string, stderr io.Writer, hist func(state.Event)) {
	err := container.Sweep(ctx, rt, p.Layout, env, false, func(f container.Found) {
		fmt.Fprintf(stderr, "bitc: removed orphaned container %s (its run is gone)\n", f.Name)
		// not "reaped": that counts dead tickets; the job is the bitc.job label (the name is the ticket's)
		hist(state.Event{Event: "container-reaped", Pool: f.Pool, Job: f.Job})
	}, func(container.Found) {})
	if err != nil {
		// an error aborts the sweep with nothing deleted; it never stops the run
		Debugf(p, stderr, "%s: container sweep aborted: %v", p.Pool.Name, err)
	}
}
