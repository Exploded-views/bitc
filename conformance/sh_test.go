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

	"github.com/Exploded-views/bitc/internal/agent"
)

// ---- SH8: check-host against a fake tree

type fakeVM struct {
	t    *testing.T
	root string
	nft  string
	open map[string]bool
}

const shNft = `{"nftables":[{"metainfo":{"version":"1.0.6"}},{"table":{"family":"inet","name":"bitc","handle":1}}]}`

func (v *fakeVM) put(p, content string, mode os.FileMode) {
	v.t.Helper()
	full := filepath.Join(v.root, p)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		v.t.Fatal(err)
	}
	_ = os.Remove(full)
	if err := os.WriteFile(full, []byte(content), mode); err != nil {
		v.t.Fatal(err)
	}
	_ = os.Chmod(full, mode)
}

func (v *fakeVM) dir(p string, mode os.FileMode) {
	v.t.Helper()
	full := filepath.Join(v.root, p)
	if err := os.MkdirAll(full, 0o755); err != nil {
		v.t.Fatal(err)
	}
	_ = os.Chmod(full, mode)
}

func (v *fakeVM) mountinfo(pid int, nsid string, lines ...string) {
	v.t.Helper()
	d := filepath.Join(v.root, fmt.Sprintf("proc/%d", pid))
	if err := os.MkdirAll(filepath.Join(d, "ns"), 0o755); err != nil {
		v.t.Fatal(err)
	}
	_ = os.Remove(filepath.Join(d, "ns/mnt"))
	if err := os.Symlink("mnt:["+nsid+"]", filepath.Join(d, "ns/mnt")); err != nil {
		v.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "mountinfo"), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		v.t.Fatal(err)
	}
}

func mi(mp, fstype string) string {
	return fmt.Sprintf("36 35 98:0 / %s rw,noatime shared:1 - %s src rw", mp, fstype)
}

// newFakeVM is a tree that passes every C.8 item.
func newFakeVM(t *testing.T) *fakeVM {
	t.Helper()
	v := &fakeVM{t: t, root: t.TempDir(), nft: shNft, open: map[string]bool{}}
	v.put("etc/bitc/host.conf", "slots=2\nresolver=9.9.9.9\ngateway=192.168.64.1\nsecret_paths=/etc/bitc/tunnel_key\n", 0o644)
	v.put("etc/passwd", "root:x:0:0:root:/root:/bin/bash\nbitc:x:995:995::/home/bitc:/bin/sh\nbitc-s1:x:996:996::/var/lib/bitc/slots/1/home:/usr/sbin/nologin\nbitc-s2:x:997:997::/var/lib/bitc/slots/2/home:/usr/sbin/nologin\n", 0o644)
	v.put("etc/group", "root:x:0:\nsudo:x:27:\nbitc:x:995:\nbitc-s1:x:996:\nbitc-s2:x:997:\n", 0o644)
	v.put("etc/shadow", "root:*:1:0:99999:7:::\nbitc:!:1:0:99999:7:::\nbitc-s1:!:1:0:99999:7:::\nbitc-s2:!:1:0:99999:7:::\n", 0o640)
	v.put("etc/sudoers", "Defaults env_reset\nroot ALL=(ALL:ALL) ALL\n@includedir /etc/sudoers.d\n", 0o440)
	v.put("etc/sudoers.d/bitc", agent.SudoersText, 0o440)
	v.put("etc/cron.allow", "root\n", 0o644)
	v.put("etc/at.allow", "root\n", 0o644)
	v.put("home/bitc/.ssh/authorized_keys", `restrict,command="/opt/bitc/sbin/bitc-login" ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIexample bitc-controller`+"\n", 0o644)
	v.put("opt/bitc/sbin/bitc-login", "#!/bin/sh\n", 0o755)
	v.put("opt/bitc/sbin/bitc-gate", "#!/bin/sh\n", 0o755)
	v.put("etc/bitc/tunnel_key", "secret", 0o600)
	for _, d := range []string{"var/lib/systemd/linger", "var/spool/cron/crontabs", "var/spool/cron/atjobs", "run", "mnt", "proc/sys/fs/binfmt_misc", "sys/bus/virtio/devices"} {
		v.dir(d, 0o755)
	}
	v.dir("var/lib/bitc", 0o700)
	v.put("proc/sys/net/ipv4/ip_forward", "1\n", 0o644)
	v.put("proc/net/unix", "Num RefCount Protocol Flags Type St Inode Path\n", 0o644)
	for _, c := range []string{"sys/fs/cgroup/cgroup.controllers", "sys/fs/cgroup/bitc/cgroup.controllers"} {
		v.put(c, "cpuset cpu io memory pids\n", 0o444)
	}
	v.put("sys/fs/cgroup/cgroup.subtree_control", "cpuset cpu io memory pids\n", 0o644)
	v.mountinfo(1, "4026531841", mi("/", "ext4"), mi("/proc", "proc"))
	return v
}

