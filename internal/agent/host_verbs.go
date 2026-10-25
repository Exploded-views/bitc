package agent

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/user"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/Exploded-views/bitc/internal/exit"
	"golang.org/x/sys/unix"
)

// The ssh host's agent verbs (providers delta C.5, C.8), reached through the gate:
//
//	check-host [--doctor]                           C.8; exit 69 on any fail line
//	slot-boot --prepare <slots> <name> <nonce> [tag=<hex>] [lease=<secs>] [payload]
//	slot-boot --run <slots> <name> <nonce>          boot payload on stdin; prints `started <name>`
//	slot-teardown <slots> <name>                    prints `torn` or `gone`
//	slot-list                                       one JSON line per slot
//	firewall-selftest [--router IP] --controller IP [--allow H:P]... [--allow-test-targets]   root, from the setup script (host_selftest.go)
//
// and the helpers the agent starts itself: slot-stage2, slot-janitor, ns-hold, job-exec,
// probe-seccomp and probe-net.

var (
	nonceRE    = regexp.MustCompile(`^[0-9a-f]{32}$`)
	tagRE      = regexp.MustCompile(`^[0-9a-f]{1,64}$`)
	slotNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
)

// ParseSlots parses "1" or "1+2+3" (ascending, no repeats, each 1..64).
func ParseSlots(s string) ([]int, bool) {
	var out []int
	for _, f := range strings.Split(s, "+") {
		n, err := strconv.Atoi(f)
		if err != nil || n < 1 || n > 64 || strconv.Itoa(n) != f || (len(out) > 0 && n <= out[len(out)-1]) {
			return nil, false
		}
		out = append(out, n)
	}
	return out, len(out) > 0
}

func loadHostConf() (HostConf, error) {
	b, err := os.ReadFile("/etc/bitc/host.conf")
	if err != nil {
		return HostConf{}, err
	}
	return ParseHostConf(b)
}

func realSlotHost(conf HostConf, log io.Writer) *SlotHost {
	return &SlotHost{Conf: conf, Sys: RealSlotSys{Log: log}, Log: log}
}

// hostVerb runs the ssh-host verbs; ok is false when args name none of them.
func hostVerb(args []string, stdin io.Reader, stdout io.Writer) (code int, ok bool) {
	switch args[0] {
	case "check-host":
		return checkHostVerb(args[1:], stdout), true
	case "slot-boot":
		return slotBootVerb(args[1:], stdin, stdout), true
	case "slot-teardown":
		return slotTeardownVerb(args[1:], stdout), true
	case "slot-list":
		return slotListVerb(stdout), true
	case "slot-stage2":
		return slotStage2(args[1:], stdout), true
	case "slot-janitor":
		return slotJanitor(args[1:]), true
	case "ns-hold":
		return nsHold(args[1:], stdin, stdout), true
	case "job-exec":
		return jobExec(args[1:]), true
	case "probe-seccomp":
		return probeSeccomp(args[1:], stdout), true
	case "probe-net":
		return probeNet(args[1:], stdout), true
	case "firewall-selftest":
		return selfTestVerb(args[1:], stdout), true
	}
	return 0, false
}

func checkHostVerb(args []string, stdout io.Writer) int {
	doctor, noFilter := false, false
	for _, a := range args {
		switch a {
		case "--doctor":
			doctor = true
		case "--test-no-seccomp-filter": // tests only; the gate's grammar never lets it through
			noFilter = true
		default:
			fmt.Fprintln(os.Stderr, "bitc agent: check-host: unknown argument")
			return 64
		}
	}
	conf, err := loadHostConf()
	if err != nil {
		fmt.Fprintln(stdout, "fail ssh-users: "+err.Error())
		return 69
	}
	env := NewHostEnv(conf, doctor)
	if noFilter {
		env.SeccompProbe = seccompProbe(true, env.selfExe())
	}
	bad := false
	for _, f := range CheckHost(env) {
		fmt.Fprintln(stdout, f.String())
		bad = bad || f.Level == "fail"
	}
	if bad {
		return 69
	}
	return 0
}

// refuse prints a refusal on stdout (stderr may not reach the controller) and exits 65.
func refuse(stdout io.Writer, why string) int {
	fmt.Fprintln(stdout, "refused: "+why)
	return 65
}

