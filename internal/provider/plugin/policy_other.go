//go:build !darwin

package plugin

// setuidPolicy: Linux sets no-new-privs in the shim, so there is nothing to decide here.
func (p *Provider) setuidPolicy() error { return nil }
