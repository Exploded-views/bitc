package agent

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
)

// Finding is one line of `check-host`: ok, warn or fail, the check id and a message.
type Finding struct{ Level, ID, Msg string }

func (f Finding) String() string {
	if f.Msg == "" {
		return f.Level + " " + f.ID
	}
	return f.Level + " " + f.ID + ": " + f.Msg
}

// ConnResult is what a connect from a job's side of the LAN block did.
type ConnResult int

const (
	ConnBlocked ConnResult = iota // timed out, EPERM or unreachable: the block works
	ConnRefused                   // an RST came back: the packet got through
	ConnOpen                      // the connection completed
)

// NetProbeResult is what item 5 learns as a slot user in the host's job network mode.
type NetProbeResult struct {
	Connect map[string]ConnResult // by "host:port"
	DNSErr  error
}

// HostEnv is everything check-host reads or runs, so tests can point it at a fake tree.
type HostEnv struct {
	Root   string // "" is the real root
	Conf   HostConf
	Doctor bool // also run items 5 and 12
	// Exec runs a command with stdin and returns its stdout.
	Exec func(stdin, name string, args ...string) ([]byte, error)
	// Owner returns the owning uid and gid of fi.
	Owner func(fi os.FileInfo) (uid, gid int)
	// OpenAs tries to open path read-only as uid:gid with no groups.
	OpenAs func(uid, gid int, path string) error
	// NetProbe runs item 5's connects and DNS query as the first slot user, in a job network
	// namespace when Conf.Netns, else in the VM's own.
	NetProbe func(targets []string) (NetProbeResult, error)
	// SeccompProbe runs item 12's calls as the first slot user under the job filter; it returns
	// "<call> <ERRNO>" lines.
	SeccompProbe func() ([]string, error)
	Environ      func() []string
	// LoginProbe runs bitc-login as the login user with SSH_AUTH_SOCK set and returns its exit
	// status (doctor only; 64 is right).
	LoginProbe func() (int, error)
	// Self is the agent binary the probes start (NewHostEnvFor sets it).
	Self string
}

func (e HostEnv) selfExe() string { return e.Self }

// SudoersText is the exact /etc/sudoers.d/bitc (providers delta C.5).
const SudoersText = "Defaults!/opt/bitc/sbin/bitc-gate !use_pty, !requiretty, !log_output, env_reset\n" +
	"bitc ALL=(root) NOPASSWD: /opt/bitc/sbin/bitc-gate\n"

// SSHDSnippetText is the exact /etc/ssh/sshd_config.d/bitc.conf.
const SSHDSnippetText = "Match User bitc\n    AuthenticationMethods publickey\n    DisableForwarding yes\n    AllowAgentForwarding no\n    PermitTTY no\n"

var authKeyRE = regexp.MustCompile(`^restrict,command="/opt/bitc/sbin/bitc-login" ssh-ed25519 [A-Za-z0-9+/=]+( \S.*)?$`)

type pwent struct {
	uid, gid    int
	home, shell string
}

type checker struct {
	e   HostEnv
	out []Finding
	bad map[string]bool
	pw  map[string]pwent
	// flagged dedups the unmounted-device warns
	flagged map[string]bool
}

func (c *checker) p(v string) string { return filepath.Join(c.e.Root, v) }

func (c *checker) fail(id, f string, a ...any) {
	c.out = append(c.out, Finding{"fail", id, fmt.Sprintf(f, a...)})
	c.bad[id] = true
}

// item runs one check and adds `ok <id>` when it failed nothing.
func (c *checker) item(id string, fn func()) {
	before := len(c.out)
	fn()
	if len(c.out) == before {
		c.out = append(c.out, Finding{Level: "ok", ID: id})
	}
}

func (c *checker) slotUsers() (names []string) {
	for i := 1; i <= c.e.Conf.Slots; i++ {
		names = append(names, "bitc-s"+strconv.Itoa(i))
	}
	return names
}

