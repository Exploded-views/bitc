//go:build privileged

package conformance

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/Exploded-views/bitc/internal/config"
	"github.com/Exploded-views/bitc/internal/provider"
	"github.com/Exploded-views/bitc/internal/provider/ssh"
)

// SH2: the pinned argv against a real sshd (the rig's, on 127.0.0.1:2222): the provider's own
// session works with exactly the one `bitc-ssh <key>` known_hosts line, the real client reads
// every pinning option as set (ssh -G), a host key mismatch is ErrUnavailable that pauses, and
// nothing is learned: the file never gains the key the server presented.
func TestSH2_PinnedArgvKnownHostsAndMismatchAgainstARealSSHD(t *testing.T) {
	r := rig(t)
	t.Setenv("PATH", "/usr/bin:/bin") // the rig puts a stand-in "ssh" in /usr/sbin for the job's git; the provider needs the real client
	pub, err := os.ReadFile("/etc/ssh/ssh_host_ed25519_key.pub")
	if err != nil {
		t.Fatal(err)
	}
	serverKey := strings.Join(strings.Fields(string(pub))[:2], " ")
	port, _ := strconv.Atoi(rigSSHPort)
	acct := t.TempDir()
	if err := os.Chmod(acct, 0o700); err != nil { // bitc refuses a group- or world-accessible state dir; t.TempDir follows the umask
		t.Fatal(err)
	}
	eff := &config.Effective{Test: &config.Test{AccountingDir: acct}}
	known := filepath.Join(acct, "ssh", "known_hosts")
	was := ssh.ResolveAgentFile
	ssh.ResolveAgentFile = func(*config.Effective, *config.Provider, string) (string, string, error) { return r.Sha, r.Bin, nil }
	t.Cleanup(func() { ssh.ResolveAgentFile = was })
	newProvider := func(hostKey string) (*ssh.Provider, ssh.Settings) {
		core := sshCore(t, hostKey, r.Key, int64(port))
		set, errs := ssh.DecodeSettings(core)
		if len(errs) != 0 {
			t.Fatalf("settings: %v", errs)
		}
		p, err := ssh.NewProvider(core, eff)
		if err != nil {
			t.Fatal(err)
		}
		return p, set
	}

	// the provider's session over the pinned key, through the gate (an installed agent answers `list`)
	p, set := newProvider(serverKey)
	if _, err := p.List(context.Background(), []string{"bitc-sh2-none"}); err != nil {
		t.Fatalf("list over the pinned host key: %v", err)
	}
	if kh, _ := os.ReadFile(known); string(kh) != "bitc-ssh "+serverKey+"\n" {
		t.Errorf("known_hosts %q, want the one aliased line %q", kh, "bitc-ssh "+serverKey)
	}

	// the real client's reading of the exact argv (ssh -G connects to nothing)
	out, err := exec.Command("/usr/bin/ssh", append([]string{"-G"}, set.Argv(known, r.Key, "probe")...)...).Output()
	if err != nil {
		t.Fatalf("ssh -G: %v", err)
	}
	cfg := "\n" + string(out)
	for _, want := range []string{"stricthostkeychecking true", "hostkeyalias bitc-ssh", "identitiesonly yes", "batchmode yes",
		"forwardagent no", "clearallforwardings yes", "connecttimeout 5", "userknownhostsfile " + known, "globalknownhostsfile /dev/null"} {
		if !strings.Contains(cfg, "\n"+want+"\n") {
			t.Errorf("ssh -G does not report %q", want)
		}
	}

	// a mismatch: the owner pinned another key than the one the server presents
	other, _ := newProvider(shOtherHostKey())
	_, err = other.List(context.Background(), []string{"bitc-sh2-none"})
	var ua *provider.ErrUnavailable
	if !errors.As(err, &ua) || ua.Local || !strings.Contains(ua.Why, "host key verification failed") {
		t.Fatalf("a host key mismatch: want ErrUnavailable that pauses, got %#v", err)
	}
	// no trust on first use: the file holds the pinned line only
	kh, _ := os.ReadFile(known)
	if string(kh) != "bitc-ssh "+shOtherHostKey()+"\n" || strings.Contains(string(kh), strings.Fields(serverKey)[1]) {
		t.Errorf("known_hosts after the mismatch: %q", kh)
	}
}
