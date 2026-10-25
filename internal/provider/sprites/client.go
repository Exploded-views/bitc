// Package sprites is the Fly Sprites client (sprites-provider.md): the REST calls a
// fresh-Sprite-per-job provider needs, the WebSocket exec channel, the error classes of 1.2 and
// the pre-attach / attached classification of 1.3. The provider (S.2) wires it into Launch.
//
// Facts the design could not verify (its section 9) are assumed in the worst direction and
// marked "assumed" below; the live probes settle them.
package sprites

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/Exploded-views/bitc/internal/provider"
)

// DefaultAPIBase is the production API. Tests use the fake's loopback URL only.
const DefaultAPIBase = "https://api.sprites.dev"

// secret hides a credential from every formatting verb.
type secret string

func (secret) String() string               { return "[redacted]" }
func (secret) GoString() string             { return "[redacted]" }
func (secret) Format(f fmt.State, _ rune)   { _, _ = io.WriteString(f, "[redacted]") }
func (secret) MarshalJSON() ([]byte, error) { return []byte(`"[redacted]"`), nil }
func (secret) MarshalText() ([]byte, error) { return []byte("[redacted]"), nil }

// Options configure a Client. Exactly one of Token and TokenFile is set.
type Options struct {
	APIBase   string // default DefaultAPIBase; http only for a loopback host
	Org       string // required; the token's org slug must equal it
	Token     string
	TokenFile string // a 0600 file owned by the user or root; read once, never logged
	HTTP      *http.Client
	// Timeout bounds one REST request (default 30s). HandshakeTimeout bounds the wait for the
	// exec upgrade answer (default 30s).
	Timeout, HandshakeTimeout time.Duration
	// NoSudo drops `sudo -n` from every agent exec (tail, read, the attach), for a Sprite whose
	// exec user is root already (probe 9.15). The default runs the agent through sudo.
	NoSudo bool
}

// Client talks to one Sprites org. It is safe for concurrent use.
type Client struct {
	base      *url.URL
	org       string
	token     secret
	http      *http.Client
	timeout   time.Duration
	handshake time.Duration

	sudo bool

	mu    sync.Mutex
	fresh map[string]int // Sprites this client created: provState of each
}

// Provisioning states of a Sprite this client created (SP8): exec needs polBuild, the attach
// needs polJob.
const (
	polNone  = iota // created, no verified policy yet
	polBuild        // network and resources policies set and verified
	polJob          // the job network policy set and verified
)

// New checks the options and returns a Client. The token is read from TokenFile when given.
func New(o Options) (*Client, error) {
	if o.Org == "" {
		return nil, errors.New("org: required")
	}
	tok := o.Token
	if o.TokenFile != "" {
		if tok != "" {
			return nil, errors.New("set one of Token and TokenFile")
		}
		var err error
		if tok, err = readTokenFile(o.TokenFile); err != nil {
			return nil, err
		}
	}
	if tok == "" {
		return nil, errors.New("no credentials (api_key or api_key_file)")
	}
	slug, has := TokenOrg(tok)
	if !has || slug != o.Org {
		return nil, &OrgTokenError{Slug: slug, Want: o.Org, HasSlug: has}
	}
	base := o.APIBase
	if base == "" {
		base = DefaultAPIBase
	}
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" {
		return nil, fmt.Errorf("api_base %q is not a base URL", base)
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !loopbackHost(u.Hostname()) {
			return nil, fmt.Errorf("api_base %q: http is allowed for a loopback host only", base)
		}
	default:
		return nil, fmt.Errorf("api_base %q: scheme must be https", base)
	}
	u.Path = ""
	c := &Client{base: u, org: o.Org, token: secret(tok), http: o.HTTP, timeout: o.Timeout, handshake: o.HandshakeTimeout, sudo: !o.NoSudo, fresh: map[string]int{}}
	if c.http == nil {
		c.http = &http.Client{}
	}
	// never follow a redirect: Go would copy the Authorization header to the target (TLS only)
	hc := *c.http
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	c.http = &hc
	if c.timeout <= 0 {
		c.timeout = 30 * time.Second
	}
	return c, nil
}

