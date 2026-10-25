package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"sync"
	"time"

	"github.com/Exploded-views/bitc/internal/exit"
)

// KeepAliveConf is the keep-alive task (sprites-provider.md 2.3): Sock is the Tasks API unix
// socket, the task lives Expire past its last renewal and is renewed every Renew. The API shape
// (POST /v1/tasks {"expire"}, POST /v1/tasks/<id> to renew, DELETE /v1/tasks/<id>) is assumed
// until probe 9.17.
type KeepAliveConf struct {
	Sock          string
	Expire, Renew time.Duration
}

var taskIDRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// expireText is d as the API's short form: "5m", "90s".
func expireText(d time.Duration) string {
	switch {
	case d%time.Minute == 0:
		return fmt.Sprintf("%dm", int64(d/time.Minute))
	case d%time.Second == 0:
		return fmt.Sprintf("%ds", int64(d/time.Second))
	}
	return d.String()
}

// startKeepAlive creates the task and renews it until stop is called, which deletes it. A failed
// creation is an error (the job must not run on a Sprite that may pause under it); a failed
// renewal prints one line per streak and the job goes on.
func startKeepAlive(c KeepAliveConf, say func(string)) (stop func(), err error) {
	hc := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", c.Sock)
		},
		DisableKeepAlives: true,
	}}
	body, _ := json.Marshal(map[string]string{"expire": expireText(c.Expire)})
	call := func(method, path string, withBody bool) (int, []byte, error) {
		var rd io.Reader
		if withBody {
			rd = bytes.NewReader(body)
		}
		req, err := http.NewRequest(method, "http://sprite"+path, rd)
		if err != nil {
			return 0, nil, err
		}
		if withBody {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := hc.Do(req)
		if err != nil {
			return 0, nil, err
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		return resp.StatusCode, b, nil
	}
	code, b, err := call("POST", "/v1/tasks", true)
	if err != nil {
		return nil, fmt.Errorf("keep-alive task: %w", err)
	}
	var created struct{ ID string }
	if code < 200 || code > 299 || json.Unmarshal(b, &created) != nil || !taskIDRE.MatchString(created.ID) {
		return nil, fmt.Errorf("keep-alive task: HTTP %d", code)
	}
	path := "/v1/tasks/" + created.ID
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	exit.Go(func() {
		defer wg.Done()
		t := time.NewTicker(c.Renew)
		defer t.Stop()
		failing := false
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				code, _, err := call("POST", path, true)
				if err != nil || code < 200 || code > 299 {
					if !failing {
						say("keep-alive renewal failed")
					}
					failing = true
					continue
				}
				failing = false
			}
		}
	})
	return func() {
		cancel()
		wg.Wait()
		_, _, _ = call("DELETE", path, false)
		hc.CloseIdleConnections()
	}, nil
}
