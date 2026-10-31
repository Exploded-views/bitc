// Linux-only: checks the Linux host agent's logic in-process (agent.HostConf and friends), which does not build elsewhere.

package conformance

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Exploded-views/bitc/internal/agent"
)

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

type fakeVM struct {
	t    *testing.T
	root string
	nft  string
	open map[string]bool
}

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

func readAll(t *testing.T, v *fakeVM, p string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(v.root, p))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