func loopbackHost(h string) bool {
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

func readTokenFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("api_key_file: %w", err)
	}
	defer f.Close()
	var st unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &st); err != nil {
		return "", fmt.Errorf("api_key_file: %w", err)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		return "", fmt.Errorf("api_key_file %s is not a regular file", path)
	}
	if st.Uid != uint32(os.Getuid()) && st.Uid != 0 {
		return "", fmt.Errorf("api_key_file %s is owned by uid %d, not by you or root", path, st.Uid)
	}
	if st.Mode&0o077 != 0 {
		return "", fmt.Errorf("api_key_file %s holds a credential and must be mode 600 (it is %o)", path, st.Mode&0o777)
	}
	b, err := io.ReadAll(io.LimitReader(f, 4097))
	if err != nil || len(b) > 4096 {
		return "", fmt.Errorf("api_key_file %s is unreadable or over 4 KiB", path)
	}
	return strings.TrimSpace(string(b)), nil
}

// String and GoString never show the token.
func (c *Client) String() string   { return fmt.Sprintf("sprites.Client{%s org=%s}", c.base.Host, c.org) }
func (c *Client) GoString() string { return c.String() }

// Org is the configured org slug.
func (c *Client) Org() string { return c.org }

// Secrets are the values a redactor must hide (the token).
func (c *Client) Secrets() []string { return []string{string(c.token)} }

// Endpoint is the host the client talks to (API and WSS share it).
func (c *Client) Endpoint() string { return c.base.String() }

var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

func checkName(n string) error {
	if !nameRE.MatchString(n) {
		return &provider.ErrIneligible{Why: "sprite name " + strconv.Quote(n) + " is not [a-z0-9-], at most 63 characters"}
	}
	return nil
}

// do sends one bounded REST request. code 0 means no answer (network error or timeout).
func (c *Client) do(ctx context.Context, method, path string, q url.Values, body any) (int, []byte) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil
		}
		rd = bytes.NewReader(b)
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	u := *c.base
	u.Path = path
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, method, u.String(), rd)
	if err != nil {
		return 0, nil
	}
	req.Header.Set("Authorization", "Bearer "+string(c.token))
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return 0, nil
	}
	return resp.StatusCode, b
}

// Sprite is the API's view of a Sprite. Organization is checked on every response.
type Sprite struct {
	ID           string            `json:"id"`
	Name         string            `json:"name"`
	Organization string            `json:"organization"`
	Status       string            `json:"status"` // cold | warm | running
	CreatedAt    time.Time         `json:"created_at"`
	Labels       map[string]string `json:"labels,omitempty"`
}

// guard enforces the org guard on a Sprite response. An absent organization is a mismatch: the
// design (6.1, probe 9.11) cannot yet say the field is on every response, and failing open would
// defeat the guard.
func (c *Client) guard(s Sprite) error {
	if s.Organization != c.org {
		return &OrgMismatchError{Name: s.Name, Got: s.Organization, Want: c.org}
	}
	return nil
}

