package config

import (
	"strings"
	"testing"
)

func cstr(s string) *string { return &s }

func TestContainerOf(t *testing.T) {
	var p problems
	c := containerOf(&p, "f.toml", "pools.box.container", &containerSchema{Image: cstr("alpine:3.20"), Runtime: cstr("docker"),
		Mounts: []string{"/a:/b", "/c:/d:ro", "/e:/f:rw"}, Cpus: 1.5, Memory: cstr("8GiB")})
	if len(p.list) != 0 {
		t.Fatalf("%v", p.list)
	}
	if c.Image != "alpine:3.20" || c.Runtime != "docker" || c.CPUs != "1.5" || c.Memory != 8<<30 || len(c.Mounts) != 3 ||
		c.Mounts[0] != (Mount{Source: "/a", Target: "/b"}) || !c.Mounts[1].ReadOnly || c.Mounts[2].ReadOnly {
		t.Fatalf("%+v", c)
	}
	// defaults: docker, and no limit unless set
	p = problems{}
	c = containerOf(&p, "f.toml", "k", &containerSchema{Image: cstr("img")})
	if len(p.list) != 0 || c.Runtime != "docker" || c.CPUs != "" || c.Memory != 0 || c.Mounts != nil {
		t.Fatalf("%+v %v", c, p.list)
	}
	c = containerOf(&p, "f.toml", "k", &containerSchema{Image: cstr("img"), Cpus: int64(2)})
	if c.CPUs != "2" || len(p.list) != 0 {
		t.Fatalf("%+v %v", c, p.list)
	}
}

func TestContainerOfProblems(t *testing.T) {
	for _, c := range []struct {
		s    containerSchema
		want string
	}{
		{containerSchema{}, "k.image is required"},
		{containerSchema{Image: cstr("")}, "k.image is required"},
		{containerSchema{Image: cstr("-x")}, "k.image"},
		{containerSchema{Image: cstr("a b")}, "k.image"},
		{containerSchema{Image: cstr("x"), Runtime: cstr("podman")}, `k.runtime = "podman"`},
		{containerSchema{Image: cstr("x"), Cpus: int64(0)}, "k.cpus = 0"},
		{containerSchema{Image: cstr("x"), Cpus: int64(1 << 20)}, "k.cpus"},
		{containerSchema{Image: cstr("x"), Cpus: 0.001}, "k.cpus"},
		{containerSchema{Image: cstr("x"), Cpus: "two"}, "k.cpus must be a number"},
		{containerSchema{Image: cstr("x"), Memory: cstr("lots")}, `k.memory = "lots"`},
		{containerSchema{Image: cstr("x"), Memory: cstr("1MiB")}, "below the 6MiB"},
		{containerSchema{Image: cstr("x"), Mounts: []string{"rel:/x"}}, "k.mounts[0]"},
	} {
		var p problems
		containerOf(&p, "f.toml", "k", &c.s)
		if len(p.list) != 1 || !strings.Contains(p.list[0], c.want) {
			t.Errorf("%+v: %v, want %q", c.s, p.list, c.want)
		}
	}
}

func TestParseMount(t *testing.T) {
	for _, c := range []struct {
		in   string
		want string // "" = valid
	}{
		{"/a:/b", ""},
		{"/a:/b:ro", ""},
		{"/a", "must be"},
		{"/a:/b:c:d", "must be"},
		{"/a:/b:zz", `option "zz"`},
		{"a:/b", "absolute"},
		{"/a:b", "absolute"},
		{"/a,x:/b", "comma"},
		{"/a:/b,readonly", "comma"},
	} {
		_, err := parseMount(c.in)
		if (c.want == "") != (err == "") || !strings.Contains(err, c.want) {
			t.Errorf("%q: %q, want %q", c.in, err, c.want)
		}
	}
}
