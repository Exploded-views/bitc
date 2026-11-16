package container

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"syscall"

	"github.com/Exploded-views/bitc/internal/exit"
)

// LifelineFD is the descriptor the guard finds the read end of bitc's lifeline pipe on.
const LifelineFD = 3

// doneLine is what bitc writes to the lifeline when the run is over and the container is
// already removed: the guard then exits without touching anything.
const doneLine = "done\n"

// Guard is the process that removes a container when bitc dies without cleaning up. bitc holds the
// write end of a pipe whose read end is the guard's fd 3; the kernel closes it when bitc ends, even
// by SIGKILL, and the guard reads EOF. It runs in its own session, so a signal to bitc or its
// process group does not reach it. This is the same lifeline idea as the macOS plugin shim, and it
// works the same on Linux, where the Pdeathsig of the docker CLI child kills the CLI but never the
// container.
type Guard struct {
	cmd *exec.Cmd
	w   *os.File
}

// StartGuard starts `exe exeArgs... <runtime> <name>` as the guard. Production passes bitc's own
// binary and "__container-guard".
func StartGuard(exe string, exeArgs []string, runtime, name string, env []string) (*Guard, error) {
	r, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(exe, append(append([]string{}, exeArgs...), runtime, name)...) //nolint:gosec // G702: exe is bitc's own binary; the arguments are the runtime path bitc found and a name it built
	cmd.Env = env
	cmd.ExtraFiles = []*os.File{r} // fd 3; stdin, stdout and stderr are the null device
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	err = cmd.Start()
	r.Close()
	if err != nil {
		w.Close()
		return nil, err
	}
	return &Guard{cmd: cmd, w: w}, nil
}

// Done tells the guard the run is over (the container is removed) and lets it exit.
func (g *Guard) Done() {
	_, _ = g.w.WriteString(doneLine)
	g.w.Close()
	exit.Go(func() { _ = g.cmd.Wait() })
}

// GuardMain is the body of `bitc __container-guard <runtime> <name>`: it waits on life; "done"
// means bitc cleaned up, and anything else (EOF) means bitc died, so it removes the container.
func GuardMain(args []string, life *os.File) int {
	if len(args) != 2 {
		return exit.Usage
	}
	r := bufio.NewReader(life)
	for {
		line, err := r.ReadString('\n')
		if line == doneLine {
			return 0
		}
		if err != nil {
			break
		}
	}
	_ = Remove(context.Background(), args[0], args[1], os.Environ())
	return 0
}
