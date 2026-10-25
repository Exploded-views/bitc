package admit

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/Exploded-views/bitc/internal/config"
	"github.com/Exploded-views/bitc/internal/proc"
)

// A pgid whose leader is a different process now (the id was reused) names an unrelated
// group: reap must kill nothing.
func TestReapSkipsReusedPgid(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); _ = cmd.Wait() })
	st, err := proc.ReadStat(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	reap(Ticket{State: "running", Pgid: cmd.Process.Pid, LeaderStart: st.Start - 1})
	time.Sleep(100 * time.Millisecond)
	if st, err := proc.ReadStat(cmd.Process.Pid); err != nil || st.State == 'Z' {
		t.Fatalf("reap killed an unrelated process group (%v, %c)", err, st.State)
	}
}

func TestAcquireWakesOnContextCancel(t *testing.T) {
	lint := &config.Pool{Name: "lint", Slots: 1, Wait: 30 * time.Second}
	l, e := setup(t, map[string]*config.Pool{"lint": lint}, nil)
	e.Host.Poll = 5 * time.Second
	h1, err := Acquire(context.Background(), req(l, e, lint))
	if err != nil {
		t.Fatal(err)
	}
	defer h1.Release()
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(200*time.Millisecond, cancel)
	t0 := time.Now()
	_, err = Acquire(ctx, req(l, e, lint))
	if err == nil || ctx.Err() == nil {
		t.Fatalf("got %v", err)
	}
	if d := time.Since(t0); d > time.Second {
		t.Fatalf("cancellation noticed after %v (poll is 5s)", d)
	}
}

// Pruning a dead high-class ticket also removes its compat priority marker.
func TestPruneRemovesDeadHighMirror(t *testing.T) {
	c, dir := legacy(t)
	lint := &config.Pool{Name: "lint", Slots: 1}
	l, e := setup(t, map[string]*config.Pool{"lint": lint}, nil)
	e.Compat = c
	if err := os.MkdirAll(l.Tickets("lint"), 0o700); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(Ticket{Pid: os.Getpid(), Class: 0, State: "waiting"})
	if err := os.WriteFile(filepath.Join(l.Tickets("lint"), "0-0000000000000000001-aaaaaaaa.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, "locks", "lint.prio."+strconv.Itoa(os.Getpid()))
	if err := os.WriteFile(marker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	h, err := Acquire(context.Background(), req(l, e, lint))
	if err != nil {
		t.Fatal(err)
	}
	h.Release()
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the dead high ticket's mirror marker survived the prune")
	}
}