func slotListVerb(stdout io.Writer) int {
	conf, err := loadHostConf()
	if err != nil {
		fmt.Fprintln(os.Stderr, "bitc agent:", err)
		return 69
	}
	owners, err := realSlotHost(conf, nil).Owners()
	if err != nil {
		fmt.Fprintln(os.Stderr, "bitc agent:", err)
		return 69
	}
	now := time.Now().Unix()
	enc := json.NewEncoder(stdout)
	for i := 1; i <= conf.Slots; i++ {
		line := struct {
			Slot int `json:"slot"`
			*SlotOwner
			Busy bool `json:"busy"`
		}{Slot: i, SlotOwner: owners[i]}
		line.Busy = owners[i] != nil && owners[i].LeaseUntil > now
		if err := enc.Encode(line); err != nil {
			return 69
		}
	}
	return 0
}

func slotTeardownVerb(args []string, stdout io.Writer) int {
	if len(args) != 2 {
		fmt.Fprintln(os.Stderr, "bitc agent: usage: slot-teardown <slots> <name>")
		return 64
	}
	slots, ok := ParseSlots(args[0])
	if !ok || !slotNameRE.MatchString(args[1]) {
		fmt.Fprintln(os.Stderr, "bitc agent: bad slots or name")
		return 64
	}
	conf, err := loadHostConf()
	if err != nil {
		fmt.Fprintln(os.Stderr, "bitc agent:", err)
		return 69
	}
	got, err := realSlotHost(conf, os.Stderr).Teardown(slots, args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, "bitc agent:", err)
		return 69
	}
	fmt.Fprintln(stdout, got)
	return 0
}

type bootArgs struct {
	slots       []int
	name, nonce string
	tag         string
	lease       time.Duration
	payload     bool
}

func parseBootArgs(args []string) (bootArgs, bool) {
	b := bootArgs{lease: 10 * time.Minute}
	if len(args) < 3 {
		return b, false
	}
	var ok bool
	if b.slots, ok = ParseSlots(args[0]); !ok || !slotNameRE.MatchString(args[1]) || !nonceRE.MatchString(args[2]) {
		return b, false
	}
	b.name, b.nonce = args[1], args[2]
	for _, a := range args[3:] {
		switch {
		case a == "payload":
			b.payload = true
		case strings.HasPrefix(a, "tag=") && tagRE.MatchString(a[4:]):
			b.tag = a[4:]
		case strings.HasPrefix(a, "lease="):
			n, err := strconv.Atoi(a[6:])
			if err != nil || n < 1 || n > 3600 {
				return b, false
			}
			b.lease = time.Duration(n) * time.Second
		default:
			return b, false
		}
	}
	return b, true
}

func slotBootVerb(args []string, stdin io.Reader, stdout io.Writer) int {
	if len(args) == 0 || (args[0] != "--prepare" && args[0] != "--run") {
		fmt.Fprintln(os.Stderr, "bitc agent: usage: slot-boot --prepare|--run <slots> <name> <nonce>")
		return 64
	}
	b, ok := parseBootArgs(args[1:])
	if !ok {
		fmt.Fprintln(os.Stderr, "bitc agent: slot-boot: bad arguments")
		return 64
	}
	if code, refused := selfTestGate("", stdout); refused {
		return code
	}
	conf, err := loadHostConf()
	if err != nil {
		fmt.Fprintln(os.Stderr, "bitc agent:", err)
		return 69
	}
	if args[0] == "--prepare" {
		return slotPrepare(conf, b, stdin, stdout)
	}
	return slotRun(conf, b, stdin, stdout)
}

func slotPrepare(conf HostConf, b bootArgs, stdin io.Reader, stdout io.Writer) int {
	h := realSlotHost(conf, os.Stderr)
	if err := h.Prepare(b.slots, b.name, b.nonce, b.tag, b.lease); err != nil {
		var busy *BusyError
		if errors.As(err, &busy) {
			return refuse(stdout, busy.Error())
		}
		fmt.Fprintln(os.Stderr, "bitc agent:", err)
		return 69
	}
	if b.payload { // outside the mutex: the repo's packages are checked and its tools fetched
		ps, err := ReadProvisionSpec(stdin)
		if err != nil {
			return refuse(stdout, err.Error())
		}
		sys := &SlotSystem{Conf: conf, Slots: b.slots}
		code := Provision(ProvisionOptions{Stdout: stdout, System: sys, ToolsRoot: toolsetDir(ps.Tools), StateRoot: slotPath(b.slots[0])}, ps)
		if code != 0 {
			return code
		}
	}
	fmt.Fprintln(stdout, "prepared "+b.name)
	return 0
}

