package agent

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func stSpec() SelfTestSpec {
	return DefaultSelfTestSpec(HostConf{Gateway: "192.168.64.1", Netns: true}, "", "10.20.0.50", nil)
}

// probeAs answers every target of spec the way a working firewall does, then applies over.
func probeAs(spec SelfTestSpec, over map[string]ConnResult) NetProbeResult {
	r := NetProbeResult{Connect: map[string]ConnResult{}}
	for _, b := range spec.Block {
		r.Connect[b] = ConnBlocked
	}
	for _, a := range spec.Allow {
		r.Connect[a] = ConnOpen
	}
	for k, v := range over {
		r.Connect[k] = v
	}
	return r
}

func TestSelfTestDefaultTargets(t *testing.T) {
	s := stSpec()
	for _, want := range []string{"192.168.1.1:80", "192.168.1.1:443", "10.20.0.50:22", "192.168.64.1:80",
		"[fd00::1]:443", "[fe80::1]:443", "100.100.100.100:80", "198.18.0.1:80"} {
		if !contains(s.Block, want) {
			t.Errorf("block list lacks %s: %v", want, s.Block)
		}
	}
	for _, want := range []string{"github.com:22", "registry.npmjs.org:443"} {
		if !contains(s.Allow, want) {
			t.Errorf("allow list lacks %s: %v", want, s.Allow)
		}
	}
	s = DefaultSelfTestSpec(HostConf{Gateway: "10.9.9.1"}, "172.16.0.1", "10.20.0.50", nil)
	if !contains(s.Block, "172.16.0.1:80") || !contains(s.Block, "172.16.0.1:443") || contains(s.Block, "192.168.1.1:80") {
		t.Errorf("a configured router is not used: %v", s.Block)
	}
	// the VM's own sshd answers when the firewall is inactive: a job namespace's host end is a
	// block target, so a non-existent router cannot make the test pass vacuously
	if contains(s.Block, "10.203.0.1:22") {
		t.Errorf("netns=false: the namespace host end %s is probed: %v", "10.203.0.1:22", s.Block)
	}
	if n := stSpec(); !contains(n.Block, "10.203.0.1:22") {
		t.Errorf("netns=true: block list lacks the namespace host end 10.203.0.1:22: %v", n.Block)
	}
}

