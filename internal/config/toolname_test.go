package config

import (
	"fmt"
	"strings"
	"testing"
)

func TestRemoteToolNamesAreValidated(t *testing.T) {
	sum := strings.Repeat("a", 64)
	for _, bad := range []string{"../../etc", "..", ".", "a/b", ".hidden", "", strings.Repeat("a", 65)} {
		var p problems
		repoRemote(&p, "/r/.bitc.toml", &repoRemoteSchema{Tools: []toolSchema{{Name: bad, URL: "https://x/y.tgz", SHA256: sum}}})
		if len(p.list) != 1 || !strings.Contains(p.list[0], fmt.Sprintf("remote.tools[0] name %q must match", bad)) {
			t.Errorf("name %q: %q", bad, p.list)
		}
	}
	var p problems
	repoRemote(&p, "/r/.bitc.toml", &repoRemoteSchema{Tools: []toolSchema{{Name: "node-20.1_x", URL: "https://x/y.tgz", SHA256: sum}}})
	if len(p.list) != 0 {
		t.Errorf("good name rejected: %q", p.list)
	}
}

func TestRemoteStepAndServiceNamesAreValidated(t *testing.T) {
	for _, bad := range []string{"npm install", "x infra", "x\nrepo", ".hidden", "a/b", strings.Repeat("a", 65)} {
		var p problems
		repoRemote(&p, "/r/.bitc.toml", &repoRemoteSchema{
			Setup:    []stepSchema{{Name: bad, Argv: []string{"true"}}},
			Services: []serviceSchema{{Name: bad, Argv: []string{"true"}, Ready: []string{"true"}}}})
		if len(p.list) != 2 || !strings.Contains(p.list[0], fmt.Sprintf("remote.setup[0] name %q must match", bad)) ||
			!strings.Contains(p.list[1], fmt.Sprintf("remote.services[0] name %q must match", bad)) {
			t.Errorf("name %q: %q", bad, p.list)
		}
	}
	var p problems
	repoRemote(&p, "/r/.bitc.toml", &repoRemoteSchema{
		Setup:    []stepSchema{{Name: "npm-install_2", Argv: []string{"true"}}},
		Services: []serviceSchema{{Name: "db.1", Argv: []string{"true"}, Ready: []string{"true"}}}})
	if len(p.list) != 0 {
		t.Errorf("good names rejected: %q", p.list)
	}
}
