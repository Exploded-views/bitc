package ssh

import (
	"strconv"
	"time"
)

// Argv is everything after the program name of one ssh invocation (providers delta C.2.2): the
// host key is pinned (StrictHostKeyChecking=yes, a dedicated known_hosts file with one aliased
// line, no global file, no key update), no agent or other forwarding is ever requested,
// only the one identity file is offered, nothing prompts, and the connect has a timeout. No
// secret is in it: identity is a path. verb is the gate's command line (one argument).
func (s Settings) Argv(knownHosts, identity, verb string) []string {
	secs := int((s.ConnectTimeout + time.Second - 1) / time.Second)
	opts := []string{
		"BatchMode=yes", "IdentitiesOnly=yes", "IdentityAgent=none", "IdentityFile=" + identity,
		"UserKnownHostsFile=" + knownHosts, "GlobalKnownHostsFile=/dev/null",
		"HostKeyAlias=bitc-ssh", "CheckHostIP=no", "UpdateHostKeys=no",
		"StrictHostKeyChecking=yes", "HostKeyAlgorithms=" + s.HostKeyType,
		"ConnectTimeout=" + strconv.Itoa(secs), "ServerAliveInterval=15", "ServerAliveCountMax=4", "LogLevel=ERROR",
		"ForwardAgent=no", "ForwardX11=no", "ClearAllForwardings=yes", "ControlMaster=no",
		"ControlPath=none", "ProxyCommand=none", "ProxyJump=none",
	}
	argv := []string{"-F", "none"}
	for _, o := range opts {
		argv = append(argv, "-o", o)
	}
	return append(argv, "-a", "-x", "-T", "-p", strconv.Itoa(s.Port), "-l", s.User, "--", s.Host, verb)
}
