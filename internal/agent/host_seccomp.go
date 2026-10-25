package agent

import (
	"fmt"
	"runtime"
	"unsafe"

	"golang.org/x/sys/unix"
)

// The job seccomp filter (providers delta C.5.3, decision 21): default allow; AF_VSOCK sockets,
// user namespaces and the kernel keyring calls return EPERM, clone3 returns ENOSYS (its flags sit
// in a struct seccomp cannot read, so glibc and Go fall back to clone), a foreign architecture is
// killed. The program is generated per architecture and checked by shape in the unit tests; the
// arm64 one cannot run on an amd64 build host.

const (
	afVsock      = 40
	cloneNewUser = 0x10000000
	x32Bit       = 0x40000000

	retAllow  = 0x7fff0000
	retKill   = 0x80000000 // SECCOMP_RET_KILL_PROCESS
	retErrno  = 0x00050000
	auditX64  = 0xc000003e
	auditArm  = 0xc00000b7
	dataNr    = 0
	dataArch  = 4
	dataArg0  = 16 // low word of args[0]; both architectures are little-endian
	seccompTS = 1  // SECCOMP_FILTER_FLAG_TSYNC
)

// seccompNums are the syscall numbers one architecture needs.
type seccompNums struct {
	audit                                      uint32
	socket, socketpair, clone, unshare, clone3 uint32
	keyctl, addKey, requestKey                 uint32
	x32                                        bool
}

var seccompTables = map[string]seccompNums{
	"amd64": {audit: auditX64, socket: 41, socketpair: 53, clone: 56, unshare: 272, clone3: 435, keyctl: 250, addKey: 248, requestKey: 249, x32: true},
	"arm64": {audit: auditArm, socket: 198, socketpair: 199, clone: 220, unshare: 97, clone3: 435, keyctl: 219, addKey: 217, requestKey: 218},
}

// bpfAsm assembles a classic BPF program with named jump targets.
type bpfAsm struct {
	ins    []unix.SockFilter
	labels map[string]int
	fix    []bpfFix
}

type bpfFix struct {
	at     int
	jt, jf string // "" means fall through (offset 0)
	isJump bool
}

func (a *bpfAsm) ld(off uint32) {
	a.ins = append(a.ins, unix.SockFilter{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: off})
}

func (a *bpfAsm) jmp(op uint16, k uint32, jt, jf string) {
	a.fix = append(a.fix, bpfFix{at: len(a.ins), jt: jt, jf: jf, isJump: true})
	a.ins = append(a.ins, unix.SockFilter{Code: unix.BPF_JMP | op | unix.BPF_K, K: k})
}

func (a *bpfAsm) ret(v uint32) {
	a.ins = append(a.ins, unix.SockFilter{Code: unix.BPF_RET | unix.BPF_K, K: v})
}

func (a *bpfAsm) label(name string) {
	if a.labels == nil {
		a.labels = map[string]int{}
	}
	a.labels[name] = len(a.ins)
}

func (a *bpfAsm) done() ([]unix.SockFilter, error) {
	for _, f := range a.fix {
		off := func(l string) (uint8, error) {
			if l == "" {
				return 0, nil
			}
			to, ok := a.labels[l]
			if !ok {
				return 0, fmt.Errorf("seccomp: undefined label %q", l)
			}
			d := to - f.at - 1
			if d < 0 || d > 255 {
				return 0, fmt.Errorf("seccomp: jump %d out of range", d)
			}
			return uint8(d), nil
		}
		var err error
		if a.ins[f.at].Jt, err = off(f.jt); err != nil {
			return nil, err
		}
		if a.ins[f.at].Jf, err = off(f.jf); err != nil {
			return nil, err
		}
	}
	return a.ins, nil
}

// JobSeccompProgram is the job seccomp filter for goarch ("amd64" or "arm64").
func JobSeccompProgram(goarch string) ([]unix.SockFilter, error) {
	n, ok := seccompTables[goarch]
	if !ok {
		return nil, fmt.Errorf("seccomp: no syscall table for %s", goarch)
	}
	var a bpfAsm
	a.ld(dataArch)
	a.jmp(unix.BPF_JEQ, n.audit, "", "kill")
	if n.x32 {
		a.ld(dataNr)
		a.jmp(unix.BPF_JSET, x32Bit, "kill", "")
	}
	a.ld(dataNr)
	// each rule: match the number, then decide on the first argument or answer outright
	a.jmp(unix.BPF_JEQ, n.socket, "domain", "")
	a.jmp(unix.BPF_JEQ, n.socketpair, "domain", "")
	a.jmp(unix.BPF_JEQ, n.unshare, "flags", "")
	a.jmp(unix.BPF_JEQ, n.clone, "flags", "")
	a.jmp(unix.BPF_JEQ, n.clone3, "enosys", "")
	a.jmp(unix.BPF_JEQ, n.keyctl, "eperm", "")
	a.jmp(unix.BPF_JEQ, n.addKey, "eperm", "")
	a.jmp(unix.BPF_JEQ, n.requestKey, "eperm", "")
	a.ret(retAllow)
	a.label("domain")
	a.ld(dataArg0)
	a.jmp(unix.BPF_JEQ, afVsock, "eperm", "allow2")
	a.label("flags")
	a.ld(dataArg0)
	a.jmp(unix.BPF_JSET, cloneNewUser, "eperm", "allow2")
	a.label("eperm")
	a.ret(retErrno | uint32(unix.EPERM))
	a.label("enosys")
	a.ret(retErrno | uint32(unix.ENOSYS))
	a.label("kill")
	a.ret(retKill)
	a.label("allow2") // jumps only go forward
	a.ret(retAllow)
	return a.done()
}

// InstallJobSeccomp sets PR_SET_NO_NEW_PRIVS and installs the filter for this architecture on
// every thread of the process (TSYNC); it is inherited by every child and cannot be removed.
func InstallJobSeccomp() error {
	prog, err := JobSeccompProgram(runtime.GOARCH)
	if err != nil {
		return err
	}
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("no_new_privs: %w", err)
	}
	fp := unix.SockFprog{Len: uint16(len(prog)), Filter: &prog[0]}
	if _, _, e := unix.Syscall(unix.SYS_SECCOMP, 1 /* SET_MODE_FILTER */, seccompTS, uintptr(unsafe.Pointer(&fp))); e != 0 {
		return fmt.Errorf("seccomp: %w", e)
	}
	return nil
}
