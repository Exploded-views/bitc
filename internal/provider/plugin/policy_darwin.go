//go:build darwin

package plugin

// darwinPolicy is what the darwin adapter does about privilege escalation by the plugin. macOS has
// no PR_SET_NO_NEW_PRIVS, so a plugin's process can run setuid and setgid programs that exist on
// the system, where Linux stops it.
type darwinPolicy int

const (
	// darwinAccept runs the plugin and documents the limit: the plugin runs as the user, so run
	// only plugins you trust as you would any program. This is the shipped behaviour. The private
	// copy of the plugin never carries a setuid or setgid bit.
	darwinAccept  darwinPolicy = iota
	darwinRefuse               // refuse every plugin (a config error, exit 78)
	darwinSandbox              // declared only: no sandbox-exec profile is implemented
)

// darwinSetuidPolicy is the one seam for the darwin no_new_privs decision. The default is accept.
var darwinSetuidPolicy = darwinAccept

// setuidPolicy refuses to start any plugin under darwinRefuse. darwinSandbox behaves as
// darwinAccept until it is implemented.
func (p *Provider) setuidPolicy() error {
	if darwinSetuidPolicy == darwinRefuse {
		return p.config("plugins are refused on macOS: it cannot stop a plugin from gaining privileges through setuid programs")
	}
	return nil
}
