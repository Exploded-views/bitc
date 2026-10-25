package config

import (
	"reflect"
	"strings"
	"testing"
)

const archSum = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func TestPoolRemoteArchDefaultsToAmd64(t *testing.T) {
	var p problems
	got := poolRemote(&p, "/r/.bitc.toml", "pools.heavy.remote", &poolRemoteSchema{})
	if len(p.list) != 0 || !reflect.DeepEqual(got.Arch, []string{"amd64"}) {
		t.Fatalf("%q %q", p.list, got.Arch)
	}
	p = problems{}
	got = poolRemote(&p, "/r/.bitc.toml", "pools.heavy.remote", &poolRemoteSchema{Arch: []string{"amd64", "arm64"}})
	if len(p.list) != 0 || !reflect.DeepEqual(got.Arch, []string{"amd64", "arm64"}) {
		t.Fatalf("%q %q", p.list, got.Arch)
	}
}

func TestPoolRemoteArchIsValidated(t *testing.T) {
	for _, tc := range []struct {
		arch []string
		want string
	}{
		{[]string{"riscv64"}, `/r/.bitc.toml: pools.heavy.remote.arch: invalid: "riscv64"`},
		{[]string{"amd64", "amd64"}, `/r/.bitc.toml: pools.heavy.remote.arch: invalid: "amd64"`},
		{[]string{""}, `/r/.bitc.toml: pools.heavy.remote.arch: invalid: ""`},
	} {
		var p problems
		poolRemote(&p, "/r/.bitc.toml", "pools.heavy.remote", &poolRemoteSchema{Arch: tc.arch})
		if len(p.list) != 1 || p.list[0] != tc.want {
			t.Errorf("%q: %q", tc.arch, p.list)
		}
	}
}

func TestToolArchDefaultsAndIsValidated(t *testing.T) {
	var p problems
	r := repoRemote(&p, "/r/.bitc.toml", &repoRemoteSchema{Tools: []toolSchema{
		{Name: "node", URL: "https://x/a.tgz", SHA256: archSum},
		{Name: "node", Arch: "arm64", URL: "https://x/b.tgz", SHA256: archSum}}})
	if len(p.list) != 0 || r.Tools[0].Arch != "amd64" || r.Tools[1].Arch != "arm64" {
		t.Fatalf("%q %+v", p.list, r.Tools)
	}
	for _, tc := range []struct {
		tools []toolSchema
		want  string
	}{
		{[]toolSchema{{Name: "node", Arch: "sparc", URL: "https://x/a.tgz", SHA256: archSum}}, "remote.tools: node/sparc: invalid arch"},
		{[]toolSchema{{Name: "node", URL: "https://x/a.tgz", SHA256: archSum}, {Name: "node", Arch: "amd64", URL: "https://x/b.tgz", SHA256: archSum}}, "remote.tools: node/amd64: duplicate"},
	} {
		var p problems
		repoRemote(&p, "/r/.bitc.toml", &repoRemoteSchema{Tools: tc.tools})
		if len(p.list) != 1 || !strings.Contains(p.list[0], tc.want) || !strings.HasPrefix(p.list[0], "/r/.bitc.toml: ") {
			t.Errorf("%q: %q", tc.want, p.list)
		}
	}
}
