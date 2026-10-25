package ssh

import (
	"slices"
	"testing"
	"time"
)

// The argv is exactly providers delta C.2.2 (SH2 runs the same argv against a client and a real sshd).
func TestArgvIsExactlyTheDesign(t *testing.T) {
	s := Settings{Host: "127.0.0.1", Port: 22022, User: "bitc", HostKey: edKey(), HostKeyType: "ssh-ed25519",
		ConnectTimeout: 5 * time.Second}
	got := s.Argv("/state/ssh/known_hosts", "/home/me/.config/bitc/ssh/vm_ed25519", "probe")
	want := []string{"-F", "none", "-o", "BatchMode=yes", "-o", "IdentitiesOnly=yes", "-o", "IdentityAgent=none",
		"-o", "IdentityFile=/home/me/.config/bitc/ssh/vm_ed25519",
		"-o", "UserKnownHostsFile=/state/ssh/known_hosts", "-o", "GlobalKnownHostsFile=/dev/null",
		"-o", "HostKeyAlias=bitc-ssh", "-o", "CheckHostIP=no", "-o", "UpdateHostKeys=no",
		"-o", "StrictHostKeyChecking=yes", "-o", "HostKeyAlgorithms=ssh-ed25519",
		"-o", "ConnectTimeout=5", "-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=4", "-o", "LogLevel=ERROR",
		"-o", "ForwardAgent=no", "-o", "ForwardX11=no", "-o", "ClearAllForwardings=yes", "-o", "ControlMaster=no",
		"-o", "ControlPath=none", "-o", "ProxyCommand=none", "-o", "ProxyJump=none",
		"-a", "-x", "-T", "-p", "22022", "-l", "bitc", "--", "127.0.0.1", "probe"}
	if !slices.Equal(got, want) {
		t.Fatalf("argv\n got %q\nwant %q", got, want)
	}
}

func TestArgvRoundsAConnectTimeoutUpToWholeSeconds(t *testing.T) {
	s := Settings{Host: "h", Port: 22, User: "u", HostKeyType: "ssh-ed25519", ConnectTimeout: 1500 * time.Millisecond}
	if !slices.Contains(s.Argv("k", "i", "probe"), "ConnectTimeout=2") {
		t.Fatalf("%q", s.Argv("k", "i", "probe"))
	}
}
