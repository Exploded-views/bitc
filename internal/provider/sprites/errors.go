package sprites

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/Exploded-views/bitc/internal/provider"
)

// ErrNotFound is matched (errors.Is) by every 404: a Sprite, a policy or a checkpoint that is not
// there. A 404 on a delete means done (sprites-provider.md 2.4).
var ErrNotFound = errors.New("sprites: not found")

// NotFoundError is the 404 answer to an API request.
type NotFoundError struct{ What string }

func (e *NotFoundError) Error() string   { return e.What + ": not found" }
func (e *NotFoundError) Is(t error) bool { return t == ErrNotFound }

// OrgMismatchError: a Sprite response names an organization other than the configured one
// (design decision 11). It is an ErrUnavailable (one pause record): errors.As finds a
// *provider.ErrUnavailable in it. The caller deletes the Sprite at once.
type OrgMismatchError struct {
	Name, Got, Want string
}

func (e *OrgMismatchError) Error() string {
	got := e.Got
	if got == "" {
		got = "no organization"
	}
	return fmt.Sprintf("sprite %s belongs to %q, not %q (org guard)", e.Name, got, e.Want)
}

// ForeignOrg names the Sprite and its organization (provider.ForeignOrg): the sweep, doctor and
// status report it and never touch it.
func (e *OrgMismatchError) ForeignOrg() (string, string) { return e.Name, e.Got }

// PausesAfterAttach: a guard hit after the attach still pauses the provider (design 6.1): the job is
// 75 as usual, and the one pause record keeps the next job from creating in a wrong org.
func (e *OrgMismatchError) PausesAfterAttach() bool { return true }

// As lets errors.As(err, &*provider.ErrUnavailable) match.
func (e *OrgMismatchError) As(target any) bool {
	if t, ok := target.(**provider.ErrUnavailable); ok {
		*t = &provider.ErrUnavailable{Why: e.Error()}
		return true
	}
	return false
}

// OrgTokenError: the token's org slug differs from the configured org, or it carries none.
// The slug is printed only when the token has the "<slug>/<rest>" shape with a slug-shaped first
// field (TokenOrg), because any other token (a Fly macaroon is base64, "/" included) would
// otherwise be printed in part or whole.
type OrgTokenError struct {
	Slug, Want string
	HasSlug    bool
}

// TokenOrg is the token's org slug: its first "/" field when that is lowercase letters, digits and
// dashes (at most 63, starting with a letter or digit); ok is false for any other token.
func TokenOrg(tok string) (slug string, ok bool) {
	slug, _, ok = strings.Cut(tok, "/")
	if !ok || !orgSlug.MatchString(slug) {
		return "", false
	}
	return slug, true
}

var orgSlug = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

func (e *OrgTokenError) Error() string {
	if !e.HasSlug {
		return fmt.Sprintf("org: token carries no org slug (expected %q)", e.Want)
	}
	return fmt.Sprintf("org: token belongs to %q, not %q", e.Slug, e.Want)
}

// PolicyMismatchError: a policy read back after it was set differs from what was set. The
// platform did not take it, so nothing may be exec'd; it is transient (retry on a fresh Sprite).
type PolicyMismatchError struct{ Name, Policy string }

func (e *PolicyMismatchError) Error() string {
	return fmt.Sprintf("sprite %s: %s policy read back differs from the one set", e.Name, e.Policy)
}
func (e *PolicyMismatchError) As(target any) bool {
	if t, ok := target.(**provider.ErrTransient); ok {
		*t = &provider.ErrTransient{Why: e.Error()}
		return true
	}
	return false
}

// errBody is the part of an API error answer the client reads. Only a code and a one-line
// message are ever shown, so a body that echoes a request cannot reach a log line.
type errBody struct {
	Error   any    `json:"error"`
	Code    string `json:"code"`
	Message string `json:"message"`
	Detail  string `json:"detail"`
	Title   string `json:"title"`
}

