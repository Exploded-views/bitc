//go:build linux

package agent

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// bootStdin is `bitc __agent --boot-stdin` (providers delta C.5 and X.1): the host's exec channel
// carries one length-prefixed boot payload on stdin instead of the pod environment, and no
// listener is bound (the spec's port is -1). The deploy key is taken out of the payload: the
// memfd that survives the re-exec (fd 3) holds the rest, and the key rides in a second one (fd 5)
// that stage 2 reads and closes before any repo code runs. A refusal prints `refused: <why>` on
// stdout (stderr may not reach the host) and exits 65 with nothing started.
func bootStdin(stdin io.Reader, stdout io.Writer) int {
	refuse := func(why string) int { fmt.Fprintln(stdout, "refused: "+why); return 65 }
	p, err := ReadBootPayload(stdin)
	if err != nil {
		return refuse(err.Error())
	}
	spec, err := DecodeSpec(p.Spec)
	if err != nil {
		return refuse("unusable spec")
	}
	if spec.Port != -1 {
		return refuse("--boot-stdin needs port -1 in the spec")
	}
	if r := p.ResultsRoot; r != "" && (!filepath.IsAbs(r) || strings.Contains(r, "..") || filepath.Clean(r) != r) {
		return refuse("results_root must be a clean absolute path")
	}
	// the payload is read: stdin is closed (replaced by /dev/null) before the re-exec
	if err := devNullOver(0); err != nil {
		return toErr(err)
	}
	sys := System(RealSystem{})
	if spec.Unprivileged && spec.TestRoot != "" {
		sys = PlainSystem{}
	}
	_ = sys.SetOOMScoreAdj()
	rest, key := p.WithoutKey()
	b, _ := json.Marshal(rest)
	if err := memfdAt(b, 3); err != nil {
		return toErr(err)
	}
	if err := memfdAt([]byte(key), 5); err != nil {
		return toErr(err)
	}
	self, _ := os.Readlink("/proc/self/exe")
	return toErr(syscall.Exec(self, []string{self, "__agent", "--stage2-stdin"}, []string{}))
}

// memfdAt puts content in a fresh memfd and leaves it open, without close-on-exec, at fd target.
// The original memfd is close-on-exec, so only the dup survives the exec. It is parked above the
// target first so it cannot sit on a target a later call needs.
func memfdAt(content []byte, target int) error {
	fd, err := unix.MemfdCreate("bitc-boot", unix.MFD_CLOEXEC)
	if err != nil {
		return err
	}
	if len(content) > 0 {
		if _, err := unix.Write(fd, content); err != nil {
			return err
		}
	}
	if _, err := unix.Seek(fd, 0, 0); err != nil {
		return err
	}
	parked, err := unix.FcntlInt(uintptr(fd), unix.F_DUPFD_CLOEXEC, 10)
	if err != nil {
		return err
	}
	return unix.Dup3(parked, target, 0)
}

// stage2Stdin is stage 2 of --boot-stdin: payload on fd 3 (no key), the deploy key on fd 5.
func stage2Stdin(stdout io.Writer) int {
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
	p.DeployKey = string(key)
	return runStage2(detachStdio(stdout), p, nil)
}

// devNullOver points fd at /dev/null.
func devNullOver(fd int) error {
	n, err := unix.Open(os.DevNull, unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(n)
	return unix.Dup3(n, fd, 0)
}

// detachStdio frees the agent from the exec channel it was booted on. The channel can drop at any
// time (a provider's disconnect limit, an ssh drop), and a write to a dead pipe on fd 1 or 2 kills
// a Go program with SIGPIPE, orphaning the job. So the agent keeps a copy of stdout on a higher fd,
// where a failed write is only an error that the log tee ignores, and fds 0, 1 and 2 become
// /dev/null for everything else (the job and its setup steps get their own).
func detachStdio(stdout io.Writer) io.Writer {
	f, ok := stdout.(*os.File)
	if !ok || f.Fd() != 1 {
		return stdout
	}
	dup, err := unix.FcntlInt(1, unix.F_DUPFD_CLOEXEC, 10)
	if err != nil {
		return stdout
	}
	for _, fd := range []int{0, 1, 2} {
		_ = devNullOver(fd)
	}
	return os.NewFile(uintptr(dup), "exec-stdout")
}
