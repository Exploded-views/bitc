package ssh

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Exploded-views/bitc/internal/config"
)

// stubSSH puts an `ssh` first on PATH that records its argv, stdin and SSH_AUTH_SOCK in rec and
// answers like a gate: it exits with the code in rec.code (default 0).
func stubSSH(t *testing.T) (rec string) {
	t.Helper()
	dir := t.TempDir()
	rec = filepath.Join(dir, "rec")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$@\" > \"$REC.args\"\n" +
		"cat > \"$REC.stdin\"\n" +
		"printf '%s' \"${SSH_AUTH_SOCK-unset}\" > \"$REC.sock\"\n" +
		"if [ -f \"$REC.code\" ]; then echo 'ssh: connect to host: refused' >&2; exit \"$(cat \"$REC.code\")\"; fi\n" +
		"echo '{\"gate_version\":1}'\n"
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0o700); err != nil { //nolint:gosec // a test stub the process must execute
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("REC", rec)
	t.Setenv("SSH_AUTH_SOCK", "/tmp/agent.sock")
	return rec
}

// The ssh client runs wherever bitc does, macOS included (#4): the system ssh is found on PATH,
// gets the pinned argv and the one-line known_hosts, never the caller's agent socket, and its
// exit status and stderr come back as the gate's.
func TestRunSendsTheVerbThroughTheSystemSSHClient(t *testing.T) {
	rec := stubSSH(t)
	acct := filepath.Join(t.TempDir(), "acct")
	p, err := NewProvider(validCore(t), &config.Effective{Test: &config.Test{AccountingDir: acct}})
	if err != nil {
		t.Fatal(err)
	}
	res, out, err := p.output(context.Background(), "probe", strings.NewReader("payload"))
	if err != nil || res.Code != 0 || strings.TrimSpace(out) != `{"gate_version":1}` {
		t.Fatalf("res %+v out %q err %v", res, out, err)
	}
	args, _ := os.ReadFile(rec + ".args")
	got := strings.Split(strings.TrimSpace(string(args)), "\n")
	if got[len(got)-1] != "probe" || got[len(got)-2] != "127.0.0.1" || !strings.Contains(string(args), "StrictHostKeyChecking=yes\n") ||
		!strings.Contains(string(args), "IdentityAgent=none\n") {
		t.Fatalf("argv %q", got)
	}
	if b, _ := os.ReadFile(rec + ".stdin"); string(b) != "payload" {
		t.Fatalf("stdin %q", b)
	}
	if b, _ := os.ReadFile(rec + ".sock"); string(b) != "unset" {
		t.Fatalf("the caller's agent socket reached ssh: %q", b)
	}
	kh, err := os.ReadFile(filepath.Join(acct, "ssh", "known_hosts"))
	if err != nil || strings.Count(string(kh), "\n") != 1 || !strings.HasPrefix(string(kh), "bitc-ssh ") {
		t.Fatalf("known_hosts %q %v", kh, err)
	}

	if err := os.WriteFile(rec+".code", []byte("255"), 0o600); err != nil {
		t.Fatal(err)
	}
	res, _, err = p.output(context.Background(), "probe", nil)
	if err != nil || res.Code != 255 || !strings.Contains(res.Stderr, "connect to host") {
		t.Fatalf("a failing ssh is a result, not an error: %+v %v", res, err)
	}
}

// With no ssh client on PATH the provider is unavailable on this machine (not a host failure).
func TestRunWithoutAnSSHClientIsUnavailableLocally(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	p, err := NewProvider(validCore(t), &config.Effective{Test: &config.Test{AccountingDir: filepath.Join(t.TempDir(), "acct")}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.run(context.Background(), "probe", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "ssh client is not on PATH") {
		t.Fatalf("got %v", err)
	}
}
