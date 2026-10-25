package conformance

// SH2, the part that needs no sshd: the provider drives the system ssh client (a stand-in script
// on PATH that records its argv and judges the host key against the pinned known_hosts file the
// way StrictHostKeyChecking=yes would), so the exact argv, the one-line known_hosts file, the
// host key mismatch answer and the no-TOFU rule are proven in `make ci`. The privileged half
// (shssh_priv_test.go) runs the same checks against a real sshd.

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Exploded-views/bitc/internal/config"
	"github.com/Exploded-views/bitc/internal/provider"
	"github.com/Exploded-views/bitc/internal/provider/ssh"
)

// fakeSSHClient judges like the real client with the pinned options: the key the "server"
// presents (FAKE_SSH_SERVER_KEY) must be the one line of the given UserKnownHostsFile, under the
// alias; otherwise it prints the client's mismatch line and exits 255. FAKE_SSH_STDERR (when
// set) is printed instead, with exit 255, as a connect failure would.
const fakeSSHClient = `#!/bin/sh
printf '%s\n' "$@" > "$FAKE_SSH_ARGV"
if [ -n "${FAKE_SSH_STDERR:-}" ]; then echo "$FAKE_SSH_STDERR" >&2; exit 255; fi
kh=
prev=
for a in "$@"; do
  case $prev in -o) case $a in UserKnownHostsFile=*) kh=${a#UserKnownHostsFile=} ;; esac ;; esac
  prev=$a
done
[ -n "$kh" ] || { echo "no known_hosts file" >&2; exit 2; }
if [ "$(wc -l < "$kh")" = 1 ] && grep -qxF "bitc-ssh $FAKE_SSH_SERVER_KEY" "$kh"; then exit 0; fi
echo "Host key verification failed." >&2
exit 255
`

// shOtherHostKey is a valid ed25519 line that differs from shHostKey.
func shOtherHostKey() string {
	b := make([]byte, 32)
	b[0] = 9
	blob := append(shWire([]byte("ssh-ed25519")), shWire(b)...)
	return "ssh-ed25519 " + base64.StdEncoding.EncodeToString(blob)
}

func sshCore(t *testing.T, hostKey, identity string, port int64) *config.Provider {
	t.Helper()
	return &config.Provider{Type: "ssh", Fixed: true, NamePrefix: "bitc", MaxLive: 1, Raw: map[string]any{
		"host": "127.0.0.1", "port": port, "user": "bitc", "host_key": hostKey, "identity_file": identity,
		"arch": "amd64", "slots": int64(1), "resolver": "9.9.9.9",
	}}
}

func TestSH2_PinnedArgvKnownHostsAndMismatchWithAStandInClient(t *testing.T) {
	dir := t.TempDir()
	bin, argvFile, acct := filepath.Join(dir, "bin"), filepath.Join(dir, "argv"), filepath.Join(dir, "account")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "ssh"), []byte(fakeSSHClient), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	t.Setenv("FAKE_SSH_ARGV", argvFile)
	t.Setenv("FAKE_SSH_SERVER_KEY", shHostKey())
	key := filepath.Join(dir, "vm_ed25519")
	shKey(t, key, "none")
	sha := strings.Repeat("ab", 32)
	was := ssh.ResolveAgentFile
	ssh.ResolveAgentFile = func(*config.Effective, *config.Provider, string) (string, string, error) { return sha, "", nil }
	t.Cleanup(func() { ssh.ResolveAgentFile = was })
	eff := &config.Effective{Test: &config.Test{AccountingDir: acct}}
	known := filepath.Join(acct, "ssh", "known_hosts")

	list := func(t *testing.T, hostKey string) error {
		t.Helper()
		p, err := ssh.NewProvider(sshCore(t, hostKey, key, 22022), eff)
		if err != nil {
			t.Fatal(err)
		}
		_, err = p.List(context.Background(), []string{"bitc"})
		return err
	}

	t.Run("the argv and the known_hosts file", func(t *testing.T) {
		if err := list(t, shHostKey()); err != nil {
			t.Fatalf("list over the pinned key: %v", err)
		}
		b, _ := os.ReadFile(argvFile)
		got := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
		want := []string{"-F", "none", "-o", "BatchMode=yes", "-o", "IdentitiesOnly=yes", "-o", "IdentityAgent=none",
			"-o", "IdentityFile=" + key, "-o", "UserKnownHostsFile=" + known, "-o", "GlobalKnownHostsFile=/dev/null",
			"-o", "HostKeyAlias=bitc-ssh", "-o", "CheckHostIP=no", "-o", "UpdateHostKeys=no",
			"-o", "StrictHostKeyChecking=yes", "-o", "HostKeyAlgorithms=ssh-ed25519",
			"-o", "ConnectTimeout=5", "-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=4", "-o", "LogLevel=ERROR",
			"-o", "ForwardAgent=no", "-o", "ForwardX11=no", "-o", "ClearAllForwardings=yes", "-o", "ControlMaster=no",
			"-o", "ControlPath=none", "-o", "ProxyCommand=none", "-o", "ProxyJump=none",
			"-a", "-x", "-T", "-p", "22022", "-l", "bitc", "--", "127.0.0.1", "agent " + sha + " list"}
		if !slices.Equal(got, want) {
			t.Errorf("argv\n got %q\nwant %q", got, want)
		}
		if kh, _ := os.ReadFile(known); string(kh) != "bitc-ssh "+shHostKey()+"\n" {
			t.Errorf("known_hosts %q, want the one aliased line", kh)
		}
	})
	t.Run("a host key mismatch is a pause and nothing is learned", func(t *testing.T) {
		t.Setenv("FAKE_SSH_SERVER_KEY", shHostKey()) // the server still presents the key it always had
		err := list(t, shOtherHostKey())             // the owner pinned another one
		var ua *provider.ErrUnavailable
		if !errors.As(err, &ua) || ua.Local || !strings.Contains(ua.Why, "host key verification failed") {
			t.Fatalf("want ErrUnavailable that pauses with the host key reason, got %#v", err)
		}
		// no trust on first use: the file holds the pinned line only, never the key the server presented
		kh, _ := os.ReadFile(known)
		if string(kh) != "bitc-ssh "+shOtherHostKey()+"\n" || strings.Contains(string(kh), strings.Fields(shHostKey())[1]) {
			t.Errorf("known_hosts after the mismatch: %q", kh)
		}
	})
	t.Run("an unreachable host is unavailable here and now, no pause", func(t *testing.T) {
		t.Setenv("FAKE_SSH_STDERR", "ssh: connect to host 127.0.0.1 port 22022: Connection refused")
		err := list(t, shHostKey())
		var ua *provider.ErrUnavailable
		if !errors.As(err, &ua) || !ua.Local || !strings.Contains(ua.Why, "connection refused") {
			t.Fatalf("want ErrUnavailable{Local} naming the refusal, got %#v", err)
		}
	})
}
