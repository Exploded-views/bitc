package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

const wantSudoers = "Defaults!/opt/bitc/sbin/bitc-gate !use_pty, !requiretty, !log_output, env_reset\n" +
	"bitc ALL=(root) NOPASSWD: /opt/bitc/sbin/bitc-gate\n"

// fakeHost is a tree that passes every check; tests break one thing at a time.
type fakeHost struct {
	t         *testing.T
	root      string
	nft       string // what `nft -j list table inet bitc` prints; "" means the table is missing
	want      string // what the rendered ruleset loads to
	sshd      string
	visudo    error
	opened    map[string]bool // path -> a slot user can open it
	net       NetProbeResult
	netErr    error
	secc      []string
	owners    map[string]int // path -> uid override (default 0)
	environ   []string
	loginExit int
}

const nftJSON = `{"nftables":[{"metainfo":{"version":"1.0.6"}},{"table":{"family":"inet","name":"bitc","handle":1}}]}`
const sshdOK = "port 22\nallowagentforwarding no\ndisableforwarding yes\nauthenticationmethods publickey\n"

func newFakeHost(t *testing.T) *fakeHost {
	t.Helper()
	h := &fakeHost{t: t, root: t.TempDir(), nft: nftJSON, want: nftJSON, sshd: sshdOK, opened: map[string]bool{}, owners: map[string]int{}, loginExit: 64,
		net: NetProbeResult{Connect: map[string]ConnResult{}}, secc: []string{
			"socket(AF_VSOCK) EPERM", "socketpair(AF_VSOCK) EPERM", "unshare(CLONE_NEWUSER) EPERM",
			"clone(CLONE_NEWUSER) EPERM", "keyctl(GET_KEYRING_ID) EPERM", "clone3 ENOSYS"}}
	h.write("etc/bitc/host.conf", "slots=2\nresolver=9.9.9.9\ngateway=192.168.64.1\nsecret_paths=/etc/bitc/tunnel_key\n", 0o644)
	h.write("etc/passwd", "root:x:0:0:root:/root:/bin/bash\nbitc:x:995:995::/home/bitc:/bin/sh\nbitc-s1:x:996:996::/var/lib/bitc/slots/1/home:/usr/sbin/nologin\nbitc-s2:x:997:997::/var/lib/bitc/slots/2/home:/usr/sbin/nologin\n", 0o644)
	h.write("etc/group", "root:x:0:\nsudo:x:27:\nbitc:x:995:\nbitc-s1:x:996:\nbitc-s2:x:997:\n", 0o644)
	h.write("etc/shadow", "root:*:1:0:99999:7:::\nbitc:!:1:0:99999:7:::\nbitc-s1:!:1:0:99999:7:::\nbitc-s2:!:1:0:99999:7:::\n", 0o640)
	h.write("etc/sudoers", "Defaults env_reset\nroot ALL=(ALL:ALL) ALL\n@includedir /etc/sudoers.d\n", 0o440)
	h.write("etc/sudoers.d/bitc", wantSudoers, 0o440)
	h.write("etc/cron.allow", "root\n", 0o644)
	h.write("etc/at.allow", "root\n", 0o644)
	h.write("home/bitc/.ssh/authorized_keys", `restrict,command="/opt/bitc/sbin/bitc-login" ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIexample bitc-controller`+"\n", 0o644)
	h.write("opt/bitc/sbin/bitc-login", "#!/bin/sh\n", 0o755)
	h.write("opt/bitc/sbin/bitc-gate", "#!/bin/sh\n", 0o755)
	h.write("etc/bitc/tunnel_key", "secret", 0o600)
	h.mkdir("var/lib/bitc", 0o700)
	h.mkdir("var/lib/systemd/linger", 0o755)
	h.mkdir("var/spool/cron/crontabs", 0o755)
	h.mkdir("var/spool/cron/atjobs", 0o755)
	h.mkdir("run", 0o755)
	h.mkdir("mnt", 0o755)
	h.mkdir("proc/sys/fs/binfmt_misc", 0o755)
	h.mkdir("sys/bus/virtio/devices", 0o755)
	h.write("proc/sys/net/ipv4/ip_forward", "1\n", 0o644)
	h.write("proc/net/unix", "Num RefCount Protocol Flags Type St Inode Path\n", 0o644)
	h.write("sys/fs/cgroup/cgroup.controllers", "cpuset cpu io memory pids\n", 0o444)
	h.write("sys/fs/cgroup/cgroup.subtree_control", "cpuset cpu io memory pids\n", 0o644)
	h.write("sys/fs/cgroup/bitc/cgroup.controllers", "cpuset cpu io memory pids\n", 0o444)
	h.pid(1, "4026531841", mountinfoLine("/", "ext4"), mountinfoLine("/proc", "proc"))
	return h
}

func (h *fakeHost) path(p string) string { return filepath.Join(h.root, p) }

func (h *fakeHost) write(p, content string, mode os.FileMode) {
	h.t.Helper()
	if err := os.MkdirAll(filepath.Dir(h.path(p)), 0o755); err != nil {
		h.t.Fatal(err)
	}
	_ = os.Remove(h.path(p))
	if err := os.WriteFile(h.path(p), []byte(content), mode); err != nil {
		h.t.Fatal(err)
	}
	_ = os.Chmod(h.path(p), mode)
}

func (h *fakeHost) mkdir(p string, mode os.FileMode) {
	h.t.Helper()
	if err := os.MkdirAll(h.path(p), 0o755); err != nil {
		h.t.Fatal(err)
	}
	_ = os.Chmod(h.path(p), mode)
}