func (v *fakeVM) env() agent.HostEnv {
	v.t.Helper()
	b, err := os.ReadFile(filepath.Join(v.root, "etc/bitc/host.conf"))
	if err != nil {
		v.t.Fatal(err)
	}
	conf, err := agent.ParseHostConf(b)
	if err != nil {
		v.t.Fatal(err)
	}
	return agent.HostEnv{Root: v.root, Conf: conf,
		Exec: func(stdin, name string, args ...string) ([]byte, error) {
			switch name {
			case "nft":
				if v.nft == "" {
					return nil, fmt.Errorf("Error: No such file or directory")
				}
				return []byte(v.nft), nil
			case "unshare":
				return []byte(shNft), nil
			case "sshd":
				return []byte("port 22\nallowagentforwarding no\ndisableforwarding yes\nauthenticationmethods publickey\n"), nil
			case "visudo":
				return nil, nil
			}
			return nil, fmt.Errorf("unexpected command %s", name)
		},
		Owner: func(os.FileInfo) (int, int) { return 0, 0 },
		OpenAs: func(uid, gid uint32, path string) error {
			if v.open[path] {
				return nil
			}
			return os.ErrPermission
		},
		Environ: func() []string { return nil },
	}
}

func (v *fakeVM) failLines() []string {
	var out []string
	for _, f := range agent.CheckHost(v.env()) {
		if f.Level == "fail" {
			out = append(out, f.String())
		}
	}
	return out
}

