package agent_test

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Exploded-views/bitc/internal/agent"
	"github.com/Exploded-views/bitc/internal/agent/agenttest"
)

// tasksAPI is a Tasks API on a unix socket that records what it is asked.
type tasksAPI struct {
	sock string

	mu         sync.Mutex
	events     []string // "create 2s", "renew", "delete"
	failRenew  bool
	failCreate bool
	onDelete   func()
}

func newTasksAPI(t *testing.T) *tasksAPI {
	t.Helper()
	d, err := os.MkdirTemp("", "tk")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	a := &tasksAPI{sock: filepath.Join(d, "api.sock")}
	l, err := net.Listen("unix", a.sock)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	handle := func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		a.mu.Lock()
		defer a.mu.Unlock()
		var req struct{ Expire string }
		_ = json.Unmarshal(b, &req)
		id := strings.TrimPrefix(r.URL.Path, "/v1/tasks")
		switch {
		case r.Method == "POST" && id == "":
			if a.failCreate {
				w.WriteHeader(500)
				return
			}
			a.events = append(a.events, "create "+req.Expire)
			_, _ = w.Write([]byte(`{"id":"t1"}`))
		case r.Method == "POST" && id == "/t1":
			if a.failRenew {
				w.WriteHeader(500)
				return
			}
			a.events = append(a.events, "renew "+req.Expire)
			_, _ = w.Write([]byte(`{"id":"t1"}`))
		case r.Method == "DELETE" && id == "/t1":
			a.events = append(a.events, "delete")
			if a.onDelete != nil {
				a.onDelete()
			}
			w.WriteHeader(204)
		default:
			w.WriteHeader(404)
		}
	}
	mux.HandleFunc("/v1/tasks", handle)
	mux.HandleFunc("/v1/tasks/", handle)
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: time.Second}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })
	return a
}

func (a *tasksAPI) log() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.events...)
}

func keepAliveFixture(t *testing.T, a *tasksAPI, script string) *agenttest.Fixture {
	t.Helper()
	f := agenttest.NewFixture(t, script)
	f.NoListener = true
	f.KeepAlive = &agent.KeepAliveConf{Sock: a.sock, Expire: 2 * time.Second, Renew: 100 * time.Millisecond}
	return f
}

func TestKeepAliveTaskIsHeldRenewedAndDeletedAtTheEnd(t *testing.T) {
	a := newTasksAPI(t)
	f := keepAliveFixture(t, a, "#!/bin/sh\nsleep 1\n")
	var resultAtDelete bool
	a.onDelete = func() { _, err := os.Stat(filepath.Join(f.Root, "j1", "result.json")); resultAtDelete = err == nil }
	log, code := f.Run(t)
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, log)
	}
	ev := a.log()
	renews := 0
	for _, e := range ev {
		if strings.HasPrefix(e, "renew") {
			renews++
		}
	}
	if len(ev) < 4 || ev[0] != "create 2s" || ev[len(ev)-1] != "delete" || renews < 3 || strings.Count(strings.Join(ev, "|"), "create") != 1 {
		t.Fatalf("task events %v", ev)
	}
	if !resultAtDelete {
		t.Fatal("the task was deleted before result.json was written")
	}
	if strings.Contains(log, "keep-alive renewal failed") {
		t.Fatalf("a good run reported a failed renewal:\n%s", log)
	}
}

func TestKeepAliveCreateFailureIsAnInfraSetupFailure(t *testing.T) {
	a := newTasksAPI(t)
	a.failCreate = true
	f := keepAliveFixture(t, a, "#!/bin/sh\necho RAN\n")
	log, code := f.Run(t)
	if !strings.Contains(log, "__BITC_SETUP_FAILED__ n0nce keep-alive infra ") || strings.Contains(log, "RAN") || strings.Contains(log, "BEGIN") || code == 0 {
		t.Fatalf("exit %d\n%s", code, log)
	}
}

func TestKeepAliveWithNoSocketIsAnInfraSetupFailure(t *testing.T) {
	a := newTasksAPI(t)
	f := keepAliveFixture(t, a, "#!/bin/sh\necho RAN\n")
	f.KeepAlive.Sock = filepath.Join(t.TempDir(), "missing.sock")
	log, _ := f.Run(t)
	if !strings.Contains(log, "__BITC_SETUP_FAILED__ n0nce keep-alive infra ") || strings.Contains(log, "RAN") {
		t.Fatalf("\n%s", log)
	}
}

func TestKeepAliveRenewalFailurePrintsOneLineAndTheJobGoesOn(t *testing.T) {
	a := newTasksAPI(t)
	a.failRenew = true
	f := keepAliveFixture(t, a, "#!/bin/sh\nsleep 1\necho done\n")
	log, code := f.Run(t)
	if code != 0 || strings.Count(log, "keep-alive renewal failed\n") != 1 || !strings.Contains(log, "__BITC_EXIT__ n0nce 0") {
		t.Fatalf("exit %d\n%s", code, log)
	}
}