// readPayload reads the boot payload frame, giving up when the controller stalls.
func readPayload(stdin io.Reader) (BootPayload, error) {
	type res struct {
		p   BootPayload
		err error
	}
	ch := make(chan res, 1)
	exit.Go(func() { p, err := ReadBootPayload(stdin); ch <- res{p, err} })
	select {
	case r := <-ch:
		return r.p, r.err
	case <-time.After(30 * time.Second):
		return BootPayload{}, ErrTruncated
	}
}

func slotRun(conf HostConf, b bootArgs, stdin io.Reader, stdout io.Writer) int {
	return RunSlot(SlotRunEnv{Host: realSlotHost(conf, os.Stderr)}, b.slots, b.name, b.nonce, stdin, stdout)
}

// SlotRunEnv is what `slot-boot --run` works against; the zero Upload and a nil Start are the
// real host, tests inject the rest.
type SlotRunEnv struct {
	Host   *SlotHost
	Upload UploadEnv
	// Start launches stage 2 with the payload (without the key) and the key; nil starts the real one.
	Start func(p BootPayload, key string) error
}

// RunSlot is `slot-boot --run` against env: the handoff, then (for an upload) the bundle, then
// stage 2.
func RunSlot(env SlotRunEnv, slots []int, name, nonce string, stdin io.Reader, stdout io.Writer) int {
	h := env.Host
	var p BootPayload
	err := h.Handoff(slots, name, nonce, func() (string, time.Time, error) {
		var err error
		if p, err = readPayload(stdin); err != nil {
			return "", time.Time{}, err
		}
		spec, err := DecodeSpec(p.Spec)
		if err != nil {
			return "", time.Time{}, errors.New("unusable spec")
		}
		switch {
		case spec.Port != -1:
			return "", time.Time{}, errors.New("--run needs port -1 in the spec")
		case spec.RootSteps || len(spec.Services) > 0:
			return "", time.Time{}, errors.New("root steps and services are not run on ssh hosts")
		case !jobIDRE.MatchString(spec.Job):
			return "", time.Time{}, errors.New("bad job id")
		case p.Deadline <= h.now().Unix():
			return "", time.Time{}, errors.New("deadline is past")
		}
		if p.Code != nil {
			if err := validateCode(p.Code, spec.Job, h.Conf.BundleCap()); err != nil {
				return "", time.Time{}, err
			}
		}
		return spec.Job, time.Unix(p.Deadline, 0), nil
	})
	var nl *NotLeasedError
	switch {
	case errors.As(err, &nl):
		return refuse(stdout, "slot "+strconv.Itoa(nl.Slot)+" not leased to "+nl.Name)
	case errors.Is(err, ErrTruncated):
		return refuse(stdout, "truncated payload")
	case err != nil:
		var be *BusyError
		if errors.As(err, &be) || strings.HasPrefix(err.Error(), "slot ") {
			fmt.Fprintln(os.Stderr, "bitc agent:", err)
			return 69
		}
		return refuse(stdout, err.Error())
	}
	if p.Code != nil {
		if err := h.ReceiveUpload(env.Upload, slots, *p.Code, time.Unix(p.Deadline, 0), stdin); err != nil {
			var ref uploadRefusal
			if errors.As(err, &ref) {
				return refuse(stdout, ref.Error())
			}
			fmt.Fprintln(os.Stderr, "bitc agent:", err)
			return 69
		}
		started := false
		defer func() {
			if !started { // stage 2 never took over: nothing may keep the bundle
				_ = h.removeUpload(slots[0])
			}
		}()
		if code := startStage2(env, slots, name, nonce, p); code != 0 {
			return code
		}
		started = true
		fmt.Fprintln(stdout, "started "+name)
		return 0
	}
	if code := startStage2(env, slots, name, nonce, p); code != 0 {
		return code
	}
	fmt.Fprintln(stdout, "started "+name)
	return 0
}

