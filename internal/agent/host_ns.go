package agent

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Exploded-views/bitc/internal/exit"
	"golang.org/x/sys/unix"
)

// Job namespaces on an ssh host (providers delta C.5 step 6). One holder process per job lives
// in new mount, PID, IPC, UTS and (netns) network namespaces; every command of the job enters
// them with nsenter and runs `bitc __agent job-exec`, which joins the slot's cgroup, drops the
// capability bounding set, installs the job seccomp filter, drops to the slot user and execs.
// When the holder exits (its stdin closes, or it is killed) the kernel kills the PID namespace.

// JobNSConf describes one job's namespaces.
type JobNSConf struct {
	Slot     int
	Uid, Gid int
	Conf     HostConf
	ToolsDir string // a host path made visible read-only at the same path
	Self     string // the agent binary
	Probe    bool   // doctor's probe namespace: slot 0, no cgroup, no slot dirs
}

// JobNS is a running holder.
type JobNS struct {
	c   JobNSConf
	cmd *exec.Cmd
	in  io.WriteCloser
	pid int
}

func ipArgs(args ...string) *exec.Cmd { return agentCmd("ip", args...) }

func run(c *exec.Cmd) error {
	out, err := c.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w: %s", strings.Join(c.Args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func writeFirst(path, v string) error { return os.WriteFile(path, []byte(v), 0o644) } //nolint:gosec // a procfs or cgroup control file

// delegateCgroup creates the bitc subtree and turns memory and pids on for it. It belongs to the
// job path (slot-boot runs it before its host check); check-host itself only reads.
func delegateCgroup() error {
	if err := os.MkdirAll(filepath.Join(cgRoot, "bitc"), 0o755); err != nil { //nolint:gosec // a cgroup directory
		return err
	}
	_ = writeFirst(filepath.Join(cgRoot, "cgroup.subtree_control"), "+memory +pids") // may already be on
	_ = writeFirst(filepath.Join(cgRoot, "bitc", "cgroup.subtree_control"), "+memory +pids")
	return nil
}

// setupCgroup creates the slot's cgroup under the delegated bitc subtree.
func setupCgroup(c JobNSConf) error {
	if err := delegateCgroup(); err != nil {
		return err
	}
	cg := cgroupSlot(c.Slot)
	if err := os.MkdirAll(cg, 0o755); err != nil { //nolint:gosec // a cgroup directory
		return err
	}
	if err := writeFirst(filepath.Join(cg, "pids.max"), "4096"); err != nil {
		return err
	}
	if c.Conf.MemoryPerSlot > 0 {
		if err := writeFirst(filepath.Join(cg, "memory.max"), strconv.FormatInt(c.Conf.MemoryPerSlot, 10)); err != nil {
			return err
		}
	}
	return nil
}

// StartJobNS creates the cgroup, starts the holder, joins it to the cgroup and wires the network.
func StartJobNS(c JobNSConf) (*JobNS, error) {
	if c.Self == "" {
		c.Self, _ = os.Readlink("/proc/self/exe")
	}
	if !c.Probe {
		if err := setupCgroup(c); err != nil {
			return nil, fmt.Errorf("cgroup: %w", err)
		}
	}
	resolv := slotPath(c.Slot, "resolv.conf")
	if c.Probe {
		resolv = "/run/bitc-probe-resolv.conf"
	}
	if err := os.WriteFile(resolv, []byte("nameserver "+c.Conf.Resolver+"\n"), 0o644); err != nil { //nolint:gosec // the job must be able to read its resolv.conf
		return nil, err
	}
	args := []string{"__agent", "ns-hold", "--slot", strconv.Itoa(c.Slot), "--resolv", resolv}
	if c.Probe {
		args = append(args, "--probe")
	}
	if c.ToolsDir != "" {
		args = append(args, "--tools", c.ToolsDir)
	}
	cmd := exec.Command(c.Self, args...)
	flags := uintptr(unix.CLONE_NEWNS | unix.CLONE_NEWPID | unix.CLONE_NEWIPC | unix.CLONE_NEWUTS)
	if c.Conf.Netns {
		flags |= unix.CLONE_NEWNET
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Cloneflags: flags, Setpgid: true}
	cmd.Env = []string{"PATH=" + agentPath}
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("namespace holder: %w", err)
	}
	n := &JobNS{c: c, cmd: cmd, in: in, pid: cmd.Process.Pid}
	fail := func(err error) (*JobNS, error) { n.Close(); return nil, err }
	if !c.Probe {
		if err := writeFirst(filepath.Join(cgroupSlot(c.Slot), "cgroup.procs"), strconv.Itoa(n.pid)); err != nil {
			return fail(fmt.Errorf("cgroup: %w", err))
		}
	}
	if c.Slot > 0 {
		_ = os.WriteFile(slotPath(c.Slot, "holder.pid"), []byte(identity(n.pid)), 0o600)
	}
	if c.Conf.Netns {
		if err := n.wireNet(); err != nil {
			return fail(fmt.Errorf("network: %w", err))
		}
	}
	ready := make(chan string, 1)
	exit.Go(func() {
		line, _ := bufio.NewReader(out).ReadString('\n')
		ready <- strings.TrimSpace(line)
	})
	select {
	case l := <-ready:
		if l != "ready" {
			return fail(fmt.Errorf("namespace holder: %s", l))
		}
	case <-time.After(30 * time.Second):
		return fail(fmt.Errorf("namespace holder: not ready after 30s"))
	}
	return n, nil
}

