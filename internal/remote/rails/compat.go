package rails

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Exploded-views/bitc/internal/config"
	"github.com/Exploded-views/bitc/internal/lock"
	"github.com/Exploded-views/bitc/internal/remote"
	"github.com/Exploded-views/bitc/internal/state"
)

// legacyLaunchLock takes <legacy_dir>/locks/runpod-launch.lock after our own (spec 12).
func legacyLaunchLock(ctx context.Context, j *remote.Job) func() {
	c := j.P.Eff.Compat
	if c == nil {
		return func() {}
	}
	l, err := lock.Open(filepath.Join(c.LegacyDir, "locks", "runpod-launch.lock"))
	if err != nil {
		return func() {}
	}
	if l.LockWait(ctx, j.P.Eff.Scale(j.Core.LaunchWait)) != nil {
		l.Close()
		return func() {}
	}
	return func() { _ = l.Unlock(); l.Close() }
}

// legacyReservation appends "<pid> <name> <usd> <expiry>" to <legacy_dir>/runpod-reservations
// so legacy launches count this job; the returned func removes the line at the job's end.
func legacyReservation(j *remote.Job, est config.Micros) func() {
	c := j.P.Eff.Compat
	if c == nil {
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
