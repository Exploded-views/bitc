package spritesfake

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Exploded-views/bitc/internal/wsmini"
)

// goroutineStacks is the text of every goroutine's stack, one block per goroutine.
func goroutineStacks() []string {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			return strings.Split(string(buf[:n]), "\n\n")
		}
		buf = make([]byte, 2*len(buf))
	}
}

// spinUntil polls the goroutine stacks, yielding the processor, until ok holds. Reading stacks
// orders nothing for the race detector, which is the point: the scenario below must not be
// ordered by the test itself. The deadline is a safety net that only fails: a condition that
// never matches (a renamed function, a changed wait reason) ends the test instead of spinning.
func spinUntil(t *testing.T, what string, ok func(stacks []string) bool) {
	t.Helper()
	deadline := time.Now().Add(time.Minute)
	for !ok(goroutineStacks()) {
		if !time.Now().Before(deadline) {
			t.Fatalf("no goroutine stack ever matched: %s", what)
		}
		runtime.Gosched()
	}
}

// parked and gone are the two conditions of the scenario. close's Wait goroutine is parked when
// its stack header carries the wait reason "sync.WaitGroup.Wait" (Go 1.24 and later print it in
// brackets; the frame itself reads sync.(*WaitGroup).Wait, which is not matched).
func closeWaitParked(s []string) bool {
	return anyStack(s, "[sync.WaitGroup.Wait", "(*Fake).close.func")
}
func closeWaitGone(s []string) bool { return !anyStack(s, "(*Fake).close.func") }

func anyStack(stacks []string, parts ...string) bool {
	for _, st := range stacks {
		match := true
		for _, p := range parts {
			match = match && strings.Contains(st, p)
		}
		if match {
			return true
		}
	}
	return false
}

// An exec connection that upgrades while close waits for the fake's goroutines must not join the
// WaitGroup: an Add from zero concurrent with Wait is WaitGroup misuse, which -race reports.
func TestExecArrivingAsCloseWaitsDoesNotJoinTheWaitGroup(t *testing.T) {
	f := New(t, Options{})
	reached, release := make(chan struct{}), make(chan struct{})
	f.mu.Lock()
	f.beforeExecJoin = func() { close(reached); <-release }
	f.mu.Unlock()

	hdr := http.Header{"Authorization": {"Bearer " + f.Token()}}
	req, _ := http.NewRequest("POST", f.URL()+"/v1/sprites", strings.NewReader(`{"name":"a"}`))
	req.Header = hdr.Clone()
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	// The exec upgrades, then stops just before it would join the WaitGroup.
	conn, err := wsmini.Dial(context.Background(), f.URL()+"/v1/sprites/a/exec?cmd=true", wsmini.DialOptions{Header: hdr, HandshakeTimeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Abort() }()
	<-reached

	// close starts waiting on the WaitGroup while the test holds it, so the wait is parked.
	f.wg.Add(1)
	go f.close()
	spinUntil(t, "close parked in WaitGroup.Wait", closeWaitParked)

	// The hold ends and the wait finishes: the counter is zero and close is no longer waiting.
	f.wg.Done()
	spinUntil(t, "close done waiting", closeWaitGone)

	// Only now may the exec go on to its join.
	close(release)
}

// A Sprite created as close waits must not start its task-socket server: that is an Add from zero
// concurrent with Wait, and a server close never sweeps. The create is refused instead.
func TestCreateArrivingAsCloseWaitsIsRefused(t *testing.T) {
	f := New(t, Options{})
	reached, release := make(chan struct{}), make(chan struct{})
	f.mu.Lock()
	f.beforeCreate = func() { close(reached); <-release }
	f.mu.Unlock()

	go func() {
		req, _ := http.NewRequest("POST", f.URL()+"/v1/sprites", strings.NewReader(`{"name":"a"}`))
		req.Header.Set("Authorization", "Bearer "+f.Token())
		req.Header.Set("Content-Type", "application/json")
		// close cuts this connection, so the answer is not the point: the Sprite is
		if resp, err := http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
		}
	}()
	<-reached

	// close starts waiting on the WaitGroup while the test holds it, so the wait is parked.
	f.wg.Add(1)
	go f.close()
	spinUntil(t, "close parked in WaitGroup.Wait", closeWaitParked)

	// The hold ends and the wait finishes: the counter is zero and close is no longer waiting.
	f.wg.Done()
	spinUntil(t, "close done waiting", closeWaitGone)

	// Only now may the create go on to take the lock.
	close(release)
	spinUntil(t, "the create handler returned", func(s []string) bool { return !anyStack(s, "(*Fake).create(") })
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sprites["a"] != nil {
		t.Fatal("a create after close added a Sprite")
	}
}

// An agent process that starts as close waits must not join the WaitGroup either, and must not
// outlive close: the launch goroutine has no wg slot of its own, so after the exec connection is
// cut it can still be starting the process.
func TestAgentStartingAsCloseWaitsDoesNotJoinTheWaitGroup(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "agent")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexec sleep 600\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	f := New(t, Options{Bin: bin})
	reached, release := make(chan struct{}), make(chan struct{})
	f.mu.Lock()
	f.beforeAgentJoin = func() { close(reached); <-release }
	f.mu.Unlock()

	hdr := http.Header{"Authorization": {"Bearer " + f.Token()}}
	req, _ := http.NewRequest("POST", f.URL()+"/v1/sprites", strings.NewReader(`{"name":"a"}`))
	req.Header = hdr.Clone()
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	// an agent exec: the sha check line the real client sends, then `__agent list`
	script := `echo "` + strings.Repeat("b", 64) + `  /opt/bitc/bin/bitc" | sha256sum -c - >/dev/null && exec /opt/bitc/bin/bitc __agent list`
	q := url.Values{"cmd": {"sh", "-c", script}}
	conn, err := wsmini.Dial(context.Background(), f.URL()+"/v1/sprites/a/exec?"+q.Encode(), wsmini.DialOptions{Header: hdr, HandshakeTimeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Abort() }()
	<-reached
	f.mu.Lock()
	pr := f.sprites["a"].procs[0]
	f.mu.Unlock()

	f.wg.Add(1)
	go f.close()
	spinUntil(t, "close parked in WaitGroup.Wait", closeWaitParked)
	f.wg.Done()
	spinUntil(t, "close done waiting", closeWaitGone)

	// Only now may the launch goroutine go on to its join.
	close(release)
	<-pr.done // the process was ended and reaped on the spot
}
