package runpod

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/Exploded-views/bitc/internal/provider"
)

// selfDelete runs in the pod at D + 10m: DELETE with the pod-scoped key; if refused, stop it
// (a stopped pod bills for disk until swept). The key travels only in a header.
func selfDelete(ctx context.Context, env provider.SelfDeleteEnv) error {
	base := env.APIBase
	if base == "" {
		base = "https://api.runpod.io"
	}
	c := &client{base: strings.TrimRight(base, "/"), key: env.ScopedKey, http: &http.Client{Timeout: 30 * time.Second}}
	code, _, err := c.do(ctx, "DELETE", "/v2/pods/"+env.ResourceID, nil)
	if err == nil && (code < 300 || code == 404) {
		return nil
	}
	_, _, err = c.do(ctx, "POST", "/v2/pods/"+env.ResourceID+"/action", map[string]string{"action": "stop"})
	return err
}