func TestSH8_CheckHostFailsOnEachItemAgainstAFakeProcTree(t *testing.T) {
	if f := newFakeVM(t).failLines(); len(f) != 0 {
		t.Fatalf("the clean fake host fails: %q", f)
	}
	rosetta := "enabled\ninterpreter /media/rosetta/rosetta\nflags: F\noffset 0\nmagic 7f454c4602010100000000000000000002003e00\nmask fffffffffffffffcfffffffffffffffffeffffff\n"
	for _, c := range []struct {
		name string
		fix  func(v *fakeVM)
		want string // the exact fail line (or its start)
	}{
		{"item 1: virtiofs mount", func(v *fakeVM) { v.mountinfo(1, "4026531841", mi("/", "ext4"), mi("/mnt/mac", "virtiofs")) },
			"fail ssh-host-mounts: /mnt/mac (virtiofs) visible; disable host file sharing"},
		{"item 1: 9p mount", func(v *fakeVM) { v.mountinfo(1, "4026531841", mi("/", "ext4"), mi("/Users", "9p")) },
			"fail ssh-host-mounts: /Users (9p) visible; disable host file sharing"},
		{"item 1: only a second mount namespace", func(v *fakeVM) { v.mountinfo(4242, "4026532000", mi("/", "ext4"), mi("/Users", "virtiofs")) },
			"fail ssh-host-mounts: /Users (virtiofs) in mount namespace of pid 4242"},
		{"item 1: virtio-fs device 0x001a beside a mounted share", func(v *fakeVM) {
			v.mountinfo(4242, "4026532000", mi("/", "ext4"), mi("/srv/data", "virtiofs"))
			v.put("sys/bus/virtio/devices/virtio3/device", "0x001a\n", 0o444)
		},
			"fail ssh-host-mounts: virtio-fs device virtio3"},
		{"item 1: 9p device 0x0009 beside a mounted share", func(v *fakeVM) {
			v.mountinfo(4242, "4026532000", mi("/", "ext4"), mi("/srv/data", "virtiofs"))
			v.put("sys/bus/virtio/devices/virtio4/device", "0x0009\n", 0o444)
		},
			"fail ssh-host-mounts: 9p device virtio4 present; remove the share from the VM"},
		{"item 1: virtiofs tag file beside a mounted share", func(v *fakeVM) {
			v.mountinfo(4242, "4026532000", mi("/", "ext4"), mi("/srv/data", "virtiofs"))
			v.put("sys/fs/virtiofs/0/tag", "share\n", 0o444)
		},
			`fail ssh-host-mounts: virtio-fs tag "share" present; remove the share from the VM`},
		{"item 2: file under /Users", func(v *fakeVM) { v.put("Users/x", "x", 0o644) }, "fail ssh-host-mounts: /Users"},
		{"item 2: file under /mnt", func(v *fakeVM) { v.put("mnt/anything/x", "x", 0o644) }, "fail ssh-host-mounts: /mnt/anything"},
		{"item 3: mac bridge", func(v *fakeVM) { v.put("opt/orbstack-guest/bin/mac", "#!/bin/sh\n", 0o755) },
			"fail ssh-mac-bridge: /opt/orbstack-guest/bin/mac present"},
		{"item 4: agent forwarding allowed", func(v *fakeVM) { v.put("run/host-services/ssh-agent.sock", "", 0o644) },
			"fail ssh-agent-forward: /run/host-services/ssh-agent.sock"},
		{"item 6: bitc shell", func(v *fakeVM) {
			v.put("etc/passwd", strings.Replace(readAll(t, v, "etc/passwd"), "/home/bitc:/bin/sh", "/home/bitc:/usr/sbin/nologin", 1), 0o644)
		}, "fail ssh-users: bitc: shell"},
		{"item 6: sudoers differs", func(v *fakeVM) { v.put("etc/sudoers.d/bitc", "bitc ALL=(ALL) NOPASSWD: ALL\n", 0o440) }, "fail ssh-users: "},
		{"item 7: table missing", func(v *fakeVM) { v.nft = "" }, "fail ssh-lan-rules: table inet bitc missing"},
		{"item 7: forwarding off", func(v *fakeVM) { v.put("proc/sys/net/ipv4/ip_forward", "0\n", 0o644) }, "fail ssh-lan-rules: "},
		{"item 8: rosetta binfmt entry (real path /proc/sys/fs/binfmt_misc)", func(v *fakeVM) { v.put("proc/sys/fs/binfmt_misc/rosetta", rosetta, 0o644) },
			"fail ssh-rosetta: binfmt_misc handler rosetta runs x86-64; disable Rosetta"},
		{"item 9: docker socket", func(v *fakeVM) { v.put("run/docker.sock", "", 0o644) }, "fail ssh-docker-socket: /run/docker.sock"},
		{"item 10: reachability key readable by a slot user", func(v *fakeVM) { v.open[filepath.Join(v.root, "etc/bitc/tunnel_key")] = true },
			"fail ssh-reach-key: /etc/bitc/tunnel_key readable by bitc-s1"},
		{"item 11: cron.allow lists a slot user", func(v *fakeVM) { v.put("etc/cron.allow", "root\nbitc-s1\n", 0o644) }, "fail ssh-cron: "},
	} {
		v := newFakeVM(t)
		c.fix(v)
		found := false
		for _, l := range v.failLines() {
			found = found || strings.HasPrefix(l, c.want)
		}
		if !found {
			t.Errorf("%s: no line starting %q in %q", c.name, c.want, v.failLines())
		}
	}
	// item 6: the cgroup check only reads. A missing bitc subtree is reported as not delegable;
	// the check creates no directory and writes no controller file.
	cg := newFakeVM(t)
	if err := os.RemoveAll(filepath.Join(cg.root, "sys/fs/cgroup/bitc")); err != nil {
		t.Fatal(err)
	}
	cg.put("sys/fs/cgroup/cgroup.subtree_control", "cpuset cpu io\n", 0o644)
	found := false
	for _, l := range cg.failLines() {
		found = found || strings.HasPrefix(l, "fail ssh-users: cgroup v2 bitc subtree not delegable")
	}
	if !found {
		t.Errorf("item 6: no bitc cgroup subtree: no line starting %q in %q", "fail ssh-users: cgroup v2 bitc subtree not delegable", cg.failLines())
	}
	if _, err := os.Stat(filepath.Join(cg.root, "sys/fs/cgroup/bitc")); err == nil {
		t.Error("item 6: check-host created /sys/fs/cgroup/bitc; it must only read")
	}
	if got := readAll(t, cg, "sys/fs/cgroup/cgroup.subtree_control"); got != "cpuset cpu io\n" {
		t.Errorf("item 6: check-host wrote the cgroup root's subtree_control: %q", got)
	}
	// the old, wrong path must not be what is read
	v := newFakeVM(t)
	v.put("proc/sys/binfmt_misc/rosetta", rosetta, 0o644)
	if f := v.failLines(); len(f) != 0 {
		t.Errorf("an entry under /proc/sys/binfmt_misc (not the kernel's path) failed: %q", f)
	}
}