// CheckHost runs C.8 items 1-4 and 6-11, and 5 and 12 (and the swap and login-wrapper lines)
// when e.Doctor. The first failing finding is what the agent reports at job start.
func CheckHost(e HostEnv) []Finding {
	c := &checker{e: e, bad: map[string]bool{}, pw: map[string]pwent{}, flagged: map[string]bool{}}
	c.loadPasswd()
	c.item("ssh-host-mounts", c.mounts)
	c.item("ssh-mac-bridge", c.macBridge)
	c.item("ssh-agent-forward", c.agentForwarding)
	if e.Doctor {
		c.item("ssh-lan", c.lan)
	}
	c.item("ssh-users", c.users)
	c.item("ssh-lan-rules", c.lanRules)
	c.item("ssh-rosetta", c.rosetta)
	c.item("ssh-docker-socket", c.sockets)
	c.item("ssh-reach-key", c.reachKeys)
	c.item("ssh-cron", c.cron)
	if e.Doctor {
		c.item("ssh-seccomp", c.seccomp)
		c.swap()
		c.out = append(c.out, GitItems(e)...)
	}
	return c.out
}

func (c *checker) loadPasswd() {
	b, _ := os.ReadFile(c.p("/etc/passwd"))
	for _, l := range strings.Split(string(b), "\n") {
		f := strings.Split(l, ":")
		if len(f) < 7 {
			continue
		}
		uid, e1 := strconv.Atoi(f[2])
		gid, e2 := strconv.Atoi(f[3])
		if e1 == nil && e2 == nil {
			c.pw[f[0]] = pwent{uid: uid, gid: gid, home: f[5], shell: f[6]}
		}
	}
}

func (c *checker) owner(fi os.FileInfo) (int, int) {
	if c.e.Owner != nil {
		return c.e.Owner(fi)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return int(st.Uid), int(st.Gid)
	}
	return -1, -1
}

// ---- items 1 and 2: mounts in every mount namespace, virtio devices, host paths

var shareFS = map[string]bool{"virtiofs": true, "9p": true, "nfs": true, "nfs4": true, "cifs": true, "smb3": true,
	"vboxsf": true, "prl_fs": true, "fakeowner": true, "fuse": true}

type mountEnt struct {
	mp, fstype string
	pid        int
}