// The controller-facing address is mandatory: the verb refuses to run without it.
func TestSelfTestVerbRequiresTheController(t *testing.T) {
	for _, args := range [][]string{nil, {"--router", "192.168.1.1"}} {
		var out bytes.Buffer
		if code := selfTestVerb(args, &out); code != 64 || out.Len() != 0 {
			t.Errorf("firewall-selftest %v: exit %d, stdout %q; want 64 and nothing (--controller is required)", args, code, out.String())
		}
	}
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

func TestEvalSelfTest(t *testing.T) {
	s := stSpec()
	if _, v := EvalSelfTest(s, probeAs(s, nil)); len(v) != 0 {
		t.Errorf("a working firewall and egress: verdicts %v, want none", v)
	}
	for _, c := range []struct {
		name string
		over map[string]ConnResult
		want []string
	}{
		{"router open", map[string]ConnResult{"192.168.1.1:80": ConnOpen}, []string{VerdictNoFirewall}},
		{"router refused", map[string]ConnResult{"192.168.1.1:443": ConnRefused}, []string{VerdictNoFirewall}},
		{"controller ssh", map[string]ConnResult{"10.20.0.50:22": ConnOpen}, []string{VerdictNoFirewall}},
		{"ula", map[string]ConnResult{"[fd00::1]:443": ConnOpen}, []string{VerdictNoFirewall}},
		{"link-local", map[string]ConnResult{"[fe80::1]:443": ConnRefused}, []string{VerdictNoFirewall}},
		{"cgnat", map[string]ConnResult{"100.100.100.100:80": ConnOpen}, []string{VerdictNoFirewall}},
		{"benchmark range", map[string]ConnResult{"198.18.0.1:80": ConnOpen}, []string{VerdictNoFirewall}},
		{"github blocked", map[string]ConnResult{"github.com:22": ConnBlocked}, []string{VerdictNoEgress}},
		{"npm refused", map[string]ConnResult{"registry.npmjs.org:443": ConnRefused}, []string{VerdictNoEgress}},
		{"vm sshd via the namespace host end", map[string]ConnResult{"10.203.0.1:22": ConnOpen}, []string{VerdictNoFirewall}},
		{"both", map[string]ConnResult{"192.168.1.1:80": ConnOpen, "github.com:22": ConnBlocked}, []string{VerdictNoFirewall, VerdictNoEgress}},
	} {
		_, v := EvalSelfTest(s, probeAs(s, c.over))
		if strings.Join(v, "|") != strings.Join(c.want, "|") {
			t.Errorf("%s: verdicts %v, want %v", c.name, v, c.want)
		}
	}
	// a required target the probe never reported (a name that did not resolve) is not open
	r := probeAs(s, nil)
	delete(r.Connect, "github.com:22")
	if _, v := EvalSelfTest(s, r); len(v) != 1 || v[0] != VerdictNoEgress {
		t.Errorf("an unreported allow target: verdicts %v, want [%s]", v, VerdictNoEgress)
	}
	// a dns failure breaks egress
	r = probeAs(s, nil)
	r.DNSErr = errors.New("i/o timeout")
	if _, v := EvalSelfTest(s, r); len(v) != 1 || v[0] != VerdictNoEgress {
		t.Errorf("dns failure: verdicts %v, want [%s]", v, VerdictNoEgress)
	}
	lines, _ := EvalSelfTest(s, probeAs(s, map[string]ConnResult{"192.168.1.1:80": ConnOpen}))
	if !contains(lines, "REACHED 192.168.1.1:80 (open)") || !contains(lines, "blocked 192.168.1.1:443") || !contains(lines, "egress ok github.com:22") {
		t.Errorf("report lines: %q", lines)
	}
}

func selfTestHost(t *testing.T, res NetProbeResult, err error) (HostEnv, *fakeHost) {
	h := newFakeHost(t)
	e := h.env(false)
	e.NetProbe = func([]string) (NetProbeResult, error) { return res, err }
	return e, h
}

func TestRunFirewallSelfTestExitsAndMarker(t *testing.T) {
	s := stSpec()
	marker := func(h *fakeHost) string { return filepath.Join(h.root, SelfTestMarker) }

	e, h := selfTestHost(t, probeAs(s, nil), nil)
	_ = os.WriteFile(marker(h), []byte("old\n"), 0o600) // a pass clears an earlier failure
	var out bytes.Buffer
	if code := RunFirewallSelfTest(e, s, &out); code != 0 {
		t.Errorf("passing self-test: exit %d, output\n%s", code, out.String())
	}
	if _, err := os.Stat(marker(h)); !os.IsNotExist(err) {
		t.Errorf("the marker survived a passing self-test: %v", err)
	}
	if !strings.Contains(out.String(), "firewall self-test: ok") {
		t.Errorf("no ok line:\n%s", out.String())
	}

	for name, tc := range map[string]struct {
		res  NetProbeResult
		err  error
		want string
	}{
		"router reached": {probeAs(s, map[string]ConnResult{"192.168.1.1:80": ConnOpen}), nil, "FIREWALL NOT ACTIVE"},
		"egress broken":  {probeAs(s, map[string]ConnResult{"github.com:22": ConnBlocked}), nil, "EGRESS BROKEN"},
		"probe error":    {NetProbeResult{}, errors.New("cannot make a namespace"), "SELF-TEST DID NOT RUN"},
	} {
		e, h := selfTestHost(t, tc.res, tc.err)
		out.Reset()
		if code := RunFirewallSelfTest(e, s, &out); code == 0 {
			t.Errorf("%s: exit 0", name)
		}
		if !strings.Contains(out.String(), tc.want) {
			t.Errorf("%s: output lacks %q:\n%s", name, tc.want, out.String())
		}
		fi, err := os.Stat(marker(h))
		if err != nil || fi.Mode().Perm() != 0o600 {
			t.Errorf("%s: marker %v %v, want a 0600 file", name, fi, err)
		}
		if !SelfTestFailed(h.root) {
			t.Errorf("%s: SelfTestFailed is false with the marker present", name)
		}
	}
}

func TestSelfTestMarkerRefusesSlotBoot(t *testing.T) {
	root := t.TempDir()
	var out bytes.Buffer
	if code, refused := selfTestGate(root, &out); refused || code != 0 || out.Len() != 0 {
		t.Errorf("no marker: code %d refused %v out %q", code, refused, out.String())
	}
	if err := os.MkdirAll(filepath.Join(root, "var/lib/bitc"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, SelfTestMarker), []byte("FIREWALL NOT ACTIVE\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, refused := selfTestGate(root, &out)
	if !refused || code != 65 || !strings.HasPrefix(out.String(), "refused: firewall self-test") {
		t.Errorf("marker present: code %d refused %v out %q; want 65 and a `refused: firewall self-test` line", code, refused, out.String())
	}
}

func TestSelfTestAllowReplacesOnlyTheReachTargets(t *testing.T) {
	conf := HostConf{Gateway: "192.168.64.1", Netns: true}
	base := DefaultSelfTestSpec(conf, "", "10.20.0.50", nil)
	got := DefaultSelfTestSpec(conf, "", "10.20.0.50", []string{"203.0.113.7:443", "example.org:22"})
	if want := []string{"203.0.113.7:443", "example.org:22"}; !equalStrings(got.Allow, want) {
		t.Errorf("Allow with --allow = %v, want %v", got.Allow, want)
	}
	if !equalStrings(got.Block, base.Block) {
		t.Errorf("--allow changed the must-fail list:\n got %v\nwant %v", got.Block, base.Block)
	}
	if want := []string{"github.com:22", "registry.npmjs.org:443"}; !equalStrings(base.Allow, want) || !equalStrings(DefaultSelfTestSpec(conf, "", "10.20.0.50", []string{}).Allow, want) {
		t.Errorf("default Allow = %v, want %v", base.Allow, want)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestValidateAllowTarget(t *testing.T) {
	private := []string{
		"10.0.0.1:80", "10.255.255.255:80", "172.16.0.1:80", "172.31.255.255:80", "10.99.0.21:22", "127.0.0.1:80",
		"169.254.169.254:80", "100.64.0.1:80", "100.127.255.255:80", "198.18.0.1:80", "198.19.255.255:80",
		"[fd00::1]:443", "[fc00::1]:443", "[fe80::1]:443", "[febf::1]:443", "[::1]:443", "[::ffff:10.0.0.1]:443",
		"[::ffff:127.0.0.1]:443", "[::ffff:8.8.8.8]:443", "[ff02::1]:443", "localhost:80", "api.localhost:80", "0.0.0.0:80", "[::]:80",
	}
	public := []string{
		"203.0.113.7:443", "8.8.8.8:53", "100.63.255.255:80", "100.128.0.1:80", "172.15.255.255:80", "172.32.0.1:80",
		"198.17.255.255:80", "198.20.0.1:80", "[2001:db8:1::1]:443", "[2606:4700::1111]:443", "github.com:22",
		"registry.npmjs.org:443", "a-b.example:65535",
	}
	malformed := []string{"", "github.com", "github.com:", "github.com:0", "github.com:65536", "github.com:x", ":22", "[::1:22",
		"2001:db8::1:443", "a b:22", "127.1:80", "999.1.1.1:80", "1.2.3:80", "host_name:22", "010.0.0.1:80", "00.1.2.3:80", "010.10.0.1:80"}
	for _, v := range private {
		if err := ValidateAllowTarget(v, false); err == nil {
			t.Errorf("ValidateAllowTarget(%q, false) accepted a private/special address", v)
		}
		if err := ValidateAllowTarget(v, true); err != nil {
			t.Errorf("ValidateAllowTarget(%q, true) = %v; the test-only flag allows it", v, err)
		}
	}
	for _, v := range public {
		for _, flag := range []bool{false, true} {
			if err := ValidateAllowTarget(v, flag); err != nil {
				t.Errorf("ValidateAllowTarget(%q, %v) = %v; want accepted", v, flag, err)
			}
		}
	}
	for _, v := range malformed {
		for _, flag := range []bool{false, true} {
			if err := ValidateAllowTarget(v, flag); err == nil {
				t.Errorf("ValidateAllowTarget(%q, %v) accepted a malformed target", v, flag)
			}
		}
	}
}

func TestSelfTestVerbArguments(t *testing.T) {
	ok := func(args ...string) (string, string, []string) {
		t.Helper()
		r, c, a, code := parseSelfTestArgs(args)
		if code != 0 {
			t.Fatalf("parseSelfTestArgs(%v) = exit %d, want 0", args, code)
		}
		return r, c, a
	}
	r, c, a := ok("--controller", "10.20.0.50", "--allow", "github.com:22", "--allow", "203.0.113.7:443", "--router", "192.168.1.1")
	if r != "192.168.1.1" || c != "10.20.0.50" || !equalStrings(a, []string{"github.com:22", "203.0.113.7:443"}) {
		t.Errorf("parsed router %q controller %q allow %v", r, c, a)
	}
	if _, _, a := ok("--controller", "10.20.0.50", "--allow-test-targets", "--allow", "10.99.0.2:443"); !equalStrings(a, []string{"10.99.0.2:443"}) {
		t.Errorf("--allow-test-targets did not admit a private target: %v", a)
	}
	for _, args := range [][]string{
		{"--controller", "10.20.0.50", "--allow", "10.0.0.1:80"},
		{"--controller", "10.20.0.50", "--allow", "[fd00::1]:443"},
		{"--controller", "10.20.0.50", "--allow", "github.com"},
		{"--controller", "10.20.0.50", "--allow"},
		{"--controller", "10.20.0.50", "--allow-test-targets", "--allow", "x"},
		{"--controller", "10.20.0.50", "--bogus", "1"},
	} {
		if _, _, _, code := parseSelfTestArgs(args); code != 64 {
			t.Errorf("parseSelfTestArgs(%v) exit %d, want 64", args, code)
		}
		var out bytes.Buffer
		if code := selfTestVerb(args, &out); code != 64 || out.Len() != 0 {
			t.Errorf("firewall-selftest %v: exit %d, stdout %q; want 64 and nothing", args, code, out.String())
		}
	}
}