func mountinfoLine(mp, fstype string) string {
	return fmt.Sprintf("36 35 98:0 / %s rw,noatime shared:1 - %s src rw", mp, fstype)
}

// pid adds /proc/<pid> in the mount namespace nsid with the given mountinfo lines.
func (h *fakeHost) pid(pid int, nsid string, lines ...string) {
	h.t.Helper()
	d := h.path(fmt.Sprintf("proc/%d", pid))
	if err := os.MkdirAll(filepath.Join(d, "ns"), 0o755); err != nil {
		h.t.Fatal(err)
	}
	_ = os.Remove(filepath.Join(d, "ns", "mnt"))
	if err := os.Symlink("mnt:["+nsid+"]", filepath.Join(d, "ns", "mnt")); err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "mountinfo"), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		h.t.Fatal(err)
	}
}

func (h *fakeHost) env(doctor bool) HostEnv {
	conf, err := ParseHostConf([]byte(readFile(h.t, h.path("etc/bitc/host.conf"))))
	if err != nil {
		h.t.Fatal(err)
	}
	return HostEnv{Root: h.root, Conf: conf, Doctor: doctor,
		Exec: func(stdin, name string, args ...string) ([]byte, error) {
			switch {
			case name == "nft" && len(args) > 0 && args[0] == "-j":
				if h.nft == "" {
					return nil, fmt.Errorf("Error: No such file or directory")
				}
				return []byte(h.nft), nil
			case name == "unshare": // the render-and-load step
				return []byte(h.want), nil
			case name == "sshd":
				return []byte(h.sshd), nil
			case name == "visudo":
				return nil, h.visudo
			}
			return nil, fmt.Errorf("unexpected command %s %v", name, args)
		},
		Owner: func(fi os.FileInfo) (int, int) {
			if uid, ok := h.owners[fi.Name()]; ok {
				return uid, 0
			}
			return 0, 0
		},
		OpenAs: func(uid, gid int, path string) error {
			if h.opened[path] {
				return nil
			}
			return os.ErrPermission
		},
		NetProbe:     func([]string) (NetProbeResult, error) { return h.net, h.netErr },
		SeccompProbe: func() ([]string, error) { return h.secc, nil },
		Environ:      func() []string { return h.environ },
		LoginProbe:   func() (int, error) { return h.loginExit, nil },
	}
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// failures returns the fail lines (as strings) of a check run.
func failures(fs []Finding) (out []string) {
	for _, f := range fs {
		if f.Level == "fail" {
			out = append(out, f.String())
		}
	}
	return out
}

// wantFail checks that exactly one fail line carries id and contains sub.
func wantFail(t *testing.T, name string, fs []Finding, id, sub string) {
	t.Helper()
	for _, f := range fs {
		if f.Level == "fail" && f.ID == id && strings.Contains(f.Msg, sub) {
			return
		}
	}
	t.Errorf("%s: no `fail %s` line containing %q in %q", name, id, sub, failures(fs))
}

// wantWarn checks that a `warn id` line has exactly the message msg.
func wantWarn(t *testing.T, name string, fs []Finding, id, msg string) {
	t.Helper()
	var got []string
	for _, f := range fs {
		if f.Level == "warn" && f.ID == id {
			if f.Msg == msg {
				return
			}
			got = append(got, f.Msg)
		}
	}
	t.Errorf("%s: no `warn %s: %s` line (warns: %q)", name, id, msg, got)
}

// noWarn checks that no warn line mentions sub.
func noWarn(t *testing.T, name string, fs []Finding, sub string) {
	t.Helper()
	for _, f := range fs {
		if f.Level == "warn" && strings.Contains(f.Msg, sub) {
			t.Errorf("%s: unexpected %s", name, f)
		}
	}
}

// withShare adds a virtiofs mount in a second mount namespace, away from every host path.
func (h *fakeHost) withShare() {
	h.t.Helper()
	h.pid(4242, "4026532000", mountinfoLine("/", "ext4"), mountinfoLine("/srv/data", "virtiofs"))
}

func TestCheckHostPassesACleanHost(t *testing.T) {
	h := newFakeHost(t)
	fs := CheckHost(h.env(false))
	if len(fs) == 0 {
		t.Fatal("no findings at all")
	}
	if f := failures(fs); len(f) != 0 {
		t.Errorf("a clean host failed: %q", f)
	}
	ids := map[string]bool{}
	for _, f := range fs {
		if f.Level == "ok" {
			ids[f.ID] = true
		}
	}
	for _, id := range []string{"ssh-host-mounts", "ssh-mac-bridge", "ssh-agent-forward", "ssh-users", "ssh-lan-rules", "ssh-rosetta", "ssh-docker-socket", "ssh-reach-key", "ssh-cron"} {
		if !ids[id] {
			t.Errorf("no `ok %s` line", id)
		}
	}
	if ids["ssh-lan"] || ids["ssh-seccomp"] {
		t.Error("items 5 and 12 ran without the doctor flag")
	}
}

func TestCheckHostLineFormat(t *testing.T) {
	for in, want := range map[Finding]string{
		{"ok", "ssh-cron", ""}:                            "ok ssh-cron",
		{"fail", "ssh-docker-socket", "/run/docker.sock"}: "fail ssh-docker-socket: /run/docker.sock",
		{"warn", "ssh-swap", "/dev/vda active"}:           "warn ssh-swap: /dev/vda active",
	} {
		if got := in.String(); got != want {
			t.Errorf("%+v prints %q, want %q", in, got, want)
		}
	}
}

func TestCheckHostItem1MountsInAnyNamespace(t *testing.T) {
	for _, fstype := range []string{"virtiofs", "9p", "nfs", "nfs4", "cifs", "smb3", "vboxsf", "prl_fs", "fakeowner", "fuse.sshfs", "fuse"} {
		h := newFakeHost(t)
		h.pid(1, "4026531841", mountinfoLine("/", "ext4"), mountinfoLine("/mnt/mac", fstype))
		wantFail(t, fstype+" in pid 1", CheckHost(h.env(false)), "ssh-host-mounts", "/mnt/mac ("+fstype+") visible; disable host file sharing")
	}
	h := newFakeHost(t) // a share only a second namespace sees; /proc/1 is clean
	h.pid(4242, "4026532000", mountinfoLine("/", "ext4"), mountinfoLine("/Users", "virtiofs"))
	wantFail(t, "second namespace", CheckHost(h.env(false)), "ssh-host-mounts", "/Users (virtiofs) in mount namespace of pid 4242")
	h = newFakeHost(t) // the lxcfs exception, and a namespace seen twice is scanned once
	h.pid(1, "4026531841", mountinfoLine("/", "ext4"), mountinfoLine("/proc/cpuinfo", "fuse.lxcfs"))
	h.pid(77, "4026531841", mountinfoLine("/", "ext4"))
	if f := failures(CheckHost(h.env(false))); len(f) != 0 {
		t.Errorf("fuse.lxcfs must be allowed: %q", f)
	}
}

func TestCheckHostItem1VirtioDevicesWhileAShareIsMounted(t *testing.T) {
	h := newFakeHost(t)
	h.withShare()
	h.write("sys/bus/virtio/devices/virtio3/device", "0x001a\n", 0o444)
	h.write("sys/fs/virtiofs/0/tag", "share\n", 0o444)
	wantFail(t, "virtio-fs", CheckHost(h.env(false)), "ssh-host-mounts", `virtio-fs device virtio3 (tag "share") present; remove the share from the VM`)
	h = newFakeHost(t)
	h.withShare()
	h.write("sys/bus/virtio/devices/virtio4/device", "0x0009\n", 0o444)
	wantFail(t, "9p", CheckHost(h.env(false)), "ssh-host-mounts", "9p device virtio4 present; remove the share from the VM")
	h = newFakeHost(t)
	h.withShare()
	h.write("sys/fs/virtiofs/0/tag", "share\n", 0o444)
	wantFail(t, "tag file only", CheckHost(h.env(false)), "ssh-host-mounts", `virtio-fs tag "share" present; remove the share from the VM`)
	h = newFakeHost(t) // a block device is fine
	h.write("sys/bus/virtio/devices/virtio1/device", "0x0002\n", 0o444)
	if f := failures(CheckHost(h.env(false))); len(f) != 0 {
		t.Errorf("a virtio block device failed: %q", f)
	}
}

func TestCheckHostItem1VirtioDevicesWithoutAShareMountWarnOnly(t *testing.T) {
	h := newFakeHost(t)
	h.write("sys/bus/virtio/devices/virtio4/device", "0x001a\n", 0o444)
	h.write("sys/fs/virtiofs/0/tag", "mac\n", 0o444)
	h.write("sys/bus/virtio/devices/virtio5/device", "0x0009\n", 0o444)
	fs := CheckHost(h.env(false))
	if f := failures(fs); len(f) != 0 {
		t.Errorf("unmounted devices failed: %q", f)
	}
	wantWarn(t, "virtio-fs", fs, "ssh-host-mounts", `virtio-fs device virtio4 (tag "mac") exported to the VM but not mounted in this machine (shared-kernel risk, decision 10)`)
	wantWarn(t, "9p", fs, "ssh-host-mounts", "9p device virtio5 exported to the VM but not mounted in this machine (shared-kernel risk, decision 10)")
	h = newFakeHost(t) // a tag with no device
	h.write("sys/fs/virtiofs/0/tag", "rosetta\n", 0o444)
	wantWarn(t, "tag only", CheckHost(h.env(false)), "ssh-host-mounts", `virtio-fs tag "rosetta" exported to the VM but not mounted in this machine (shared-kernel risk, decision 10)`)
	h = newFakeHost(t) // fuse.lxcfs is not a share, so the devices still only warn
	h.write("sys/bus/virtio/devices/virtio4/device", "0x001a\n", 0o444)
	h.pid(1, "4026531841", mountinfoLine("/", "ext4"), mountinfoLine("/proc/cpuinfo", "fuse.lxcfs"))
	if f := failures(CheckHost(h.env(false))); len(f) != 0 {
		t.Errorf("devices beside lxcfs failed: %q", f)
	}
}

func TestCheckHostItem1VirtioDevicesWithABlindMountScanFail(t *testing.T) {
	h := newFakeHost(t)
	h.write("sys/bus/virtio/devices/virtio4/device", "0x001a\n", 0o444)
	h.write("sys/fs/virtiofs/0/tag", "mac\n", 0o444)
	if err := os.RemoveAll(h.path("proc/1")); err != nil { // no readable mountinfo anywhere
		t.Fatal(err)
	}
	fs := CheckHost(h.env(false))
	wantFail(t, "blind scan", fs, "ssh-host-mounts", `virtio-fs device virtio4 (tag "mac") present; remove the share from the VM`)
	noWarn(t, "blind scan", fs, "exported to the VM")
}

func TestCheckHostItem1DevicesPlusAShareInASecondNamespaceFail(t *testing.T) {
	h := newFakeHost(t)
	h.write("sys/bus/virtio/devices/virtio4/device", "0x001a\n", 0o444)
	h.write("sys/fs/virtiofs/0/tag", "mac\n", 0o444)
	fs := CheckHost(h.env(false))
	if f := failures(fs); len(f) != 0 {
		t.Fatalf("precondition: devices alone must not fail: %q", f)
	}
	h.pid(4242, "4026532000", mountinfoLine("/", "ext4"), mountinfoLine("/data", "9p"))
	fs = CheckHost(h.env(false))
	wantFail(t, "device with share", fs, "ssh-host-mounts", `virtio-fs device virtio4 (tag "mac") present; remove the share from the VM`)
	noWarn(t, "device with share", fs, "exported to the VM")
}

func TestCheckHostItem2HostPaths(t *testing.T) {
	h := newFakeHost(t)
	h.pid(1, "4026531841", mountinfoLine("/", "ext4"), mountinfoLine("/opt/orbstack-guest", "overlay"))
	wantFail(t, "orbstack overlay", CheckHost(h.env(false)), "ssh-host-mounts", "/opt/orbstack-guest")
	for _, p := range []string{"Users", "Volumes", "mnt/mac", "private", "opt/orbstack-guest", "mnt/anything"} {
		h := newFakeHost(t)
		h.write(p+"/file", "x", 0o644)
		wantFail(t, p, CheckHost(h.env(false)), "ssh-host-mounts", "/"+strings.TrimSuffix(p, "/file"))
	}
	h = newFakeHost(t) // empty host-path directories only warn (changed in H.1b: /mnt entries too)
	h.mkdir("Users", 0o755)
	h.mkdir("opt/orbstack-guest", 0o755)
	fs := CheckHost(h.env(false))
	if f := failures(fs); len(f) != 0 {
		t.Errorf("empty directories failed: %q", f)
	}
	wantWarn(t, "empty /Users", fs, "ssh-host-mounts", "/Users holds only empty directories (not mounted)")
}

func TestCheckHostItem2EmptyPlaceholdersWarnOnly(t *testing.T) {
	h := newFakeHost(t)
	h.mkdir("Users/alice", 0o755)
	h.mkdir("Users/alice/Documents/deep", 0o755)
	h.mkdir("mnt/linux", 0o755)
	h.mkdir("mnt/mac", 0o755)
	h.mkdir("opt/orbstack-guest/etc", 0o755)
	fs := CheckHost(h.env(false))
	if f := failures(fs); len(f) != 0 {
		t.Errorf("empty placeholders failed: %q", f)
	}
	for _, p := range []string{"/Users", "/mnt/linux", "/mnt/mac", "/opt/orbstack-guest"} {
		wantWarn(t, p, fs, "ssh-host-mounts", p+" holds only empty directories (not mounted)")
	}
	noWarn(t, "absent paths", fs, "/Volumes")
	noWarn(t, "absent paths", fs, "/private")
}

func TestCheckHostItem2FilesUnderHostPathsFail(t *testing.T) {
	h := newFakeHost(t)
	h.write("Users/alice/notes.txt", "x", 0o644)
	fs := CheckHost(h.env(false))
	wantFail(t, "file in /Users/alice", fs, "ssh-host-mounts", "/Users/alice/notes.txt (regular file) under host path /Users")
	noWarn(t, "file in /Users/alice", fs, "/Users holds")
	h = newFakeHost(t)
	h.write("mnt/x/file", "x", 0o644)
	wantFail(t, "file in /mnt/x", CheckHost(h.env(false)), "ssh-host-mounts", "/mnt/x/file (regular file)")
	h = newFakeHost(t) // an entry directly under /mnt that is not a directory
	h.write("mnt/stray", "x", 0o644)
	wantFail(t, "file in /mnt", CheckHost(h.env(false)), "ssh-host-mounts", "/mnt/stray (regular file) is not a directory")
	h = newFakeHost(t) // a deeper file among empty directories
	h.mkdir("Volumes/a/b/c", 0o755)
	h.write("Volumes/a/b/c/f", "x", 0o644)
	wantFail(t, "deep file", CheckHost(h.env(false)), "ssh-host-mounts", "/Volumes/a/b/c/f (regular file)")
}

func TestCheckHostItem2NeverFollowsSymlinks(t *testing.T) {
	h := newFakeHost(t)
	h.mkdir("opt/orbstack-guest/etc", 0o755)
	if err := os.Symlink(h.path("var/lib"), h.path("opt/orbstack-guest/etc/link")); err != nil {
		t.Fatal(err)
	}
	fs := CheckHost(h.env(false))
	wantFail(t, "symlink to a directory", fs, "ssh-host-mounts", "/opt/orbstack-guest/etc/link (symlink) under host path /opt/orbstack-guest")
	for _, f := range fs { // the target's contents were never listed
		if strings.Contains(f.Msg, "/var/lib") {
			t.Errorf("the walk followed a symlink: %v", f)
		}
	}
	h = newFakeHost(t) // a dangling symlink, and a symlink as the host path itself
	if err := os.Symlink("/nonexistent", h.path("Users")); err != nil {
		t.Fatal(err)
	}
	wantFail(t, "symlinked host path", CheckHost(h.env(false)), "ssh-host-mounts", "/Users (symlink) is not a directory")
	h = newFakeHost(t) // a symlink loop is a symlink, not a hang
	h.mkdir("Users/a", 0o755)
	if err := os.Symlink(h.path("Users"), h.path("Users/a/loop")); err != nil {
		t.Fatal(err)
	}
	wantFail(t, "loop", CheckHost(h.env(false)), "ssh-host-mounts", "/Users/a/loop (symlink)")
}

func TestCheckHostItem2OtherEntryTypesFail(t *testing.T) {
	h := newFakeHost(t)
	h.mkdir("Users/a", 0o755)
	if err := syscall.Mkfifo(h.path("Users/a/pipe"), 0o600); err != nil {
		t.Fatal(err)
	}
	wantFail(t, "fifo", CheckHost(h.env(false)), "ssh-host-mounts", "/Users/a/pipe (fifo)")
}

func TestCheckHostItem2MountUnderAHostPathFails(t *testing.T) {
	for _, mp := range []string{"/opt/orbstack-guest/bin", "/Users/alice", "/Volumes/x/y", "/private/var", "/mnt/mac/sub"} {
		h := newFakeHost(t)
		h.pid(1, "4026531841", mountinfoLine("/", "ext4"), mountinfoLine(mp, "overlay"))
		fs := CheckHost(h.env(false))
		wantFail(t, mp, fs, "ssh-host-mounts", mp+" (overlay) visible; disable host file sharing")
	}
	h := newFakeHost(t) // only in a second namespace
	h.pid(4242, "4026532000", mountinfoLine("/", "ext4"), mountinfoLine("/Users/alice", "tmpfs"))
	wantFail(t, "second namespace", CheckHost(h.env(false)), "ssh-host-mounts", "/Users/alice (tmpfs) in mount namespace of pid 4242")
	h = newFakeHost(t) // a sibling that merely shares a prefix is not under the host path
	h.pid(1, "4026531841", mountinfoLine("/", "ext4"), mountinfoLine("/Users-old", "tmpfs"), mountinfoLine("/privately", "tmpfs"))
	if f := failures(CheckHost(h.env(false))); len(f) != 0 {
		t.Errorf("prefix-only siblings failed: %q", f)
	}
}

func TestCheckHostItem2EntryCapAndDepthCapFail(t *testing.T) {
	h := newFakeHost(t)
	for i := 0; i <= 10000; i++ {
		h.mkdir(fmt.Sprintf("Users/d%05d", i), 0o755)
	}
	fs := CheckHost(h.env(false))
	wantFail(t, "10001 entries", fs, "ssh-host-mounts", "/Users is too large to verify")
	noWarn(t, "10001 entries", fs, "/Users holds")
	h = newFakeHost(t) // exactly the cap is still verified
	for i := 0; i < 10000; i++ {
		h.mkdir(fmt.Sprintf("Users/d%05d", i), 0o755)
	}
	fs = CheckHost(h.env(false))
	if f := failures(fs); len(f) != 0 {
		t.Errorf("10000 entries failed: %q", f)
	}
	wantWarn(t, "10000 entries", fs, "ssh-host-mounts", "/Users holds only empty directories (not mounted)")
	h = newFakeHost(t) // 17 levels under the host path
	h.mkdir("Volumes"+strings.Repeat("/d", 17), 0o755)
	wantFail(t, "too deep", CheckHost(h.env(false)), "ssh-host-mounts", "/Volumes is too deep to verify")
	h = newFakeHost(t) // 16 levels are verified
	h.mkdir("Volumes"+strings.Repeat("/d", 16), 0o755)
	fs = CheckHost(h.env(false))
	if f := failures(fs); len(f) != 0 {
		t.Errorf("16 levels failed: %q", f)
	}
}

func TestCheckHostWarnsDoNotFailTheRun(t *testing.T) {
	h := newFakeHost(t)
	h.mkdir("Users/alice", 0o755)
	h.write("sys/bus/virtio/devices/virtio4/device", "0x001a\n", 0o444)
	fs := CheckHost(h.env(false))
	if f := failures(fs); len(f) != 0 {
		t.Errorf("warns turned into fails: %q", f)
	}
	warns := 0
	for _, f := range fs {
		if f.Level == "warn" {
			warns++
		}
	}
	if warns != 2 {
		t.Errorf("want 2 warns, got %d in %v", warns, fs)
	}
}

func TestCheckHostItem3MacBridge(t *testing.T) {
	h := newFakeHost(t)
	h.write("opt/orbstack-guest/bin/mac", "#!/bin/sh\n", 0o755)
	wantFail(t, "file under bin", CheckHost(h.env(false)), "ssh-mac-bridge", "/opt/orbstack-guest/bin/mac present")
	for _, name := range []string{"mac", "macctl", "open"} {
		h := newFakeHost(t)
		h.write("opt/orbstack-guest/helper", "x", 0o755)
		h.mkdir("usr/local/bin", 0o755)
		if err := os.Symlink("/opt/orbstack-guest/helper", h.path("usr/local/bin/"+name)); err != nil {
			t.Fatal(err)
		}
		wantFail(t, name, CheckHost(h.env(false)), "ssh-mac-bridge", "/usr/local/bin/"+name)
	}
	h = newFakeHost(t) // an unrelated `open` is not a bridge
	h.write("usr/bin/open", "#!/bin/sh\n", 0o755)
	if f := failures(CheckHost(h.env(false))); len(f) != 0 {
		t.Errorf("an unrelated open failed: %q", f)
	}
}

func TestCheckHostItem4AgentForwarding(t *testing.T) {
	for name, tc := range map[string]struct{ sshd, why string }{
		"agent allowed":      {"allowagentforwarding yes\ndisableforwarding yes\n", "allowagentforwarding"},
		"forwarding enabled": {"allowagentforwarding no\ndisableforwarding no\n", "disableforwarding"},
		"keys missing":       {"port 22\n", "allowagentforwarding"},
	} {
		h := newFakeHost(t)
		h.sshd = tc.sshd
		wantFail(t, name, CheckHost(h.env(false)), "ssh-agent-forward", tc.why)
	}
	for _, p := range []string{"opt/orbstack-guest/run/agent.sock", "run/host-services/ssh-agent.sock"} {
		h := newFakeHost(t)
		h.write(p, "", 0o644)
		wantFail(t, p, CheckHost(h.env(false)), "ssh-agent-forward", "/"+p)
	}
}

func TestCheckHostDoctorRunsTheLoginWrapperWithAnAgentSocket(t *testing.T) {
	h := newFakeHost(t)
	h.loginExit = 64
	if f := failures(CheckHost(h.env(true))); len(f) != 0 {
		t.Errorf("a wrapper that refuses a forwarded agent failed: %q", f)
	}
	h.loginExit = 0
	wantFail(t, "wrapper let it through", CheckHost(h.env(true)), "ssh-agent-forward", "bitc-login exited 0 with SSH_AUTH_SOCK set, want 64")
	if f := failures(CheckHost(h.env(false))); len(f) != 0 {
		t.Errorf("the wrapper probe ran without the doctor flag: %q", f)
	}
}

func TestCheckHostDoctorWarnsOnSwap(t *testing.T) {
	h := newFakeHost(t)
	h.write("proc/swaps", "Filename Type Size Used Priority\n/dev/vda2 partition 1048576 0 -2\n", 0o444)
	found := false
	for _, f := range CheckHost(h.env(true)) {
		found = found || (f.Level == "warn" && f.ID == "ssh-swap" && f.Msg == "/dev/vda2 active; per-job secrets may reach disk")
	}
	if !found {
		t.Error("no `warn ssh-swap: /dev/vda2 active; per-job secrets may reach disk`")
	}
	for _, f := range CheckHost(h.env(false)) {
		if f.ID == "ssh-swap" {
			t.Error("swap is a doctor-only line")
		}
	}
}

func TestCheckHostItem6Users(t *testing.T) {
	type tc struct {
		name, file, from, to, why string
	}
	for _, c := range []tc{
		{"bitc shell", "etc/passwd", "/home/bitc:/bin/sh", "/home/bitc:/usr/sbin/nologin", "bitc: shell"},
		{"bitc password", "etc/shadow", "bitc:!:", "bitc:$6$abc$def:", "bitc: password"},
		{"bitc group", "etc/group", "sudo:x:27:", "sudo:x:27:bitc", "bitc: group sudo"},
		{"slot user missing", "etc/passwd", "bitc-s2:x:997:997::/var/lib/bitc/slots/2/home:/usr/sbin/nologin\n", "", "bitc-s2: missing"},
		{"slot password", "etc/shadow", "bitc-s1:!:", "bitc-s1:$6$abc$def:", "bitc-s1: password"},
		{"slot shell", "etc/passwd", "slots/1/home:/usr/sbin/nologin", "slots/1/home:/bin/bash", "bitc-s1: shell"},
		{"slot group", "etc/group", "sudo:x:27:", "sudo:x:27:bitc-s2", "bitc-s2: group sudo"},
		{"sudo rule for slot", "etc/sudoers", "root ALL", "bitc-s1 ALL=(ALL) NOPASSWD: ALL\nroot ALL", "sudo rule"},
		{"sudo rule for bitc", "etc/sudoers", "root ALL", "bitc ALL=(ALL) ALL\nroot ALL", "sudo rule"},
		{"sudoers differs", "etc/sudoers.d/bitc", "NOPASSWD: /opt/bitc/sbin/bitc-gate", "NOPASSWD: ALL", "sudoers.d/bitc"},
		{"authorized_keys options", "home/bitc/.ssh/authorized_keys", `restrict,command=`, `command=`, "authorized_keys"},
		{"authorized_keys second line", "home/bitc/.ssh/authorized_keys", "bitc-controller\n", "bitc-controller\nssh-ed25519 AAAA other\n", "authorized_keys"},
	} {
		h := newFakeHost(t)
		p := h.path(c.file)
		if !strings.Contains(readFile(t, p), c.from) {
			t.Fatalf("%s: fixture lacks %q", c.name, c.from)
		}
		h.write(c.file, strings.Replace(readFile(t, p), c.from, c.to, 1), 0o644)
		wantFail(t, c.name, CheckHost(h.env(false)), "ssh-users", c.why)
	}
	h := newFakeHost(t)
	h.visudo = fmt.Errorf("syntax error")
	wantFail(t, "visudo", CheckHost(h.env(false)), "ssh-users", "visudo")
}

func TestCheckHostItem6Modes(t *testing.T) {
	for name, fix := range map[string]func(h *fakeHost){
		"state dir mode":  func(h *fakeHost) { h.mkdir("var/lib/bitc", 0o755) },
		"state dir owner": func(h *fakeHost) { h.owners["bitc"] = 1000 },
		"gate mode":       func(h *fakeHost) { h.write("opt/bitc/sbin/bitc-gate", "#!/bin/sh\n", 0o775) },
		"login owner":     func(h *fakeHost) { h.owners["bitc-login"] = 995 },
	} {
		h := newFakeHost(t)
		fix(h)
		wantFail(t, name, CheckHost(h.env(false)), "ssh-users", "")
	}
}

func TestCheckHostItem6Cgroup(t *testing.T) {
	h := newFakeHost(t)
	h.write("sys/fs/cgroup/bitc/cgroup.controllers", "cpu\n", 0o444)
	wantFail(t, "no memory controller", CheckHost(h.env(false)), "ssh-users", "cgroup")
	h = newFakeHost(t)
	if err := os.RemoveAll(h.path("sys/fs/cgroup")); err != nil {
		t.Fatal(err)
	}
	wantFail(t, "no cgroup v2", CheckHost(h.env(false)), "ssh-users", "cgroup")
}

func TestCheckHostItem7LanRules(t *testing.T) {
	h := newFakeHost(t)
	h.nft = ""
	wantFail(t, "table missing", CheckHost(h.env(false)), "ssh-lan-rules", "table inet bitc missing")
	h = newFakeHost(t)
	h.nft = strings.Replace(nftJSON, `"family":"inet"`, `"family":"ip"`, 1)
	wantFail(t, "table differs", CheckHost(h.env(false)), "ssh-lan-rules", "table inet bitc differs")
	h = newFakeHost(t)
	h.write("proc/sys/net/ipv4/ip_forward", "0\n", 0o644)
	wantFail(t, "forwarding off", CheckHost(h.env(false)), "ssh-lan-rules", "ip_forward")
	h = newFakeHost(t) // handles differ after a reload: still equal
	h.nft = strings.Replace(nftJSON, `"handle":1`, `"handle":42`, 1)
	if f := failures(CheckHost(h.env(false))); len(f) != 0 {
		t.Errorf("a different handle failed: %q", f)
	}
}

func TestCheckHostItem8BinfmtRosetta(t *testing.T) {
	rosetta := "enabled\ninterpreter /media/rosetta/rosetta\nflags: F\noffset 0\nmagic 7f454c4602010100000000000000000002003e00\nmask fffffffffffffffcfffffffffffffffffeffffff\n"
	h := newFakeHost(t)
	h.write("proc/sys/fs/binfmt_misc/rosetta", rosetta, 0o644)
	wantFail(t, "rosetta magic", CheckHost(h.env(false)), "ssh-rosetta", "binfmt_misc handler rosetta runs x86-64; disable Rosetta")
	h = newFakeHost(t) // an x86-64 magic under another name
	h.write("proc/sys/fs/binfmt_misc/x64", strings.Replace(rosetta, "rosetta/rosetta", "bin/emu", 1), 0o644)
	wantFail(t, "x86-64 magic", CheckHost(h.env(false)), "ssh-rosetta", "handler x64")
	h = newFakeHost(t) // a name that says rosetta, any magic
	h.write("proc/sys/fs/binfmt_misc/Rosetta-2", "enabled\ninterpreter /x\nmagic 4d5a\n", 0o644)
	wantFail(t, "rosetta name", CheckHost(h.env(false)), "ssh-rosetta", "Rosetta-2")
	h = newFakeHost(t) // disabled entries, other machines and the control files are fine
	h.write("proc/sys/fs/binfmt_misc/off", strings.Replace(rosetta, "enabled", "disabled", 1), 0o644)
	h.write("proc/sys/fs/binfmt_misc/arm", "enabled\ninterpreter /usr/bin/qemu\nmagic 7f454c460201010000000000000000000200b700\n", 0o644)
	h.write("proc/sys/fs/binfmt_misc/status", "enabled\n", 0o644)
	h.write("proc/sys/fs/binfmt_misc/register", "", 0o200)
	if f := failures(CheckHost(h.env(false))); len(f) != 0 {
		t.Errorf("harmless binfmt entries failed: %q", f)
	}
}

func TestCheckHostItem9ContainerSockets(t *testing.T) {
	for _, p := range []string{"run/docker.sock", "var/run/containerd/containerd.sock", "opt/orbstack-guest/run/orbstack.sock", "run/podman/podman.sock"} {
		h := newFakeHost(t)
		h.write(p, "", 0o644)
		wantFail(t, p, CheckHost(h.env(false)), "ssh-docker-socket", "/"+p)
	}
	h := newFakeHost(t)
	h.write("proc/net/unix", "Num RefCount Protocol Flags Type St Inode Path\n0000: 00000002 00000000 00010000 0001 01 77 /srv/other/docker.sock\n", 0o644)
	wantFail(t, "bound socket elsewhere", CheckHost(h.env(false)), "ssh-docker-socket", "/srv/other/docker.sock")
	h = newFakeHost(t)
	h.write("etc/environment", "PATH=/bin\nDOCKER_HOST=unix:///run/x.sock\n", 0o644)
	wantFail(t, "etc/environment", CheckHost(h.env(false)), "ssh-docker-socket", "DOCKER_HOST")
	h = newFakeHost(t)
	h.environ = []string{"DOCKER_HOST=tcp://x"}
	wantFail(t, "root environment", CheckHost(h.env(false)), "ssh-docker-socket", "DOCKER_HOST")
}

func TestCheckHostItem10ReachabilityKey(t *testing.T) {
	h := newFakeHost(t)
	h.opened[h.path("etc/bitc/tunnel_key")] = true
	wantFail(t, "readable by slot user", CheckHost(h.env(false)), "ssh-reach-key", "/etc/bitc/tunnel_key readable by bitc-s1")
	h = newFakeHost(t)
	h.write("etc/bitc/tunnel_key", "secret", 0o640)
	wantFail(t, "mode", CheckHost(h.env(false)), "ssh-reach-key", "0600")
	h = newFakeHost(t)
	h.owners["tunnel_key"] = 1000
	wantFail(t, "owner", CheckHost(h.env(false)), "ssh-reach-key", "root")
	h = newFakeHost(t)
	h.owners["bitc"] = 996 // a parent directory not root-owned (the dir under etc)
	wantFail(t, "parent owner", CheckHost(h.env(false)), "ssh-reach-key", "directory")
	h = newFakeHost(t)
	h.mkdir("etc/bitc", 0o777)
	wantFail(t, "parent writable", CheckHost(h.env(false)), "ssh-reach-key", "directory")
	h = newFakeHost(t)
	if err := os.Remove(h.path("etc/bitc/tunnel_key")); err != nil {
		t.Fatal(err)
	}
	wantFail(t, "missing", CheckHost(h.env(false)), "ssh-reach-key", "tunnel_key")
}

func TestCheckHostItem11CronAtLinger(t *testing.T) {
	for name, fix := range map[string]func(h *fakeHost){
		"cron.allow missing": func(h *fakeHost) { _ = os.Remove(h.path("etc/cron.allow")) },
		"at.allow missing":   func(h *fakeHost) { _ = os.Remove(h.path("etc/at.allow")) },
		"cron.allow lists":   func(h *fakeHost) { h.write("etc/cron.allow", "root\nbitc-s2\n", 0o644) },
		"at.allow lists":     func(h *fakeHost) { h.write("etc/at.allow", "bitc-s1\n", 0o644) },
		"crontab exists":     func(h *fakeHost) { h.write("var/spool/cron/crontabs/bitc-s1", "* * * * * x\n", 0o600) },
		"at job owned":       func(h *fakeHost) { h.write("var/spool/cron/atjobs/a0001", "x", 0o700); h.owners["a0001"] = 997 },
		"linger file":        func(h *fakeHost) { h.write("var/lib/systemd/linger/bitc-s1", "", 0o644) },
	} {
		h := newFakeHost(t)
		fix(h)
		wantFail(t, name, CheckHost(h.env(false)), "ssh-cron", "")
	}
}

func TestCheckHostDoctorItem5LanConnects(t *testing.T) {
	h := newFakeHost(t)
	h.net.Connect["192.168.1.1:80"] = ConnRefused
	h.net.Connect["[2606:4700:4700::1111]:443"] = ConnOpen
	h.net.Connect["10.0.0.1:80"] = ConnBlocked
	fs := CheckHost(h.env(true))
	wantFail(t, "refused", fs, "ssh-lan", "bitc-s1 reached 192.168.1.1:80 (refused)")
	wantFail(t, "open", fs, "ssh-lan", "bitc-s1 reached [2606:4700:4700::1111]:443 (connected)")
	for _, f := range fs {
		if strings.Contains(f.Msg, "10.0.0.1") {
			t.Errorf("a blocked target was reported: %v", f)
		}
	}
	h = newFakeHost(t)
	h.net.DNSErr = fmt.Errorf("i/o timeout")
	wantFail(t, "dns", CheckHost(h.env(true)), "ssh-lan", "dns query to 9.9.9.9 failed")
	h = newFakeHost(t)
	h.netErr = fmt.Errorf("cannot make a namespace")
	wantFail(t, "probe error", CheckHost(h.env(true)), "ssh-lan", "cannot make a namespace")
	h = newFakeHost(t)
	var asked []string
	env := h.env(true)
	env.NetProbe = func(targets []string) (NetProbeResult, error) { asked = targets; return h.net, nil }
	CheckHost(env)
	for _, want := range []string{"192.168.64.1:80", "192.168.1.1:80", "10.0.0.1:80", "100.100.100.100:80", "198.18.0.1:80", "169.254.169.254:80", "[fe80::1]:80", "[2606:4700:4700::1111]:443"} {
		found := false
		for _, a := range asked {
			found = found || a == want
		}
		if !found {
			t.Errorf("item 5 never tried %s (asked %v)", want, asked)
		}
	}
}

func TestCheckHostDoctorItem12Seccomp(t *testing.T) {
	h := newFakeHost(t)
	h.secc = []string{"socket(AF_VSOCK) EAFNOSUPPORT", "unshare(CLONE_NEWUSER) EPERM", "clone3 EPERM", "keyctl(GET_KEYRING_ID) 12345"}
	fs := CheckHost(h.env(true))
	wantFail(t, "vsock", fs, "ssh-seccomp", "socket(AF_VSOCK) returned EAFNOSUPPORT, want EPERM")
	wantFail(t, "clone3", fs, "ssh-seccomp", "clone3 returned EPERM, want ENOSYS")
	wantFail(t, "keyctl", fs, "ssh-seccomp", "keyctl(GET_KEYRING_ID) returned 12345, want EPERM")
	h = newFakeHost(t)
	h.secc = nil
	wantFail(t, "silent probe", CheckHost(h.env(true)), "ssh-seccomp", "")
	h = newFakeHost(t)
	if f := failures(CheckHost(h.env(true))); len(f) != 0 {
		t.Errorf("a host that passes every doctor probe failed: %q", f)
	}
}

func TestSudoersAndSSHDTextsAreExact(t *testing.T) {
	if SudoersText != wantSudoers {
		t.Errorf("SudoersText = %q, want %q", SudoersText, wantSudoers)
	}
	want := "Match User bitc\n    AuthenticationMethods publickey\n    DisableForwarding yes\n    AllowAgentForwarding no\n    PermitTTY no\n"
	if SSHDSnippetText != want {
		t.Errorf("SSHDSnippetText = %q, want %q", SSHDSnippetText, want)
	}
}
