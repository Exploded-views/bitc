package agent

import (
	"strings"
	"testing"
)

var nftConf = HostConf{Slots: 2, Resolver: "9.9.9.9", Gateway: "192.168.64.1", Netns: true}

func TestRenderNftFillsTheThreeValues(t *testing.T) {
	out, err := RenderNft(nftConf, []int{1001, 1002})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"elements = { 1001, 1002 }",
		`iifname "bv*" ip daddr 9.9.9.9 meta l4proto { udp, tcp } th dport 53 accept`,
		`iifname "bv*" ip daddr 192.168.64.1 drop`,
		`ip daddr 9.9.9.9 meta l4proto { udp, tcp } th dport 53 accept`,
		"ip saddr 10.203.0.0/16 oifname != \"bv*\" masquerade",
		"100.64.0.0/10", "198.18.0.0/15", "169.254.0.0/16", "192.168.0.0/16", "172.16.0.0/12", "224.0.0.0/4",
		`iifname "bv*" meta nfproto ipv6 drop`,
		`meta skuid @slot_uids meta nfproto ipv6 drop`,
		"delete table inet bitc",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered ruleset lacks %q", want)
		}
	}
	if strings.Contains(out, "<") {
		t.Errorf("a placeholder is left in the ruleset: %s", out)
	}
}

func TestRenderNftRefusesBadInput(t *testing.T) {
	for name, tc := range map[string]struct {
		c    HostConf
		uids []int
	}{
		"no uids":      {nftConf, nil},
		"bad resolver": {HostConf{Slots: 1, Resolver: "9.9.9.9; flush ruleset", Gateway: "10.0.0.1"}, []int{1}},
		"bad gateway":  {HostConf{Slots: 1, Resolver: "9.9.9.9", Gateway: "x"}, []int{1}},
		"uid zero":     {nftConf, []int{0}},
	} {
		if out, err := RenderNft(tc.c, tc.uids); err == nil {
			t.Errorf("%s: rendered %d bytes, want an error", name, len(out))
		}
	}
}

const nftLoaded = `{"nftables":[{"metainfo":{"version":"1.0.6","release_name":"x","json_schema_version":1}},
 {"table":{"family":"inet","name":"bitc","handle":3}},
 {"set":{"family":"inet","name":"lan4","table":"bitc","type":"ipv4_addr","handle":4,"flags":["interval"],"elem":[{"prefix":{"addr":"10.0.0.0","len":8}},{"prefix":{"addr":"192.168.0.0","len":16}}]}},
 {"rule":{"family":"inet","table":"bitc","chain":"input","handle":9,"expr":[{"drop":null}]}}]}`

func TestCanonNftIgnoresHandlesMetainfoAndElementOrder(t *testing.T) {
	a, err := CanonNft([]byte(nftLoaded))
	if err != nil || a == "" {
		t.Fatalf("canonical form %q, %v", a, err)
	}
	other := strings.NewReplacer(`"handle":3`, `"handle":77`, `"handle":9`, `"handle":1`, `"release_name":"x"`, `"release_name":"y"`).Replace(nftLoaded)
	other = strings.Replace(other, `[{"prefix":{"addr":"10.0.0.0","len":8}},{"prefix":{"addr":"192.168.0.0","len":16}}]`, `[{"prefix":{"addr":"192.168.0.0","len":16}},{"prefix":{"addr":"10.0.0.0","len":8}}]`, 1)
	b, err := CanonNft([]byte(other))
	if err != nil || a != b {
		t.Errorf("handles, metainfo and set element order must not matter:\n%s\n%s\n%v", a, b, err)
	}
}

func TestCanonNftSeesARuleChange(t *testing.T) {
	a, _ := CanonNft([]byte(nftLoaded))
	b, _ := CanonNft([]byte(strings.Replace(nftLoaded, `{"drop":null}`, `{"accept":null}`, 1)))
	if a == "" || a == b {
		t.Errorf("a drop turned into an accept compares equal: %q vs %q", a, b)
	}
	c, _ := CanonNft([]byte(strings.Replace(nftLoaded, `"len":8`, `"len":9`, 1)))
	if a == c {
		t.Error("a changed set element compares equal")
	}
}

func TestCanonNftRejectsGarbage(t *testing.T) {
	if _, err := CanonNft([]byte("not json")); err == nil {
		t.Error("garbage canonicalised without an error")
	}
}