func unescapeMount(s string) string {
	return strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`).Replace(s)
}

// scanMounts reads /proc/<pid>/mountinfo for one pid of every distinct mount namespace (grouped
// by the link text of ns/mnt, pids in order) and returns the mounts and the init namespace's pid.
func (c *checker) scanMounts() (mounts []mountEnt) {
	ents, _ := os.ReadDir(c.p("/proc"))
	var pids []int
	for _, en := range ents {
		if n, err := strconv.Atoi(en.Name()); err == nil && en.IsDir() {
			pids = append(pids, n)
		}
	}
	sort.Ints(pids)
	seen := map[string]bool{}
	for _, pid := range pids {
		ns, err := os.Readlink(c.p(fmt.Sprintf("/proc/%d/ns/mnt", pid)))
		if err != nil || seen[ns] {
			continue
		}
		b, err := os.ReadFile(c.p(fmt.Sprintf("/proc/%d/mountinfo", pid)))
		if err != nil {
			continue
		}
		seen[ns] = true
		for _, l := range strings.Split(string(b), "\n") {
			left, right, ok := strings.Cut(l, " - ")
			if !ok {
				continue
			}
			lf, rf := strings.Fields(left), strings.Fields(right)
			if len(lf) < 5 || len(rf) < 1 {
				continue
			}
			mounts = append(mounts, mountEnt{unescapeMount(lf[4]), rf[0], pid})
		}
	}
	return mounts
}

var hostPaths = []string{"/Users", "/Volumes", "/mnt/mac", "/private", "/opt/orbstack-guest"}

func isShare(m mountEnt) bool {
	return (shareFS[m.fstype] || strings.HasPrefix(m.fstype, "fuse.")) && m.fstype != "fuse.lxcfs"
}

func (c *checker) warn(id, f string, a ...any) {
	c.out = append(c.out, Finding{"warn", id, fmt.Sprintf(f, a...)})
}

// The host-path walk stops at these limits and fails as too large to verify.
const (
	walkMaxEntries = 10000
	walkMaxDepth   = 16
)

func (c *checker) mounts() {
	reported := map[string]bool{}
	say := func(key, f string, a ...any) {
		if !reported[key] {
			reported[key] = true
			c.fail("ssh-host-mounts", f, a...)
		}
	}
	mounts := c.scanMounts()
	initPID := -1
	if len(mounts) > 0 {
		initPID = mounts[0].pid
	}
	where := func(m mountEnt) string {
		if m.pid == initPID {
			return "visible; disable host file sharing"
		}
		return fmt.Sprintf("in mount namespace of pid %d", m.pid)
	}
	anyShare := false
	for _, m := range mounts {
		if isShare(m) {
			anyShare = true
			say(m.mp+m.fstype, "%s (%s) %s", m.mp, m.fstype, where(m))
			continue
		}
		hit := strings.HasPrefix(m.mp, "/mnt/")
		for _, hp := range hostPaths {
			if m.mp == hp || strings.HasPrefix(m.mp, hp+"/") {
				hit = true
			}
		}
		if hit {
			say(m.mp, "%s (%s) %s", m.mp, m.fstype, where(m))
		}
	}
	roots := append([]string(nil), hostPaths...)
	if ents, err := os.ReadDir(c.p("/mnt")); err == nil {
		for _, en := range ents {
			roots = append(roots, "/mnt/"+en.Name())
		}
	}
	done := map[string]bool{}
	for _, r := range roots {
		if !done[r] {
			done[r] = true
			c.hostTree(r, say)
		}
	}
	// A blind scan (no namespace read) cannot rule a share out, so it keeps the fail.
	c.virtio(say, anyShare || len(mounts) == 0)
}

func entryType(m fs.FileMode) string {
	switch {
	case m.IsRegular():
		return "regular file"
	case m&fs.ModeSymlink != 0:
		return "symlink"
	case m&fs.ModeSocket != 0:
		return "socket"
	case m&fs.ModeNamedPipe != 0:
		return "fifo"
	case m&fs.ModeDevice != 0:
		return "device"
	}
	return "special file"
}

// hostTree checks one host path (or /mnt entry): absent is fine, a tree of directories only is a
// warn (an unmounted placeholder), anything else fails. It uses Lstat and ReadDir, so a symlink
// is reported and never followed.
func (c *checker) hostTree(root string, say func(key, f string, a ...any)) {
	fi, err := os.Lstat(c.p(root))
	if err != nil {
		if !os.IsNotExist(err) {
			say(root, "%s cannot be verified: %v", root, err)
		}
		return
	}
	if !fi.IsDir() {
		say(root, "%s (%s) is not a directory; remove the host share", root, entryType(fi.Mode()))
		return
	}
	n := 0
	var walk func(dir string, depth int) bool
	walk = func(dir string, depth int) bool { // false: stop, a failure was said
		ents, err := os.ReadDir(c.p(dir))
		if err != nil {
			say(root, "%s cannot be verified: %v", dir, err)
			return false
		}
		for _, en := range ents {
			n++
			p := dir + "/" + en.Name()
			if n > walkMaxEntries {
				say(root, "%s is too large to verify (over %d entries); remove the host share", root, walkMaxEntries)
				return false
			}
			if !en.IsDir() {
				say(root, "%s (%s) under host path %s; remove the host share", p, entryType(en.Type()), root)
				return false
			}
			if depth > walkMaxDepth {
				say(root, "%s is too deep to verify (over %d levels); remove the host share", root, walkMaxDepth)
				return false
			}
			if !walk(p, depth+1) {
				return false
			}
		}
		return true
	}
	if walk(root, 1) {
		c.warn("ssh-host-mounts", "%s holds only empty directories (not mounted)", root)
	}
}

// virtio finds shares that are exported but not mounted. With a share mounted somewhere a job
// after an escalation could reach more of it, so that fails; with none, this machine cannot see
// the share and the shared VM's exposure is the accepted shared-kernel risk (warn).
func (c *checker) virtio(say func(key, f string, a ...any), anyShare bool) {
	flag := func(key, what, fixText string) {
		if anyShare {
			say(key, "%s present; %s", what, fixText)
		} else if !c.flagged[key] {
			c.flagged[key] = true
			c.warn("ssh-host-mounts", "%s exported to the VM but not mounted in this machine (shared-kernel risk, decision 10)", what)
		}
	}
	var tags []string
	tagDirs, _ := os.ReadDir(c.p("/sys/fs/virtiofs"))
	for _, d := range tagDirs {
		if b, err := os.ReadFile(c.p("/sys/fs/virtiofs/" + d.Name() + "/tag")); err == nil {
			tags = append(tags, strings.TrimSpace(string(b)))
		}
	}
	used := map[string]bool{}
	devs, _ := os.ReadDir(c.p("/sys/bus/virtio/devices"))
	for _, d := range devs {
		b, err := os.ReadFile(c.p("/sys/bus/virtio/devices/" + d.Name() + "/device"))
		if err != nil {
			continue
		}
		switch strings.TrimSpace(string(b)) {
		case "0x001a":
			tag := "?"
			if len(tags) == 1 {
				tag = tags[0]
				used[tag] = true
			}
			flag("virtio:"+d.Name(), fmt.Sprintf(`virtio-fs device %s (tag "%s")`, d.Name(), tag), "remove the share from the VM")
		case "0x0009":
			flag("virtio:"+d.Name(), fmt.Sprintf("9p device %s", d.Name()), "remove the share from the VM")
		}
	}
	for _, t := range tags {
		if !used[t] {
			flag("tag:"+t, fmt.Sprintf(`virtio-fs tag "%s"`, t), "remove the share from the VM")
		}
	}
}

// ---- item 3: the Mac command bridge

func (c *checker) macBridge() {
	if ents, err := os.ReadDir(c.p("/opt/orbstack-guest/bin")); err == nil {
		for _, en := range ents {
			c.fail("ssh-mac-bridge", "/opt/orbstack-guest/bin/%s present", en.Name())
		}
	}
	for _, dir := range []string{"/usr/local/sbin", "/usr/local/bin", "/usr/sbin", "/usr/bin", "/sbin", "/bin"} {
		for _, name := range []string{"mac", "macctl", "open"} {
			p := path.Join(dir, name)
			target := p
			for i := 0; i < 8; i++ {
				fi, err := os.Lstat(c.p(target))
				if err != nil || fi.Mode()&os.ModeSymlink == 0 {
					break
				}
				l, _ := os.Readlink(c.p(target))
				if !path.IsAbs(l) {
					l = path.Join(path.Dir(target), l)
				}
				target = l
			}
			if _, err := os.Lstat(c.p(p)); err == nil && strings.HasPrefix(target, "/opt/orbstack-guest/") {
				c.fail("ssh-mac-bridge", "%s resolves to %s", p, target)
			}
		}
	}
}

// ---- item 4: agent forwarding

func (c *checker) agentForwarding() {
	out, err := c.e.Exec("", "sshd", "-T", "-C", "user=bitc,host=bitc,addr=127.0.0.1")
	if err != nil {
		c.fail("ssh-agent-forward", "sshd -T failed: %v", err)
	} else {
		got := map[string]string{}
		for _, l := range strings.Split(string(out), "\n") {
			if k, v, ok := strings.Cut(strings.TrimSpace(l), " "); ok {
				got[k] = v
			}
		}
		for k, want := range map[string]string{"allowagentforwarding": "no", "disableforwarding": "yes"} {
			if got[k] != want {
				c.fail("ssh-agent-forward", "sshd option %s is %q for user bitc, want %q", k, got[k], want)
			}
		}
	}
	for _, d := range []string{"/opt/orbstack-guest/run", "/run/host-services"} {
		ents, _ := os.ReadDir(c.p(d))
		for _, en := range ents {
			c.fail("ssh-agent-forward", "%s/%s present", d, en.Name())
		}
	}
	if c.e.Doctor && c.e.LoginProbe != nil {
		if code, err := c.e.LoginProbe(); err != nil || code != 64 {
			c.fail("ssh-agent-forward", "bitc-login exited %d with SSH_AUTH_SOCK set, want 64 (%v)", code, err)
		}
	}
}

// ---- item 5 (doctor): the LAN block as a slot user

var lanTargets = []string{"192.168.1.1:80", "10.0.0.1:80", "100.100.100.100:80", "198.18.0.1:80",
	"169.254.169.254:80", "[fe80::1]:80", "[2606:4700:4700::1111]:443"}

func (c *checker) lan() {
	targets := append([]string{c.e.Conf.Gateway + ":80", "host.orb.internal:80"}, lanTargets...)
	res, err := c.e.NetProbe(targets)
	if err != nil {
		c.fail("ssh-lan", "cannot probe: %v", err)
		return
	}
	keys := make([]string, 0, len(res.Connect))
	for k := range res.Connect {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		switch res.Connect[k] {
		case ConnRefused:
			c.fail("ssh-lan", "bitc-s1 reached %s (refused)", k)
		case ConnOpen:
			c.fail("ssh-lan", "bitc-s1 reached %s (connected)", k)
		}
	}
	if res.DNSErr != nil {
		c.fail("ssh-lan", "dns query to %s failed: %v", c.e.Conf.Resolver, res.DNSErr)
	}
}

// ---- item 6: users, sudo, keys, modes, cgroup

var nologinShells = map[string]bool{"/usr/sbin/nologin": true, "/sbin/nologin": true, "/bin/false": true, "/usr/bin/false": true}

func (c *checker) shadowLocked(user string) (known, locked bool) {
	b, err := os.ReadFile(c.p("/etc/shadow"))
	if err != nil {
		return false, false
	}
	for _, l := range strings.Split(string(b), "\n") {
		f := strings.Split(l, ":")
		if len(f) > 1 && f[0] == user {
			return true, strings.HasPrefix(f[1], "!") || strings.HasPrefix(f[1], "*")
		}
	}
	return false, false
}

func (c *checker) supplementary(user string) (groups []string) {
	b, _ := os.ReadFile(c.p("/etc/group"))
	for _, l := range strings.Split(string(b), "\n") {
		f := strings.Split(l, ":")
		if len(f) == 4 {
			for _, m := range strings.Split(f[3], ",") {
				if m == user {
					groups = append(groups, f[0])
				}
			}
		}
	}
	return groups
}

func (c *checker) users() {
	f := func(format string, a ...any) { c.fail("ssh-users", format, a...) }
	login, ok := c.pw["bitc"]
	switch {
	case !ok:
		f("bitc: missing")
	default:
		if login.shell != "/bin/sh" {
			f("bitc: shell is %q, want /bin/sh", login.shell)
		}
		if known, locked := c.shadowLocked("bitc"); !known || !locked {
			f("bitc: password is usable or unreadable; lock it")
		}
		for _, g := range c.supplementary("bitc") {
			f("bitc: group %s (no supplementary groups)", g)
		}
		c.authorizedKeys(login, f)
	}
	for _, u := range c.slotUsers() {
		pe, ok := c.pw[u]
		if !ok {
			f("%s: missing", u)
			continue
		}
		if !nologinShells[pe.shell] {
			f("%s: shell %q allows login", u, pe.shell)
		}
		if known, locked := c.shadowLocked(u); !known || !locked {
			f("%s: password is usable or unreadable; lock it", u)
		}
		for _, g := range c.supplementary(u) {
			f("%s: group %s (no supplementary groups)", u, g)
		}
	}
	c.sudoRules(f)
	if b, err := os.ReadFile(c.p("/etc/sudoers.d/bitc")); err != nil || string(b) != SudoersText {
		f("/etc/sudoers.d/bitc differs from the expected text (%v)", err)
	}
	if _, err := c.e.Exec("", "visudo", "-c"); err != nil {
		f("visudo -c failed: %v", err)
	}
	c.modes(f)
	c.cgroup(f)
}

func (c *checker) authorizedKeys(login pwent, f func(string, ...any)) {
	ak := path.Join(login.home, ".ssh", "authorized_keys")
	b, err := os.ReadFile(c.p(ak))
	if err != nil {
		f("authorized_keys: %v", err)
		return
	}
	n := 0
	for _, l := range strings.Split(string(b), "\n") {
		if l = strings.TrimSpace(l); l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		n++
		if !authKeyRE.MatchString(l) {
			f("authorized_keys: line %d is not `restrict,command=\"/opt/bitc/sbin/bitc-login\" ssh-ed25519 <key>`", n)
		}
	}
	if n != 1 {
		f("authorized_keys: %d key lines, want exactly 1", n)
	}
	for p, mode := range map[string]os.FileMode{ak: 0o644, path.Dir(ak): 0o755} {
		if fi, err := os.Stat(c.p(p)); err != nil {
			f("authorized_keys: %v", err)
		} else if u, _ := c.owner(fi); u != 0 || fi.Mode().Perm() != mode {
			f("authorized_keys: %s must be root-owned %04o", p, mode)
		}
	}
}

