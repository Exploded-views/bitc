//go:build linux

package agent

import (
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// The firewall self-test (providers delta C.5.3, owner requirement). The setup script ends with
// it: from inside the host, as the first slot user and through the same network path jobs use,
// every LAN-side target must fail and the two internet targets a job needs must work. Any other
// outcome writes a root-only marker; while it exists, slot-boot refuses (fail closed). A later
// passing self-test removes it.

// SelfTestMarker is the file whose presence means the last self-test did not pass.
const SelfTestMarker = "/var/lib/bitc/selftest-failed"

// The verdicts the self-test prints.
const (
	VerdictNoFirewall = "FIREWALL NOT ACTIVE"
	VerdictNoEgress   = "EGRESS BROKEN"
	verdictDidNotRun  = "SELF-TEST DID NOT RUN"
)

// SelfTestSpec lists what must fail (Block) and what must work (Allow), as host:port.
type SelfTestSpec struct{ Block, Allow []string }

// DefaultSelfTestSpec is the owner's list: the LAN router on 80 and 443 (router, default
// 192.168.1.1), the controller-facing LAN address on 22 (controller, required: the verb refuses
// to run without it), the VM's gateway, a ULA, a link-local, a 100.64/10 and a 198.18/15 sample
// must fail; github.com:22 and registry.npmjs.org:443 must work, unless allow (the --allow
// targets, validated by ValidateAllowTarget) names the reachable targets instead. allow never
// touches Block or the firewall rules: it only changes what the test expects to reach. The VM's own addresses (the
// namespace host end 10.203.0.1:22 and the VM's interface addresses) must fail too: with the
// firewall inactive the VM's own sshd answers, so they cannot pass vacuously the way a router
// that does not exist on this LAN can.
func DefaultSelfTestSpec(conf HostConf, router, controller string, allow []string) SelfTestSpec {
	if router == "" {
		router = "192.168.1.1"
	}
	s := SelfTestSpec{
		Block: []string{router + ":80", router + ":443"},
		Allow: []string{"github.com:22", "registry.npmjs.org:443"},
	}
	if len(allow) > 0 {
		s.Allow = append([]string(nil), allow...) // only which targets must be reachable; Block is untouched
	}
	if controller != "" {
		s.Block = append(s.Block, controller+":22")
	}
	s.Block = append(s.Block, selfTargets(conf)...)
	if conf.Gateway != "" {
		s.Block = append(s.Block, conf.Gateway+":80")
	}
	s.Block = append(s.Block, "[fd00::1]:443", "[fe80::1]:443", "100.100.100.100:80", "198.18.0.1:80")
	return s
}

// EvalSelfTest turns probe results into report lines and verdicts (none means the test passed).
// A target the probe did not report counts as blocked, which fails an Allow target.
func EvalSelfTest(spec SelfTestSpec, res NetProbeResult) (lines, verdicts []string) {
	reached, broken := false, false
	for _, b := range spec.Block {
		switch res.Connect[b] {
		case ConnOpen:
			lines = append(lines, "REACHED "+b+" (open)")
			reached = true
		case ConnRefused:
			lines = append(lines, "REACHED "+b+" (refused)")
			reached = true
		default:
			lines = append(lines, "blocked "+b)
		}
	}
	for _, a := range spec.Allow {
		if res.Connect[a] == ConnOpen {
			lines = append(lines, "egress ok "+a)
		} else {
			lines = append(lines, "NO EGRESS "+a)
			broken = true
		}
	}
	if res.DNSErr != nil {
		lines = append(lines, "NO EGRESS dns: "+res.DNSErr.Error())
		broken = true
	}
	if reached {
		verdicts = append(verdicts, VerdictNoFirewall)
	}
	if broken {
		verdicts = append(verdicts, VerdictNoEgress)
	}
	return lines, verdicts
}

// RunFirewallSelfTest probes with e.NetProbe, prints the report to out and keeps the marker
// (under e.Root) in step with the result. It returns 0 for a pass and 69 for anything else.
func RunFirewallSelfTest(e HostEnv, spec SelfTestSpec, out io.Writer) int {
	marker := filepath.Join(e.Root, SelfTestMarker)
	fail := func(verdicts ...string) int {
		for _, v := range verdicts {
			fmt.Fprintln(out, v)
		}
		if err := os.WriteFile(marker, []byte(strings.Join(verdicts, "; ")+"\n"), 0o600); err != nil {
			fmt.Fprintf(out, "cannot write %s: %v\n", SelfTestMarker, err)
		}
		_ = os.Chmod(marker, 0o600)
		fmt.Fprintln(out, "slot-boot is refused until a firewall self-test passes")
		return 69
	}
	res, err := e.NetProbe(append(append([]string(nil), spec.Block...), spec.Allow...))
	if err != nil {
		fmt.Fprintf(out, "cannot probe: %v\n", err)
		return fail(verdictDidNotRun)
	}
	lines, verdicts := EvalSelfTest(spec, res)
	for _, l := range lines {
		fmt.Fprintln(out, l)
	}
	if len(verdicts) > 0 {
		return fail(verdicts...)
	}
	if err := os.Remove(marker); err != nil && !os.IsNotExist(err) {
		fmt.Fprintf(out, "cannot remove %s: %v\n", SelfTestMarker, err)
		return 69
	}
	fmt.Fprintln(out, "firewall self-test: ok")
	return 0
}

// SelfTestFailed reports whether the marker exists under root.
func SelfTestFailed(root string) bool {
	_, err := os.Lstat(filepath.Join(root, SelfTestMarker))
	return err == nil
}

// selfTestGate refuses slot-boot (65, on stdout) while the marker exists.
func selfTestGate(root string, stdout io.Writer) (int, bool) {
	if !SelfTestFailed(root) {
		return 0, false
	}
	return refuse(stdout, "firewall self-test has not passed; run ssh-host-setup.sh again on the host"), true
}

// ValidateAllowTarget checks one --allow value, host:port with the IPv6 host in brackets. The
// host may be a DNS name or a literal address; a literal private (RFC1918), loopback, link-local,
// CGNAT (100.64/10), 198.18/15, unspecified, multicast or IPv6 ULA address is refused unless
// testTargets (--allow-test-targets) is set, as is localhost. The documentation ranges (203.0.113/24,
// 2001:db8::/32) are not private.
func ValidateAllowTarget(v string, testTargets bool) error {
	host, port, err := net.SplitHostPort(v)
	if err != nil || host == "" {
		return fmt.Errorf("--allow %q: want host:port (an IPv6 host in brackets)", v)
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 || strconv.Itoa(n) != port {
		return fmt.Errorf("--allow %q: the port must be 1-65535", v)
	}
	if strings.ContainsAny(v, " \t\r\n") {
		return fmt.Errorf("--allow %q: no whitespace", v)
	}
	if a, err := netip.ParseAddr(host); err == nil {
		if strings.HasPrefix(v, "[") != a.Is6() {
			return fmt.Errorf("--allow %q: an IPv6 host goes in brackets, an IPv4 host does not", v)
		}
		if !testTargets && (a.Is4In6() || specialAddr(a)) { // an IPv4-mapped literal is refused as the script does
			return fmt.Errorf("--allow %q: a private, loopback, link-local, multicast, IPv4-mapped, CGNAT or 198.18/15 address (only with --allow-test-targets)", v)
		}
		return nil
	}
	if strings.HasPrefix(v, "[") || strings.Contains(host, ":") {
		return fmt.Errorf("--allow %q: not a valid IPv6 address", v)
	}
	labels := strings.Split(strings.ToLower(host), ".")
	for _, l := range labels {
		if l == "" || len(l) > 63 || l[0] == '-' || l[len(l)-1] == '-' || strings.Trim(l, "abcdefghijklmnopqrstuvwxyz0123456789-") != "" {
			return fmt.Errorf("--allow %q: not a valid host name", v)
		}
	}
	if last := labels[len(labels)-1]; last[0] < 'a' || last[0] > 'z' {
		return fmt.Errorf("--allow %q: not a valid host name or IP literal", v) // 127.1, 0x7f.1 and the like
	}
	if !testTargets && (labels[len(labels)-1] == "localhost") {
		return fmt.Errorf("--allow %q: localhost (only with --allow-test-targets)", v)
	}
	return nil
}

// specialAddr reports an address a job must not be allowed to count on reaching.
func specialAddr(a netip.Addr) bool {
	if a.IsLoopback() || a.IsPrivate() || a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() || a.IsMulticast() || a.IsUnspecified() {
		return true
	}
	if a.Is4() {
		b := a.As4()
		return b[0] == 0 || b[0] >= 224 || (b[0] == 100 && b[1]&0xc0 == 64) || (b[0] == 198 && b[1]&0xfe == 18)
	}
	return false
}

// parseSelfTestArgs reads `[--router IPV4] --controller IPV4 [--allow HOST:PORT]... [--allow-test-targets]`.
// code is 0 or 64 (and the usage or reason is on stderr).
func parseSelfTestArgs(args []string) (router, controller string, allow []string, code int) {
	usage := func() (string, string, []string, int) {
		fmt.Fprintln(os.Stderr, "bitc agent: usage: firewall-selftest [--router IPV4] --controller IPV4 [--allow HOST:PORT]... [--allow-test-targets]")
		return "", "", nil, 64
	}
	testTargets := false
	for _, a := range args {
		if a == "--allow-test-targets" {
			testTargets = true
		}
	}
	for i := 0; i < len(args); {
		if args[i] == "--allow-test-targets" {
			i++
			continue
		}
		if i+1 >= len(args) {
			return usage()
		}
		v := args[i+1]
		switch args[i] {
		case "--router", "--controller":
			if !ipv4Literal(v) {
				return usage()
			}
			if args[i] == "--router" {
				router = v
			} else {
				controller = v
			}
		case "--allow":
			if err := ValidateAllowTarget(v, testTargets); err != nil {
				fmt.Fprintln(os.Stderr, "bitc agent: firewall-selftest: "+err.Error())
				return "", "", nil, 64
			}
			allow = append(allow, v)
		default:
			fmt.Fprintln(os.Stderr, "bitc agent: firewall-selftest: unknown argument")
			return "", "", nil, 64
		}
		i += 2
	}
	if controller == "" {
		return usage() // the controller-facing LAN address is a mandatory target
	}
	return router, controller, allow, 0
}

// selfTestVerb is `firewall-selftest [--router IP] --controller IP [--allow HOST:PORT]...
// [--allow-test-targets]`, run by the setup script as root on the host (it is not in the gate's
// grammar). --allow only changes which targets the test expects to reach; it never changes a rule.
func selfTestVerb(args []string, stdout io.Writer) int {
	router, controller, allow, code := parseSelfTestArgs(args)
	if code != 0 {
		return code
	}
	conf, err := loadHostConf()
	if err != nil {
		fmt.Fprintln(stdout, verdictDidNotRun+": "+err.Error())
		return 69
	}
	return RunFirewallSelfTest(NewHostEnv(conf, false), DefaultSelfTestSpec(conf, router, controller, allow), stdout)
}
