package status

import (
	"fmt"
	"io"
	"os"
	"runtime"

	"golang.org/x/sys/unix"

	"github.com/Exploded-views/bitc/internal/admit"
	"github.com/Exploded-views/bitc/internal/config"
	"github.com/Exploded-views/bitc/internal/lock"
	"github.com/Exploded-views/bitc/internal/state"
)

// RemoteDoctor runs the --remote checks. unusable reports a configuration the host
// cannot run (an agent module_dir that is not a clean git tree): doctor exits 78 for it, not 69.
var RemoteDoctor func(e *config.Effective, out io.Writer) (failed, unusable bool)

// Doctor prints "ok|warn|fail <check>: <detail>" lines; failed reports any fail (spec 3.3).
func Doctor(e *config.Effective, l state.Layout, out io.Writer) (failed bool) {
	red := Redactor(e)
	line := func(level, check, detail string) {
		detail = red.String(detail)
		fmt.Fprintf(out, "%s %s: %s\n", level, check, detail)
		failed = failed || level == "fail"
	}
	switch runtime.GOOS {
	case "linux":
		line("ok", "linux", runtime.GOOS+"/"+runtime.GOARCH)
	case "darwin":
		line("warn", "linux", runtime.GOOS+"/"+runtime.GOARCH+" client build: local pools only; "+
			"memory admission, remote providers and the remote agent need Linux")
	default:
		line("fail", "linux", "bitc needs Linux, this is "+runtime.GOOS)
	}
	line("ok", "config", fmt.Sprintf("host %s, repo %s", orNone(e.HostFile), orNone(e.RepoFile)))
	line("ok", "state dir", l.Root+" (private, local filesystem, schema "+fmt.Sprint(state.SchemaVersion)+")")
	// a private probe file, so two doctors at once never contend for it
	if tmp, err := os.CreateTemp(l.Root, ".doctor-*.lock"); err != nil {
		line("fail", "flock", err.Error())
	} else if f, err := lock.Open(tmp.Name()); err != nil {
		tmp.Close()
		_ = os.Remove(tmp.Name())
		line("fail", "flock", err.Error())
	} else {
		ok, err := f.TryLock()
		f.Close()
		tmp.Close()
		_ = os.Remove(tmp.Name())
		if !ok || err != nil {
			line("fail", "flock", fmt.Sprintf("cannot flock in %s: %v", l.Root, err))
		} else {
			line("ok", "flock", "works in the state dir")
		}
	}
	root := ""
	if e.Test != nil {
		root = e.Test.ProcfsRoot
	}
	if avail, ok, src := admit.ReadMemory(root); ok {
		line("ok", "meminfo", fmt.Sprintf("%d MiB available (%s)", avail>>20, src))
	} else if runtime.GOOS == "linux" { // elsewhere a pool's memory key is a config error
		line("warn", "meminfo", "cannot read "+src+"; memory admission admits with a warning")
	}
	var st unix.Statfs_t
	if unix.Statfs(l.Root, &st) == nil && onTmpfs(&st) {
		line("warn", "state dir", "is on tmpfs: tickets and history vanish on reboot")
	}
	for _, h := range admit.LegacyHolders(e.Compat) {
		line("warn", "legacy", h)
	}
	return failed
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}
