package provider

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"time"
)

// OwnerTag is HMAC(install_secret, name): sweep tears down only resources whose tag verifies.
func OwnerTag(secret []byte, name string) string {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(name))
	return hex.EncodeToString(m.Sum(nil))
}

// OwnerMatches compares in constant time.
func OwnerMatches(secret []byte, name, tag string) bool {
	return hmac.Equal([]byte(OwnerTag(secret, name)), []byte(tag))
}

// maxName is a conservative resource-name limit (a DNS label); the live proof checks RunPod's.
const maxName = 63

// ResourceName is <prefix>-<install16>-<pool>-<size>-<job8>-<E unix> (spec 8.7). Only the pool
// may contain '-', so the name parses from both ends; the pool is truncated to fit.
func ResourceName(prefix, install, pool, size, job string, expiry time.Time) string {
	return ResourceNameMax(prefix, install, pool, size, job, expiry, maxName)
}

// ResourceNameMax is ResourceName for a provider with a shorter limit (a plugin's max_name, at
// least 60): only the pool is cut, never below one character, so job8 and E always survive.
func ResourceNameMax(prefix, install, pool, size, job string, expiry time.Time, limit int) string {
	sum := sha256.Sum256([]byte(job))
	tail := "-" + size + "-" + hex.EncodeToString(sum[:])[:8] + "-" + strconv.FormatInt(expiry.Unix(), 10)
	head := prefix + "-" + install + "-"
	room := min(limit, maxName) - len(head) - len(tail)
	if room < 1 { // unreachable with the loader's 10-character cap on name_prefix and size keys
		room = 1
	}
	if len(pool) > room {
		pool = strings.TrimRight(pool[:room], "-")
	}
	return head + pool + tail
}

// OwnerMatchesPrefix compares tag, in constant time, with the leading characters of the owner
// tag of name: a resource whose name carries a short tag (Sprites: 8 hex) verifies with it.
func OwnerMatchesPrefix(secret []byte, name, tag string) bool {
	full := OwnerTag(secret, name)
	return len(tag) > 0 && len(tag) <= len(full) && hmac.Equal([]byte(full[:len(tag)]), []byte(tag))
}

// Namer is implemented by a provider whose resource names follow their own rule (Sprites carries
// the owner tag in the name, sprites-provider.md 2.1). limit is Info.MaxName.
type Namer interface {
	ResourceName(secret []byte, prefix, install, pool, size, job string, expiry time.Time, limit int) string
}

// NameFor is the resource name of a job on p: p's own rule when it has one, else the shared
// <prefix>-<install16>-<pool>-<size>-<job8>-<E> of ResourceNameMax.
func NameFor(p Provider, secret []byte, prefix, install, pool, size, job string, expiry time.Time, limit int) string {
	if n, ok := p.(Namer); ok {
		return n.ResourceName(secret, prefix, install, pool, size, job, expiry, limit)
	}
	return ResourceNameMax(prefix, install, pool, size, job, expiry, limit)
}