func (c *checker) sudoRules(f func(string, ...any)) {
	var files []string
	files = append(files, "/etc/sudoers")
	ents, _ := os.ReadDir(c.p("/etc/sudoers.d"))
	for _, en := range ents {
		if en.Name() != "bitc" {
			files = append(files, "/etc/sudoers.d/"+en.Name())
		}
	}
	for _, file := range files {
		b, _ := os.ReadFile(c.p(file))
		for _, l := range strings.Split(string(b), "\n") {
			if l = strings.TrimSpace(l); l == "" || strings.HasPrefix(l, "#") {
				continue
			}
			for _, tok := range strings.FieldsFunc(l, func(r rune) bool { return strings.ContainsRune(" \t,=:()", r) }) {
				if tok == "bitc" || tok == "%bitc" || strings.HasPrefix(tok, "bitc-s") || strings.HasPrefix(tok, "%bitc-s") {
					f("sudo rule names %s in %s", tok, file)
				}
			}
		}
	}
}

func (c *checker) modes(f func(string, ...any)) {
	for p, mode := range map[string]os.FileMode{"/var/lib/bitc": 0o700, "/opt/bitc/sbin/bitc-gate": 0o755, "/opt/bitc/sbin/bitc-login": 0o755} {
		fi, err := os.Stat(c.p(p))
		if err != nil {
			f("%s: %v", p, err)
			continue
		}
		if u, g := c.owner(fi); u != 0 || g != 0 || fi.Mode().Perm() != mode {
			f("%s must be root:root %04o", p, mode)
		}
	}
}

