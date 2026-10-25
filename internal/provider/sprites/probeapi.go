package sprites

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/Exploded-views/bitc/internal/provider"
)

// The calls below exist for the live probe kit (cmd/bitc-sprites-probe). The provider
// does not use them. Their request shapes are the research sheet's best reading of OpenAPI 0.1.12
// and are unverified (design 9): a probe records the status it gets and does not trust a shape.

// OrgInfo is the answer of GET /v1/organization: the slug (the first of "slug", "name", "id" or
// "organization" that is a string) and the whole body.
type OrgInfo struct {
	Slug string
	Raw  json.RawMessage
}

// Raw sends one bounded JSON request to a /v1/ path and returns the status (0 for no answer, and
// for a path outside /v1/ or with a dot-dot) and the body. It is the probe kit's way to try an
// endpoint or a body the typed methods do not know.
func (c *Client) Raw(ctx context.Context, method, path string, q url.Values, body any) (int, []byte) {
	if !strings.HasPrefix(path, "/v1/") || strings.Contains(path, "..") || strings.ContainsAny(path, "?#") {
		return 0, nil
	}
	return c.do(ctx, method, path, q, body)
}

// Organization reads GET /v1/organization.
func (c *Client) Organization(ctx context.Context) (OrgInfo, error) {
	code, b := c.do(ctx, "GET", "/v1/organization", nil, nil)
	if err := mapHTTP(code, b, "get organization"); err != nil {
		return OrgInfo{}, err
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(b, &m) != nil {
		return OrgInfo{}, &provider.ErrTransient{Why: "get organization: unusable answer"}
	}
	o := OrgInfo{Raw: json.RawMessage(bytes.TrimSpace(b))}
	for _, k := range []string{"slug", "name", "id", "organization"} {
		var s string
		if json.Unmarshal(m[k], &s) == nil && s != "" {
			o.Slug = s
			break
		}
	}
	return o, nil
}

var ckptIDRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

func (c *Client) ckptPath(name, id string) (string, error) {
	if err := checkName(name); err != nil {
		return "", err
	}
	if !ckptIDRE.MatchString(id) {
		return "", &provider.ErrIneligible{Why: "checkpoint id is not a plain word"}
	}
	return "/v1/sprites/" + name + "/checkpoints/" + id, nil
}

// CreateCheckpoint takes a checkpoint of name (POST .../checkpoint). The answer may be one JSON
// object or NDJSON lines; the first object with an "id" is the checkpoint.
func (c *Client) CreateCheckpoint(ctx context.Context, name, comment string) (Checkpoint, error) {
	if err := checkName(name); err != nil {
		return Checkpoint{}, err
	}
	code, b := c.do(ctx, "POST", "/v1/sprites/"+name+"/checkpoint", nil, map[string]string{"comment": comment})
	if err := mapHTTP(code, b, "create checkpoint"); err != nil {
		return Checkpoint{}, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	for {
		var cp Checkpoint
		if dec.Decode(&cp) != nil {
			break
		}
		if cp.ID != "" {
			return cp, nil
		}
	}
	return Checkpoint{}, &provider.ErrTransient{Why: "create checkpoint: unusable answer"}
}

// GetCheckpoint reads one checkpoint; a 404 is ErrNotFound.
func (c *Client) GetCheckpoint(ctx context.Context, name, id string) (Checkpoint, error) {
	p, err := c.ckptPath(name, id)
	if err != nil {
		return Checkpoint{}, err
	}
	code, b := c.do(ctx, "GET", p, nil, nil)
	if err := mapHTTP(code, b, "get checkpoint"); err != nil {
		return Checkpoint{}, err
	}
	var cp Checkpoint
	if json.Unmarshal(b, &cp) != nil || cp.ID == "" {
		return Checkpoint{}, &provider.ErrTransient{Why: "get checkpoint: unusable answer"}
	}
	return cp, nil
}

// RestoreCheckpoint restores name to the checkpoint.
func (c *Client) RestoreCheckpoint(ctx context.Context, name, id string) error {
	p, err := c.ckptPath(name, id)
	if err != nil {
		return err
	}
	code, b := c.do(ctx, "POST", p+"/restore", nil, nil)
	return mapHTTP(code, b, "restore checkpoint")
}

// DeleteCheckpoint deletes one checkpoint; a 404 is ErrNotFound.
func (c *Client) DeleteCheckpoint(ctx context.Context, name, id string) error {
	p, err := c.ckptPath(name, id)
	if err != nil {
		return err
	}
	code, b := c.do(ctx, "DELETE", p, nil, nil)
	return mapHTTP(code, b, "delete checkpoint")
}

func (c *Client) fsQuery(name, path string) (url.Values, error) {
	if err := checkName(name); err != nil {
		return nil, err
	}
	if !strings.HasPrefix(path, "/") || strings.ContainsRune(path, 0) || len(path) > 4096 {
		return nil, &provider.ErrIneligible{Why: "file path must be absolute"}
	}
	return url.Values{"path": {path}}, nil
}

// stream sends one request with ctx as its only time bound (a 200 MiB read outlasts the REST
// timeout) and copies a 2xx body to w. A non-2xx answer maps like any other.
func (c *Client) stream(ctx context.Context, method, p string, q url.Values, body io.Reader, w io.Writer, what string) (int64, error) {
	u := *c.base
	u.Path = p
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return 0, &provider.ErrIneligible{Why: what + ": bad request"}
	}
	req.Header.Set("Authorization", "Bearer "+string(c.token))
	if body != nil {
		req.Header.Set("Content-Type", "application/octet-stream")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, mapHTTP(0, nil, what)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return 0, mapHTTP(resp.StatusCode, b, what)
	}
	n, err := io.Copy(w, resp.Body)
	if err != nil {
		return n, &provider.ErrTransient{Why: what + ": the stream broke"}
	}
	return n, nil
}

// FSRead streams a file of the Sprite into w (GET .../fs/read?path=) and returns the byte count.
// Only ctx bounds it.
func (c *Client) FSRead(ctx context.Context, name, path string, w io.Writer) (int64, error) {
	q, err := c.fsQuery(name, path)
	if err != nil {
		return 0, err
	}
	return c.stream(ctx, "GET", "/v1/sprites/"+name+"/fs/read", q, nil, w, "fs read")
}

// FSWrite writes data to a file of the Sprite (PUT .../fs/write?path=, parents created).
func (c *Client) FSWrite(ctx context.Context, name, path string, data []byte) error {
	q, err := c.fsQuery(name, path)
	if err != nil {
		return err
	}
	q.Set("mkdirParents", "true")
	_, err = c.stream(ctx, "PUT", "/v1/sprites/"+name+"/fs/write", q, bytes.NewReader(data), io.Discard, "fs write")
	return err
}