func parseErr(b []byte) (code, msg string) {
	var e errBody
	if json.Unmarshal(b, &e) != nil {
		return "", ""
	}
	code = strings.ToLower(e.Code)
	switch v := e.Error.(type) {
	case string:
		if code == "" && !strings.ContainsAny(v, " ") {
			code = strings.ToLower(v)
		}
		msg = v
	case map[string]any:
		if s, _ := v["code"].(string); s != "" && code == "" {
			code = strings.ToLower(s)
		}
		if s, _ := v["message"].(string); s != "" {
			msg = s
		}
	}
	for _, s := range []string{e.Message, e.Detail, e.Title} {
		if msg == "" {
			msg = s
		}
	}
	return code, oneLine(msg)
}

func oneLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	s = strings.Map(func(r rune) rune {
		if r < ' ' || r == 0x7f {
			return -1
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	if len(s) > 200 {
		s = strings.ToValidUTF8(s[:200], "")
	}
	return s
}

// quotaText: the codes and texts that mean "no capacity now", whatever the status.
func quotaText(code string, body []byte) bool {
	l := strings.ToLower(string(body))
	for _, k := range []string{"sprite_creation_rate_limited", "concurrent_sprite_limit_exceeded", "max sprites per org exceeded"} {
		if code == k || strings.Contains(l, k) {
			return true
		}
	}
	return false
}

// mapHTTP is the error class of an answer (sprites-provider.md 1.2): 2xx nil; the quota and
// rate-limit codes (any status) and 429 transient; 401, 402 and 403 unavailable; 404 NotFoundError;
// other 4xx ineligible; 5xx and no answer (code 0) transient.
func mapHTTP(code int, body []byte, what string) error {
	why := fmt.Sprintf("%s: HTTP %d", what, code)
	ec, msg := parseErr(body)
	if code >= 400 && code < 500 {
		if ec != "" && !strings.Contains(msg, ec) {
			why += ": " + ec
		}
		if msg != "" {
			why += ": " + msg
		}
	}
	switch {
	case code >= 200 && code < 300:
		return nil
	case code == 0:
		return &provider.ErrTransient{Why: what + ": network error"}
	case code >= 400 && quotaText(ec, body):
		return &provider.ErrTransient{Why: why + " (capacity)"}
	case code == 429:
		return &provider.ErrTransient{Why: why}
	case code == 401 || code == 402 || code == 403:
		return &provider.ErrUnavailable{Why: why}
	case code == 404:
		return &NotFoundError{What: what}
	case code >= 400 && code < 500:
		return &provider.ErrIneligible{Why: fmt.Sprintf("%s: HTTP %d", what, code)} // never with the payload
	default:
		return &provider.ErrTransient{Why: why}
	}
}

// PreAttach wraps a provider error class in ErrPreAttach for the Sprite id (the name): the
// resource exists and no job input reached it.
func PreAttach(id string, err error) error {
	if err == nil {
		return nil
	}
	var pa *provider.ErrPreAttach
	if errors.As(err, &pa) {
		return err
	}
	var tr *provider.ErrTransient
	var un *provider.ErrUnavailable
	var in *provider.ErrIneligible
	var su *provider.ErrSetup
	if !errors.As(err, &tr) && !errors.As(err, &un) && !errors.As(err, &in) && !errors.As(err, &su) {
		err = &provider.ErrTransient{Why: err.Error()} // ErrPreAttach carries one of the four classes
	}
	return &provider.ErrPreAttach{ID: id, Err: err}
}

// TailExitError: `__agent tail` ended with a nonzero exit status (66: the job's directory is not
// there yet, which the first tail after the attach can meet). It is transient.
type TailExitError struct{ Code int }

func (e *TailExitError) Error() string { return fmt.Sprintf("agent tail exited %d", e.Code) }
func (e *TailExitError) As(target any) bool {
	if t, ok := target.(**provider.ErrTransient); ok {
		*t = &provider.ErrTransient{Why: e.Error()}
		return true
	}
	return false
}
