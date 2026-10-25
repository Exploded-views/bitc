package agent

import (
	"testing"

	"golang.org/x/sys/unix"
)

// bpfRun interprets a classic BPF seccomp program over one syscall and returns its verdict.
// It knows exactly the instructions the filter may use; anything else fails the test.
func bpfRun(t *testing.T, prog []unix.SockFilter, arch, nr uint32, arg0 uint64) uint32 {
	t.Helper()
	data := func(off uint32) uint32 {
		switch off {
		case 0:
			return nr
		case 4:
			return arch
		case 16:
			return uint32(arg0)
		case 20:
			return uint32(arg0 >> 32)
		}
		t.Fatalf("filter reads seccomp_data offset %d", off)
		return 0
	}
	var a uint32
	for pc := 0; pc < len(prog); pc++ {
		in := prog[pc]
		switch in.Code {
		case unix.BPF_LD | unix.BPF_W | unix.BPF_ABS:
			a = data(in.K)
		case unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K:
			if a == in.K {
				pc += int(in.Jt)
			} else {
				pc += int(in.Jf)
			}
		case unix.BPF_JMP | unix.BPF_JSET | unix.BPF_K:
			if a&in.K != 0 {
				pc += int(in.Jt)
			} else {
				pc += int(in.Jf)
			}
		case unix.BPF_JMP | unix.BPF_JA:
			pc += int(in.K)
		case unix.BPF_RET | unix.BPF_K:
			return in.K
		default:
			t.Fatalf("instruction %d: unexpected opcode %#x", pc, in.Code)
		}
	}
	t.Fatal("program fell off its end")
	return 0
}

const (
	wantAllow  = 0x7fff0000
	wantKill   = 0x80000000
	wantEPERM  = 0x00050000 | 1
	wantENOSYS = 0x00050000 | 38
	tAuditX64  = 0xc000003e
	tAuditA64  = 0xc00000b7
)

type scCase struct {
	name string
	nr   uint32
	arg0 uint64
	want uint32
}

func scTable(arch string) (uint32, []scCase) {
	const newUser = 0x10000000
	if arch == "amd64" {
		return tAuditX64, []scCase{
			{"socket(AF_VSOCK)", 41, 40, wantEPERM}, {"socket(AF_INET)", 41, 2, wantAllow}, {"socket(AF_UNIX)", 41, 1, wantAllow},
			{"socketpair(AF_VSOCK)", 53, 40, wantEPERM}, {"socketpair(AF_UNIX)", 53, 1, wantAllow},
			{"unshare(NEWUSER)", 272, newUser, wantEPERM}, {"unshare(NEWNS)", 272, 0x00020000, wantAllow},
			{"clone(NEWUSER|SIGCHLD)", 56, newUser | 17, wantEPERM}, {"clone(plain fork)", 56, 17, wantAllow},
			{"clone3", 435, 0, wantENOSYS},
			{"keyctl", 250, 0, wantEPERM}, {"add_key", 248, 0, wantEPERM}, {"request_key", 249, 0, wantEPERM},
			{"read", 0, 0, wantAllow}, {"execve", 59, 0, wantAllow}, {"openat", 257, 0, wantAllow},
		}
	}
	return tAuditA64, []scCase{
		{"socket(AF_VSOCK)", 198, 40, wantEPERM}, {"socket(AF_INET6)", 198, 10, wantAllow},
		{"socketpair(AF_VSOCK)", 199, 40, wantEPERM}, {"socketpair(AF_UNIX)", 199, 1, wantAllow},
		{"unshare(NEWUSER)", 97, newUser, wantEPERM}, {"unshare(0)", 97, 0, wantAllow},
		{"clone(NEWUSER)", 220, newUser, wantEPERM}, {"clone(plain fork)", 220, 17, wantAllow},
		{"clone3", 435, 0, wantENOSYS},
		{"keyctl", 219, 0, wantEPERM}, {"add_key", 217, 0, wantEPERM}, {"request_key", 218, 0, wantEPERM},
		{"read", 63, 0, wantAllow}, {"execve", 221, 0, wantAllow}, {"openat", 56, 0, wantAllow},
	}
}

func TestJobSeccompVerdictsPerArch(t *testing.T) {
	for _, arch := range []string{"amd64", "arm64"} {
		prog, err := JobSeccompProgram(arch)
		if err != nil || len(prog) == 0 {
			t.Fatalf("%s: program %d instructions, err %v", arch, len(prog), err)
		}
		audit, cases := scTable(arch)
		for _, c := range cases {
			if got := bpfRun(t, prog, audit, c.nr, c.arg0); got != c.want {
				t.Errorf("%s %s (nr %d, arg0 %#x): verdict %#x, want %#x", arch, c.name, c.nr, c.arg0, got, c.want)
			}
		}
	}
}

func TestJobSeccompArgumentIsLowWordOnly(t *testing.T) {
	// the domain and flags are 32-bit ints: junk in the high half of the register must not hide AF_VSOCK
	for _, arch := range []string{"amd64", "arm64"} {
		prog, _ := JobSeccompProgram(arch)
		audit, cases := scTable(arch)
		for _, c := range cases {
			if c.name == "socket(AF_VSOCK)" {
				if got := bpfRun(t, prog, audit, c.nr, c.arg0|0xdeadbeef<<32); got != wantEPERM {
					t.Errorf("%s: AF_VSOCK with a dirty high word got verdict %#x, want EPERM", arch, got)
				}
			}
		}
	}
}

func TestJobSeccompKillsForeignArchAndX32(t *testing.T) {
	for _, arch := range []string{"amd64", "arm64"} {
		prog, _ := JobSeccompProgram(arch)
		audit, _ := scTable(arch)
		foreign := uint32(0x40000003) // AUDIT_ARCH_I386
		if arch == "amd64" {
			if got := bpfRun(t, prog, tAuditA64, 41, 40); got != wantKill {
				t.Errorf("amd64 program, arm64 syscall: verdict %#x, want KILL_PROCESS", got)
			}
		} else if got := bpfRun(t, prog, tAuditX64, 198, 40); got != wantKill {
			t.Errorf("arm64 program, amd64 syscall: verdict %#x, want KILL_PROCESS", got)
		}
		if got := bpfRun(t, prog, foreign, 102, 0); got != wantKill {
			t.Errorf("%s: 32-bit compat call: verdict %#x, want KILL_PROCESS", arch, got)
		}
		if arch == "amd64" { // x32 numbers carry bit 30 and would dodge the number matches
			if got := bpfRun(t, prog, audit, 0x40000000|41, 40); got != wantKill {
				t.Errorf("amd64: x32-ABI socket: verdict %#x, want KILL_PROCESS", got)
			}
		}
	}
}

func TestJobSeccompUnknownArch(t *testing.T) {
	if _, err := JobSeccompProgram("riscv64"); err == nil {
		t.Error("riscv64: no error for an arch with no syscall table")
	}
}

func TestJobSeccompProgramFitsTheKernelLimit(t *testing.T) {
	for _, arch := range []string{"amd64", "arm64"} {
		prog, _ := JobSeccompProgram(arch)
		if n := len(prog); n == 0 || n > 4096 {
			t.Errorf("%s: %d instructions, want 1..4096", arch, n)
		}
	}
}