// startStage2 hands the payload (key aside) to the detached stage 2 and returns 0, or 69.
func startStage2(env SlotRunEnv, slots []int, name, nonce string, p BootPayload) int {
	rest, key := p.WithoutKey()
	if env.Start != nil {
		if err := env.Start(rest, key); err != nil {
			fmt.Fprintln(os.Stderr, "bitc agent:", err)
			return 69
		}
		return 0
	}
	body, _ := json.Marshal(rest)
	f3, err := memfdFile(body)
	if err != nil {
		fmt.Fprintln(os.Stderr, "bitc agent:", err)
		return 69
	}
	f5, err := memfdFile([]byte(key))
	if err != nil {
		fmt.Fprintln(os.Stderr, "bitc agent:", err)
		return 69
	}
	self, _ := os.Readlink("/proc/self/exe")
	c := exec.Command(self, "__agent", "slot-stage2", slotsArg(slots), name, nonce)
	c.Env = []string{}
	c.ExtraFiles = []*os.File{f3, nil, f5}
	c.Stderr = os.Stderr // the gate points it at the slot's agent.log
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := c.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "bitc agent:", err)
		return 69
	}
	_ = c.Process.Release()
	return 0
}

func slotsArg(slots []int) string {
	var s []string
	for _, i := range slots {
		s = append(s, strconv.Itoa(i))
	}
	return strings.Join(s, "+")
}

// memfdFile is a memfd holding content, positioned at 0, as an *os.File.
func memfdFile(content []byte) (*os.File, error) {
	fd, err := unix.MemfdCreate("bitc-boot", unix.MFD_CLOEXEC)
	if err != nil {
		return nil, err
	}
	if len(content) > 0 {
		if _, err := unix.Write(fd, content); err != nil {
			unix.Close(fd)
			return nil, err
		}
	}
	if _, err := unix.Seek(fd, 0, 0); err != nil {
		unix.Close(fd)
		return nil, err
	}
	return os.NewFile(uintptr(fd), "boot"), nil
}

// slotStage2 is the detached agent of an ssh job: payload on fd 3 (no key), deploy key on fd 5.
func slotStage2(args []string, stdout io.Writer) int {
	if len(args) != 3 {
		return 64
	}
	slots, ok := ParseSlots(args[0])
	if !ok {
		return 64
	}
	f := os.NewFile(3, "boot")
	b, _ := io.ReadAll(f)
	f.Close()
	kf := os.NewFile(5, "key")
	key, _ := io.ReadAll(kf)
	kf.Close()
	var p BootPayload
	if json.Unmarshal(b, &p) != nil {
		return 69
	}
	spec, err := DecodeSpec(p.Spec)
	if err != nil {
		return 69
	}
	conf, err := loadHostConf()
	if err != nil {
		fmt.Fprintln(os.Stderr, "bitc agent:", err)
		return 69
	}
	self, _ := os.Readlink("/proc/self/exe")
	_ = RealSystem{}.SetOOMScoreAdj()
	sys := &SlotSystem{Conf: conf, Slots: slots, Self: self, ToolsDir: toolsetDir(spec.Tools), Sys: RealSlotSys{Log: os.Stderr}}
	tools := sys.ToolsDir
	if tools == "" {
		tools = stateDir + "/tools/none"
	}
	o := Options{Stdout: stdout, ResultsRoot: stateDir + "/results", ToolsRoot: tools, WorkRoot: slotPath(slots[0], "work"),
		System: sys, NoServe: true, Deadline: time.Unix(p.Deadline, 0), StateRoot: slotPath(slots[0]), KeyRoot: slotPath(slots[0], "key"),
		AfterCommand: sys.Reap, Code: p.Code, UploadLock: stateDir + "/upload.lock",
		HostCheck: func() error {
			if err := delegateCgroup(); err != nil { // the check below only reads the subtree
				return fmt.Errorf("cgroup: %w", err)
			}
			for _, f := range CheckHost(NewHostEnv(conf, false)) {
				if f.Level == "fail" {
					return errors.New(f.String())
				}
			}
			return nil
		}}
	code := Run(contextBackground(), o, spec, Secrets{DeployKey: string(key)})
	sys.Close()
	// At D + 10m the slot is freed if nobody has done it (providers delta C.3.6); the agent itself
	// has to exit right after its EXIT marker, so a small detached process keeps the date.
	j := exec.Command(self, "__agent", "slot-janitor", args[0], args[1], strconv.FormatInt(time.Unix(p.Deadline, 0).Add(10*time.Minute).Unix(), 10))
	j.Env = []string{}
	j.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if j.Start() == nil {
		_ = j.Process.Release()
	}
	return code
}

