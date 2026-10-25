package remote

import (
	"context"
	"errors"
	"io"
	"sort"
	"time"

	"github.com/Exploded-views/bitc/internal/config"
	"github.com/Exploded-views/bitc/internal/exit"
	"github.com/Exploded-views/bitc/internal/provider"
	"github.com/Exploded-views/bitc/internal/redact"
	"github.com/Exploded-views/bitc/internal/state"
)

// PreAttachDiscard is what RunJob returns when Launch made a resource, failed before any job
// input reached it, and the effective boundary is "attach" (spec 8.2): the resource was torn
// down (Confirmed) or left for the sweep, and Walk decides on a retry or the next provider.
type PreAttachDiscard struct {
	ID        string
	Inner     error // ErrTransient, ErrUnavailable or ErrIneligible
	Confirmed bool  // the resource is confirmed gone
}

func (d *PreAttachDiscard) Error() string { return d.Inner.Error() }
func (d *PreAttachDiscard) Unwrap() error { return d.Inner }

// carriesResource: the error names a resource that exists (or may exist).
func carriesResource(err error) bool {
	var pre *provider.ErrPreAttach
	var att *provider.ErrAttached
	return errors.As(err, &pre) || errors.As(err, &att)
}

// boundary is the effective fallback boundary: the host key, never later than the declaration.
func (j *Job) boundary() provider.Boundary {
	b, _ := provider.ParseBoundary(j.Core.FallbackUntil)
	if c := j.Prov.Info().Caps.FallbackUntil; b > c {
		b = c
	}
	return b
}

// tearDownByID tears the resource down (10 s bound); true when it is confirmed gone.
func (j *Job) tearDownByID(id string) bool {
	if id == "" {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return j.Prov.Teardown(ctx, id) == nil
}

// classifyLaunch applies spec 8.2 to a Launch error that names a resource. ErrAttached, a
// pre-attach failure under "resource", and a repo-class setup failure are read as attached: the
// resource is torn down and Walk ends the call (75, or 69). A pre-attach failure under "attach"
// becomes a *PreAttachDiscard. Either way j.Created is set: the resource existed and bills.
func (j *Job) classifyLaunch(err error, launchedAt time.Time) error {
	var att *provider.ErrAttached
	var pre *provider.ErrPreAttach
	j.Created = launchedAt
	switch {
	case errors.As(err, &att):
		if !att.Ambiguous { // an ambiguous plugin launch was reaped by name by the adapter (A.5.4)
			j.tearDownByID(att.ID)
		}
	case errors.As(err, &pre):
		confirmed := j.tearDownByID(pre.ID)
		if _, repo := repoSetupFailure(pre.Err); !repo && j.boundary() == provider.UntilAttach {
			return &PreAttachDiscard{ID: pre.ID, Inner: pre.Err, Confirmed: confirmed}
		}
	}
	return err
}

// newAttempt gives the retry its own resource name (the expiry moves one second, so job8 and the
// name format stay intact) and owner tag, and clears what the discarded attempt set.
func (j *Job) newAttempt() error {
	acct, err := state.AccountDir(j.P.Eff)
	if err != nil {
		return err
	}
	secret, err := state.InstallSecret(acct)
	if err != nil {
		return err
	}
	s := j.Spec
	s.Expiry = s.Expiry.Add(time.Second)
	s.Name = provider.NameFor(j.Prov, secret, j.Core.NamePrefix, s.Install, s.Pool, s.SizeKey, j.P.Job, s.Expiry, nameLimit(s.MaxName))
	s.Owner = provider.OwnerTag(secret, s.Name)
	j.Created, j.Handle, j.Estimate = time.Time{}, nil, 0
	return nil
}

// discarded adds the "<id> torn down before attach" / "<id> left for sweep" suffix (spec B.3.6)
// to the inner error, which stays reachable for errors.As.
type discarded struct {
	inner  error
	suffix string
}

func (d *discarded) Error() string { return d.inner.Error() + "; " + d.suffix }
func (d *discarded) Unwrap() error { return d.inner }

// retryRails runs the retry's rails under wctx (cut short by a signal) and reports a signal caught
// before or during them as Interrupted, whatever the rails answered; a release the rails made is
// still returned so Walk's cleanup drops the reservation.
func (j *Job) retryRails(wctx, parent context.Context) (func(), error) {
	if ie := j.interruption(parent); ie != nil {
		return nil, ie
	}
	release, err := Rails(wctx, j)
	if ie := j.interruption(parent); ie != nil {
		return release, ie
	}
	return release, err
}

// nameLimit is the longest resource name for a provider's Info.MaxName (0 means the 63 default).
func nameLimit(maxName int) int {
	if maxName <= 0 || maxName > 63 {
		return 63
	}
	return maxName
}

// NewProvider builds a section's provider and starts it when it needs a process (a plugin).
func NewProvider(ctx context.Context, typ string, core *config.Provider, e *config.Effective, rt provider.Runtime) (provider.Provider, error) {
	f, ok := provider.Lookup(typ, core)
	if !ok {
		return nil, errors.New("not available in this build")
	}
	p, err := f.New(core, e)
	if err != nil {
		return nil, err
	}
	if st, ok := p.(provider.Starter); ok {
		if err := st.Start(ctx, rt); err != nil {
			CloseProvider(p)
			return nil, err
		}
	}
	return p, nil
}

// CloseProvider stops a provider that holds a process.
func CloseProvider(p provider.Provider) {
	if c, ok := p.(io.Closer); ok {
		_ = c.Close()
	}
}

// CheckPlugins starts every configured plugin once, as `bitc config check` does (providers delta
// A.3.1, A.8): a configuration error (hello, configure, the [test] endpoint gate) is 78 with the
// host-file line, a plugin that cannot start is 69.
func CheckPlugins(ctx context.Context, e *config.Effective, stderr io.Writer) error {
	types := make([]string, 0, len(e.Providers))
	for typ, core := range e.Providers {
		if core.Plugin != nil {
			types = append(types, typ)
		}
	}
	sort.Strings(types)
	for _, typ := range types {
		core := e.Providers[typ]
		p, err := NewProvider(ctx, typ, core, e, provider.Runtime{Red: redact.New(), Stderr: stderr, Debug: e.LogLevel == "debug"})
		if err != nil {
			var ec *provider.ErrConfig
			if errors.As(err, &ec) {
				return exit.Configf("%s: %s", e.HostFile, ec.Why)
			}
			return exit.Unavailablef("%s: providers.%s: %v", e.HostFile, typ, err)
		}
		CloseProvider(p)
	}
	return nil
}
