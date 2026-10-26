//go:build !linux

package admit

import (
	"context"
	"time"
)

// waker off Linux has no inotify: a waiter sleeps the whole poll interval (or until ctx is done)
// and so notices a freed slot up to one poll late.
type waker struct{}

func newWaker([]string) *waker { return &waker{} }

func (w *waker) sleep(ctx context.Context, d time.Duration) {
	if d <= 0 {
		return
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

func (w *waker) close() {}