// slotJanitor: <slots> <name> <unix time>. It frees the slots at that time if name still owns them.
func slotJanitor(args []string) int {
	if len(args) != 3 {
		return 64
	}
	slots, ok := ParseSlots(args[0])
	at, err := strconv.ParseInt(args[2], 10, 64)
	if !ok || err != nil || !slotNameRE.MatchString(args[1]) {
		return 64
	}
	conf, err := loadHostConf()
	if err != nil {
		return 69
	}
	h := realSlotHost(conf, nil)
	for {
		owners, err := h.Owners()
		if err == nil && (owners[slots[0]] == nil || owners[slots[0]].Name != args[1]) {
			return 0 // freed by the controller, or leased again
		}
		if time.Now().Unix() >= at {
			if err := h.Release(slots, args[1]); err != nil {
				return 69
			}
			return 0
		}
		time.Sleep(15 * time.Second)
	}
}

// ---- helpers of doctor (check-host --doctor)

func slotCred(slot int) (*syscall.SysProcAttr, error) {
	u, err := user.Lookup("bitc-s" + strconv.Itoa(slot))
	if err != nil {
		return nil, err
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)
	return &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid), Groups: []uint32{}}}, nil
}

func openAs(uid, gid int, path string) error {
	c := exec.Command("/bin/sh", "-c", `: < "$1"`, "sh", path)
	c.Dir, c.Env = "/", []string{"PATH=" + agentPath}
	c.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid), Groups: []uint32{}}}
	return c.Run()
}

// NewHostEnv is check-host's view of the real host.
func NewHostEnv(conf HostConf, doctor bool) HostEnv {
	self, _ := os.Readlink("/proc/self/exe")
	return NewHostEnvFor(conf, doctor, self)
}

// NewHostEnvFor is NewHostEnv with the agent binary that doctor's helper processes (the network
// and seccomp probes, which run as a slot user) are started from.
func NewHostEnvFor(conf HostConf, doctor bool, self string) HostEnv {
	return HostEnv{Conf: conf, Doctor: doctor, Environ: os.Environ, OpenAs: openAs, Self: self,
		Exec: func(stdin, name string, args ...string) ([]byte, error) {
			c := agentCmd(name, args...)
			c.Stdin = strings.NewReader(stdin)
			var errb strings.Builder
			c.Stderr = &errb
			out, err := c.Output()
			if err != nil {
				return out, fmt.Errorf("%w: %s", err, strings.TrimSpace(errb.String()))
			}
			return out, nil
		},
		NetProbe:     netProbe(conf, self),
		SeccompProbe: seccompProbe(false, self),
		LoginProbe: func() (int, error) {
			u, err := user.Lookup("bitc")
			if err != nil {
				return 0, err
			}
			uid, _ := strconv.Atoi(u.Uid)
			gid, _ := strconv.Atoi(u.Gid)
			c := exec.Command("/bin/sh", "-c", "/opt/bitc/sbin/bitc-login")
			c.Env = []string{"PATH=" + agentPath, "SSH_AUTH_SOCK=/nonexistent/agent.sock", "SSH_ORIGINAL_COMMAND=probe"}
			c.Dir = "/"
			c.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid), Groups: []uint32{}}}
			err = c.Run()
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				return ee.ExitCode(), nil
			}
			return 0, err
		},
	}
}

func seccompProbe(noFilter bool, self string) func() ([]string, error) {
	return func() ([]string, error) {
		attr, err := slotCred(1)
		if err != nil {
			return nil, err
		}
		args := []string{"__agent", "probe-seccomp"}
		if noFilter {
			args = append(args, "--no-filter")
		}
		c := exec.Command(self, args...)
		c.Env, c.Dir, c.SysProcAttr = []string{}, "/", attr
		out, err := c.Output()
		if err != nil {
			return nil, err
		}
		return strings.Split(strings.TrimSpace(string(out)), "\n"), nil
	}
}

