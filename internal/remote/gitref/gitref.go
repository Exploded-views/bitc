// Package gitref holds the git helpers of the git-push transport (spec 8.4): clone URL
// derivation, known hosts, and job-ref sweeping. It imports nothing from bitc.
package gitref

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// CloneURL derives the SSH clone URL from a remote URL; userinfo is never sent.
func CloneURL(remoteURL string) (string, error) {
	if strings.HasPrefix(remoteURL, "https://") || strings.HasPrefix(remoteURL, "http://") {
		u, err := url.Parse(remoteURL)
		if err != nil {
			return "", fmt.Errorf("cannot parse the remote URL: %s", RedactURL(err.Error()))
		}
		if u.Port() != "" {
			return "", fmt.Errorf("the remote %s has a port; set clone_url in the [[repos]] grant", u.Hostname())
		}
		return "git@" + u.Hostname() + ":" + strings.TrimSuffix(strings.TrimPrefix(u.Path, "/"), ".git") + ".git", nil
	}
	if strings.HasPrefix(remoteURL, "ssh://") {
		u, err := url.Parse(remoteURL)
		if err != nil {
			return "", fmt.Errorf("cannot parse the remote URL: %s", RedactURL(err.Error()))
		}
		if u.User != nil {
			u.User = url.User(u.User.Username()) // minus any password
		}
		return u.String(), nil
	}
	if at, rest, ok := strings.Cut(remoteURL, "@"); ok && strings.Contains(rest, ":") { // scp-style
		user, _, _ := strings.Cut(at, ":")
		return user + "@" + rest, nil
	}
	return "", fmt.Errorf("cannot derive an SSH clone URL from %q", RedactURL(remoteURL))
}

// RepoID is 16 hex of sha256 of the clone URL.
func RepoID(clone string) string {
	s := sha256.Sum256([]byte(clone))
	return hex.EncodeToString(s[:])[:16]
}

// HostOf returns the host of an SSH clone URL.
func HostOf(clone string) string {
	if u, err := url.Parse(clone); err == nil && u.Host != "" {
		return u.Hostname()
	}
	_, rest, _ := strings.Cut(clone, "@")
	h, _, _ := strings.Cut(rest, ":")
	return h
}

// KnownHostsHas reports whether an OpenSSH known_hosts text has a key for host.
func KnownHostsHas(kh, host string) bool {
	for _, line := range strings.Split(kh, "\n") {
		f := strings.Fields(line)
		if len(f) >= 3 {
			for _, h := range strings.Split(f[0], ",") {
				if h == host {
					return true
				}
			}
		}
	}
	return false
}

// DeleteRefDetached hands a ref deletion to a new session so an interrupted caller never waits
// for the network (spec 8.3 cleanup order).
func DeleteRefDetached(wt, remoteName, ref string) {
	c := exec.Command("git", "-C", wt, "push", "-q", remoteName, ":"+ref)
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	c.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	_ = c.Start()
	if c.Process != nil {
		_ = c.Process.Release()
	}
}

// AgentRefPrefix holds the hidden refs that carry a source = "build" agent commit (spec 8.4).
const AgentRefPrefix = "refs/bitc/agent/"

// SweepRefs deletes own refs (this install) older than olderThan (spec 8.3 leak control).
func SweepRefs(ctx context.Context, wt, remoteName, prefix, install string, olderThan time.Duration, dry bool) ([]string, error) {
	out, err := exec.CommandContext(ctx, "git", "-C", wt, "ls-remote", remoteName, prefix+install+"/*").Output()
	if err != nil {
		return nil, err
	}
	var swept []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Fields(line)
		if len(f) != 2 {
			continue
		}
		parts := strings.Split(f[1][strings.LastIndex(f[1], "/")+1:], "-")
		if len(parts) != 3 {
			continue
		}
		ts, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil || time.Since(time.Unix(ts, 0)) < olderThan {
			continue
		}
		if !dry {
			if err := exec.CommandContext(ctx, "git", "-C", wt, "push", "-q", remoteName, ":"+f[1]).Run(); err != nil {
				continue
			}
		}
		swept = append(swept, f[1])
	}
	return swept, nil
}

// FirstLine is the first line of s, trimmed.
func FirstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

var userinfoRE = regexp.MustCompile(`://[^/@\s]+@`)

// RedactURL drops userinfo from any URL in a message (git can echo the remote URL).
func RedactURL(s string) string { return userinfoRE.ReplaceAllString(s, "://") }
