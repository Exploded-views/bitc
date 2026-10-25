package agent

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseHostConfReadsEveryKey(t *testing.T) {
	c, err := ParseHostConf([]byte("# host\nslots=2\nresolver=9.9.9.9\ngateway=192.168.64.1\nnetns=true\nmemory_per_slot=16GiB\nsecret_paths=/var/lib/tailscale/tailscaled.state /etc/bitc/tunnel_ed25519\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := HostConf{Slots: 2, Resolver: "9.9.9.9", Gateway: "192.168.64.1", Netns: true, MemoryPerSlot: 16 << 30,
		SecretPaths: []string{"/var/lib/tailscale/tailscaled.state", "/etc/bitc/tunnel_ed25519"}}
	if !reflect.DeepEqual(c, want) {
		t.Errorf("parsed %+v, want %+v", c, want)
	}
}

func TestParseHostConfDefaults(t *testing.T) {
	c, err := ParseHostConf([]byte("slots=1\nresolver=9.9.9.9\ngateway=10.0.0.1\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !c.Netns || c.MemoryPerSlot != 0 || len(c.SecretPaths) != 0 {
		t.Errorf("defaults: netns %v memory %d secrets %v, want netns true, 0, none", c.Netns, c.MemoryPerSlot, c.SecretPaths)
	}
}

func TestParseHostConfRejects(t *testing.T) {
	for in, why := range map[string]string{
		"resolver=9.9.9.9\ngateway=10.0.0.1\n":                                      "slots",
		"slots=0\nresolver=9.9.9.9\ngateway=10.0.0.1\n":                             "slots",
		"slots=65\nresolver=9.9.9.9\ngateway=10.0.0.1\n":                            "slots",
		"slots=1\ngateway=10.0.0.1\n":                                               "resolver",
		"slots=1\nresolver=dns.example\ngateway=10.0.0.1\n":                         "resolver",
		"slots=1\nresolver=9.9.9.9\n":                                               "gateway",
		"slots=1\nresolver=9.9.9.9\ngateway=::1\n":                                  "gateway",
		"slots=2\nresolver=9.9.9.9\ngateway=10.0.0.1\nnetns=false\n":                "netns",
		"slots=1\nresolver=9.9.9.9\ngateway=10.0.0.1\nnetns=maybe\n":                "netns",
		"slots=1\nresolver=9.9.9.9\ngateway=10.0.0.1\nbogus=1\n":                    "bogus",
		"slots=1\nresolver=9.9.9.9\ngateway=10.0.0.1\nmemory_per_slot=lots\n":       "memory_per_slot",
		"slots=1\nresolver=9.9.9.9\ngateway=10.0.0.1\nsecret_paths=relative/path\n": "secret_paths",
		"slots=1\nslots=2\nresolver=9.9.9.9\ngateway=10.0.0.1\n":                    "slots",
	} {
		_, err := ParseHostConf([]byte(in))
		if err == nil || !strings.Contains(err.Error(), why) {
			t.Errorf("%q: error %v, want one naming %q", in, err, why)
		}
	}
}

func TestParseHostConfMemorySuffixes(t *testing.T) {
	for in, want := range map[string]int64{"512MiB": 512 << 20, "16GiB": 16 << 30, "1TiB": 1 << 40, "1048576": 1 << 20, "2KiB": 2048} {
		c, err := ParseHostConf([]byte("slots=1\nresolver=9.9.9.9\ngateway=10.0.0.1\nmemory_per_slot=" + in + "\n"))
		if err != nil || c.MemoryPerSlot != want {
			t.Errorf("memory_per_slot=%s: %d, %v; want %d", in, c.MemoryPerSlot, err, want)
		}
	}
}