// probeSeccomp is run as a slot user: it installs the job filter (unless --no-filter) and prints
// what each denied call returns. The calls are made raw; the clone is made to fail EINVAL when
// unfiltered (CLONE_NEWUSER with CLONE_THREAD) so it can never create a process.
func probeSeccomp(args []string, stdout io.Writer) int {
	if len(args) != 1 || args[0] != "--no-filter" {
		if err := InstallJobSeccomp(); err != nil {
			fmt.Fprintln(os.Stderr, "bitc agent: probe-seccomp:", err)
			return 69
		}
	}
	report := func(call string, r uintptr, e syscall.Errno) {
		if e == 0 {
			fmt.Fprintf(stdout, "%s OK\n", call)
			return
		}
		fmt.Fprintf(stdout, "%s %s\n", call, unix.ErrnoName(e))
	}
	runtime.LockOSThread()
	r, _, e := unix.Syscall(unix.SYS_SOCKET, 40, unix.SOCK_STREAM, 0)
	report("socket(AF_VSOCK)", r, e)
	var sv [2]int32
	r, _, e = unix.Syscall6(unix.SYS_SOCKETPAIR, 40, unix.SOCK_STREAM, 0, uintptr(unsafePtr(&sv)), 0, 0)
	report("socketpair(AF_VSOCK)", r, e)
	r, _, e = unix.Syscall(unix.SYS_UNSHARE, unix.CLONE_NEWUSER, 0, 0)
	report("unshare(CLONE_NEWUSER)", r, e)
	r, _, e = unix.Syscall6(unix.SYS_CLONE, unix.CLONE_NEWUSER|unix.CLONE_THREAD, 0, 0, 0, 0, 0)
	report("clone(CLONE_NEWUSER)", r, e)
	r, _, e = unix.Syscall(unix.SYS_KEYCTL, 0 /* KEYCTL_GET_KEYRING_ID */, ^uintptr(2) /* KEY_SPEC_SESSION_KEYRING */, 0)
	report("keyctl(GET_KEYRING_ID)", r, e)
	r, _, e = unix.Syscall(unix.SYS_CLONE3, 0, 0, 0)
	report("clone3", r, e)
	return 0
}

// ---- item 5

func isGlobalV6(ip net.IP) bool { return ip.To4() == nil && ip.IsGlobalUnicast() }

// selfTargets are the VM's own addresses a job must not reach: its IPv4 addresses (port 22) and
// IPv6 global address and prefix::1 (port 443). With netns=false a job reaches its own VM's
// addresses through lo by design (the output chain accepts lo), so they are not asked.
func selfTargets(conf HostConf) (ts []string) {
	if !conf.Netns {
		return nil
	}
	ts = append(ts, "10.203.0.1:22") // the host end of the probe's veth
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		ipn, ok := a.(*net.IPNet)
		if !ok || ipn.IP.IsLoopback() {
			continue
		}
		if v4 := ipn.IP.To4(); v4 != nil {
			ts = append(ts, v4.String()+":22")
		} else if isGlobalV6(ipn.IP) {
			ts = append(ts, "["+ipn.IP.String()+"]:443")
			p := ipn.IP.Mask(net.CIDRMask(64, 128))
			p[15] = 1
			ts = append(ts, "["+p.String()+"]:443")
		}
	}
	return ts
}

func appendMissing(l []string, more ...string) []string {
	have := map[string]bool{}
	for _, x := range l {
		have[x] = true
	}
	for _, x := range more {
		if !have[x] {
			l = append(l, x)
			have[x] = true
		}
	}
	return l
}

