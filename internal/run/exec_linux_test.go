// Linux-only: opens a pty with Linux ioctls (TIOCSPTLCK, TIOCGPTN), which do not exist on darwin.

package run

import (
	"context"
	"fmt"
	"os"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

func TestTTYStdinStartedWithoutAGroup(t *testing.T) {
	m, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Skip("no /dev/ptmx:", err)
	}
	defer m.Close()
	if err := unix.IoctlSetPointerInt(int(m.Fd()), unix.TIOCSPTLCK, 0); err != nil {
		t.Skip(err)
	}
	n, err := unix.IoctlGetInt(int(m.Fd()), unix.TIOCGPTN)
	if err != nil {
		t.Skip(err)
	}
	sl, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Skip(err)
	}
	defer sl.Close()
	s := spec(t, "true")
	s.Stdin = sl
	gotPgid := -1
	s.Started = func(_, pgid int, _ uint64) { gotPgid = pgid }
	if _, err := Exec(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if gotPgid != 0 {
		t.Fatalf("pgid %d, want 0 for a TTY run", gotPgid)
	}
}
