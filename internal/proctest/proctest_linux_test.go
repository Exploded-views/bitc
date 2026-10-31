//go:build linux

package proctest

import (
	"fmt"
	"io/fs"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// Linux-only: these swap the /proc read to reach its error branches, which a real process rarely hits.
func stubReadStat(t *testing.T, err error) {
	t.Helper()
	orig := readStat
	t.Cleanup(func() { readStat = orig })
	readStat = func(string) ([]byte, error) { return nil, err }
}

func TestStatReadRaceCountsAsGone(t *testing.T) {
	for name, err := range map[string]error{
		"reaped mid-read (ESRCH)": fmt.Errorf("read: %w", syscall.ESRCH),
		"no such file":            fs.ErrNotExist,
	} {
		t.Run(name, func(t *testing.T) {
			stubReadStat(t, err)
			if !zombieOrExited(4242) {
				t.Fatal("not reported gone")
			}
		})
	}
}

func TestStatReadFailurePanicsNamingThePid(t *testing.T) {
	stubReadStat(t, syscall.EIO)
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("no panic on an unexpected read error")
		}
		if msg := fmt.Sprint(r); !strings.Contains(msg, strconv.Itoa(4242)) {
			t.Fatalf("panic %q does not mention the pid", msg)
		}
	}()
	zombieOrExited(4242)
}
