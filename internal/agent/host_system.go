package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
)

// SlotSystem is the System of an ssh host's job (providers delta C.5): the job runs as the
// slot's user, inside the job namespaces, and nothing is installed (packages are only checked).
// The namespaces start with the first command, so provisioning and the key fetch run outside.
type SlotSystem struct {
	Conf     HostConf
	Slots    []int
	Self     string
	ToolsDir string
	Sys      RealSlotSys

	mu  sync.Mutex
	ns  *JobNS
	err error
}

func (s *SlotSystem) first() int { return s.Slots[0] }

func (s *SlotSystem) SetOOMScoreAdj() error { return RealSystem{}.SetOOMScoreAdj() }

func (s *SlotSystem) MountTmpfs(dir string) (func() error, error) {
	return RealSystem{}.MountTmpfs(dir)
}

// InstallPackages never installs: a package that is not there is the host's gap (infra).
func (s *SlotSystem) InstallPackages(names []string) error {
	for _, n := range names {
		out, err := agentCmd("dpkg-query", "-W", "-f=${Status}", n).Output()
		if err != nil || !strings.Contains(string(out), "install ok installed") {
			return fmt.Errorf("package %s not installed on this host", n)
		}
	}
	return nil
}

func (s *SlotSystem) EnsureJobUser() (int, int, string, error) {
	uid, gid, err := s.Sys.UID(s.first())
	if err != nil {
		return 0, 0, "", err
	}
	return uid, gid, slotPath(s.first(), "home"), nil
}

// Chown changes ownership of the whole tree at path (links themselves, never their targets).
func (s *SlotSystem) Chown(path string, uid, gid int) error {
	return filepath.WalkDir(path, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return os.Lchown(p, uid, gid) //nolint:gosec // Lchown never follows a link
	})
}

// Cmd prepares argv for the job user; the namespace wrapping happens in FinishCmd, when the
// caller has set Dir and Env. A root command is refused: ssh hosts run no root steps.
func (s *SlotSystem) Cmd(argv []string, asRoot bool, uid, gid int) *exec.Cmd {
	c := exec.Command(argv[0], argv[1:]...)
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if asRoot {
		c.Err = errors.New("root steps are not run on ssh hosts")
	}
	return c
}

// FinishCmd starts the job's namespaces on first use and rewrites c to run inside them.
func (s *SlotSystem) FinishCmd(c *exec.Cmd) {
	if c.Err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ns == nil && s.err == nil {
		uid, gid, err := s.Sys.UID(s.first())
		if err == nil {
			s.ns, err = StartJobNS(JobNSConf{Slot: s.first(), Uid: uid, Gid: gid, Conf: s.Conf, ToolsDir: s.ToolsDir, Self: s.Self})
		}
		s.err = err
	}
	if s.err != nil {
		c.Err = fmt.Errorf("job namespaces: %w", s.err)
		return
	}
	if err := s.ns.Wrap(c); err != nil {
		c.Err = err
	}
}

// Reap kills what the job left behind: the cgroup and every process of the slot user.
func (s *SlotSystem) Reap() {
	uid, _, err := s.Sys.UID(s.first())
	if err == nil {
		_ = s.Sys.KillSlot(s.first(), uid)
	}
}

// Close ends the namespaces.
func (s *SlotSystem) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ns.Close()
	s.ns = nil
}

// toolsetDir is the shared, read-only tools dir for exactly this set of tools.
func toolsetDir(tools []Tool) string {
	if len(tools) == 0 {
		return ""
	}
	b, _ := json.Marshal(tools)
	sum := sha256.Sum256(b)
	return filepath.Join(stateDir, "tools", hex.EncodeToString(sum[:])[:16])
}