// wireNet is the veth pair bv<slot> (host side 10.203.<slot>.1/30) and eth0 in the job's
// namespace (10.203.<slot>.2/30, default route via .1).
func (n *JobNS) wireNet() error {
	i := strconv.Itoa(n.c.Slot)
	host, job := "10.203."+i+".1/30", "10.203."+i+".2/30"
	_ = ipArgs("link", "del", "bv"+i).Run() // a stale one
	steps := []*exec.Cmd{
		ipArgs("link", "add", "bv"+i, "type", "veth", "peer", "name", "eth0", "netns", strconv.Itoa(n.pid)),
		ipArgs("addr", "add", host, "dev", "bv"+i),
		ipArgs("link", "set", "bv"+i, "up"),
	}
	inside := func(a ...string) *exec.Cmd {
		return agentCmd("nsenter", append([]string{"--target", strconv.Itoa(n.pid), "--net", "--", "/usr/bin/env", "-i", "ip"}, a...)...)
	}
	steps = append(steps,
		inside("addr", "add", job, "dev", "eth0"),
		inside("link", "set", "eth0", "up"),
		inside("link", "set", "lo", "up"),
		inside("route", "add", "default", "via", "10.203."+i+".1"))
	for _, s := range steps {
		if err := run(s); err != nil {
			return err
		}
	}
	return nil
}

// Close ends the holder, which ends the namespaces and every process in them.
func (n *JobNS) Close() {
	if n == nil || n.cmd == nil {
		return
	}
	_ = n.in.Close()
	done := make(chan struct{})
	exit.Go(func() { _ = n.cmd.Wait(); close(done) })
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		_ = syscall.Kill(n.pid, syscall.SIGKILL)
		<-done
	}
	if n.c.Slot > 0 {
		_ = os.Remove(slotPath(n.c.Slot, "holder.pid"))
	}
	if n.c.Conf.Netns {
		_ = ipArgs("link", "del", "bv"+strconv.Itoa(n.c.Slot)).Run()
	}
}

// Wrap rewrites c (a command prepared for the job: Path, Args, Env, Dir) into the nsenter and
// job-exec form. The job's environment travels on an inherited memfd, never in the argv or in
// the env of the root processes in between.
func (n *JobNS) Wrap(c *exec.Cmd) error {
	envJSON, _ := json.Marshal(c.Env)
	fd, err := unix.MemfdCreate("bitc-job-env", unix.MFD_CLOEXEC)
	if err != nil {
		return err
	}
	if _, err := unix.Write(fd, envJSON); err != nil {
		unix.Close(fd)
		return err
	}
	if _, err := unix.Seek(fd, 0, 0); err != nil {
		unix.Close(fd)
		return err
	}
	f := os.NewFile(uintptr(fd), "job-env")
	nsenter, ok := lookIn(agentPath, "nsenter")
	if !ok {
		return fmt.Errorf("nsenter not found (util-linux)")
	}
	args := []string{"nsenter", "--target", strconv.Itoa(n.pid), "--mount", "--pid", "--ipc", "--uts"}
	if n.c.Conf.Netns {
		args = append(args, "--net")
	}
	cg := ""
	if !n.c.Probe {
		cg = cgroupSlot(n.c.Slot)
	}
	args = append(args, "--", n.c.Self, "__agent", "job-exec", "--uid", strconv.Itoa(n.c.Uid), "--gid", strconv.Itoa(n.c.Gid),
		"--cgroup", cg, "--cwd", c.Dir, "--prog", c.Path, "--")
	args = append(args, c.Args...)
	c.Path, c.Args, c.Env, c.Dir = nsenter, args, []string{"PATH=" + agentPath}, ""
	c.ExtraFiles = append([]*os.File{f}, c.ExtraFiles...)
	return nil
}