func netProbe(conf HostConf, self string) func(targets []string) (NetProbeResult, error) {
	return func(targets []string) (NetProbeResult, error) {
		res := NetProbeResult{Connect: map[string]ConnResult{}}
		u, err := user.Lookup("bitc-s1")
		if err != nil {
			return res, err
		}
		uid, _ := strconv.Atoi(u.Uid)
		gid, _ := strconv.Atoi(u.Gid)
		targets = appendMissing(append([]string(nil), targets...), selfTargets(conf)...)
		c := exec.Command(self, append([]string{"__agent", "probe-net"}, targets...)...)
		c.Dir = "/"
		c.Env = []string{"PATH=" + agentPath}
		if conf.Netns {
			ns, err := StartJobNS(JobNSConf{Slot: 0, Uid: uid, Gid: gid, Conf: conf, Self: self, Probe: true})
			if err != nil {
				return res, err
			}
			defer ns.Close()
			if err := ns.Wrap(c); err != nil {
				return res, err
			}
		} else {
			c.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid), Groups: []uint32{}}}
		}
		out, err := c.Output()
		if err != nil {
			return res, fmt.Errorf("probe: %w", err)
		}
		for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			k, v, ok := strings.Cut(l, " ")
			switch {
			case !ok:
			case k == "dns":
				if v != "ok" {
					res.DNSErr = errors.New(v)
				}
			case v == "open":
				res.Connect[k] = ConnOpen
			case v == "refused":
				res.Connect[k] = ConnRefused
			}
		}
		return res, nil
	}
}

// probeNet is run as the slot user in the job's network mode: `probe-net <host:port>...`. It
// prints `<target> open|refused|blocked` per target and `dns ok|<why>` for the resolver in
// /etc/resolv.conf.
func probeNet(targets []string, stdout io.Writer) int {
	var wg sync.WaitGroup
	var mu sync.Mutex
	lines := []string{}
	for _, t := range targets {
		wg.Add(1)
		exit.Go(func() {
			defer wg.Done()
			dial := t
			if h, p, err := net.SplitHostPort(t); err == nil && net.ParseIP(h) == nil {
				addrs, err := net.LookupHost(h)
				if err != nil || len(addrs) == 0 {
					return // the name does not exist here: nothing to reach
				}
				dial = net.JoinHostPort(addrs[0], p)
			}
			state := "blocked"
			conn, err := net.DialTimeout("tcp", dial, 3*time.Second)
			switch {
			case err == nil:
				conn.Close()
				state = "open"
			case errors.Is(err, syscall.ECONNREFUSED):
				state = "refused"
			}
			mu.Lock()
			lines = append(lines, t+" "+state)
			mu.Unlock()
		})
	}
	dnsState := "ok"
	wg.Add(1)
	exit.Go(func() {
		defer wg.Done()
		if err := dnsQuery(); err != nil {
			dnsState = err.Error()
		}
	})
	wg.Wait()
	sort.Strings(lines)
	for _, l := range lines {
		fmt.Fprintln(stdout, l)
	}
	fmt.Fprintln(stdout, "dns "+dnsState)
	return 0
}

// dnsQuery sends one A query for example.com to the first nameserver of /etc/resolv.conf.
func dnsQuery() error {
	b, err := os.ReadFile("/etc/resolv.conf")
	if err != nil {
		return err
	}
	ns := ""
	for _, l := range strings.Split(string(b), "\n") {
		if f := strings.Fields(l); len(f) == 2 && f[0] == "nameserver" {
			ns = f[1]
			break
		}
	}
	if ns == "" {
		return errors.New("no nameserver")
	}
	q := make([]byte, 12, 64)
	binary.BigEndian.PutUint16(q[0:], 0xb17c)
	binary.BigEndian.PutUint16(q[2:], 0x0100) // recursion desired
	binary.BigEndian.PutUint16(q[4:], 1)
	for _, label := range []string{"example", "com"} {
		q = append(q, byte(len(label)))
		q = append(q, label...)
	}
	q = append(q, 0, 0, 1, 0, 1)
	c, err := net.DialTimeout("udp", net.JoinHostPort(ns, "53"), 3*time.Second)
	if err != nil {
		return err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := c.Write(q); err != nil {
		return err
	}
	resp := make([]byte, 512)
	n, err := c.Read(resp)
	if err != nil {
		return err
	}
	if n < 12 || binary.BigEndian.Uint16(resp[0:]) != 0xb17c {
		return errors.New("bad response")
	}
	return nil
}

func contextBackground() context.Context { return context.Background() }

func unsafePtr(p *[2]int32) unsafe.Pointer { return unsafe.Pointer(p) }
