package admit

import (
	"context"
	"time"

	"github.com/Exploded-views/bitc/internal/exit"

	"golang.org/x/sys/unix"
)

// waker sleeps up to the poll interval, waking early when a ticket leaves a watched dir.
type waker struct{ fd int }

func newWaker(dirs []string) *waker {
	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if err != nil {
		return &waker{fd: -1}
	}
	for _, d := range dirs {
		_, _ = unix.InotifyAddWatch(fd, d, unix.IN_DELETE|unix.IN_MOVED_FROM)
	}
	return &waker{fd: fd}
}

// sleep also returns as soon as ctx is done.
func (w *waker) sleep(ctx context.Context, d time.Duration) {
	if d <= 0 {
		return
	}
	// a self-pipe written on ctx.Done wakes the poll below
	pr, pw := -1, -1
	var pipe [2]int
	if err := unix.Pipe2(pipe[:], unix.O_CLOEXEC|unix.O_NONBLOCK); err == nil {
		pr, pw = pipe[0], pipe[1]
		stop := make(chan struct{})
		done := make(chan struct{})
		exit.Go(func() {
			defer close(done)
			select {
			case <-ctx.Done():
				_, _ = unix.Write(pw, []byte{1})
			case <-stop:
			}
		})
		defer func() {
			close(stop)
			<-done
			unix.Close(pr)
			unix.Close(pw)
		}()
	}
	fds := []unix.PollFd{{Fd: int32(pr), Events: unix.POLLIN}}
	if w.fd >= 0 {
		fds = append(fds, unix.PollFd{Fd: int32(w.fd), Events: unix.POLLIN})
	}
	if pr < 0 && w.fd < 0 {
		select {
		case <-ctx.Done():
		case <-time.After(d):
		}
		return
	}
	_, _ = unix.Poll(fds, int(d/time.Millisecond)+1)
	if w.fd < 0 {
		return
	}
	buf := make([]byte, 4096)
	for {
		if n, err := unix.Read(w.fd, buf); n <= 0 || err != nil {
			break
		}
	}
}

func (w *waker) close() {
	if w.fd >= 0 {
		unix.Close(w.fd)
	}
}