func (c *checker) cgroup(f func(string, ...any)) {
	root := "/sys/fs/cgroup"
	b, err := os.ReadFile(c.p(root + "/cgroup.controllers"))
	if err != nil {
		f("cgroup v2 not available: %v", err)
		return
	}
	need := func(list, where string) bool {
		have := map[string]bool{}
		for _, w := range strings.Fields(list) {
			have[w] = true
		}
		if !have["memory"] || !have["pids"] {
			f("cgroup v2 bitc subtree not delegable: %s lacks memory and pids (has %q)", where, strings.TrimSpace(list))
			return false
		}
		return true
	}
	if !need(string(b), root) {
		return
	}
	// read only: delegation (creating the subtree, +memory +pids) belongs to setupCgroup on the
	// job path and to the setup script, never to this check
	sb, err := os.ReadFile(c.p(root + "/bitc/cgroup.controllers"))
	if err != nil {
		f("cgroup v2 bitc subtree not delegable: %v", err)
		return
	}
	need(string(sb), root+"/bitc")
}

// ---- item 7: the nft table and forwarding

func (c *checker) slotUIDs() ([]int, error) {
	var uids []int
	for _, u := range c.slotUsers() {
		pe, ok := c.pw[u]
		if !ok {
			return nil, fmt.Errorf("%s is missing", u)
		}
		uids = append(uids, pe.uid)
	}
	return uids, nil
}

