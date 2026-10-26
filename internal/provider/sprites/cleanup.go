package sprites

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/Exploded-views/bitc/internal/config"
	"github.com/Exploded-views/bitc/internal/provider"
	"github.com/Exploded-views/bitc/internal/state"
)

// spriteDeleter is the one write a cleanup makes.
type spriteDeleter interface {
	DeleteSprite(ctx context.Context, name string) error
}

// cleanupAPI is all of the API a cleanup handle holds: list and delete. The Client behind it has
// create, exec and attach too, but the handle never sees them (no field or method reaches them).
type cleanupAPI interface {
	spriteLister
	spriteDeleter
}

// Cleaner is the cleanup-only Sprites handle (config.Provider.CleanupOnly): Sprites jobs are
// refused in this release (#6), and `bitc sweep` may still list, owner-check and delete what
// 0.1.0 left. It is built from the API token and endpoint alone; no deploy key, env or job
// setting is read. It implements provider.Cleaner and SweepRules, nothing else.
type Cleaner struct {
	api      cleanupAPI
	set      Settings
	eff      *config.Effective
	prefix   string
	timeout  time.Duration
	secretMu sync.Mutex
	secret   []byte
}

// NewCleaner builds the handle (provider.Factory.NewCleaner).
func NewCleaner(core *config.Provider, eff *config.Effective) (*Cleaner, error) {
	s, errs := DecodeSettings(core.Raw, core.NamePrefix)
	if len(errs) > 0 {
		return nil, errors.New(errs[0])
	}
	if core.APIKey == "" {
		return nil, &provider.ErrUnavailable{Why: "no credentials (set api_key or api_key_file)", Local: true}
	}
	cli, err := New(Options{APIBase: s.APIBase, Org: s.Org, Token: core.APIKey,
		Timeout: max(eff.Scale(30*time.Second), 10*time.Second), HandshakeTimeout: max(eff.Scale(30*time.Second), 500*time.Millisecond)})
	if err != nil {
		return nil, err
	}
	return &Cleaner{api: cli, set: s, eff: eff, prefix: core.NamePrefix, timeout: core.StartTimeout}, nil
}

// List lists the Sprites under the prefixes (a transient failure is retried once).
func (c *Cleaner) List(ctx context.Context, prefixes []string) ([]provider.Resource, error) {
	return listResources(ctx, c.api, c.set, c.eff, prefixes)
}

// Owner is the owner tag of a Sprite whose name verifies under this install's secret, else "".
// It makes no API call: the tag is part of the name.
func (c *Cleaner) Owner(_ context.Context, id string) (string, error) {
	c.secretMu.Lock()
	defer c.secretMu.Unlock()
	if c.secret == nil {
		acct, err := state.AccountDir(c.eff)
		if err != nil {
			return "", err
		}
		if c.secret, err = state.InstallSecret(acct); err != nil {
			return "", err
		}
	}
	if _, ok := ParseName(c.prefix, id); !ok || !VerifyName(c.secret, id) {
		return "", nil
	}
	return provider.OwnerTag(c.secret, id), nil
}

// Teardown deletes the Sprite (404 is done).
func (c *Cleaner) Teardown(ctx context.Context, id string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return c.api.DeleteSprite(ctx, id)
}

// SweepRules are the provider's (one Sprite per job, no self-delete; the timer is required).
func (c *Cleaner) SweepRules() provider.SweepPolicy {
	return provider.SweepPolicy{Noun: "sprite", GoneAfter: c.timeout, TimerRequired: true}
}