func readAll(t *testing.T, v *fakeVM, p string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(v.root, p))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

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

// SH25: the self-test's decision logic with injected probe results (no network).
func TestSH25_FirewallSelfTestDecisionsAndMarker(t *testing.T) {
	conf := agent.HostConf{Gateway: "192.168.64.1", Netns: true}
	spec := agent.DefaultSelfTestSpec(conf, "", "10.20.0.50", nil)
	res := func(over map[string]agent.ConnResult) agent.NetProbeResult {
		r := agent.NetProbeResult{Connect: map[string]agent.ConnResult{}}
		for _, b := range spec.Block {
			r.Connect[b] = agent.ConnBlocked
		}
		for _, a := range spec.Allow {
			r.Connect[a] = agent.ConnOpen
		}
		for k, v := range over {
			r.Connect[k] = v
		}
		return r
	}
	run := func(root string, r agent.NetProbeResult, err error) (int, string) {
		var out bytes.Buffer
		env := agent.HostEnv{Root: root, NetProbe: func([]string) (agent.NetProbeResult, error) { return r, err }}
		code := agent.RunFirewallSelfTest(env, spec, &out)
		return code, out.String()
	}
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "var/lib/bitc"), 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(root, agent.SelfTestMarker)
	for name, tc := range map[string]struct {
		over map[string]agent.ConnResult
		want string
	}{
		"LAN router open":    {map[string]agent.ConnResult{"192.168.1.1:80": agent.ConnOpen}, "FIREWALL NOT ACTIVE"},
		"controller refused": {map[string]agent.ConnResult{"10.20.0.50:22": agent.ConnRefused}, "FIREWALL NOT ACTIVE"},
		"ULA reached":        {map[string]agent.ConnResult{"[fd00::1]:443": agent.ConnOpen}, "FIREWALL NOT ACTIVE"},
		"link-local reached": {map[string]agent.ConnResult{"[fe80::1]:443": agent.ConnOpen}, "FIREWALL NOT ACTIVE"},
		"CGNAT reached":      {map[string]agent.ConnResult{"100.100.100.100:80": agent.ConnOpen}, "FIREWALL NOT ACTIVE"},
		"198.18 reached":     {map[string]agent.ConnResult{"198.18.0.1:80": agent.ConnOpen}, "FIREWALL NOT ACTIVE"},
		// firewall inactive on a 10.x LAN: the default router does not exist (times out, "blocked")
		// but the VM's own sshd answers on the namespace host end
		"own sshd answers":  {map[string]agent.ConnResult{"10.203.0.1:22": agent.ConnOpen}, "FIREWALL NOT ACTIVE"},
		"github:22 blocked": {map[string]agent.ConnResult{"github.com:22": agent.ConnBlocked}, "EGRESS BROKEN"},
		"npm:443 blocked":   {map[string]agent.ConnResult{"registry.npmjs.org:443": agent.ConnBlocked}, "EGRESS BROKEN"},
	} {
		code, out := run(root, res(tc.over), nil)
		if code == 0 || !strings.Contains(out, tc.want) {
			t.Errorf("%s: exit %d, output %q; want non-zero and %q", name, code, out, tc.want)
		}
		if fi, err := os.Stat(marker); err != nil || fi.Mode().Perm() != 0o600 {
			t.Errorf("%s: marker %v, %v; want a 0600 file", name, fi, err)
		}
	}
	if code, out := run(root, agent.NetProbeResult{}, fmt.Errorf("no namespace")); code == 0 || !strings.Contains(out, "SELF-TEST DID NOT RUN") {
		t.Errorf("a probe that could not run: exit %d, output\n%s", code, out)
	}
	if code, out := run(root, res(nil), nil); code != 0 || !strings.Contains(out, "firewall self-test: ok") {
		t.Errorf("a pass: exit %d, output %q", code, out)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Errorf("a passing self-test left the marker: %v", err)
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

func TestSH26_SetupPrintEqualsTheManagedFilesText(t *testing.T) {
	if got := setupPrint(t, "sudoers"); got != agent.SudoersText {
		t.Errorf("--print sudoers = %q, want %q", got, agent.SudoersText)
	}
	if got := setupPrint(t, "sshd"); got != agent.SSHDSnippetText {
		t.Errorf("--print sshd = %q, want %q", got, agent.SSHDSnippetText)
	}
	conf := agent.HostConf{Slots: 2, Resolver: "9.9.9.9", Gateway: "192.168.64.1", Netns: true}
	want, err := agent.RenderNft(conf, []int{996, 997})
	if err != nil {
		t.Fatal(err)
	}
	got := setupPrint(t, "nft", "--resolver", "9.9.9.9", "--gateway", "192.168.64.1", "--uids", "996, 997")
	if got != want {
		t.Errorf("--print nft differs from RenderNft:\n--- script\n%s\n--- agent\n%s", got, want)
	}
	hc := setupPrint(t, "host.conf", "--slots", "2", "--resolver", "9.9.9.9", "--gateway", "192.168.64.1", "--memory-per-slot", "16GiB", "--secret-path", "/etc/bitc/tunnel_key")
	if wantHC := "slots=2\nresolver=9.9.9.9\ngateway=192.168.64.1\nnetns=true\nmemory_per_slot=16GiB\nsecret_paths=/etc/bitc/tunnel_key\n"; hc != wantHC {
		t.Errorf("--print host.conf = %q, want %q", hc, wantHC)
	}
	parsed, err := agent.ParseHostConf([]byte(hc))
	if err != nil || parsed.Slots != 2 || parsed.MemoryPerSlot != 16<<30 || len(parsed.SecretPaths) != 1 {
		t.Errorf("the agent cannot read the host.conf the script prints: %+v, %v", parsed, err)
	}
	key := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIexample"
	if got, wantAK := setupPrint(t, "authorized-keys", "--controller-key", key), `restrict,command="/opt/bitc/sbin/bitc-login" `+key+" bitc-controller\n"; got != wantAK {
		t.Errorf("--print authorized-keys = %q, want %q", got, wantAK)
	}
	// the script writes no kernel state: it has no sysctl text and only reads ip_forward
	if out, err := exec.Command("/bin/sh", "../scripts/ssh-host-setup.sh", "--print", "sysctl").Output(); err == nil {
		t.Errorf("--print sysctl printed %q and exited 0; the script must not manage sysctl", out)
	}
	// the gate and login wrapper the script installs are the files in the tree, and parse as sh
	for _, f := range []string{"ssh-gate.sh", "ssh-login.sh", "ssh-host-setup.sh"} {
		if out, err := exec.Command("/bin/sh", "-n", "../scripts/"+f).CombinedOutput(); err != nil {
			t.Errorf("sh -n %s: %v\n%s", f, err, out)
		}
	}
	// the self-test is part of the script and its fail-closed words are the agent's
	b, err := os.ReadFile("../scripts/ssh-host-setup.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{"firewall-selftest", agent.SelfTestMarker, "--dry-run", "/proc/sys/net/ipv4/ip_forward"} {
		if !strings.Contains(string(b), w) {
			t.Errorf("ssh-host-setup.sh never mentions %q", w)
		}
	}
	if strings.Contains(string(b), "sysctl") {
		t.Error("ssh-host-setup.sh mentions sysctl: it must only read /proc/sys/net/ipv4/ip_forward and never write kernel state")
	}
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

func TestSH29_AllowValidationMatchesTheAgent(t *testing.T) {
	for _, v := range append(append(append([]string{}, allowPrivate...), allowPublic...), allowMalformed...) {
		for _, flag := range []bool{false, true} {
			var extra []string
			if flag {
				extra = []string{"--allow-test-targets"}
			}
			_, code, stderr := setupAllow(v, extra...)
			wantErr := agent.ValidateAllowTarget(v, flag) != nil
			if wantErr && code != 2 {
				t.Errorf("script --allow %q (test targets %v): exit %d, want 2 as the agent refuses it", v, flag, code)
			}
			if !wantErr && code != 0 {
				t.Errorf("script --allow %q (test targets %v): exit %d (%s), want 0 as the agent accepts it", v, flag, code, strings.TrimSpace(stderr))
			}
		}
	}
	for _, v := range allowPrivate {
		if _, code, _ := setupAllow(v); code != 2 {
			t.Errorf("script --allow %q without --allow-test-targets: exit %d, want 2", v, code)
		}
		if _, code, _ := setupAllow(v, "--allow-test-targets"); code != 0 {
			t.Errorf("script --allow %q with --allow-test-targets: exit %d, want 0", v, code)
		}
	}
	for _, v := range allowPublic {
		if _, code, _ := setupAllow(v); code != 0 {
			t.Errorf("script --allow %q: exit %d, want 0", v, code)
		}
	}
	for _, v := range allowMalformed {
		if _, code, _ := setupAllow(v, "--allow-test-targets"); code != 2 {
			t.Errorf("script --allow %q: exit %d, want 2", v, code)
		}
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