// CreateSprite creates name with labels. It returns:
//   - the Sprite on 201;
//   - ErrTransient when nothing was created (429 and the quota codes, whatever their status);
//   - ErrPreAttach{ID: name} when a Sprite may exist: a 5xx, a network error or a timeout on the
//     POST itself, a 409 (the name exists, left by an earlier attempt), or an org mismatch
//     (the Sprite exists and the caller must delete it);
//   - ErrUnavailable for 401/402/403, ErrIneligible for other 4xx.
//
// A 400 that blames the labels is retried once without them (labels are secondary, 2.1).
func (c *Client) CreateSprite(ctx context.Context, name string, labels map[string]string) (Sprite, error) {
	if err := checkName(name); err != nil {
		return Sprite{}, err
	}
	body := map[string]any{"name": name}
	if len(labels) > 0 {
		body["labels"] = labels
	}
	code, b := c.do(ctx, "POST", "/v1/sprites", nil, body)
	if code == 400 && len(labels) > 0 && strings.Contains(strings.ToLower(string(b)), "label") {
		delete(body, "labels")
		code, b = c.do(ctx, "POST", "/v1/sprites", nil, body)
	}
	if code != 201 && code != 200 {
		err := mapHTTP(code, b, "create sprite")
		ec, _ := parseErr(b)
		switch {
		case code == 409:
			return Sprite{}, PreAttach(name, &provider.ErrTransient{Why: "create sprite: HTTP 409: the name exists"})
		case (code == 0 || code >= 500) && !quotaText(ec, b):
			return Sprite{}, PreAttach(name, err) // 5xx or no answer: the Sprite may have been made
		}
		return Sprite{}, err
	}
	var s Sprite
	if json.Unmarshal(b, &s) != nil || s.Name != name {
		// created, but the answer is unusable: it is an ambiguous create
		return Sprite{}, PreAttach(name, &provider.ErrTransient{Why: "create sprite: unusable answer"})
	}
	if err := c.guard(s); err != nil {
		return s, PreAttach(name, err)
	}
	c.mu.Lock()
	c.fresh[name] = polNone
	c.mu.Unlock()
	return s, nil
}

// GetSprite reads one Sprite; a 404 is ErrNotFound. An org mismatch returns the Sprite and an
// OrgMismatchError.
func (c *Client) GetSprite(ctx context.Context, name string) (Sprite, error) {
	if err := checkName(name); err != nil {
		return Sprite{}, err
	}
	code, b := c.do(ctx, "GET", "/v1/sprites/"+name, nil, nil)
	if err := mapHTTP(code, b, "get sprite"); err != nil {
		return Sprite{}, err
	}
	var s Sprite
	if json.Unmarshal(b, &s) != nil || s.Name != name {
		return Sprite{}, &provider.ErrTransient{Why: "get sprite: unusable answer"}
	}
	return s, c.guard(s)
}

type listPage struct {
	Sprites []Sprite `json:"sprites"`
	HasMore bool     `json:"has_more"`
	Next    string   `json:"next_continuation_token"`
}

const maxPages = 1000

