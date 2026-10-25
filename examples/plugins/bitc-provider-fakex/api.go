package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// The fakex API is a small REST API for instances (see the fake in ./fakeapi):
//
//	POST   /v1/instances                 {name, size, owner, expires}   create; the instance has no job yet
//	GET    /v1/instances?cursor=&limit=  {instances: [...], next}       one page; next is "" on the last
//	GET    /v1/instances/{id|name}       the instance (404 when gone)
//	DELETE /v1/instances/{id|name}       delete (404 when already gone)
//	POST   /v1/instances/{id}/job        {job, deploy_key}              start the job: from here job input is on the instance
//	GET    /v1/instances/{id}/logs?since=N   {lines, next}              the agent's log lines
//	GET    /v1/instances/{id}/files/{name}?offset=N   bytes             out.log, err.log, result.json, state.json, artifacts.tar.gz
//	GET    /v1/billing/today             {usd}
//
// An instance is {id, name, owner, size, state, why, created, expires, usd_per_hour, exit, began}
// with state provisioning, ready, failed, running, exited.

type instance struct {
	ID         string  `json:"id"`
	Name       string  `json:"name"`
	Owner      string  `json:"owner"`
	Size       string  `json:"size"`
	State      string  `json:"state"`
	Why        string  `json:"why"`
	Created    string  `json:"created"`
	Expires    string  `json:"expires"`
	USDPerHour float64 `json:"usd_per_hour"`
	Exit       int     `json:"exit"`
	Began      bool    `json:"began"`
}

// apiClient talks to the API with the api_key from fd 3.
type apiClient struct {
	base string
	key  string
	hc   *http.Client
}

func newAPIClient(base, key string) *apiClient {
	return &apiClient{base: strings.TrimRight(base, "/"), key: key, hc: &http.Client{Timeout: 60 * time.Second}}
}

// do sends one request and returns the status and body. A transport failure is err; any HTTP
// answer, error statuses included, is returned for the caller to classify.
func (a *apiClient) do(ctx context.Context, method, path string, body any) (int, []byte, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.base+path, rd)
	if err != nil {
		return 0, nil, err
	}
	if a.key != "" {
		req.Header.Set("Authorization", "Bearer "+a.key)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.hc.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return resp.StatusCode, b, err
}

// classify maps a failed call to a protocol error (A.6): what is worth retrying is `transient`
// (the network, 429, 5xx), what no retry can fix on this account is `unavailable` (401, 403), and
// a request the API refuses is `ineligible`.
func classify(op string, status int, body []byte, err error) *rpcError {
	switch {
	case err != nil:
		return transient("%s: %v", op, err)
	case status == http.StatusTooManyRequests || status >= 500:
		return transient("%s: HTTP %d", op, status)
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return &rpcError{Class: "unavailable", Why: fmt.Sprintf("%s: HTTP %d", op, status)}
	case status >= 400:
		return &rpcError{Class: "ineligible", Why: fmt.Sprintf("%s: HTTP %d: %s", op, status, oneLine(body))}
	}
	return nil
}

func oneLine(b []byte) string {
	s := strings.TrimSpace(string(b))
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

func (a *apiClient) create(ctx context.Context, name, size, owner, expires string) (instance, *rpcError) {
	status, b, err := a.do(ctx, "POST", "/v1/instances", map[string]string{"name": name, "size": size, "owner": owner, "expires": expires})
	if e := classify("create", status, b, err); e != nil {
		return instance{}, e
	}
	var in instance
	if json.Unmarshal(b, &in) != nil || in.ID == "" {
		return instance{}, transient("create: the answer has no id")
	}
	return in, nil
}

// get returns the instance, or found=false when the API has none by that id or name.
func (a *apiClient) get(ctx context.Context, idOrName string) (in instance, found bool, e *rpcError) {
	status, b, err := a.do(ctx, "GET", "/v1/instances/"+url.PathEscape(idOrName), nil)
	if status == http.StatusNotFound {
		return instance{}, false, nil
	}
	if e := classify("get", status, b, err); e != nil {
		return instance{}, false, e
	}
	if json.Unmarshal(b, &in) != nil {
		return instance{}, false, transient("get: bad answer")
	}
	return in, true, nil
}

// delete removes the instance; one that is already gone is success (teardown is idempotent).
func (a *apiClient) delete(ctx context.Context, idOrName string) *rpcError {
	status, b, err := a.do(ctx, "DELETE", "/v1/instances/"+url.PathEscape(idOrName), nil)
	if status == http.StatusNotFound {
		return nil
	}
	return classify("delete", status, b, err)
}

// list follows every page: a client that reads only the first page would hide resources from the
// cap, max_live and the sweep (conformance row PS1).
func (a *apiClient) list(ctx context.Context) ([]instance, *rpcError) {
	var all []instance
	cursor := ""
	for {
		status, b, err := a.do(ctx, "GET", "/v1/instances?limit=100&cursor="+url.QueryEscape(cursor), nil)
		if e := classify("list", status, b, err); e != nil {
			return nil, e
		}
		var page struct {
			Instances []instance `json:"instances"`
			Next      string     `json:"next"`
		}
		if json.Unmarshal(b, &page) != nil {
			return nil, transient("list: bad answer")
		}
		all = append(all, page.Instances...)
		if page.Next == "" {
			return all, nil
		}
		cursor = page.Next
	}
}

// file reads a result file of an instance from offset. ok=false means it is not available (yet).
func (a *apiClient) file(ctx context.Context, id, name string, offset int64) (data []byte, ok bool, e *rpcError) {
	status, b, err := a.do(ctx, "GET", fmt.Sprintf("/v1/instances/%s/files/%s?offset=%d", url.PathEscape(id), name, offset), nil)
	if err == nil && (status == http.StatusNotFound || status == http.StatusServiceUnavailable) {
		return nil, false, nil
	}
	if e := classify("read "+name, status, b, err); e != nil {
		return nil, false, e
	}
	return b, true, nil
}

// logs returns the agent's log lines from index since, and the index to ask for next.
func (a *apiClient) logs(ctx context.Context, id string, since int) (lines []string, next int, e *rpcError) {
	status, b, err := a.do(ctx, "GET", fmt.Sprintf("/v1/instances/%s/logs?since=%d", url.PathEscape(id), since), nil)
	if e := classify("logs", status, b, err); e != nil {
		return nil, since, e
	}
	var r struct {
		Lines []string `json:"lines"`
		Next  int      `json:"next"`
	}
	if json.Unmarshal(b, &r) != nil {
		return nil, since, transient("logs: bad answer")
	}
	return r.Lines, r.Next, nil
}
