package agent

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
)

// mountRec is a mount function that notes the key dir's state at mount and unmount time.
type mountRec struct {
	dir          string
	keyAtMount   bool // a key file existed when the tmpfs was mounted (it must not)
	keyAtUnmount bool // the key file still existed when it was unmounted (it must not)
	mounted      bool
	unmounted    bool
	fail         error
	saw          atomic.Bool // the key file was seen on the mounted dir at some point
}

func (m *mountRec) mount(dir string) (func() error, error) {
	if m.fail != nil {
		return nil, m.fail
	}
	m.dir, m.mounted = dir, true
	_, err := os.Stat(filepath.Join(dir, "key"))
	m.keyAtMount = err == nil
	stop := make(chan struct{})
	go func() { // the key is written between the mount and the fetch: poll until the unmount
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := os.Stat(filepath.Join(dir, "key")); err == nil {
				m.saw.Store(true)
			}
			runtime.Gosched()
		}
	}()
	return func() error {
		close(stop)
		m.unmounted = true
		_, err := os.Stat(filepath.Join(dir, "key"))
		m.keyAtUnmount = err == nil
		return nil
	}, nil
}

func sshSpec(t *testing.T) Spec {
	t.Helper()
	// a non-local clone URL takes the key path; the fetch itself fails (no network, no ssh
	// server), which is all these tests need: the key was written and must be gone afterwards
	return Spec{Job: "j1", CloneURL: "git@example.invalid:o/r.git", Ref: "refs/x", Sha: strings.Repeat("1", 40), Unprivileged: true,
		KnownHosts: "example.invalid ssh-ed25519 AAAA"}
}

func TestDeployKeyLivesOnlyOnTheMountedTmpfsAndIsGoneBeforeTheFetchReturns(t *testing.T) {
	m := &mountRec{}
	root := t.TempDir()
	keyDir := filepath.Join(root, "key")
	err := fetchCode(sshSpec(t), filepath.Join(root, "work"), keyDir, "SECRET-KEY-BYTES", m.mount)
	if err == nil {
		t.Fatal("the fetch cannot succeed here")
	}
	switch {
	case !m.mounted || m.dir != keyDir:
		t.Fatalf("the key dir was not mounted (mounted %v at %q)", m.mounted, m.dir)
	case m.keyAtMount:
		t.Fatal("the key existed before the tmpfs was mounted")
	case !m.saw.Load():
		t.Fatal("the key never appeared on the mounted dir")
	case !m.unmounted:
		t.Fatal("the tmpfs was not unmounted")
	case m.keyAtUnmount:
		t.Fatal("the key was still on the tmpfs when it was unmounted (shred comes first)")
	}
	if _, err := os.Stat(keyDir); err == nil {
		t.Fatal("the key dir is still there after the fetch")
	}
}

func TestDeployKeyMountFailureIsInfraAndNoKeyIsWritten(t *testing.T) {
	m := &mountRec{fail: errors.New("operation not permitted")}
	root := t.TempDir()
	keyDir := filepath.Join(root, "key")
	err := fetchCode(sshSpec(t), filepath.Join(root, "work"), keyDir, "SECRET-KEY-BYTES", m.mount)
	var se *stepError
	if !errors.As(err, &se) || se.Name != "key-mount" || se.Repo {
		t.Fatalf("want an infra key-mount error, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(keyDir, "key")); err == nil {
		t.Fatal("a key was written although the tmpfs could not be mounted")
	}
}
