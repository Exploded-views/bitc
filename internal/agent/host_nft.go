//go:build linux

package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// nftTemplate is the C.8.7 ruleset. The first two lines make loading idempotent (the table is
// created empty and deleted before it is defined). <uids>, <resolver> and <gateway> are rendered
// from host.conf; scripts/ssh-host-setup.sh renders the same text with sed, and a test keeps the
// two equal.
const nftTemplate = `table inet bitc
delete table inet bitc
table inet bitc {
  set slot_uids { typeof meta skuid; elements = { <uids> } }
  set lan4 { type ipv4_addr; flags interval; elements = {
    0.0.0.0/8, 10.0.0.0/8, 100.64.0.0/10, 127.0.0.0/8, 169.254.0.0/16, 172.16.0.0/12,
    192.0.0.0/24, 192.168.0.0/16, 198.18.0.0/15, 224.0.0.0/4, 240.0.0.0/4 } }
  chain input {
    type filter hook input priority 0; policy accept;
    iifname "bv*" drop
  }
  chain forward {
    type filter hook forward priority 0; policy accept;
    iifname "bv*" ip daddr <resolver> meta l4proto { udp, tcp } th dport 53 accept
    iifname "bv*" ip daddr <gateway> drop
    iifname "bv*" ip daddr @lan4 drop
    iifname "bv*" meta nfproto ipv6 drop
    oifname "bv*" ct state established,related accept
    oifname "bv*" drop
  }
  chain output {
    type filter hook output priority 0; policy accept;
    meta skuid != @slot_uids accept
    oifname "lo" accept
    meta skuid @slot_uids meta nfproto ipv6 drop
    ip daddr <resolver> meta l4proto { udp, tcp } th dport 53 accept
    ip daddr <gateway> drop
    ip daddr @lan4 drop
  }
  chain postrouting {
    type nat hook postrouting priority srcnat; policy accept;
    ip saddr 10.203.0.0/16 oifname != "bv*" masquerade
  }
}
`

// NftTemplate returns the ruleset template with its three placeholders (for the setup script test).
func NftTemplate() string { return nftTemplate }

// RenderNft renders the C.8.7 ruleset for conf and the slot users' uids.
func RenderNft(c HostConf, uids []int) (string, error) {
	if len(uids) == 0 {
		return "", errors.New("nft: no slot uids")
	}
	if !ipv4Literal(c.Resolver) || !ipv4Literal(c.Gateway) {
		return "", fmt.Errorf("nft: resolver and gateway must be IPv4 literals: %q, %q", c.Resolver, c.Gateway)
	}
	var us []string
	for _, u := range uids {
		if u < 1 {
			return "", fmt.Errorf("nft: bad slot uid %d", u)
		}
		us = append(us, strconv.Itoa(u))
	}
	return strings.NewReplacer("<uids>", strings.Join(us, ", "), "<resolver>", c.Resolver, "<gateway>", c.Gateway).Replace(nftTemplate), nil
}

// CanonNft canonicalises `nft -j list table inet bitc` output for comparison: handles and the
// metainfo object are dropped and set elements sorted; rule order is kept.
func CanonNft(j []byte) (string, error) {
	var v any
	if err := json.Unmarshal(j, &v); err != nil {
		return "", fmt.Errorf("nft json: %w", err)
	}
	out, err := json.Marshal(canonNode("", v))
	return string(out), err
}

func canonNode(key string, v any) any {
	switch x := v.(type) {
	case map[string]any:
		m := map[string]any{}
		for k, e := range x {
			if k == "handle" || k == "metainfo" {
				continue
			}
			m[k] = canonNode(k, e)
		}
		return m
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = canonNode(key, e)
		}
		if key == "elem" {
			sort.Slice(out, func(i, j int) bool {
				a, _ := json.Marshal(out[i])
				b, _ := json.Marshal(out[j])
				return string(a) < string(b)
			})
		}
		return out
	}
	return v
}