func (c *checker) lanRules() {
	if b, _ := os.ReadFile(c.p("/proc/sys/net/ipv4/ip_forward")); strings.TrimSpace(string(b)) != "1" {
		c.fail("ssh-lan-rules", "net.ipv4.ip_forward is not 1")
	}
	loaded, err := c.e.Exec("", "nft", "-j", "list", "table", "inet", "bitc")
	if err != nil {
		c.fail("ssh-lan-rules", "table inet bitc missing")
		return
	}
	uids, err := c.slotUIDs()
	if err != nil {
		c.fail("ssh-lan-rules", "cannot render the expected ruleset: %v", err)
		return
	}
	text, err := RenderNft(c.e.Conf, uids)
	if err != nil {
		c.fail("ssh-lan-rules", "cannot render the expected ruleset: %v", err)
		return
	}
	// the expected form is what the kernel makes of the rendered text, in a throwaway namespace
	want, err := c.e.Exec(text, "unshare", "-n", "sh", "-c", "nft -f - && nft -j list table inet bitc")
	if err != nil {
		c.fail("ssh-lan-rules", "cannot load the expected ruleset: %v", err)
		return
	}
	a, e1 := CanonNft(loaded)
	b, e2 := CanonNft(want)
	if e1 != nil || e2 != nil || a != b {
		c.fail("ssh-lan-rules", "table inet bitc differs")
	}
}

// ---- item 8: x86-64 binfmt handlers

