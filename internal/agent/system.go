package agent

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// System is every root action, so tests can run the agent as a plain user.
type System interface {
	SetOOMScoreAdj() error
	InstallPackages(names []string) error
	EnsureJobUser() (uid, gid int, home string, err error)
	Chown(path string, uid, gid int) error
	// Cmd prepares argv to run as root (asRoot) or as the job user.
	Cmd(argv []string, asRoot bool, uid, gid int) *exec.Cmd
	// MountTmpfs mounts a fresh private tmpfs on dir (created 0700) and returns the unmount; the
	// deploy key of the exec delivery lives only there (sprites-provider.md 4, 6.4).
	MountTmpfs(dir string) (unmount func() error, err error)
}

// RealSystem runs as root in the pod.
type RealSystem struct{}

func (RealSystem) SetOOMScoreAdj() error {
	return os.WriteFile("/proc/self/oom_score_adj", []byte("-1000"), 0o600)
}

func (RealSystem) InstallPackages(names []string) error {
	if len(names) == 0 {
		return nil
	}
	env := []string{"PATH=" + agentPath, "DEBIAN_FRONTEND=noninteractive"}
	if _, ok := lookIn(agentPath, "apt-get"); ok {
		up := agentCmd("apt-get", "update", "-qq")
		up.Env = env
		if out, err := up.CombinedOutput(); err != nil {
			return fmt.Errorf("apt-get update: %w: %s", err, out)
		}
		c := agentCmd("apt-get", append([]string{"install", "-y", "-qq", "--no-install-recommends"}, names...)...)
		c.Env = env
		if out, err := c.CombinedOutput(); err != nil {
			return fmt.Errorf("apt-get install: %w: %s", err, out)
		}
		return nil
	}
	c := agentCmd("apk", append([]string{"add", "-q", "--no-cache"}, names...)...)
	c.Env = env
	if out, err := c.CombinedOutput(); err != nil {
		return fmt.Errorf("apk add: %w: %s", err, out)
	}
	return nil
}

const jobUID, jobGID = 10001, 10001

func (RealSystem) EnsureJobUser() (int, int, string, error) {
	home := "/home/bitcjob"
	if err := ensureDir(filepath.Dir(home), 0o755); err != nil { // /home: the job user traverses it
		return 0, 0, "", err
	}
	if err := ensureDir(home, 0o700); err != nil {
		return 0, 0, "", err
	}
	// no useradd dependency: an /etc/passwd line is enough for HOME and tools that ask; once, so
	// provisioning and the attach (both ensure the user) leave one line
	_ = ensurePasswdLine("/etc/passwd", "bitcjob", fmt.Sprintf("bitcjob:x:%d:%d:bitc job:%s:/bin/sh", jobUID, jobGID, home))
	return jobUID, jobGID, home, os.Chown(home, jobUID, jobGID)
}

func (RealSystem) MountTmpfs(dir string) (func() error, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := unix.Mount("tmpfs", dir, "tmpfs", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, "mode=0700,size=1m"); err != nil {
		return nil, err
	}
	return func() error { return unix.Unmount(dir, 0) }, nil
}

func (RealSystem) Chown(path string, uid, gid int) error {
	return agentCmd("chown", "-R", fmt.Sprintf("%d:%d", uid, gid), path).Run()
}

func (RealSystem) Cmd(argv []string, asRoot bool, uid, gid int) *exec.Cmd {
	c := exec.Command(argv[0], argv[1:]...)
	if !asRoot {
		c.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)}, Setpgid: true}
	} else {
		c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	}
	return c
}

// PlainSystem runs everything as the current user (tests and the RunPod fake; only with
// Spec.Unprivileged, which the host sets only when its [test] table is present).
type PlainSystem struct{}

func (PlainSystem) SetOOMScoreAdj() error { return nil }
func (PlainSystem) MountTmpfs(string) (func() error, error) {
	return func() error { return nil }, nil
}
func (PlainSystem) InstallPackages([]string) error { return nil }
func (PlainSystem) Chown(string, int, int) error   { return nil }
func (PlainSystem) EnsureJobUser() (int, int, string, error) {
	return os.Getuid(), os.Getgid(), os.Getenv("HOME"), nil
}
func (PlainSystem) Cmd(argv []string, _ bool, _, _ int) *exec.Cmd {
	c := exec.Command(argv[0], argv[1:]...)
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return c
}

// ensurePasswdLine appends line to the passwd file at path unless a line for name exists.
func ensurePasswdLine(path, name, line string) error {
	if b, err := os.ReadFile(path); err == nil {
		for _, l := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(l, name+":") {
				return nil
			}
		}
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = fmt.Fprintln(f, line)
	return err
}
