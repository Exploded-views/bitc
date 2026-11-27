package liveprobe

import (
	"encoding/json"
	"math"
	"time"
	"unicode/utf8"
)

// The caps of a table write's body. They are the values the probes send and the guard's own
// limit: the spend model is a flat hourly price with no idea of size, so a body is capped here.
const (
	MaxRAMMB   = 2048
	MaxCPUs    = 2
	MaxLimitMB = 16384
)

// MaxRunAfterDisconnect is the largest ExecRequest.MaxRunAfterDisconnect a probe may send to Run
// or Exec: 30s is the largest value sent today.
const MaxRunAfterDisconnect = 30 * time.Second

// The three routes of the raw write table.
const (
	routeSprite     = "sprite"
	routePrivileges = "privileges"
	routeResources  = "resources"
)

// checkTableBody checks the body of a raw table write and returns the bytes to send. It marshals
// the caller's value, decodes it into a map and checks that map, and returns the re-marshal of the
// map: what was checked is exactly what is sent, so a duplicate key or a custom marshaller cannot
// send something the check did not see. The body must be a non-empty JSON object, and so must each
// nested object. Any other key, value or type fails.
func checkTableBody(route string, body any) (json.RawMessage, bool) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, false
	}
	var m map[string]any
	if json.Unmarshal(b, &m) != nil || len(m) == 0 {
		return nil, false
	}
	var ok bool
	switch route {
	case routeSprite:
		ok = checkSpriteBody(m)
	case routePrivileges:
		ok = checkPrivilegesBody(m)
	case routeResources:
		ok = checkResourcesBody(m)
	}
	if !ok {
		return nil, false
	}
	out, err := json.Marshal(m)
	if err != nil {
		return nil, false
	}
	return out, true
}

// object is v as a non-empty JSON object.
func object(v any) (map[string]any, bool) {
	m, ok := v.(map[string]any)
	return m, ok && len(m) > 0
}

// onlyKeys reports whether every key of m is one of keys.
func onlyKeys(m map[string]any, keys ...string) bool {
	for k := range m {
		found := false
		for _, a := range keys {
			found = found || k == a
		}
		if !found {
			return false
		}
	}
	return true
}

// whole is v as a whole number from 1 to hi. encoding/json decodes every number as a float64.
func whole(v any, hi int) bool {
	f, ok := v.(float64)
	return ok && !math.IsNaN(f) && !math.IsInf(f, 0) && f == math.Trunc(f) && f >= 1 && f <= float64(hi)
}

// PUT and PATCH /v1/sprites/{s}: labels (only bitc, a string of at most 16 characters) and config
// (only ram_mb and cpus).
func checkSpriteBody(m map[string]any) bool {
	if !onlyKeys(m, "labels", "config") {
		return false
	}
	if v, has := m["labels"]; has {
		l, ok := object(v)
		if !ok || len(l) != 1 {
			return false
		}
		s, ok := l["bitc"].(string)
		if !ok || utf8.RuneCountInString(s) > 16 {
			return false
		}
	}
	if v, has := m["config"]; has {
		c, ok := object(v)
		if !ok || !onlyKeys(c, "ram_mb", "cpus") {
			return false
		}
		if v, has := c["ram_mb"]; has && !whole(v, MaxRAMMB) {
			return false
		}
		if v, has := c["cpus"]; has && !whole(v, MaxCPUs) {
			return false
		}
	}
	return true
}

// POST .../policy/privileges: exactly one key, profile or mode, with a value from the list.
func checkPrivilegesBody(m map[string]any) bool {
	if len(m) != 1 || !onlyKeys(m, "profile", "mode") {
		return false
	}
	for _, v := range m {
		s, ok := v.(string)
		if !ok || (s != "noNewPrivileges" && s != "minimal") {
			return false
		}
	}
	return true
}

// POST .../policy/resources: memory with limit_mb and autoscale, which must be false (autoscale is
// the uncapped spend path).
func checkResourcesBody(m map[string]any) bool {
	if !onlyKeys(m, "memory") {
		return false
	}
	mm, ok := object(m["memory"])
	if !ok || !onlyKeys(mm, "limit_mb", "autoscale") {
		return false
	}
	if v, has := mm["limit_mb"]; has && !whole(v, MaxLimitMB) {
		return false
	}
	if v, has := mm["autoscale"]; has {
		if b, ok := v.(bool); !ok || b {
			return false
		}
	}
	return true
}