func (c *checker) rosetta() {
	ents, _ := os.ReadDir(c.p("/proc/sys/fs/binfmt_misc"))
	for _, en := range ents {
		if en.Name() == "register" || en.Name() == "status" {
			continue
		}
		b, err := os.ReadFile(c.p("/proc/sys/fs/binfmt_misc/" + en.Name()))
		if err != nil {
			continue
		}
		lines := strings.Split(string(b), "\n")
		if len(lines) == 0 || strings.TrimSpace(lines[0]) != "enabled" {
			continue
		}
		var interp, magic string
		for _, l := range lines[1:] {
			if v, ok := strings.CutPrefix(l, "interpreter "); ok {
				interp = v
			}
			if v, ok := strings.CutPrefix(l, "magic "); ok {
				magic = strings.ToLower(strings.TrimSpace(v))
			}
		}
		x86 := len(magic) >= 40 && magic[:12] == "7f454c460201" && magic[36:40] == "3e00"
		if x86 || strings.Contains(strings.ToLower(en.Name()+interp), "rosetta") {
			c.fail("ssh-rosetta", "binfmt_misc handler %s runs x86-64; disable Rosetta", en.Name())
		}
	}
}

// ---- item 9: container-engine sockets

func engineName(n string) bool {
	n = strings.ToLower(n)
	for _, k := range []string{"docker", "containerd", "podman", "orbstack"} {
		if strings.Contains(n, k) {
			return true
		}
	}
	return false
}

func (c *checker) sockets() {
	seen := map[string]bool{}
	for _, root := range []string{"/run", "/var/run", "/opt/orbstack-guest"} {
		base := c.p(root)
		_ = filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				if strings.Count(strings.TrimPrefix(p, base), "/") > 6 {
					return filepath.SkipDir
				}
				return nil
			}
			if engineName(d.Name()) && (d.Type()&fs.ModeSocket != 0 || strings.HasSuffix(d.Name(), ".sock")) {
				v := "/" + strings.TrimPrefix(strings.TrimPrefix(p, c.e.Root), "/")
				if !seen[v] {
					seen[v] = true
					c.fail("ssh-docker-socket", "%s", v)
				}
			}
			return nil
		})
	}
	if f, err := os.Open(c.p("/proc/net/unix")); err == nil {
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			fs := strings.Fields(sc.Text())
			if len(fs) >= 8 && strings.HasPrefix(fs[7], "/") && engineName(path.Base(fs[7])) && !seen[fs[7]] {
				seen[fs[7]] = true
				c.fail("ssh-docker-socket", "%s", fs[7])
			}
		}
		f.Close()
	}
	if b, _ := os.ReadFile(c.p("/etc/environment")); bytes.Contains(append([]byte("\n"), b...), []byte("\nDOCKER_HOST=")) {
		c.fail("ssh-docker-socket", "DOCKER_HOST set in /etc/environment")
	}
	if c.e.Environ != nil {
		for _, kv := range c.e.Environ() {
			if strings.HasPrefix(kv, "DOCKER_HOST=") {
				c.fail("ssh-docker-socket", "DOCKER_HOST set in root's environment")
			}
		}
	}
}

// ---- item 10: the reachability credential

func (c *checker) reachKeys() {
	for _, sp := range c.e.Conf.SecretPaths {
		fi, err := os.Stat(c.p(sp))
		if err != nil {
			c.fail("ssh-reach-key", "%s: %v", sp, err)
			continue
		}
		if u, _ := c.owner(fi); u != 0 || fi.Mode().Perm() != 0o600 || !fi.Mode().IsRegular() {
			uid, _ := c.owner(fi)
			c.fail("ssh-reach-key", "%s must be a regular file owned by root with mode 0600 (is uid %d mode %04o)", sp, uid, fi.Mode().Perm())
		}
		for d := path.Dir(sp); ; d = path.Dir(d) {
			if di, err := os.Stat(c.p(d)); err == nil {
				if u, _ := c.owner(di); u != 0 || di.Mode().Perm()&0o022 != 0 {
					c.fail("ssh-reach-key", "%s: directory %s is not root-owned or is group/world-writable", sp, d)
				}
			}
			if d == "/" {
				break
			}
		}
		for _, u := range c.slotUsers() {
			if pe, ok := c.pw[u]; ok && c.e.OpenAs != nil && c.e.OpenAs(pe.uid, pe.gid, c.p(sp)) == nil {
				c.fail("ssh-reach-key", "%s readable by %s", sp, u)
			}
		}
	}
}

// ---- item 11: cron, at and linger

