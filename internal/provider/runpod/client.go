package runpod

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Exploded-views/bitc/internal/provider"
)

// client talks REST v2. The key goes only in the Authorization header (never argv, a file or a URL).
type client struct {
	base string
	key  string
	http *http.Client
	// timeout bounds every API request (scaled 10s); the http.Client carries no global Timeout,
	// which would cut the SSE log stream. Zero means 30s.
	timeout time.Duration
}

func (c *client) do(ctx context.Context, method, path string, body any) (int, []byte, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		rd = bytes.NewReader(b)
	}
	d := c.timeout
	if d <= 0 {
		d = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	return resp.StatusCode, b, nil
}

// mapHTTP: 2xx ok; 400 with capacity text transient; other 400/422 ineligible; 401/402/403
// unavailable; 413 ineligible; 429/5xx transient; no response (0) transient (spec 8.7).
func mapHTTP(code int, body []byte, what string) error {
	why := fmt.Sprintf("%s: HTTP %d", what, code)
	if m := apiMessage(body); m != "" && code >= 400 && code < 500 {
		why += ": " + m
	}
	switch {
	case code >= 200 && code < 300:
		return nil
	case code == 0:
		return &provider.ErrTransient{Why: what + ": network error"}
	case code == 400 && capacityText(body):
		return &provider.ErrTransient{Why: why + " (no capacity)"}
	case code == 400 || code == 422 || code == 413:
		return &provider.ErrIneligible{Why: why}
	case code == 401 || code == 402 || code == 403:
		return &provider.ErrUnavailable{Why: why}
	default:
		return &provider.ErrTransient{Why: why}
	}
}

// apiMessage is the first line of the API's error message (problem+json detail or title, or
// message or error), at most 200 bytes; "" when there is none. Only those fields are read, so a
// body that echoes the request env cannot leak it into a log line.
func apiMessage(b []byte) string {
	var e map[string]any
	if json.Unmarshal(b, &e) != nil {
		return ""
	}
	for _, k := range []string{"detail", "message", "error", "title"} {
		s, _ := e[k].(string)
		s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
		s = strings.Map(func(r rune) rune {
			if r < ' ' || r == 0x7f {
				return -1
			}
			return r
		}, s)
		if s = strings.TrimSpace(s); s != "" {
			if len(s) > 200 {
				s = strings.ToValidUTF8(s[:200], "")
			}
			return s
		}
	}
	return ""
}

func capacityText(b []byte) bool {
	s := strings.ToLower(string(b))
	return strings.Contains(s, "no longer any instances") || strings.Contains(s, "capacity") || strings.Contains(s, "not available")
}

// newReq builds an authorised request without a timeout of its own (the caller's ctx bounds it).
func (c *client) newReq(ctx context.Context, method, path string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	return req, nil
}

// idleBody is a response body whose reads must make progress: a Read that stalls for d cancels
// the request (the results server can hold a connection open without sending), and Close releases it.
type idleBody struct {
	rc     io.ReadCloser
	t      *time.Timer
	d      time.Duration
	cancel context.CancelFunc
}

func (b *idleBody) Read(p []byte) (int, error) {
	b.t.Reset(b.d) // the bound covers the server's stall, not the consumer's time between Reads
	n, err := b.rc.Read(p)
	b.t.Stop()
	return n, err
}

func (b *idleBody) Close() error {
	b.t.Stop()
	b.cancel()
	return b.rc.Close()
}

// doBounded sends req and returns a response whose headers must arrive, and whose body must keep
// making progress, within d each. The caller's ctx still bounds the whole.
func doBounded(ctx context.Context, hc *http.Client, req *http.Request, d time.Duration) (*http.Response, error) {
	ctx, cancel := context.WithCancel(ctx)
	t := time.AfterFunc(d, cancel)
	resp, err := hc.Do(req.WithContext(ctx))
	if err != nil {
		t.Stop()
		cancel()
		return nil, err
	}
	t.Stop() // the first Read arms it
	resp.Body = &idleBody{rc: resp.Body, t: t, d: d, cancel: cancel}
	return resp, nil
}

// requestBound is the bound on one API or results-server request: 10s on the [test] time scale,
// never under 3s (a loaded test host must not time out a healthy fake).
func requestBound(scale func(time.Duration) time.Duration) time.Duration {
	if scale == nil {
		return 10 * time.Second
	}
	return max(scale(10*time.Second), 3*time.Second)
}