// ---- the holder (`__agent ns-hold`), PID 1 of the job's namespaces

func nsHold(args []string, stdin io.Reader, stdout io.Writer) int {
	slot, tools, resolv, probe := 0, "", "", false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--probe":
			probe = true
		case "--slot", "--tools", "--resolv":
			if i+1 >= len(args) {
				return 64
			}
			switch args[i] {
			case "--slot":
				slot, _ = strconv.Atoi(args[i+1])
			case "--tools":
				tools = args[i+1]
			default:
				resolv = args[i+1]
			}
			i++
		}
	}
	fail := func(err error) int { fmt.Fprintln(stdout, "error: "+err.Error()); return 69 }
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		return fail(fmt.Errorf("make private: %w", err))
	}
	_ = unix.Sethostname([]byte("bitc-job"))
	if err := holderMounts(slot, tools, resolv, probe); err != nil {
		return fail(err)
	}
	// as PID 1 the holder reaps every orphan of the job
	ch := make(chan os.Signal, 16)
	signal.Notify(ch, syscall.SIGCHLD)
	exit.Go(func() {
		for range ch {
			for {
				if pid, _ := syscall.Wait4(-1, nil, syscall.WNOHANG, nil); pid <= 0 {
					break
				}
			}
		}
	})
	fmt.Fprintln(stdout, "ready")
	_, _ = io.Copy(io.Discard, stdin) // returns when the agent closes it or dies
	return 0
}

// resolvTarget is the file /etc/resolv.conf really is (it is often a link into /run).
func resolvTarget() string {
	if r, err := filepath.EvalSymlinks("/etc/resolv.conf"); err == nil {
		return r
	}
	if t, err := os.Readlink("/etc/resolv.conf"); err == nil { // dangling: bind over the link's target
		if !filepath.IsAbs(t) {
			t = filepath.Join("/etc", t)
		}
		return t
	}
	return "/etc/resolv.conf"
}

func bindMount(src, dst string, readOnly bool) error {
	if fi, err := os.Stat(src); err == nil && !fi.IsDir() {
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil { //nolint:gosec // a mount point inside the job namespace
			return err
		}
		if _, err := os.Stat(dst); err != nil {
			f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY, 0o644) //nolint:gosec // a bind-mount target inside the job namespace
			if err != nil {
				return err
			}
			f.Close()
		}
	} else if err := os.MkdirAll(dst, 0o755); err != nil { //nolint:gosec // a mount point inside the job namespace
		return err
	}
	if err := unix.Mount(src, dst, "", unix.MS_BIND, ""); err != nil {
		return fmt.Errorf("bind %s on %s: %w", src, dst, err)
	}
	if readOnly {
		if err := unix.Mount("", dst, "", unix.MS_BIND|unix.MS_REMOUNT|unix.MS_RDONLY|unix.MS_NOSUID|unix.MS_NODEV, ""); err != nil {
			return fmt.Errorf("remount %s read-only: %w", dst, err)
		}
	}
	return nil
}

