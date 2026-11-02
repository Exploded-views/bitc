package rails

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Exploded-views/bitc/internal/config"
	"github.com/Exploded-views/bitc/internal/lock"
	"github.com/Exploded-views/bitc/internal/provider"
	"github.com/Exploded-views/bitc/internal/remote"
	"github.com/Exploded-views/bitc/internal/state"
)

// legacyApplies: the legacy launch lock and reservation file are RunPod's (spec 12), so only a
// runpod job takes part. "fake" is the conformance stand-in for RunPod.
func legacyApplies(j *remote.Job) bool { return j.Type == "runpod" || j.Type == "fake" }

// legacyLaunchLock takes <legacy_dir>/locks/runpod-launch.lock after our own (spec 12). Without
// [compat] it is a no-op. If the lock cannot be opened or is not taken within launch_wait it
// returns an *provider.ErrTransient: the launch is refused (75), because a launch the legacy
// launcher cannot see would bypass the shared spend cap.
func legacyLaunchLock(ctx context.Context, j *remote.Job) (func(), error) {
	c := j.P.Eff.Compat
	if c == nil || !legacyApplies(j) {
		return func() {}, nil
	}
	path := filepath.Join(c.LegacyDir, "locks", "runpod-launch.lock")
	l, err := lock.Open(path)
	if err != nil {
		var pe *os.PathError
		if errors.As(err, &pe) && pe.Path == path {
			err = pe.Err // the path is already named once
		}
		return nil, &provider.ErrTransient{Why: "legacy launch lock " + path + ": " + err.Error()}
	}
	if l.LockWait(ctx, j.P.Eff.Scale(j.Core.LaunchWait)) != nil {
		l.Close()
		return nil, &provider.ErrTransient{Why: "legacy launch lock " + path + ": busy"}
	}
	return func() { _ = l.Unlock(); l.Close() }, nil
}

// legacyReservation appends "<pid> <name> <usd> <expiry>" to <legacy_dir>/runpod-reservations
// so legacy launches count this job; the returned func removes the line at the job's end.
func legacyReservation(j *remote.Job, est config.Micros) func() {
	c := j.P.Eff.Compat
	if c == nil || !legacyApplies(j) {
		return func() {}
	}
	path := filepath.Join(c.LegacyDir, "runpod-reservations")
	line := fmt.Sprintf("%d %s %.2f %d", os.Getpid(), j.Spec.Name, float64(est)/1e6, j.Spec.Expiry.Unix())
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return func() {}
	}
	fmt.Fprintln(f, line)
	f.Close()
	return func() {
		l, err := lock.Open(filepath.Join(c.LegacyDir, "locks", "runpod-launch.lock"))
		if err != nil {
			return
		}
		defer l.Close()
		if l.LockWait(context.Background(), j.Core.LaunchWait) != nil {
			return
		}
		defer l.Unlock()
		b, err := os.ReadFile(path)
		if err != nil {
			return
		}
		var kept []string
		for _, x := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
			if x != line && x != "" {
				kept = append(kept, x)
			}
		}
		out := strings.Join(kept, "\n")
		if out != "" {
			out += "\n"
		}
		_ = state.WriteFileAtomic(path, []byte(out))
	}
}
