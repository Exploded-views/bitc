package config

import (
	"os"
	"os/user"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// ExperimentalRemoteRefusal is the config problem for a remote provider without
// [host] experimental_remote = true (README "Known issues").
const ExperimentalRemoteRefusal = "remote providers are experimental in this release and off by default; " +
	"their spend accounting is still under review (see https://github.com/Exploded-views/bitc#known-issues). " +
	"To use them anyway, set [host] experimental_remote = true."

// ExperimentalRemoteCleanup follows the refusal for sweep, doctor --remote and a host that an
// earlier version left remote reservations or ledger lines on.
const ExperimentalRemoteCleanup = "To clean up resources left by an earlier version, set the flag and run bitc sweep."

// checkExperimentalRemote refuses remote providers unless the host file opts in: any
// [providers.<type>] table, or a pool naming a provider other than local, is one problem. Off
// Linux the platform refusal of every provider (checkPlatform) stands instead. It only reads.
func checkExperimentalRemote(p *problems, e *Effective, host fileSchema, goos string, cleanup bool) {
	if goos != "linux" || e.Host.ExperimentalRemote {
		return
	}
	remote := len(host.Providers) > 0
	for _, ps := range host.Pools {
		if ps == nil {
			continue
		}
		for _, t := range ps.Providers {
			remote = remote || t != "local"
		}
	}
	if !remote {
		return
	}
	if cleanup || remoteLeftovers(e) {
		p.addf("%s\n%s", ExperimentalRemoteRefusal, ExperimentalRemoteCleanup)
		return
	}
	p.addf("%s", ExperimentalRemoteRefusal)
}

// accountDir is state.AccountDir without creating it: <passwd home>/.local/state/bitc/account,
// "" when there is no home. Unit tests replace it so they never read this account's real dir.
var accountDir = func() string {
	if u, err := user.Current(); err == nil && u.HomeDir != "" {
		return filepath.Join(u.HomeDir, ".local", "state", "bitc", "account")
	}
	return ""
}

// remoteLeftovers reports whether any provider type has an unfinished reservation (RemoteLeftovers).
func remoteLeftovers(e *Effective) bool { return len(RemoteLeftovers(e)) > 0 }

// RemoteLeftovers lists, sorted, the provider types whose remote/<type>/reservations.json in the
// accounting dir lists a job: a run that never finished (bitc was killed, the host lost power),
// whose resource may still exist and cost money until a sweep with that provider configured
// removes it. The ledger is not used: it is append-only, so its lines only say that remote jobs
// ran once, and a sweep closes no launch. It only reads, never creates the dir, and never blocks.
func RemoteLeftovers(e *Effective) []string {
	dir := accountDir()
	if e.Test != nil && e.Test.AccountingDir != "" {
		dir = e.Test.AccountingDir
	}
	if dir == "" {
		return nil
	}
	types, _ := os.ReadDir(filepath.Join(dir, "remote")) // sorted by name
	var out []string
	for _, t := range types {
		// a type-shaped name only: it is printed by doctor and sweep
		if t.IsDir() && pluginTypeRE.MatchString(t.Name()) && listsAJob(filepath.Join(dir, "remote", t.Name(), "reservations.json")) {
			out = append(out, t.Name())
		}
	}
	return out
}

// UnconfiguredLeftovers is RemoteLeftovers without the types the host configures (a sweep of
// those removes what is left): what doctor and sweep point out on their own.
func UnconfiguredLeftovers(e *Effective) []string {
	var out []string
	for _, t := range RemoteLeftovers(e) {
		if e.Providers[t] == nil {
			out = append(out, t)
		}
	}
	return out
}

// LeftoverHint is doctor's warn detail and sweep's hint for a type in UnconfiguredLeftovers.
func LeftoverHint(typ string) string {
	return typ + ": an earlier version's run may have left paid resources (an unfinished reservation in " +
		"the accounting dir); configure [providers." + typ + "] with [host] experimental_remote = true and run bitc sweep"
}

// leftoverPeek bounds the read of a reservations file: enough to tell "[]" from a list.
const leftoverPeek = 64

// listsAJob reports whether path is a regular file holding more than an empty JSON list. It opens
// without following a link and without blocking (a FIFO or device is never waited on), checks
// the open file with fstat, and reads at most leftoverPeek bytes.
func listsAJob(path string) bool {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return false
	}
	defer unix.Close(fd)
	var st unix.Stat_t
	if unix.Fstat(fd, &st) != nil || st.Mode&unix.S_IFMT != unix.S_IFREG {
		return false
	}
	buf := make([]byte, leftoverPeek)
	n, _ := unix.Read(fd, buf)
	if n <= 0 {
		return false
	}
	// an empty list ("[" and whitespace, cut by the peek or closed by "]") is none; anything else
	// counts: an object in a list, or a damaged file (it may still list a job)
	s := strings.Join(strings.Fields(string(buf[:n])), "")
	if rest, ok := strings.CutPrefix(s, "["); ok {
		return rest != "" && rest != "]"
	}
	return s != "" && s != "null"
}
