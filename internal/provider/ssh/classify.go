package ssh

import (
	"regexp"
	"strings"
)

// Class is what the ssh client's stderr says about how far a session got.
type Class int

const (
	ClassOther   Class = iota // not a pre-session line: the session may have been up
	ClassConnect              // no connection was made: refused, timeout, no route, DNS, key exchange cut
	ClassHostKey              // the host presented another key than the pinned one
	ClassAuth                 // the host refused the identity
)

var (
	connectRE = regexp.MustCompile(`^ssh: connect to host (\S+) port [0-9]+: (Connection refused|Connection timed out|No route to host|Network is unreachable)$`)
	resolveRE = regexp.MustCompile(`^ssh: Could not resolve hostname (\S+): .+$`)
	kexRE     = regexp.MustCompile(`^kex_exchange_identification: .+$`)
	authRE    = regexp.MustCompile(`^(\S+)@(\S+): Permission denied \(publickey\)\.$`)
)

const hostKeyLine = "Host key verification failed."

// lineClass classifies one stderr line. host and user are checked when not empty.
func lineClass(l, user, host string) (Class, string) {
	if m := connectRE.FindStringSubmatch(l); m != nil && (host == "" || m[1] == host) {
		return ClassConnect, map[string]string{"Connection refused": "connection refused", "Connection timed out": "connect timed out",
			"No route to host": "no route to host", "Network is unreachable": "network is unreachable"}[m[2]]
	}
	if m := resolveRE.FindStringSubmatch(l); m != nil && (host == "" || m[1] == host) {
		return ClassConnect, "cannot resolve hostname"
	}
	if kexRE.MatchString(l) {
		return ClassConnect, "connection closed during key exchange"
	}
	if l == hostKeyLine {
		return ClassHostKey, "host key verification failed (the pinned host_key does not match)"
	}
	if m := authRE.FindStringSubmatch(l); m != nil && (user == "" || (m[1] == user && m[2] == host)) {
		return ClassAuth, "permission denied (publickey)"
	}
	return ClassOther, ""
}

// ClassifySession matches the client's result against the pre-session whitelist of providers
// delta C.5.1 item 3: only exit 255 with stderr that is exactly one of the listed lines (with
// LogLevel=ERROR) proves that no channel and no request existed. Anything else, a second line
// or other text included, is ClassOther: the session may have been up.
func ClassifySession(code int, stderr, user, host string) (Class, string) {
	if code != 255 {
		return ClassOther, ""
	}
	lines := strings.Split(strings.TrimRight(stderr, "\r\n"), "\n")
	if len(lines) != 1 {
		return ClassOther, ""
	}
	return lineClass(strings.TrimRight(lines[0], "\r"), user, host)
}

// ClassifyLoose is for the steps before the resource point, where nothing of this job exists on
// the host: the whitelisted line may sit among others (the host-key mismatch prints a banner).
func ClassifyLoose(code int, stderr string) (Class, string) {
	if code != 255 {
		return ClassOther, ""
	}
	for _, l := range strings.Split(stderr, "\n") {
		if c, why := lineClass(strings.TrimRight(l, "\r"), "", ""); c != ClassOther {
			return c, why
		}
	}
	return ClassOther, ""
}