func (c *checker) cron() {
	slots := map[string]bool{}
	for _, u := range c.slotUsers() {
		slots[u] = true
	}
	for _, f := range []string{"/etc/cron.allow", "/etc/at.allow"} {
		b, err := os.ReadFile(c.p(f))
		if err != nil {
			c.fail("ssh-cron", "%s missing: slot users must be denied", f)
			continue
		}
		for _, l := range strings.Split(string(b), "\n") {
			if slots[strings.TrimSpace(l)] {
				c.fail("ssh-cron", "%s lists %s", f, strings.TrimSpace(l))
			}
		}
	}
	for _, u := range c.slotUsers() {
		if _, err := os.Lstat(c.p("/var/spool/cron/crontabs/" + u)); err == nil {
			c.fail("ssh-cron", "%s has a crontab", u)
		}
		if _, err := os.Lstat(c.p("/var/lib/systemd/linger/" + u)); err == nil {
			c.fail("ssh-cron", "%s has linger enabled", u)
		}
	}
	uids := map[int]string{}
	for _, u := range c.slotUsers() {
		uids[c.pw[u].uid] = u
	}
	for _, d := range []string{"/var/spool/cron/atjobs", "/var/spool/at"} {
		ents, _ := os.ReadDir(c.p(d))
		for _, en := range ents {
			if fi, err := en.Info(); err == nil {
				if uid, _ := c.owner(fi); uids[uid] != "" {
					c.fail("ssh-cron", "%s owns the at job %s", uids[uid], en.Name())
				}
			}
		}
	}
}

// ---- item 12 (doctor): the job seccomp filter, seen from a slot user

var seccompWant = []struct{ call, want string }{
	{"socket(AF_VSOCK)", "EPERM"}, {"socketpair(AF_VSOCK)", "EPERM"}, {"unshare(CLONE_NEWUSER)", "EPERM"},
	{"clone(CLONE_NEWUSER)", "EPERM"}, {"keyctl(GET_KEYRING_ID)", "EPERM"}, {"clone3", "ENOSYS"},
}

func (c *checker) seccomp() {
	lines, err := c.e.SeccompProbe()
	if err != nil {
		c.fail("ssh-seccomp", "probe failed: %v", err)
		return
	}
	got := map[string]string{}
	for _, l := range lines {
		if k, v, ok := strings.Cut(l, " "); ok {
			got[k] = v
		}
	}
	for _, w := range seccompWant {
		r, ok := got[w.call]
		if !ok {
			r = "nothing"
		}
		if r != w.want {
			c.fail("ssh-seccomp", "%s returned %s, want %s", w.call, r, w.want)
		}
	}
}

func (c *checker) swap() {
	b, err := os.ReadFile(c.p("/proc/swaps"))
	if err != nil {
		return
	}
	for i, l := range strings.Split(string(b), "\n") {
		if f := strings.Fields(l); i > 0 && len(f) > 0 {
			c.out = append(c.out, Finding{"warn", "ssh-swap", f[0] + " active; per-job secrets may reach disk"})
		}
	}
}

// gitChangelogPaths are the Debian and Ubuntu package changelogs of git, under the host root.
var gitChangelogPaths = []string{"/usr/share/doc/git/changelog.Debian.gz", "/usr/share/doc/git/changelog.gz"}

// gitChangelogMax bounds what is read of a changelog once decompressed.
const gitChangelogMax = 16 << 20

// GitItems are the two doctor items the controller's ssh-git rule judges for the upload
// transport: ssh-git-version, the text of `git --version` of the git the stage-2 code step runs
// (found on the agent's PATH, like every step), and ssh-git-changelog, the first line of the
// package changelog under e.Root that names CVE-2024-32002 (empty when there is none). The
// judgement itself stays controller-side. A host whose git cannot be run prints neither item.
func GitItems(e HostEnv) []Finding {
	out, err := e.Exec("", "git", "--version")
	if err != nil {
		return nil
	}
	version, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	return []Finding{{"ok", "ssh-git-version", version}, {"ok", "ssh-git-changelog", gitChangelogLine(e.Root)}}
}

func gitChangelogLine(root string) string {
	for _, p := range gitChangelogPaths {
		f, err := os.Open(filepath.Join(root, p))
		if err != nil {
			continue
		}
		var r io.Reader = f
		if strings.HasSuffix(p, ".gz") {
			zr, err := gzip.NewReader(f)
			if err != nil {
				f.Close()
				continue
			}
			r = zr
		}
		sc := bufio.NewScanner(io.LimitReader(r, gitChangelogMax))
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			if l := strings.TrimSpace(sc.Text()); strings.Contains(l, "CVE-2024-32002") {
				f.Close()
				return l
			}
		}
		f.Close()
	}
	return ""
}