// ListSprites lists every Sprite whose name starts with prefix (the API cannot filter by label),
// following the continuation token to the end. A page repeating the token, or more than 1000
// pages, is an error. Any Sprite of another org fails the list (OrgMismatchError, the Sprite
// named). The result is sorted by name.
func (c *Client) ListSprites(ctx context.Context, prefix string) ([]Sprite, error) {
	var out []Sprite
	seen := map[string]bool{}
	tok := ""
	for page := 0; ; page++ {
		if page >= maxPages {
			return nil, &provider.ErrTransient{Why: "list sprites: more than 1000 pages"}
		}
		q := url.Values{"prefix": {prefix}, "max_results": {"100"}}
		if tok != "" {
			q.Set("continuation_token", tok)
		}
		code, b := c.do(ctx, "GET", "/v1/sprites", q, nil)
		if err := mapHTTP(code, b, "list sprites"); err != nil {
			return nil, err
		}
		var p listPage
		if json.Unmarshal(b, &p) != nil {
			return nil, &provider.ErrTransient{Why: "list sprites: unusable answer"}
		}
		for _, s := range p.Sprites {
			if err := c.guard(s); err != nil {
				return nil, err
			}
			if strings.HasPrefix(s.Name, prefix) && !seen[s.Name] {
				seen[s.Name] = true
				out = append(out, s)
			}
		}
		if !p.HasMore {
			break
		}
		if p.Next == "" || p.Next == tok {
			return nil, &provider.ErrTransient{Why: "list sprites: the continuation token did not advance"}
		}
		tok = p.Next
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// ListConnections lists the org's Connectors (OAuth connections) by name, `GET /v1/oauth/connections`.
//
// The answer's shape is unverified (probe 9.9): a JSON array, or an object holding one under
// "connections", "items" or "data"; an element is a string or an object with a "name", "id" or
// "provider". Only the names are used, sorted.
func (c *Client) ListConnections(ctx context.Context) ([]string, error) {
	code, b := c.do(ctx, "GET", "/v1/oauth/connections", nil, nil)
	if err := mapHTTP(code, b, "list connections"); err != nil {
		return nil, err
	}
	var list []json.RawMessage
	if json.Unmarshal(b, &list) != nil {
		var obj map[string]json.RawMessage
		if json.Unmarshal(b, &obj) != nil {
			return nil, &provider.ErrTransient{Why: "list connections: unusable answer"}
		}
		for _, k := range []string{"connections", "items", "data"} {
			if raw, ok := obj[k]; ok {
				if json.Unmarshal(raw, &list) != nil {
					return nil, &provider.ErrTransient{Why: "list connections: unusable answer"}
				}
				break
			}
		}
	}
	names := []string{}
	for _, raw := range list {
		var s string
		var o struct{ Name, ID, Provider string }
		switch {
		case json.Unmarshal(raw, &s) == nil:
		case json.Unmarshal(raw, &o) == nil:
			s = firstNonEmpty(o.Name, o.Provider, o.ID)
		}
		names = append(names, firstNonEmpty(s, "(unnamed)"))
	}
	sort.Strings(names)
	return names, nil
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}

// DeleteSprite deletes name. nil means it is gone (204 or 404); any error means unconfirmed.
func (c *Client) DeleteSprite(ctx context.Context, name string) error {
	if err := checkName(name); err != nil {
		return err
	}
	code, b := c.do(ctx, "DELETE", "/v1/sprites/"+name, nil, nil)
	err := mapHTTP(code, b, "delete sprite")
	if err == nil || errors.Is(err, ErrNotFound) {
		c.mu.Lock()
		delete(c.fresh, name)
		c.mu.Unlock()
		return nil
	}
	return err
}

// NetRule is one network policy rule: an explicit domain (exact or "*.d") and an action.
type NetRule struct {
	Domain string `json:"domain"`
	Action string `json:"action"` // allow
}

// NetworkPolicy is a list of allowed domains. There is no "include" and no "*" (design decision 4).
type NetworkPolicy struct {
	Rules []NetRule `json:"rules"`
}

// AllowDomains builds an allow-only policy from explicit domains, sorted and de-duplicated.
func AllowDomains(domains ...string) (NetworkPolicy, error) {
	set := map[string]bool{}
	for _, d := range domains {
		if !domainRE.MatchString(d) {
			return NetworkPolicy{}, fmt.Errorf("domain %q is not allowed (explicit domains only)", d)
		}
		set[d] = true
	}
	p := NetworkPolicy{Rules: []NetRule{}}
	for d := range set {
		p.Rules = append(p.Rules, NetRule{Domain: d, Action: "allow"})
	}
	sort.Slice(p.Rules, func(i, j int) bool { return p.Rules[i].Domain < p.Rules[j].Domain })
	return p, nil
}

var domainRE = regexp.MustCompile(`^(\*\.)?([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]([a-z0-9-]{0,61}[a-z0-9])?$`)

// Resources is the memory policy: a limit in MiB, never autoscaled.
type Resources struct {
	LimitMB int `json:"limit_mb"`
}

type resourcesBody struct {
	Memory struct {
		LimitMB   int  `json:"limit_mb"`
		Autoscale bool `json:"autoscale"`
	} `json:"memory"`
}

func (c *Client) policyPath(name, kind string) string {
	return "/v1/sprites/" + name + "/policy/" + kind
}

// ApplyBuildPolicies sets the build network policy and the resources policy of name and reads
// each back; they must match (PolicyMismatchError otherwise). Only then may the Sprite be exec'd
// (SP8): Exec refuses a Sprite this client created until this has succeeded.
func (c *Client) ApplyBuildPolicies(ctx context.Context, name string, build NetworkPolicy, res Resources) error {
	if err := c.setNetwork(ctx, name, build); err != nil {
		return err
	}
	if err := c.setResources(ctx, name, res); err != nil {
		return err
	}
	c.advance(name, polBuild)
	return nil
}

// ApplyJobPolicy replaces the network policy with the job policy and verifies it. The attach
// (Attach) of a Sprite this client created needs it.
func (c *Client) ApplyJobPolicy(ctx context.Context, name string, job NetworkPolicy) error {
	// the job policy comes after the build policies and the resources policy were verified: a
	// Sprite this client created must not skip them (SP8)
	c.mu.Lock()
	cur, guarded := c.fresh[name]
	c.mu.Unlock()
	if guarded && cur < polBuild {
		return ErrNotProvisioned
	}
	if err := c.setNetwork(ctx, name, job); err != nil {
		return err
	}
	c.advance(name, polJob)
	return nil
}

func (c *Client) advance(name string, to int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if cur, ok := c.fresh[name]; ok && cur < to {
		c.fresh[name] = to
	}
}

func (c *Client) setNetwork(ctx context.Context, name string, p NetworkPolicy) error {
	if err := checkName(name); err != nil {
		return err
	}
	if len(p.Rules) == 0 {
		p.Rules = []NetRule{}
	}
	for _, r := range p.Rules {
		if r.Action != "allow" || !domainRE.MatchString(r.Domain) {
			return &provider.ErrIneligible{Why: "network policy rule " + strconv.Quote(r.Domain) + " is not an explicit allow"}
		}
	}
	code, b := c.do(ctx, "POST", c.policyPath(name, "network"), nil, p)
	if err := mapHTTP(code, b, "set network policy"); err != nil {
		return err
	}
	code, b = c.do(ctx, "GET", c.policyPath(name, "network"), nil, nil)
	if err := mapHTTP(code, b, "get network policy"); err != nil {
		return err
	}
	var got NetworkPolicy
	if json.Unmarshal(b, &got) != nil || !samePolicy(p, got) {
		return &PolicyMismatchError{Name: name, Policy: "network"}
	}
	return nil
}

func samePolicy(a, b NetworkPolicy) bool {
	norm := func(p NetworkPolicy) []string {
		var s []string
		for _, r := range p.Rules {
			s = append(s, r.Action+" "+r.Domain)
		}
		sort.Strings(s)
		return s
	}
	x, y := norm(a), norm(b)
	if len(x) != len(y) {
		return false
	}
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}

func (c *Client) setResources(ctx context.Context, name string, r Resources) error {
	if r.LimitMB <= 0 {
		return &provider.ErrIneligible{Why: "resources policy: limit_mb must be positive"}
	}
	var body resourcesBody
	body.Memory.LimitMB, body.Memory.Autoscale = r.LimitMB, false
	code, b := c.do(ctx, "POST", c.policyPath(name, "resources"), nil, body)
	if err := mapHTTP(code, b, "set resources policy"); err != nil {
		return err
	}
	code, b = c.do(ctx, "GET", c.policyPath(name, "resources"), nil, nil)
	if err := mapHTTP(code, b, "get resources policy"); err != nil {
		return err
	}
	var got resourcesBody
	if json.Unmarshal(b, &got) != nil || got != body {
		return &PolicyMismatchError{Name: name, Policy: "resources"}
	}
	return nil
}

// Checkpoint is one checkpoint of a Sprite. Auto marks the platform's automatic ones.
type Checkpoint struct {
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"created_at"`
	Auto      bool      `json:"auto"`
}

// ListCheckpoints lists the checkpoints of name. Assumed (probe 9.1): the list includes the
// automatic ones, which is the worst case for the secrecy tests.
func (c *Client) ListCheckpoints(ctx context.Context, name string) ([]Checkpoint, error) {
	if err := checkName(name); err != nil {
		return nil, err
	}
	code, b := c.do(ctx, "GET", "/v1/sprites/"+name+"/checkpoints", nil, nil)
	if err := mapHTTP(code, b, "list checkpoints"); err != nil {
		return nil, err
	}
	var out struct {
		Checkpoints []Checkpoint `json:"checkpoints"`
	}
	if json.Unmarshal(b, &out) != nil {
		return nil, &provider.ErrTransient{Why: "list checkpoints: unusable answer"}
	}
	return out.Checkpoints, nil
}

// Sudo reports whether agent execs run through `sudo -n` (the default).
func (c *Client) Sudo() bool { return c.sudo }
