package liveprobe

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"time"

	"github.com/Exploded-views/bitc/internal/provider/sprites"
)

// ErrSecondSprite: a probe tried to create a Sprite while another was live (max-live is 1).
var ErrSecondSprite = errors.New("liveprobe: a second live Sprite was refused (max-live is 1)")

// Env is what a probe sees: the guarded API, the group's Sprite (empty for a probe that needs
// none), the run's random marker and a few helpers. The marker is never printed whole.
type Env struct {
	API    API
	Cfg    Config
	Sprite string
	Marker string // 64 hex digits; ShortMarker is the first 8
	State  map[string]any

	now        func() time.Time
	sleep      func(ctx context.Context, d time.Duration) error
	tryCreate  func(ctx context.Context, name string, labels map[string]string) (int, []byte)
	deleteSelf func(ctx context.Context) error
	nextName   func() string
}

// ShortMarker is the only part of the marker that may be printed.
func (e *Env) ShortMarker() string { return e.Marker[:8] }

// Now is the run's clock.
func (e *Env) Now() time.Time { return e.now() }

// Sleep waits d (or until ctx ends).
func (e *Env) Sleep(ctx context.Context, d time.Duration) error { return e.sleep(ctx, d) }

// NextName is the next Sprite name of the run, bitcproof-s0-<run>-<n>.
func (e *Env) NextName() string { return e.nextName() }

// TryCreate sends a raw create (so a name the client would refuse still reaches the server). The
// Sprite is registered first: if it exists afterwards the caller deletes it before the next create.
func (e *Env) TryCreate(ctx context.Context, name string, labels map[string]string) (int, []byte) {
	return e.tryCreate(ctx, name, labels)
}

// DeleteSelf deletes the group's Sprite now (probe 25 needs it gone).
func (e *Env) DeleteSelf(ctx context.Context) error { return e.deleteSelf(ctx) }

// Sh runs script with sh -c as the exec user.
func (e *Env) Sh(ctx context.Context, script string, stdin []byte) (sprites.ExecResult, error) {
	return e.API.Run(ctx, e.Sprite, sprites.ExecRequest{Cmd: []string{"sh", "-c", script}}, stdin)
}

const rootWrap = `if [ "$(id -u)" = 0 ]; then exec sh -c "$1"; else exec sudo -n sh -c "$1"; fi`

// Root runs script as root: directly when the exec user is root, else through sudo -n.
func (e *Env) Root(ctx context.Context, script string, stdin []byte) (sprites.ExecResult, error) {
	return e.API.Run(ctx, e.Sprite, sprites.ExecRequest{Cmd: []string{"sh", "-c", rootWrap, "_", script}}, stdin)
}

// Nobody runs script as uid 65534 (through root and setpriv).
func (e *Env) Nobody(ctx context.Context, script string) (sprites.ExecResult, error) {
	if _, err := e.Root(ctx, "cat > /tmp/bitcproof-nobody.sh && chmod 644 /tmp/bitcproof-nobody.sh", []byte(script)); err != nil {
		return sprites.ExecResult{}, err
	}
	return e.Root(ctx, "setpriv --reuid=65534 --regid=65534 --clear-groups sh /tmp/bitcproof-nobody.sh", nil)
}

// out is the text of a command result: stdout, then (after a marker line) the tail of stderr.
func out(r sprites.ExecResult) string {
	s := string(r.Stdout)
	if len(r.Stderr) > 0 {
		s += "\n--stderr--\n" + string(r.Stderr)
	}
	return s
}

// rawJSON runs a raw API call and returns the status and the one-line body.
func (e *Env) rawJSON(ctx context.Context, method, path string, body any) (int, string) {
	code, b := e.API.Raw(ctx, method, path, url.Values(nil), body)
	return code, oneLine(string(b), 160)
}

func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		s = strings.ToValidUTF8(s[:n], "") + "..."
	}
	return s
}
