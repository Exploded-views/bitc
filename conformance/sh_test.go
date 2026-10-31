package conformance

// The non-privileged rows of the SH family (providers delta C.10, H.1): the host checks against
// a fake /proc and /sys tree, the gate script run as the current user with its three paths
// pointed at a temp tree, the setup script's printed texts, and the firewall self-test's
// decision logic with injected probe results. Nothing here touches the real host's kernel
// state, a real network or root.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// ---- SH8: check-host against a fake tree

const shNft = `{"nftables":[{"metainfo":{"version":"1.0.6"}},{"table":{"family":"inet","name":"bitc","handle":1}}]}`

// ---- SH11 and the self-test marker: the gate script as the current user

// gateRig runs a copy of ssh-gate.sh whose three absolute paths point into a temp tree. The
// copy must differ from the original in exactly those three lines.
type gateRig struct {
	t                   *testing.T
	script, base, state string
	conf                string
	sha                 string
}

func newGateRig(t *testing.T) *gateRig {
	t.Helper()
	src, err := os.ReadFile("../scripts/ssh-gate.sh")
	if err != nil {
		t.Fatal(err)
	}
	d := t.TempDir()
	g := &gateRig{t: t, base: filepath.Join(d, "opt"), state: filepath.Join(d, "state"), conf: filepath.Join(d, "host.conf")}
	body := string(src)
	for from, to := range map[string]string{"BASE=/opt/bitc\n": "BASE=" + g.base + "\n", "STATE=/var/lib/bitc\n": "STATE=" + g.state + "\n", "CONF=/etc/bitc/host.conf\n": "CONF=" + g.conf + "\n"} {
		if strings.Count(body, from) != 1 {
			t.Fatalf("ssh-gate.sh lacks the line %q: the test seam is stale", from)
		}
		body = strings.Replace(body, from, to, 1)
	}
	g.script = filepath.Join(d, "gate.sh")
	for _, p := range []string{g.base, g.state, filepath.Join(g.state, "slots/1")} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for p, c := range map[string]string{g.script: body, g.conf: "slots=2\nresolver=9.9.9.9\ngateway=192.168.64.1\n"} {
		if err := os.WriteFile(p, []byte(c), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// a fake agent that prints what the gate handed it
	fake := "#!/bin/sh\necho \"AGENT $*\"\n"
	sum := sha256.Sum256([]byte(fake))
	g.sha = hex.EncodeToString(sum[:])
	ad := filepath.Join(g.base, "agent", g.sha)
	if err := os.MkdirAll(ad, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ad, "bitc"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	return g
}

// run executes the gate with one argument; it returns stdout, the exit code.
func (g *gateRig) run(stdin, cmd string) (string, int) {
	g.t.Helper()
	c := exec.Command("/bin/sh", g.script, cmd)
	c.Stdin = strings.NewReader(stdin)
	var out bytes.Buffer
	c.Stdout = &out
	err := c.Run()
	if ee := (*exec.ExitError)(nil); errors.As(err, &ee) {
		return out.String(), ee.ExitCode()
	}
	if err != nil {
		g.t.Fatal(err)
	}
	return out.String(), 0
}

const (
	nonce32 = "0123456789abcdef0123456789abcdef"
)

func TestSH11_GateRejectsEveryMalformedVerbWith64AndEmptyStdout(t *testing.T) {
	g := newGateRig(t)
	a := "agent " + g.sha + " "
	bad := []string{
		"", " probe", "probe ", "probe  x", "probe x", "probe;id", "probe\nid", "Probe", "probe$(id)", "probe`id`", "probe|id",
		"install", "install xyz", "install " + g.sha + " extra", "install " + strings.ToUpper(g.sha),
		"agent", "agent " + g.sha, "agent xyz check-host", a + "frobnicate", a + "check-host --x", a + "check-host --doctor extra",
		a + "slot-boot", a + "slot-boot --prepare 1 job", a + "slot-boot --other 1 job " + nonce32,
		a + "slot-boot --prepare 0 job " + nonce32, a + "slot-boot --prepare 3 job " + nonce32, a + "slot-boot --prepare 2+1 job " + nonce32,
		a + "slot-boot --prepare 1+1 job " + nonce32, a + "slot-boot --prepare a job " + nonce32,
		a + "slot-boot --prepare 1 -job " + nonce32, a + "slot-boot --prepare 1 a/b " + nonce32, a + "slot-boot --prepare 1 job abc",
		a + "slot-boot --prepare 1 job " + nonce32 + " tag=XYZ", a + "slot-boot --prepare 1 job " + nonce32 + " lease=99999",
		a + "slot-boot --prepare 1 job " + nonce32 + " lease=5 tag=ab", a + "slot-boot --prepare 1 job " + nonce32 + " payload payload",
		a + "slot-boot --prepare 1 job " + nonce32 + " bogus", a + "slot-boot --run 1 job " + nonce32 + " extra",
		a + "tail job 1 2", a + "tail job 1 2 x", a + "tail -x 1 2 3", a + "read job state.json", a + "read job ../x 0", a + "read job state.json.bak 0", a + "read job state.json x",
		a + "list extra", a + "teardown 1", a + "teardown 3 job", a + "teardown 1 a/b", a + "teardown 1 job extra",
	}
	for _, cmd := range bad {
		if out, code := g.run("", cmd); code != 64 || out != "" {
			t.Errorf("gate %q: exit %d, stdout %q; want 64 and nothing", cmd, code, out)
		}
	}
	for _, args := range [][]string{nil, {"probe", "probe"}} {
		c := exec.Command("/bin/sh", append([]string{g.script}, args...)...)
		var out bytes.Buffer
		c.Stdout = &out
		err := c.Run()
		if ee := (*exec.ExitError)(nil); !errors.As(err, &ee) || ee.ExitCode() != 64 || out.Len() != 0 {
			t.Errorf("gate with %d arguments: err %v, stdout %q; want 64 and nothing", len(args), err, out.String())
		}
	}
	// a well-formed verb for an agent that is not installed is not a grammar error
	if _, code := g.run("", "agent "+strings.Repeat("a", 64)+" list"); code != 66 {
		t.Errorf("an uninstalled agent: exit %d, want 66", code)
	}
}

func TestSH11_GateForwardsWellFormedVerbsAndInstallRejectsAShaMismatch(t *testing.T) {
	g := newGateRig(t)
	a := "agent " + g.sha + " "
	for cmd, want := range map[string]string{
		a + "check-host":          "AGENT __agent check-host",
		a + "check-host --doctor": "AGENT __agent check-host --doctor",
		a + "list":                "AGENT __agent slot-list",
		a + "teardown 1+2 job-1":  "AGENT __agent slot-teardown 1+2 job-1",
		a + "slot-boot --prepare 1+2 job-1 " + nonce32:                        "AGENT __agent slot-boot --prepare 1+2 job-1 " + nonce32,
		a + "slot-boot --prepare 1 j " + nonce32 + " tag=ab lease=30 payload": "AGENT __agent slot-boot --prepare 1 j " + nonce32 + " tag=ab lease=30 payload",
		a + "slot-boot --run 1 j " + nonce32:                                  "AGENT __agent slot-boot --run 1 j " + nonce32,
		a + "read job result.json 0":                                          "AGENT __agent --root " + g.state + "/results read job result.json 0",
	} {
		if out, code := g.run("", cmd); code != 0 || strings.TrimSpace(out) != want {
			t.Errorf("gate %q: exit %d, stdout %q; want %q", cmd, code, out, want)
		}
	}
	// install: the bytes go in only when they hash to the sha
	payload := "#!/bin/sh\necho installed\n"
	sum := sha256.Sum256([]byte(payload))
	good := hex.EncodeToString(sum[:])
	wrong := strings.Repeat("0", 64)
	if out, code := g.run(payload, "install "+wrong); code != 65 || out != "" {
		t.Errorf("install with a wrong sha: exit %d, stdout %q; want 65 and nothing", code, out)
	}
	if _, err := os.Stat(filepath.Join(g.base, "agent", wrong)); err == nil {
		t.Error("a mismatching install left a directory behind")
	}
	if ents, _ := filepath.Glob(filepath.Join(g.base, "agent", ".install.*")); len(ents) != 0 {
		t.Errorf("a mismatching install left temp files: %v", ents)
	}
	if out, code := g.run(payload, "install "+good); code != 0 || strings.TrimSpace(out) != "installed "+good {
		t.Errorf("install with the right sha: exit %d, stdout %q", code, out)
	}
	if b, err := os.ReadFile(filepath.Join(g.base, "agent", good, "bitc")); err != nil || string(b) != payload {
		t.Errorf("installed bytes: %q, %v", b, err)
	}
	// an installed agent whose bytes were changed is not run
	if err := os.WriteFile(filepath.Join(g.base, "agent", good, "bitc"), []byte("tampered"), 0o755); err != nil {
		t.Fatal(err)
	}
	if out, code := g.run("", "agent "+good+" list"); code != 65 || out != "" {
		t.Errorf("a tampered agent: exit %d, stdout %q; want 65 and nothing", code, out)
	}
}

// SH25: the firewall self-test's marker makes the gate refuse every slot-boot (fail closed).
func TestSH25_SelfTestMarkerMakesTheGateRefuseSlotBoot(t *testing.T) {
	g := newGateRig(t)
	boot := "agent " + g.sha + " slot-boot --prepare 1 job " + nonce32
	if out, code := g.run("", boot); code != 0 || !strings.HasPrefix(out, "AGENT ") {
		t.Fatalf("without the marker: exit %d, stdout %q", code, out)
	}
	if err := os.WriteFile(filepath.Join(g.state, "selftest-failed"), []byte("FIREWALL NOT ACTIVE\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, cmd := range []string{boot, "agent " + g.sha + " slot-boot --run 1 job " + nonce32} {
		out, code := g.run("", cmd)
		if code != 65 || !strings.HasPrefix(out, "refused: firewall self-test") || strings.Contains(out, "AGENT") {
			t.Errorf("gate %q with the marker: exit %d, stdout %q; want 65, a refusal and no agent run", cmd, code, out)
		}
	}
	// the marker does not stop reading results, listing or tearing down
	for _, cmd := range []string{"probe", "agent " + g.sha + " list", "agent " + g.sha + " teardown 1 job"} {
		if out, code := g.run("", cmd); code != 0 || out == "" {
			t.Errorf("gate %q with the marker: exit %d, stdout %q; want it to work", cmd, code, out)
		}
	}
	// a malformed boot is still 64 with nothing on stdout, whatever part is malformed
	a := "agent " + g.sha + " slot-boot "
	for _, cmd := range []string{
		a + "--prepare 9 job " + nonce32,
		a + "--prepare 1 job " + nonce32 + " bogus",
		a + "--prepare 1 job " + nonce32 + " lease=99999",
		a + "--prepare 1 job " + nonce32 + " lease=5 tag=ab",
		a + "--run 1 job " + nonce32 + " extra",
		a + "--other 1 job " + nonce32,
	} {
		if out, code := g.run("", cmd); code != 64 || out != "" {
			t.Errorf("malformed boot %q with the marker: exit %d, stdout %q; want 64 and nothing", cmd, code, out)
		}
	}
}

// ---- SH26: the setup script prints exactly the texts the agent checks and the runbook shows

func setupPrint(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("/bin/sh", append([]string{"../scripts/ssh-host-setup.sh", "--print"}, args...)...).Output()
	if err != nil {
		t.Fatalf("ssh-host-setup.sh --print %v: %v", args, err)
	}
	return string(out)
}

// The controller-facing address is a mandatory self-test target: a real run without it stops
// before anything changes (validation runs before the root check, so this is safe unprivileged).
func TestSH27_SetupRequiresTheControllerAddress(t *testing.T) {
	out, err := exec.Command("/bin/sh", "../scripts/ssh-host-setup.sh", "--resolver", "9.9.9.9", "--gateway", "192.168.64.1",
		"--controller-key", "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIexample", "--agent", "/bin/true").CombinedOutput()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 2 || !strings.Contains(string(out), "--controller IPV4 is required") {
		t.Errorf("setup without --controller: err %v, output %q; want exit 2 and \"--controller IPV4 is required\"", err, out)
	}
}

// ---- SH28: the runbook and the setup script's header state what the script really does

func TestSH28_RunbookAndHeaderCoverNetnsFalseUndoAndAgentHash(t *testing.T) {
	rb, err := os.ReadFile("../docs/ssh-host.md")
	if err != nil {
		t.Fatal(err)
	}
	sc, err := os.ReadFile("../scripts/ssh-host-setup.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{
		"sysctl -w net.ipv4.ip_forward=0", // undo: deleting the file does not turn forwarding off
		"--netns false",                   // the weaker mode is documented
		"loopback",                        // and its consequence
		"policy accept",                   // forwarding with the forward chain's policy
		"sha256sum",                       // verify the --agent binary before the root run
	} {
		if !strings.Contains(string(rb), w) {
			t.Errorf("runbook never mentions %q", w)
		}
	}
	head := string(sc)
	if i := strings.Index(head, "\n\n"); i > 0 {
		head = head[:i]
	}
	for _, w := range []string{"/run/bitc-probe-resolv.conf", "bv0"} {
		if !strings.Contains(head, w) {
			t.Errorf("ssh-host-setup.sh header never lists %q", w)
		}
	}
}

// ---- SH29: --allow changes only which targets the self-test expects to reach

var (
	allowPrivate = []string{
		"10.0.0.1:80", "172.16.0.1:80", "172.31.255.255:80", "10.99.0.21:22", "127.0.0.1:80", "169.254.169.254:80",
		"100.64.0.1:80", "100.127.255.255:80", "198.18.0.1:80", "198.19.255.255:80", "[fd00::1]:443", "[fc00::1]:443",
		"[fe80::1]:443", "[febf::1]:443", "[::1]:443", "[::ffff:10.0.0.1]:443", "localhost:80", "0.0.0.0:80",
		"[ff02::1]:443", "[::ffff:8.8.8.8]:443",
	}
	allowPublic = []string{
		"203.0.113.7:443", "8.8.8.8:53", "100.63.255.255:80", "100.128.0.1:80", "172.15.255.255:80", "172.32.0.1:80",
		"198.17.255.255:80", "198.20.0.1:80", "[2001:db8:1::1]:443", "github.com:22", "registry.npmjs.org:443",
	}
	allowMalformed = []string{"github.com", "github.com:", "github.com:0", "github.com:65536", "github.com:x", ":22",
		"2001:db8::1:443", "127.1:80", "999.1.1.1:80", "1.2.3:80", "010.0.0.1:80", "00.1.2.3:80", "010.10.0.1:80"}
)

// setupAllow runs the script's --print nft with --allow VALUE (and extra flags): validation runs before
// --print and before anything else, so this is safe unprivileged.
func setupAllow(v string, extra ...string) (string, int, string) {
	args := append([]string{"../scripts/ssh-host-setup.sh", "--print", "nft", "--resolver", "9.9.9.9", "--gateway", "192.168.64.1", "--uids", "996, 997", "--allow", v}, extra...)
	cmd := exec.Command("/bin/sh", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return string(out), ee.ExitCode(), stderr.String()
	}
	if err != nil {
		return string(out), -1, err.Error()
	}
	return string(out), 0, stderr.String()
}

func TestSH29_AllowNeverChangesTheRuleset(t *testing.T) {
	plain := setupPrint(t, "nft", "--resolver", "9.9.9.9", "--gateway", "192.168.64.1", "--uids", "996, 997")
	for _, extra := range [][]string{nil, {"--allow-test-targets"}} {
		for _, v := range []string{"203.0.113.7:443", "example.org:22", "[2001:db8:1::1]:443"} {
			out, code, stderr := setupAllow(v, extra...)
			if code != 0 || out != plain {
				t.Errorf("--print nft --allow %s %v: exit %d (%s); ruleset identical to the one without --allow: %v", v, extra, code, stderr, out == plain)
			}
		}
	}
	if out, code, _ := setupAllow("10.99.0.2:443", "--allow-test-targets"); code != 0 || out != plain {
		t.Errorf("--allow 10.99.0.2:443 --allow-test-targets: exit %d, ruleset identical: %v", code, out == plain)
	}
}

// The script hands the targets to the verb; the ruleset text (nft_rules) never reads them.
func TestSH29_ScriptPassesAllowToTheSelfTestOnly(t *testing.T) {
	b, err := os.ReadFile("../scripts/ssh-host-setup.sh")
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	i := strings.Index(s, "firewall-selftest --router")
	if i < 0 || !strings.Contains(s[i:i+400], "--allow") || !strings.Contains(s[i:], "--allow-test-targets") {
		t.Error("ssh-host-setup.sh does not pass --allow / --allow-test-targets to firewall-selftest")
	}
	j := strings.Index(s, "nft_rules() {")
	if j < 0 {
		t.Fatal("no nft_rules function")
	}
	if end := strings.Index(s[j:], "\n}\n"); end < 0 || strings.Contains(s[j:j+end], "ALLOW") {
		t.Error("nft_rules reads ALLOW: --allow must not change a rule")
	}
}

// The fail-closed marker is transient state, not managed configuration: a real run writes it
// silently (a passing self-test removes it again), so a second run prints no "change:" line for it.
// (The behaviour itself is checked by TestSH18 in the hosted rig; this guards the wiring here.)
func TestSH18_MarkerWriteIsNotAReportedChange(t *testing.T) {
	b, err := os.ReadFile("../scripts/ssh-host-setup.sh")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), `act "mark the firewall self-test as not passed`) {
		t.Error("ssh-host-setup.sh reports the selftest-failed marker through act, so every real run prints a change: line for it")
	}
	if !strings.Contains(string(b), `would: mark the firewall self-test as not passed`) {
		t.Error("a dry run no longer says it would mark the self-test as not passed")
	}
}

// ---- SH33: setup removes the known OrbStack leftovers after the unmount

const obsHelper = "../scripts/ssh-host-orbstack-clean.sh"

// obsTree is a fake root for the cleanup helper. It is a temp dir; the helper is never run
// against the real / and never as root.
type obsTree struct {
	t    *testing.T
	root string
}

func newObsTree(t *testing.T) *obsTree {
	t.Helper()
	return &obsTree{t: t, root: t.TempDir()}
}

func (o *obsTree) p(rel string) string { return filepath.Join(o.root, rel) }

func (o *obsTree) dir(rel string) {
	o.t.Helper()
	if err := os.MkdirAll(o.p(rel), 0o755); err != nil {
		o.t.Fatal(err)
	}
}

func (o *obsTree) file(rel, content string, mode os.FileMode) {
	o.t.Helper()
	o.dir(filepath.Dir(rel))
	if err := os.WriteFile(o.p(rel), []byte(content), mode); err != nil {
		o.t.Fatal(err)
	}
	if err := os.Chmod(o.p(rel), mode); err != nil {
		o.t.Fatal(err)
	}
}

func (o *obsTree) link(rel, text string) {
	o.t.Helper()
	o.dir(filepath.Dir(rel))
	if err := os.Symlink(text, o.p(rel)); err != nil {
		o.t.Fatal(err)
	}
}

// owner is the owner's case: the guest file behind an absolute resolv.conf link, the /mnt/linux
// link and the empty placeholder directories.
func (o *obsTree) owner(linkText string) {
	o.file("opt/orbstack-guest/etc/resolv.conf", "nameserver 192.0.2.1\n", 0o644)
	o.link("etc/resolv.conf", linkText)
	o.link("mnt/linux", "/opt/orbstack-guest/mnt/linux")
	o.dir("mnt/mac")
	o.dir("mnt/machines")
	o.dir("Users/alice")
}

func (o *obsTree) run(args ...string) (string, int) {
	o.t.Helper()
	cmd := exec.Command("/bin/sh", append([]string{obsHelper}, append(args, o.root)...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return string(out) + stderr.String(), ee.ExitCode()
	}
	if err != nil {
		o.t.Fatalf("run %s: %v", obsHelper, err)
	}
	return string(out) + stderr.String(), 0
}

func (o *obsTree) mustRun(args ...string) string {
	o.t.Helper()
	out, code := o.run(args...)
	if code != 0 {
		o.t.Fatalf("%s %v exited %d: %s", obsHelper, args, code, out)
	}
	return out
}

// kind describes a path without following links: "", "link:<text>", "dir", "file:<mode>:<bytes>".
func obsKind(p string) string {
	fi, err := os.Lstat(p)
	if err != nil {
		return ""
	}
	switch {
	case fi.Mode()&os.ModeSymlink != 0:
		s, _ := os.Readlink(p)
		return "link:" + s
	case fi.IsDir():
		return "dir"
	}
	b, _ := os.ReadFile(p)
	return fmt.Sprintf("file:%o:%s", fi.Mode().Perm(), b)
}

// obsSnapshot records every entry under dir (Lstat, so links are not followed) with its kind.
func obsSnapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	m := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		fi, ferr := d.Info()
		if ferr != nil {
			return ferr
		}
		m[rel] = obsKind(p) + fmt.Sprintf("|%o", fi.Mode())
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func obsSame(a, b map[string]string) string {
	for k, v := range a {
		if b[k] != v {
			return fmt.Sprintf("%q: %q became %q", k, v, b[k])
		}
	}
	for k, v := range b {
		if _, ok := a[k]; !ok {
			return fmt.Sprintf("%q appeared as %q", k, v)
		}
	}
	return ""
}

func obsWant(t *testing.T, out string, lines ...string) {
	t.Helper()
	for _, l := range lines {
		if !strings.Contains(out+"\n", l+"\n") {
			t.Errorf("output lacks the line %q:\n%s", l, out)
		}
	}
}

func obsNoPath(t *testing.T, o *obsTree, rels ...string) {
	t.Helper()
	for _, r := range rels {
		if k := obsKind(o.p(r)); k != "" {
			t.Errorf("%s still exists (%s)", r, k)
		}
	}
}

func TestSH33_OwnersCaseIsCleanedAndASecondRunIsOk(t *testing.T) {
	for _, linkText := range []string{"/opt/orbstack-guest/etc/resolv.conf", "../opt/orbstack-guest/etc/resolv.conf"} {
		o := newObsTree(t)
		o.owner(linkText)
		out := o.mustRun()
		if got := obsKind(o.p("etc/resolv.conf")); got != "file:644:nameserver 192.0.2.1\n" {
			t.Errorf("%s: /etc/resolv.conf is %q, want a regular 0644 copy", linkText, got)
		}
		if got := obsKind(o.p("var/lib/bitc/resolv.conf.link")); !strings.HasPrefix(got, "file:600:"+linkText) {
			t.Errorf("%s: saved resolv link text is %q", linkText, got)
		}
		if got := obsKind(o.p("var/lib/bitc/mnt-linux.link")); !strings.HasPrefix(got, "file:600:/opt/orbstack-guest/mnt/linux") {
			t.Errorf("%s: saved /mnt/linux link text is %q", linkText, got)
		}
		if got := obsKind(o.p("var/lib/bitc")); got != "dir" {
			t.Errorf("state dir is %q", got)
		}
		if fi, err := os.Stat(o.p("var/lib/bitc")); err != nil || fi.Mode().Perm() != 0o700 {
			t.Errorf("state dir mode: %v %v", fi, err)
		}
		obsNoPath(t, o, "opt/orbstack-guest", "mnt/linux", "mnt/mac", "mnt/machines", "Users", "etc/resolv.conf.bitc-new")
		if obsKind(o.p("mnt")) != "dir" {
			t.Errorf("/mnt was removed")
		}
		obsWant(t, out,
			"change: replace the /etc/resolv.conf link into /opt/orbstack-guest with a regular copy",
			"change: remove OrbStack leftover /opt/orbstack-guest/etc/resolv.conf",
			"change: remove OrbStack leftover /mnt/linux",
			"change: remove OrbStack leftover /opt/orbstack-guest/etc",
			"change: remove OrbStack leftover /opt/orbstack-guest",
			"change: remove OrbStack leftover /mnt/mac",
			"change: remove OrbStack leftover /mnt/machines",
			"change: remove OrbStack leftover /Users/alice",
			"change: remove OrbStack leftover /Users")
		if strings.Contains(out, o.root) {
			t.Errorf("output prints ROOT: %s", out)
		}
		before := obsSnapshot(t, o.root)
		if again := o.mustRun(); again != "ok: no OrbStack leftovers\n" {
			t.Errorf("second run printed %q, want only the ok line", again)
		}
		if d := obsSame(before, obsSnapshot(t, o.root)); d != "" {
			t.Errorf("second run changed the tree: %s", d)
		}
	}
}

func TestSH33_ResolvConfWithoutARegularTargetIsKept(t *testing.T) {
	warn := "warning: /etc/resolv.conf points into /opt/orbstack-guest but its target is not a regular file; fix DNS by hand"
	for name, setup := range map[string]func(*obsTree){
		"missing": func(o *obsTree) { o.link("etc/resolv.conf", "/opt/orbstack-guest/etc/resolv.conf") },
		"dir": func(o *obsTree) {
			o.dir("opt/orbstack-guest/etc/resolv.conf")
			o.link("etc/resolv.conf", "/opt/orbstack-guest/etc/resolv.conf")
		},
		"chained": func(o *obsTree) {
			o.link("opt/orbstack-guest/etc/resolv.conf", "/etc/hostname")
			o.link("etc/resolv.conf", "/opt/orbstack-guest/etc/resolv.conf")
		},
		"dotdot": func(o *obsTree) {
			o.file("etc/real", "x\n", 0o644)
			o.link("etc/resolv.conf", "/opt/orbstack-guest/../../etc/real")
		},
	} {
		o := newObsTree(t)
		setup(o)
		before := obsSnapshot(t, o.root)
		out := o.mustRun()
		if name == "dotdot" {
			if strings.Contains(out, warn) || obsSame(before, obsSnapshot(t, o.root)) != "" {
				t.Errorf("%s: a link that leaves /opt/orbstack-guest was touched: %s", name, out)
			}
			continue
		}
		obsWant(t, out, warn)
		if got := obsKind(o.p("etc/resolv.conf")); !strings.HasPrefix(got, "link:") {
			t.Errorf("%s: /etc/resolv.conf is now %q", name, got)
		}
		if d := obsSame(before, obsSnapshot(t, o.root)); d != "" {
			t.Errorf("%s: the tree changed: %s", name, d)
		}
	}
}

func TestSH33_GuestResolvIsKeptWhileDnsPointsIntoIt(t *testing.T) {
	o := newObsTree(t)
	o.file("opt/orbstack-guest/etc/resolv.conf", "nameserver 192.0.2.1\n", 0o644)
	o.link("etc/resolv.conf", "/opt/orbstack-guest/etc/resolv.conf")
	// a link whose directory chain is itself a link is no regular target either
	o.dir("elsewhere/etc")
	o.file("elsewhere/etc/resolv.conf", "nameserver 192.0.2.9\n", 0o644)
	if err := os.RemoveAll(o.p("opt/orbstack-guest")); err != nil {
		t.Fatal(err)
	}
	o.link("opt/orbstack-guest", o.p("elsewhere"))
	before := obsSnapshot(t, o.root)
	out := o.mustRun()
	obsWant(t, out, "warning: /etc/resolv.conf points into /opt/orbstack-guest but its target is not a regular file; fix DNS by hand")
	if d := obsSame(before, obsSnapshot(t, o.root)); d != "" {
		t.Errorf("a link on the target path was followed: %s", d)
	}
	if k := obsKind(o.p("elsewhere/etc/resolv.conf")); k != "file:644:nameserver 192.0.2.9\n" {
		t.Errorf("the file outside ROOT changed: %q", k)
	}

	// With a real directory but a target that is not regular, the guest file is kept with a reason.
	o2 := newObsTree(t)
	o2.file("opt/orbstack-guest/etc/resolv.conf", "nameserver 192.0.2.1\n", 0o644)
	o2.link("etc/resolv.conf", "/opt/orbstack-guest/etc/missing")
	out = o2.mustRun()
	obsWant(t, out, "note: /opt/orbstack-guest/etc/resolv.conf kept: /etc/resolv.conf still points into /opt/orbstack-guest")
	if k := obsKind(o2.p("opt/orbstack-guest/etc/resolv.conf")); k != "file:644:nameserver 192.0.2.1\n" {
		t.Errorf("guest file after a kept run: %q", k)
	}
}

func TestSH33_OtherResolvConfIsUntouched(t *testing.T) {
	for name, setup := range map[string]func(*obsTree){
		"regular": func(o *obsTree) { o.file("etc/resolv.conf", "nameserver 192.0.2.7\n", 0o600) },
		"systemd": func(o *obsTree) { o.link("etc/resolv.conf", "/run/systemd/resolve/stub-resolv.conf") },
		"rel":     func(o *obsTree) { o.link("etc/resolv.conf", "../run/systemd/resolve/stub-resolv.conf") },
	} {
		o := newObsTree(t)
		setup(o)
		before := obsSnapshot(t, o.root)
		out := o.mustRun()
		if out != "ok: no OrbStack leftovers\n" {
			t.Errorf("%s: output %q", name, out)
		}
		if d := obsSame(before, obsSnapshot(t, o.root)); d != "" {
			t.Errorf("%s: the tree changed: %s", name, d)
		}
	}
}

func TestSH33_MntLinuxThatIsNotALinkStays(t *testing.T) {
	note := "note: /mnt/linux is not a symlink; left in place (check-host will fail)"
	for name, setup := range map[string]func(*obsTree){
		"file": func(o *obsTree) { o.file("mnt/linux", "x\n", 0o644) },
		"dir":  func(o *obsTree) { o.dir("mnt/linux") },
	} {
		o := newObsTree(t)
		setup(o)
		before := obsSnapshot(t, o.root)
		out := o.mustRun()
		obsWant(t, out, note)
		if d := obsSame(before, obsSnapshot(t, o.root)); d != "" {
			t.Errorf("%s: the tree changed: %s", name, d)
		}
	}
}

func TestSH33_NonEmptyAndUnknownEntriesStay(t *testing.T) {
	o := newObsTree(t)
	o.file("Users/name/keep.txt", "mine\n", 0o640)
	o.file("opt/orbstack-guest/stray.txt", "unknown\n", 0o640)
	o.dir("opt/orbstack-guest/etc")
	o.link("mnt/mac", "/somewhere") // another link: never touched, mnt/mac is not a directory
	before := obsSnapshot(t, o.root)
	out := o.mustRun()
	obsWant(t, out,
		"change: remove OrbStack leftover /opt/orbstack-guest/etc",
		"note: /opt/orbstack-guest is not empty; left in place",
		"note: /Users/name is not empty; left in place",
		"note: /Users is not empty; left in place")
	after := obsSnapshot(t, o.root)
	delete(before, "opt/orbstack-guest/etc")
	if d := obsSame(before, after); d != "" {
		t.Errorf("unexpected change: %s", d)
	}
	if k := obsKind(o.p("Users/name/keep.txt")); k != "file:640:mine\n" {
		t.Errorf("kept file: %q", k)
	}
	if k := obsKind(o.p("opt/orbstack-guest/stray.txt")); k != "file:640:unknown\n" {
		t.Errorf("unknown file: %q", k)
	}
	if k := obsKind(o.p("mnt/mac")); k != "link:/somewhere" {
		t.Errorf("mnt/mac link: %q", k)
	}
}

func TestSH33_SymlinksAreNeverFollowed(t *testing.T) {
	o := newObsTree(t)
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(outside, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(outside, "guest", "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "guest", "etc", "resolv.conf"), []byte("nameserver 192.0.2.5\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	o.link("Users/name", filepath.Join(outside, "empty"))
	o.link("opt/orbstack-guest", filepath.Join(outside, "guest"))
	o.link("etc/resolv.conf", "/opt/orbstack-guest/etc/resolv.conf")
	o.link("mnt/mac", filepath.Join(outside, "empty"))
	beforeOut := obsSnapshot(t, outside)
	beforeRoot := obsSnapshot(t, o.root)
	out := o.mustRun()
	if d := obsSame(beforeOut, obsSnapshot(t, outside)); d != "" {
		t.Errorf("something outside ROOT changed: %s\n%s", d, out)
	}
	if d := obsSame(beforeRoot, obsSnapshot(t, o.root)); d != "" {
		t.Errorf("a link-based path was removed or replaced: %s\n%s", d, out)
	}
	for _, r := range []string{"Users/name", "opt/orbstack-guest", "mnt/mac"} {
		if k := obsKind(o.p(r)); !strings.HasPrefix(k, "link:") {
			t.Errorf("%s is no longer a link: %q", r, k)
		}
	}

	// a state dir behind a link: nothing is written outside ROOT, the link stays in place
	o2 := newObsTree(t)
	o2.owner("/opt/orbstack-guest/etc/resolv.conf")
	out2 := t.TempDir()
	o2.link("var", out2)
	o2.mustRun()
	if n := len(mustReadDir(t, out2)); n != 0 {
		t.Errorf("a state dir was written through a link: %d entries outside ROOT", n)
	}
	if k := obsKind(o2.p("etc/resolv.conf")); !strings.HasPrefix(k, "link:") {
		t.Errorf("resolv.conf replaced although its link text could not be saved: %q", k)
	}
}

func mustReadDir(t *testing.T, d string) []os.DirEntry {
	t.Helper()
	es, err := os.ReadDir(d)
	if err != nil {
		t.Fatal(err)
	}
	return es
}

func TestSH33_DryRunChangesNothing(t *testing.T) {
	o := newObsTree(t)
	o.owner("/opt/orbstack-guest/etc/resolv.conf")
	o.file("Users/bob/x", "x\n", 0o600)
	before := obsSnapshot(t, o.root)
	out := o.mustRun("--dry-run")
	if d := obsSame(before, obsSnapshot(t, o.root)); d != "" {
		t.Errorf("--dry-run changed the tree: %s", d)
	}
	obsWant(t, out,
		"would: replace the /etc/resolv.conf link into /opt/orbstack-guest with a regular copy",
		"would: remove OrbStack leftover /opt/orbstack-guest/etc/resolv.conf",
		"would: remove OrbStack leftover /mnt/linux",
		"would: remove OrbStack leftover /opt/orbstack-guest/etc",
		"would: remove OrbStack leftover /opt/orbstack-guest",
		"would: remove OrbStack leftover /mnt/mac",
		"would: remove OrbStack leftover /Users/alice")
	if strings.Contains(out, "change:") || strings.Contains(out, "ok:") {
		t.Errorf("--dry-run printed a change: or ok: line:\n%s", out)
	}
	if k := obsKind(o.p("var")); k != "" {
		t.Errorf("--dry-run created %q", k)
	}
}

func TestSH33_HelperRejectsBadArguments(t *testing.T) {
	for _, args := range [][]string{{}, {"--bogus", t.TempDir()}, {"relative/root"}, {""}} {
		cmd := exec.Command("/bin/sh", append([]string{obsHelper}, args...)...)
		err := cmd.Run()
		var ee *exec.ExitError
		if !errors.As(err, &ee) || ee.ExitCode() != 2 {
			t.Errorf("%v: got %v, want exit 2", args, err)
		}
	}
}

func TestSH33_SetupCallsTheHelperAfterTheUnmountAndDocumentsIt(t *testing.T) {
	b, err := os.ReadFile("../scripts/ssh-host-setup.sh")
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	um := strings.Index(s, `act "unmount /opt/orbstack-guest"`)
	call := strings.Index(s, `ssh-host-orbstack-clean.sh" /`)
	if um < 0 || call < um {
		t.Errorf("setup does not call the OrbStack cleanup after the unmount (unmount at %d, call at %d)", um, call)
	}
	for _, w := range []string{
		"note: /opt/orbstack-guest is still mounted; leftovers not removed",
		"would: remove the known OrbStack leftovers (/opt/orbstack-guest/etc/resolv.conf, /mnt/linux, empty /opt/orbstack-guest, /mnt/mac, /mnt/machines, /Users dirs) after the unmount",
		"--dry-run /",
	} {
		if !strings.Contains(s, w) {
			t.Errorf("ssh-host-setup.sh lacks %q", w)
		}
	}
	head := s
	if i := strings.Index(head, "\n\n"); i > 0 {
		head = head[:i]
	}
	if !strings.Contains(head, "ssh-host-orbstack-clean.sh") {
		t.Error("the setup header does not list the cleanup helper")
	}
	rb, err := os.ReadFile("../docs/ssh-host.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{"/var/lib/bitc/resolv.conf.link", "/var/lib/bitc/mnt-linux.link", "orb restart", "ssh-host-orbstack-clean.sh"} {
		if !strings.Contains(string(rb), w) {
			t.Errorf("runbook never mentions %q", w)
		}
	}
}