// holderMounts builds the job's view: its own /tmp, /var/tmp, /run/lock, /dev/shm, /dev/mqueue
// and /proc, one nameserver line in /etc/resolv.conf, and a /var/lib/bitc that holds nothing
// but the slot's work and home (and the shared tools, read-only) at their usual paths.
func holderMounts(slot int, tools, resolvSrc string, probe bool) error {
	sd := slotPath(slot)
	if probe { // doctor's namespace: only a private /proc and the resolver
		if err := bindMount(resolvSrc, resolvTarget(), true); err != nil {
			return err
		}
		return unix.Mount("proc", "/proc", "proc", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, "")
	}
	for src, dst := range map[string]string{sd + "/tmp": "/tmp", sd + "/vartmp": "/var/tmp", sd + "/runlock": "/run/lock"} {
		if err := bindMount(src, dst, false); err != nil {
			return err
		}
	}
	if err := bindMount(resolvSrc, resolvTarget(), true); err != nil {
		return err
	}
	stage := "/run/bitc-stage-" + strconv.Itoa(slot)
	if err := os.MkdirAll(stage, 0o700); err != nil {
		return err
	}
	moves := map[string]string{"work": sd + "/work", "home": sd + "/home"}
	if err := bindMount(sd+"/work", stage+"/work", false); err != nil {
		return err
	}
	if err := bindMount(sd+"/home", stage+"/home", false); err != nil {
		return err
	}
	if tools != "" {
		if err := bindMount(tools, stage+"/tools", true); err != nil {
			return err
		}
		moves["tools"] = tools
	}
	if err := unix.Mount("tmpfs", stateDir, "tmpfs", unix.MS_NOSUID|unix.MS_NODEV, "mode=0711,size=1m"); err != nil {
		return fmt.Errorf("hide %s: %w", stateDir, err)
	}
	for from, to := range moves {
		if err := os.MkdirAll(to, 0o711); err != nil { //nolint:gosec // a mount point inside the job namespace
			return err
		}
		if err := unix.Mount(stage+"/"+from, to, "", unix.MS_MOVE, ""); err != nil {
			return fmt.Errorf("move %s: %w", to, err)
		}
	}
	if err := unix.Mount("tmpfs", "/dev/shm", "tmpfs", unix.MS_NOSUID|unix.MS_NODEV, "mode=1777"); err != nil {
		return fmt.Errorf("/dev/shm: %w", err)
	}
	if fi, err := os.Stat("/dev/mqueue"); err == nil && fi.IsDir() {
		if err := unix.Mount("mqueue", "/dev/mqueue", "mqueue", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, ""); err != nil {
			return fmt.Errorf("/dev/mqueue: %w", err)
		}
	}
	if err := unix.Mount("proc", "/proc", "proc", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, ""); err != nil {
		return fmt.Errorf("/proc: %w", err)
	}
	return nil
}

// ---- job-exec: the last root step before the job

func jobExec(args []string) int {
	var uid, gid int
	var cgroup, cwd, prog string
	i := 0
	for ; i < len(args) && args[i] != "--"; i += 2 {
		if i+1 >= len(args) {
			return 64
		}
		switch args[i] {
		case "--uid":
			uid, _ = strconv.Atoi(args[i+1])
		case "--gid":
			gid, _ = strconv.Atoi(args[i+1])
		case "--cgroup":
			cgroup = args[i+1]
		case "--cwd":
			cwd = args[i+1]
		case "--prog":
			prog = args[i+1]
		}
	}
	argv := args[min(i+1, len(args)):]
	if uid < 1 || gid < 1 || prog == "" || len(argv) == 0 {
		fmt.Fprintln(os.Stderr, "bitc agent: job-exec: bad arguments")
		return 64
	}
	f := os.NewFile(3, "job-env")
	var env []string
	b, _ := io.ReadAll(f)
	f.Close()
	if json.Unmarshal(b, &env) != nil {
		fmt.Fprintln(os.Stderr, "bitc agent: job-exec: no environment")
		return 69
	}
	die := func(what string, err error) int {
		fmt.Fprintf(os.Stderr, "bitc agent: job-exec: %s: %v\n", what, err)
		return 69
	}
	if cgroup != "" {
		if err := writeFirst(filepath.Join(cgroup, "cgroup.procs"), "0"); err != nil {
			return die("cgroup", err)
		}
	}
	if cwd != "" {
		if err := os.Chdir(cwd); err != nil {
			return die("chdir", err)
		}
	}
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return die("no_new_privs", err)
	}
	last := 40
	if b, err := os.ReadFile("/proc/sys/kernel/cap_last_cap"); err == nil {
		if n, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
			last = n
		}
	}
	for c := 0; c <= last; c++ {
		if err := unix.Prctl(unix.PR_CAPBSET_DROP, uintptr(c), 0, 0, 0); err != nil && err != unix.EINVAL {
			return die("capability bounding set", err)
		}
	}
	if err := InstallJobSeccomp(); err != nil {
		return die("seccomp", err)
	}
	if err := syscall.Setgroups([]int{}); err != nil {
		return die("setgroups", err)
	}
	if err := syscall.Setgid(gid); err != nil {
		return die("setgid", err)
	}
	if err := syscall.Setuid(uid); err != nil {
		return die("setuid", err)
	}
	err := syscall.Exec(prog, argv, env)
	fmt.Fprintf(os.Stderr, "bitc agent: cannot start %s: %v\n", argv[0], err)
	return 127
}
