//go:build privileged

package conformance

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Exploded-views/bitc/internal/agent"
)

// SH18: the gate's wiring through a real sshd, bitc-login and sudo, and the setup script (the
// runbook) that makes it.
func TestSH18_GateWiringThroughSshdLoginAndSudo(t *testing.T) {
	r := rig(t)
	out, errOut, code := r.ctl("probe", nil)
	if code != 0 || !strings.Contains(out, `"gate_version":1`) || !strings.Contains(out, `"slots":2`) || !strings.Contains(out, `"users_ok":true`) {
		t.Errorf("probe through ssh: code %d, out %q, err %q", code, out, errOut)
	}
	// the client's command reaches the gate as its one argument, nothing else does
	if out, _, code := r.ctl("probe; id", nil); code != 64 || out != "" {
		t.Errorf("a command with a second statement: code %d, out %q; want 64 and nothing on stdout", code, out)
	}
	// bitc-login refuses a forwarded agent before sudo
	script := `runuser -u bitc -- env SSH_AUTH_SOCK=/tmp/agent.sock SSH_ORIGINAL_COMMAND=probe /bin/sh -c /opt/bitc/sbin/bitc-login`
	c := exec.Command("/bin/sh", "-c", script)
	o, err := c.Output()
	if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 64 || len(o) != 0 {
		t.Errorf("bitc-login with SSH_AUTH_SOCK set: err %v, out %q; want exit 64 and no output", err, o)
	}
	// a bitc whose shell is nologin cannot be the forced command's shell: doctor says so
	sh(t, "usermod -s /usr/sbin/nologin bitc")
	chk, _ := r.checkHost("--doctor")
	sh(t, "usermod -s /bin/sh bitc")
	if !strings.Contains(chk, "fail ssh-users: bitc: shell") {
		t.Errorf("shell nologin: no `fail ssh-users: bitc: shell` line in\n%s", chk)
	}
	// the only sudo rule is the gate's
	if out := sh(t, "sudo -l -U bitc"); !strings.Contains(out, "(root) NOPASSWD: /opt/bitc/sbin/bitc-gate") || strings.Contains(out, "ALL") && !strings.Contains(out, "(root) NOPASSWD: /opt/bitc/sbin/bitc-gate\n") {
		t.Errorf("sudo -l -U bitc:\n%s", out)
	}
}

// The setup script is idempotent, prints its changes, refuses a non-root user and changes
// nothing under --dry-run.
func TestSH18_SetupScriptIsIdempotentDryRunsAndRefusesNonRoot(t *testing.T) {
	r := rig(t)
	if r.SetupDryErr != "" || !strings.Contains(r.SetupDry, "would: create user bitc-s1") || strings.Contains(r.SetupDry, "change:") {
		t.Errorf("--dry-run: err %q, output\n%s", r.SetupDryErr, r.SetupDry)
	}
	if r.SnapBefore != r.SnapAfterDry {
		t.Errorf("a dry run changed the host: snapshot %s -> %s", r.SnapBefore, r.SnapAfterDry)
	}
	if !strings.Contains(r.NonRoot, "must run as root") || !strings.Contains(r.NonRoot, "exit=2") {
		t.Errorf("a non-root run: %q", r.NonRoot)
	}
	if !strings.Contains(r.Setup1, "change: write /opt/bitc/sbin/bitc-gate") || !strings.Contains(r.Setup1, "change: create user bitc-s2") {
		t.Errorf("the first run printed no changes:\n%s", r.Setup1)
	}
	if strings.Contains(r.Setup2, "change:") {
		t.Errorf("the second run changed something:\n%s", r.Setup2)
	}
	// netns=false with two slots is refused before anything changes
	out, err := rigRun(`sh /src/scripts/ssh-host-setup.sh --netns false --slots 2 --resolver ` + rigResolver + ` --gateway ` + rigGateway + ` --controller-key 'ssh-ed25519 AAAA' --dry-run`)
	if err == nil || !strings.Contains(out, "--netns false requires --slots 1") {
		t.Errorf("--netns false --slots 2: %v %q", err, out)
	}
	// every listed mode and owner is as the layout says
	for p, want := range map[string]string{"/opt/bitc/sbin/bitc-gate": "755 root", "/opt/bitc/sbin/bitc-login": "755 root", "/var/lib/bitc": "700 root",
		"/var/lib/bitc/slots/1/work": "700 bitc-s1", "/var/lib/bitc/slots/2/tmp": "700 bitc-s2", "/etc/sudoers.d/bitc": "440 root", "/etc/bitc/host.conf": "644 root",
		"/home/bitc/.ssh/authorized_keys": "644 root", "/home/bitc/.ssh": "755 root", "/etc/cron.allow": "644 root", "/etc/at.allow": "644 root"} {
		if got := strings.TrimSpace(sh(t, "stat -c '%a %U' "+p)); got != want {
			t.Errorf("%s is %q, want %q", p, got, want)
		}
	}
}

// SH11: install rejects a sha mismatch; the agent verbs re-hash what they run.
func TestSH11_InstallRejectsAShaMismatchAndAgentsAreRehashed(t *testing.T) {
	r := rig(t)
	bad := strings.Repeat("ab", 32)
	out, errOut, code := r.ctl("install "+bad, []byte("not an agent"))
	if code != 65 || out != "" || !strings.Contains(errOut, "sha256 mismatch") || exists("/opt/bitc/agent/"+bad) {
		t.Errorf("install with a wrong sha: code %d, out %q, err %q, dir exists %v", code, out, errOut, exists("/opt/bitc/agent/"+bad))
	}
	if out, errOut, code := r.ctl("agent "+bad+" list", nil); code != 66 || out != "" {
		t.Errorf("agent of an unknown sha: code %d, out %q, err %q; want 66", code, out, errOut)
	}
	// a swapped binary is caught at the next use
	other := strings.Repeat("cd", 32)
	sh(t, "mkdir -p /opt/bitc/agent/"+other+" && printf '#!/bin/sh\\necho pwned\\n' > /opt/bitc/agent/"+other+"/bitc && chmod 755 /opt/bitc/agent/"+other+"/bitc")
	defer sh(t, "rm -rf /opt/bitc/agent/"+other)
	if out, errOut, code := r.ctl("agent "+other+" list", nil); code != 65 || out != "" || !strings.Contains(errOut, "sha256 mismatch") {
		t.Errorf("a binary that does not hash to its directory: code %d, out %q, err %q; want 65", code, out, errOut)
	}
	// only the newest three installs stay
	for i := 0; i < 4; i++ {
		body := []byte(fmt.Sprintf("#!/bin/sh\necho %d\n", i))
		sum := strings.Fields(sh(t, fmt.Sprintf("printf '#!/bin/sh\\necho %d\\n' | sha256sum", i)))[0]
		if _, _, c := r.ctl("install "+sum, body); c != 0 {
			t.Fatalf("install %d: code %d", i, c)
		}
		time.Sleep(1100 * time.Millisecond)
	}
	if n := strings.TrimSpace(sh(t, "ls /opt/bitc/agent | grep -Ec '^[0-9a-f]{64}$'")); n != "3" {
		t.Errorf("%s agents kept, want the newest 3", n)
	}
	// the rig's own agent was among the oldest: put it back for the tests after this one
	bin, _ := os.ReadFile(r.Bin)
	if out, _, code := r.ctl("install "+r.Sha, bin); code != 0 || !strings.HasPrefix(out, "installed") {
		t.Fatalf("reinstall of the rig's agent: %q %d", out, code)
	}
}

// SH14: the slot handoff.
func TestSH14_SlotHandoffOwnerNonceAndLease(t *testing.T) {
	r := rig(t)
	a, out, _, code := r.prepare("1", "")
	if code != 0 || out != "prepared "+a.Name+"\n" {
		t.Fatalf("prepare: %q code %d", out, code)
	}
	defer a.teardown()
	b, out, _, code := r.prepare("1", "")
	if code != 65 || out != "refused: slot 1 busy on host\n" {
		t.Errorf("a second controller's prepare of a leased slot: %q code %d; want the busy refusal and 65", out, code)
	}
	stale := *a
	stale.Nonce = randHex(16)
	if out, _, code := stale.run("true"); code != 65 || out != "refused: slot 1 not leased to "+a.Name+"\n" {
		t.Errorf("--run with a stale nonce: %q code %d", out, code)
	}
	if out, _, code := b.run("true"); code != 65 || out != "refused: slot 1 not leased to "+b.Name+"\n" {
		t.Errorf("--run by a name that never got the slot: %q code %d", out, code)
	}
	// a truncated payload starts nothing
	frame := a.frame("true", time.Minute, 10*time.Minute)
	if out, _, code := r.agentCtl("slot-boot --run 1 "+a.Name+" "+a.Nonce, frame[:len(frame)/2]); code != 65 || out != "refused: truncated payload\n" {
		t.Errorf("--run with half a payload: %q code %d", out, code)
	}
	if out, _, code := r.agentCtl("slot-boot --run 1 "+a.Name+" "+a.Nonce, frame[:len(frame)/2]); code != 65 {
		t.Errorf("a refused payload must leave the lease alone so the controller can retry: %q code %d", out, code)
	}
	// an unfinished lease can be taken once it has run out
	c, out, _, code := r.prepare("2", " lease=1")
	if code != 0 {
		t.Fatalf("prepare slot 2 with lease=1: %q %d", out, code)
	}
	defer c.teardown()
	time.Sleep(2200 * time.Millisecond)
	d, out, _, code := r.prepare("2", "")
	defer d.teardown()
	if code != 0 || out != "prepared "+d.Name+"\n" {
		t.Errorf("prepare over an expired lease: %q code %d", out, code)
	}
	// and the full path works for the holder of the nonce, after which the slot is busy until the deadline
	if out, errOut, code := a.run("echo hello"); code != 0 || out != "started "+a.Name+"\n" {
		t.Fatalf("--run by the holder: %q %q code %d", out, errOut, code)
	}
	if res := a.wait(t); res.Out != "hello\n" || res.Exit != 0 {
		t.Errorf("job output %q exit %d", res.Out, res.Exit)
	}
	if _, out, _, code := r.prepare("1", ""); code != 65 || !strings.Contains(out, "busy") {
		t.Errorf("a finished but unreleased job's slot: %q code %d; want busy", out, code)
	}
}

// SH15: a teardown by name acts only on that name's slot.
func TestSH15_StaleTeardownIsGoneAndKillsNothing(t *testing.T) {
	r := rig(t)
	j, out, _, code := r.prepare("1", "")
	if code != 0 {
		t.Fatalf("prepare: %q", out)
	}
	if out, errOut, code := j.run("sleep 120"); code != 0 {
		t.Fatalf("run: %q %q", out, errOut)
	}
	deadline := time.Now().Add(30 * time.Second)
	for procsOfUser("bitc-s1") == 0 && time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
	}
	if procsOfUser("bitc-s1") == 0 {
		t.Fatal("the job never started")
	}
	out, _, code = r.agentCtl("teardown 1 some-other-job", nil)
	if code != 0 || out != "gone\n" {
		t.Errorf("teardown of a name that does not own the slot: %q code %d; want gone", out, code)
	}
	if procsOfUser("bitc-s1") == 0 {
		t.Error("a stale teardown killed the running job")
	}
	if own, _, _ := r.agentCtl("list", nil); !strings.Contains(own, `"name":"`+j.Name+`"`) {
		t.Errorf("the slot lost its owner: %s", own)
	}
	if out, _, code := j.teardown(); code != 0 || out != "torn\n" {
		t.Errorf("own teardown: %q code %d", out, code)
	}
	if n := procsOfUser("bitc-s1"); n != 0 {
		t.Errorf("%d processes of bitc-s1 after the real teardown", n)
	}
	if out, _, _ := j.teardown(); out != "gone\n" {
		t.Errorf("second teardown: %q, want gone", out)
	}
}

// SH17: a reachability key a slot user can open fails check-host before attach.
func TestSH17_ReachabilityKeyReadableBySlotUser(t *testing.T) {
	r := rig(t)
	if out, code := r.checkHost(); code != 0 || len(fails(out)) != 0 {
		t.Fatalf("the set-up host does not pass check-host: code %d\n%s", code, out)
	}
	sh(t, "chmod 0644 /etc/bitc/tunnel_key")
	defer sh(t, "chmod 0600 /etc/bitc/tunnel_key")
	out, errOut, code := r.agentCtl("check-host", nil)
	if code != 69 || !strings.Contains(out, "fail ssh-reach-key: /etc/bitc/tunnel_key readable by bitc-s1") {
		t.Errorf("check-host with a world-readable key: code %d, out\n%s%s", code, out, errOut)
	}
	if !strings.Contains(out, "mode 0600") {
		t.Errorf("no line naming the mode in\n%s", out)
	}
}

// SH20: the nft table gone after a reboot fails the next check-host.
func TestSH20_NftTableRemovedFailsCheckHost(t *testing.T) {
	r := rig(t)
	sh(t, "nft delete table inet bitc")
	defer sh(t, "nft -f /etc/nftables.d/bitc.nft")
	out, _, code := r.agentCtl("check-host", nil)
	if code != 69 || !strings.Contains(out, "fail ssh-lan-rules: table inet bitc missing") {
		t.Errorf("check-host without the table: code %d\n%s", code, out)
	}
	// a changed rule is a different table
	sh(t, "nft -f /etc/nftables.d/bitc.nft && nft flush chain inet bitc forward")
	out, _, code = r.agentCtl("check-host", nil)
	if code != 69 || !strings.Contains(out, "fail ssh-lan-rules: table inet bitc differs") {
		t.Errorf("check-host with an emptied chain: code %d\n%s", code, out)
	}
	sh(t, "nft -f /etc/nftables.d/bitc.nft")
	if out, code := r.checkHost(); code != 0 {
		t.Errorf("check-host after the ruleset is loaded again: code %d\n%s", code, out)
	}
}

var _ = net.Dial
var _ = regexp.MustCompile
var _ = agent.Main

// SH7: isolation between slots and between jobs.
func TestSH7_JobsAreIsolatedAndLeaveNothingBehind(t *testing.T) {
	r := rig(t)
	// a job in slot 2 leaves its files behind (its slot is not torn down yet)
	j2, res2 := r.job(t, "2", `echo S2 > secret2; echo S2 > /tmp/secret2-tmp; echo ok`)
	defer j2.teardown()
	if res2.Exit != 0 || res2.Out != "ok\n" {
		t.Fatalf("slot 2 job: %+v", res2)
	}
	sh(t, "mkdir -p /var/lib/systemd/linger /srv/ww")
	jA, resA := r.job(t, "1", `
echo A > /tmp/a-tmp; echo A > /var/tmp/a-vartmp; echo A > /run/lock/a-lock; echo A > /dev/shm/a-shm; echo A > "$HOME/a-home"
ipcmk -M 1024 >/dev/null 2>&1 && echo shm-created
python3 - <<'PY'
import ctypes, os
libc = ctypes.CDLL(None, use_errno=True)
fd = libc.mq_open(b"/bitcq", os.O_CREAT | os.O_RDWR, 0o600, None)
print("mq", "ok" if fd >= 0 else "fail %d" % ctypes.get_errno())
PY
out=$(echo '* * * * * true' | crontab - 2>&1); echo "crontab rc=$?"
out=$(echo true | at now 2>&1); echo "at rc=$?"
( setsid sh -c 'sleep 600' >/dev/null 2>&1 & )
( sh -c 'sleep 601 &' )
echo A > /srv/ww/leftover
echo done
`)
	if resA.Exit != 0 || !strings.Contains(resA.Out, "shm-created") || !strings.Contains(resA.Out, "mq ok") || !strings.HasSuffix(resA.Out, "done\n") {
		t.Fatalf("job A did not do its part: %+v", resA)
	}
	for _, denied := range []string{"crontab rc=", "at rc="} {
		i := strings.Index(resA.Out, denied)
		if i < 0 || strings.HasPrefix(resA.Out[i+len(denied):], "0\n") {
			t.Errorf("%s: want a non-zero status in\n%s", denied, resA.Out)
		}
	}
	// the daemons the job double-forked are gone with the job
	deadline := time.Now().Add(10 * time.Second)
	for procsOfUser("bitc-s1") != 0 && time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
	}
	if n := procsOfUser("bitc-s1"); n != 0 {
		t.Errorf("%d processes of bitc-s1 survive the end of the job", n)
	}
	// a linger file planted now is removed and reported by the teardown, with the stray file
	sh(t, "touch /var/lib/systemd/linger/bitc-s1")
	out, errOut, code := jA.teardown()
	if code != 0 || out != "torn\n" {
		t.Errorf("teardown: %q %q code %d", out, errOut, code)
	}
	if exists("/var/lib/systemd/linger/bitc-s1") || !strings.Contains(errOut, "pod: removed linger of bitc-s1") {
		t.Errorf("planted linger file: exists %v, teardown stderr %q", exists("/var/lib/systemd/linger/bitc-s1"), errOut)
	}
	if exists("/srv/ww/leftover") || !regexp.MustCompile(`pod: removed [1-9][0-9]* stray files of bitc-s1`).MatchString(errOut) {
		t.Errorf("stray file in a world-writable dir: exists %v, teardown stderr %q", exists("/srv/ww/leftover"), errOut)
	}
	// job B reuses slot 1 and finds nothing of A, nothing of slot 2, nothing of the host
	jB, resB := r.job(t, "1", `
for p in /tmp/a-tmp /var/tmp/a-vartmp /run/lock/a-lock /dev/shm/a-shm "$HOME/a-home"; do [ -e "$p" ] && echo "LEAK $p"; done
echo "tmpfiles $(find /tmp /var/tmp /run/lock /dev/shm -mindepth 1 | wc -l)"
echo "shm $(ipcs -m | grep -c '^0x')"
echo "mq $(ls -A /dev/mqueue | wc -l)"
echo "slot2: $(ls /var/lib/bitc/slots/2 2>&1 | head -1)"
echo "secret2: $(cat /var/lib/bitc/slots/2/work/*/secret2 2>&1 | head -1)"
echo "work: $(ls -d /var/lib/bitc/slots/1/work 2>&1)"
echo "state: $(ls /var/lib/bitc 2>&1 | head -1)"
echo "results: $(ls /var/lib/bitc/results 2>&1 | head -1)"
echo "owner: $(cat /var/lib/bitc/slots/1/owner 2>&1 | head -1)"
echo "uid $(id -u) caps $(grep CapEff /proc/self/status | cut -f2) nnp $(grep NoNewPrivs /proc/self/status | cut -f2)"
`)
	defer jB.teardown()
	for _, l := range strings.Split(resB.Out, "\n") {
		if strings.HasPrefix(l, "LEAK") {
			t.Errorf("job B sees what job A left: %s", l)
		}
	}
	for _, want := range []string{"tmpfiles 0\n", "shm 0\n", "mq 0\n", "No such file or directory", "work: /var/lib/bitc/slots/1/work\n"} {
		if !strings.Contains(resB.Out, want) {
			t.Errorf("job B's view lacks %q:\n%s", want, resB.Out)
		}
	}
	if strings.Contains(resB.Out, "S2") || strings.Contains(resB.Out, "owner: {") || strings.Contains(resB.Out, "results: job") {
		t.Errorf("job B reads another slot's or the host's files:\n%s", resB.Out)
	}
	if !strings.Contains(resB.Out, "caps 0000000000000000") || !strings.Contains(resB.Out, "nnp 1") {
		t.Errorf("job B keeps privileges:\n%s", resB.Out)
	}
}

// netJob starts a job on slots that prints the connect results of the given probe lines and
// returns its output; extra is shell run before the probes.
func netProbes(targets string) string {
	return "python3 - <<'PY'\n" + probePy + targets + "\nPY\n"
}

// probeLines parses `<host> <port> open|refused|blocked` lines into a map keyed by host:port.
func probeLines(out string) map[string]string {
	m := map[string]string{}
	for _, l := range strings.Split(out, "\n") {
		if f := strings.Fields(l); len(f) >= 3 && (f[2] == "open" || f[2] == "refused" || f[2] == "blocked") {
			m[f[0]+":"+f[1]] = f[2]
		}
	}
	return m
}

// netPair runs two concurrent jobs, one per slot, that listen on :3000 and :5432 and probe the
// other job, the VM, the LAN and the internet; it returns their parsed probe lines and outputs.
func netPair(t *testing.T, r *shRigT) (got [2]map[string]string, outs [2]string) {
	t.Helper()
	listen := func(tag string) string {
		return `python3 - <<'PY' &
import socket, threading, time
for port in (3000, 5432):
    s = socket.socket(); s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1); s.bind(("0.0.0.0", port)); s.listen(8)
    def loop(s=s):
        while True:
            c, _ = s.accept(); c.sendall(b"` + tag + `\n"); c.close()
    threading.Thread(target=loop, daemon=True).start()
time.sleep(40)
PY
sleep 2
`
	}
	other := map[string]string{"1": "10.203.2.2", "2": "10.203.1.2"}
	script := func(slot string) string {
		return listen("J"+slot) + `echo "dns $(python3 -c "
import socket
s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM); s.settimeout(3)
s.sendto(b'\\xb1\\x7c\\x01\\x00\\x00\\x01\\x00\\x00\\x00\\x00\\x00\\x00\\x07example\\x03com\\x00\\x00\\x01\\x00\\x01', ('` + rigResolver + `', 53))
print('ok' if len(s.recvfrom(512)[0]) >= 12 else 'short')
" 2>&1 | tail -1)"
echo "addr $(ip -4 -o addr show dev eth0 | awk '{print $4}')"
echo "lo-self $(python3 -c "
import socket
c = socket.create_connection(('127.0.0.1', 3000), 3); print(c.recv(8).decode().strip())
")"
echo "own-addr $(python3 -c "
import socket
c = socket.create_connection(('10.203.` + slot + `.2', 5432), 3); print(c.recv(8).decode().strip())
")"
sleep 5
` + netProbes(fmt.Sprintf(`c("%s", 3000)
c("%s", 5432)
c("10.203.%s.1", 2222)
c("%s", 80)
c("%s", 80)
c("%s", 80)
c("%s", 443)
c("%s", 443, socket.AF_INET6)
c("169.254.169.254", 80)
c("100.64.0.1", 80)
c("198.18.0.1", 80)`, other[slot], other[slot], slot, rigLANDevice, rigGateway, rigResolver, rigInternet, rigV6Device))
	}
	var jobs [2]*rigJob
	for i, slot := range []string{"1", "2"} {
		j, out, errOut, code := r.prepare(slot, "")
		if code != 0 {
			t.Fatalf("prepare %s: %q %q", slot, out, errOut)
		}
		jobs[i] = j
		t.Cleanup(func() { j.teardown() })
		if out, errOut, code := j.run(script(slot)); code != 0 {
			t.Fatalf("run %s: %q %q", slot, out, errOut)
		}
	}
	for i, j := range jobs {
		res := j.wait(t)
		outs[i] = res.Out
		got[i] = probeLines(res.Out)
		slot := fmt.Sprint(i + 1)
		if !strings.Contains(res.Out, "dns ok") || !strings.Contains(res.Out, "lo-self J"+slot) || !strings.Contains(res.Out, "own-addr J"+slot) || !strings.Contains(res.Out, "addr 10.203."+slot+".2/30") {
			t.Errorf("slot %s job: DNS to the resolver, its own listeners or its address are wrong:\n%s", slot, res.Out)
		}
		j.teardown()
	}
	return got, outs
}

// SH16: two concurrent jobs each in their own network namespace, no route to the other job, the
// VM or the LAN, DNS to the resolver only.
func TestSH16_ConcurrentJobsHaveSeparateNetworksAndNoLAN(t *testing.T) {
	r := rig(t)
	other := map[string]string{"1": "10.203.2.2", "2": "10.203.1.2"}
	blocked := func(slot string) []string {
		return []string{other[slot] + ":3000", other[slot] + ":5432", "10.203." + slot + ".1:2222", rigLANDevice + ":80", rigGateway + ":80", rigResolver + ":80",
			rigV6Device + ":443", "169.254.169.254:80", "100.64.0.1:80", "198.18.0.1:80"}
	}
	got, outs := netPair(t, r)
	for i, slot := range []string{"1", "2"} {
		for _, k := range blocked(slot) {
			if got[i][k] != "blocked" {
				t.Errorf("slot %s job: %s is %q, want blocked\n%s", slot, k, got[i][k], outs[i])
			}
		}
		if got[i][rigInternet+":443"] != "open" {
			t.Errorf("slot %s job: the internet stand-in is %q, want open (NAT works)\n%s", slot, got[i][rigInternet+":443"], outs[i])
		}
	}
	// control: with the LAN and job-to-job drops taken out of the ruleset the same probes get
	// through, so the blocked results above are the ruleset's doing and not an artifact. A job
	// would refuse to start on such a host (check-host), so the namespaces are started directly.
	sh(t, `grep -v -e 'ip daddr @lan4 drop' -e '^    iifname "bv\*" ip daddr `+rigGateway+` drop' -e '^    oifname "bv\*" drop' /etc/nftables.d/bitc.nft > /tmp/shrig/permissive.nft && nft -f /tmp/shrig/permissive.nft`)
	defer sh(t, "nft -f /etc/nftables.d/bitc.nft")
	conf := agent.HostConf{Slots: 2, Resolver: rigResolver, Gateway: rigGateway, Netns: true}
	var nss [2]*agent.JobNS
	for i := range nss {
		ns, err := agent.StartJobNS(agent.JobNSConf{Slot: i + 1, Uid: uidOf(t, fmt.Sprintf("bitc-s%d", i+1)), Gid: gidOf(t, fmt.Sprintf("bitc-s%d", i+1)), Conf: conf, Self: "/opt/bitc/agent/" + r.Sha + "/bitc"})
		if err != nil {
			t.Fatalf("namespace of slot %d: %v", i+1, err)
		}
		nss[i] = ns
		defer ns.Close()
	}
	srv := exec.Command("python3", "-c", "import socket,time\ns=socket.socket();s.bind(('0.0.0.0',3000));s.listen(5)\nwhile True:\n c,_=s.accept();c.sendall(b'J2\\n');c.close()")
	if err := nss[1].Wrap(srv); err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = srv.Process.Kill(); _ = srv.Wait() }()
	time.Sleep(time.Second)
	probe := exec.Command("python3", "-c", probePy+"c('10.203.2.2', 3000)\nc('"+rigLANDevice+"', 80)\nc('"+rigGateway+"', 80)\n")
	if err := nss[0].Wrap(probe); err != nil {
		t.Fatal(err)
	}
	pout, err := probe.CombinedOutput()
	if err != nil {
		t.Fatalf("control probe: %v %s", err, pout)
	}
	ctl := probeLines(string(pout))
	for _, k := range []string{"10.203.2.2:3000", rigLANDevice + ":80", rigGateway + ":80"} {
		if ctl[k] != "open" {
			t.Errorf("control: %s is %q without the drops, want open\n%s", k, ctl[k], pout)
		}
	}
	sh(t, "nft -f /etc/nftables.d/bitc.nft")
	// doctor sees the same from a probe namespace and passes
	out, code := r.checkHost("--doctor")
	if code != 0 || !strings.Contains(out, "ok ssh-lan\n") || len(fails(out)) != 0 {
		t.Errorf("check-host --doctor: code %d\n%s", code, out)
	}
}

// SH23: the job seccomp filter, seen from inside a real job and by doctor's item 12.
func TestSH23_JobSeccompFilterDeniesVsockUserNamespacesAndKeyrings(t *testing.T) {
	r := rig(t)
	j, res := r.job(t, "1", `
python3 - <<'PY'
import ctypes, os, socket, subprocess
libc = ctypes.CDLL(None, use_errno=True)
def sc(nr, *a):
    ctypes.set_errno(0)
    r = libc.syscall(nr, *a)
    return os.strerror(ctypes.get_errno()) if r < 0 else "ok"
try:
    socket.socket(40, socket.SOCK_STREAM); print("vsock ok")
except OSError as e: print("vsock", os.strerror(e.errno))
ctypes.set_errno(0); print("unshare", "ok" if libc.unshare(0x10000000) == 0 else os.strerror(ctypes.get_errno()))
print("clone", sc(56, 0x10000000 | 17, 0, 0, 0, 0))
print("keyctl", sc(250, 0, -3, 0))
print("add_key", sc(248, b"user", b"k", b"v", 1, -3))
print("clone3", sc(435, 0, 0))
pid = os.fork()
if pid == 0: os._exit(7)
print("fork", os.waitpid(pid, 0)[1] >> 8)
print("exec", subprocess.run(["git", "--version"], capture_output=True, text=True).stdout.split()[0])
PY
`)
	defer j.teardown()
	for _, want := range []string{"vsock Operation not permitted\n", "unshare Operation not permitted\n", "clone Operation not permitted\n", "keyctl Operation not permitted\n",
		"add_key Operation not permitted\n", "clone3 Function not implemented\n", "fork 7\n", "exec git\n"} {
		if !strings.Contains(res.Out, want) {
			t.Errorf("job output lacks %q:\n%s", want, res.Out)
		}
	}
	// doctor's item 12 passes under the filter and fails without it
	out, code := r.checkHost("--doctor")
	if code != 0 || !strings.Contains(out, "ok ssh-seccomp\n") {
		t.Errorf("check-host --doctor with the filter: code %d\n%s", code, out)
	}
	out, code = r.checkHost("--doctor", "--test-no-seccomp-filter")
	if code != 69 || !strings.Contains(out, "fail ssh-seccomp: ") || !strings.Contains(out, "clone3 returned") || !strings.Contains(out, "want ENOSYS") {
		t.Errorf("check-host --doctor without the filter: code %d\n%s", code, out)
	}
}

// SH24: the LAN block for IPv6 and the VM's own namespace (netns = false, one slot).
func TestSH24_NoIPv6AndDoctorProbesTheVMNamespace(t *testing.T) {
	r := rig(t)
	r.reconfigure(t, "--slots 1 --netns false")
	defer r.defaultConfig(t)
	// the VM can reach the IPv6 device itself (control for the job's result below)
	if out, err := rigRun(`python3 -c "import socket; socket.create_connection(('` + rigV6Device + `', 443), 3).close(); print('v6 ok')"`); err != nil || !strings.Contains(out, "v6 ok") {
		t.Fatalf("the VM cannot reach the simulated IPv6 device: %v %s", err, out)
	}
	script := netProbes(fmt.Sprintf(`c("%s", 443, socket.AF_INET6)
c("%s", 443)
c("%s", 80)
c("%s", 80)`, rigV6Device, rigInternet, rigLANDevice, rigGateway))
	j, res := r.job(t, "1", script)
	defer j.teardown()
	got := probeLines(res.Out)
	for k, want := range map[string]string{rigV6Device + ":443": "blocked", rigInternet + ":443": "open", rigLANDevice + ":80": "blocked", rigGateway + ":80": "blocked"} {
		if got[k] != want {
			t.Errorf("netns=false job: %s is %q, want %q\n%s", k, got[k], want, res.Out)
		}
	}
	// negative control: without the table the same job reaches the IPv6 device
	sh(t, "nft delete table inet bitc")
	defer sh(t, "nft -f /etc/nftables.d/bitc.nft")
	uid := uidOf(t, "bitc-s1")
	out := sh(t, fmt.Sprintf(`setpriv --reuid=%d --regid=%d --clear-groups python3 -c "%s
c('%s', 443, socket.AF_INET6)
c('%s', 80)"`, uid, uid, strings.ReplaceAll(probePy, `"`, `\"`), rigV6Device, rigLANDevice))
	ctl := probeLines(out)
	if ctl[rigV6Device+":443"] != "open" || ctl[rigLANDevice+":80"] != "open" {
		t.Errorf("control without the table: %v\n%s", ctl, out)
	}
	sh(t, "nft -f /etc/nftables.d/bitc.nft")
	// doctor's item 5 runs in the VM's namespace: a listener on the VM's loopback is reachable
	// from its probe (in a job namespace it would be the job's own, empty loopback)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	target := ln.Addr().String()
	conf := agent.HostConf{Slots: 1, Resolver: rigResolver, Gateway: rigGateway, Netns: false}
	selfBin := "/opt/bitc/agent/" + r.Sha + "/bitc"
	res2, err := agent.NewHostEnvFor(conf, true, selfBin).NetProbe([]string{target})
	if err != nil || res2.Connect[target] != agent.ConnOpen {
		t.Errorf("netns=false: the probe's view of the VM's loopback listener %s is %v (err %v), want open (the VM's own namespace)", target, res2.Connect[target], err)
	}
	conf.Netns = true
	r.defaultConfig(t)
	res3, err := agent.NewHostEnvFor(conf, true, selfBin).NetProbe([]string{target})
	if err != nil || res3.Connect[target] == agent.ConnOpen {
		t.Errorf("netns=true: the probe reached the VM's loopback listener %s (err %v); it must run in a job namespace", target, err)
	}
	if res3.DNSErr != nil {
		t.Errorf("netns=true: the probe's DNS query to the resolver failed: %v", res3.DNSErr)
	}
}

// ---- SH22: the controller-side tunnel account (C.6.3), with a real sshd on its own port

func tunnelSSH(args ...string) *exec.Cmd { return tunnelSSHCmd("", args...) }

// tunnelSSHCmd is tunnelSSH with a remote command after the host.
func tunnelSSHCmd(remote string, args ...string) *exec.Cmd {
	base := []string{"-F", "none", "-o", "BatchMode=yes", "-o", "IdentitiesOnly=yes", "-i", rigDir + "/tk", "-o", "UserKnownHostsFile=/dev/null",
		"-o", "StrictHostKeyChecking=no", "-o", "LogLevel=ERROR", "-o", "ExitOnForwardFailure=yes", "-p", "2223", "-l", "bitc-tunnel"}
	all := append(append(base, args...), "127.0.0.1")
	if remote != "" {
		all = append(all, remote)
	}
	return exec.Command("/usr/bin/ssh", all...)
}

// banner connects to addr and returns what the far end sends first ("" when it just closes).
func banner(addr string) string {
	c, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		return "dial: " + err.Error()
	}
	defer c.Close()
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 64)
	n, _ := c.Read(buf)
	return string(buf[:n])
}

func TestSH22_TunnelAccountCanOnlyListenOnItsOnePort(t *testing.T) {
	r := rig(t)
	sh(t, `ssh-keygen -q -t ed25519 -N '' -f `+rigDir+`/tk
useradd --system --user-group --no-create-home --home-dir /home/bitc-tunnel --shell /usr/sbin/nologin bitc-tunnel && usermod -L bitc-tunnel
mkdir -p /home/bitc-tunnel/.ssh && chmod 755 /home/bitc-tunnel /home/bitc-tunnel/.ssh`)
	pub := strings.Join(strings.Fields(sh(t, "cat "+rigDir+"/tk.pub"))[:2], " ")
	sh(t, `sh /src/scripts/ssh-host-setup.sh --print tunnel-authorized-keys --tunnel-key '`+pub+`' > /home/bitc-tunnel/.ssh/authorized_keys && chmod 644 /home/bitc-tunnel/.ssh/authorized_keys
(echo 'Port 2223'; echo 'ListenAddress 127.0.0.1'; echo 'HostKey /etc/ssh/ssh_host_ed25519_key'; echo 'PidFile `+rigDir+`/sshd2.pid'; echo 'UsePAM yes'
 sh /src/scripts/ssh-host-setup.sh --print tunnel-sshd) > `+rigDir+`/sshd2_config
/usr/sbin/sshd -f `+rigDir+`/sshd2_config -E `+rigDir+`/sshd2.log </dev/null >/dev/null 2>&1`)
	defer sh(t, "kill $(cat "+rigDir+"/sshd2.pid)")
	// the controller's check: sshd -T prints the C.6.3 values for the tunnel user
	got := sh(t, "/usr/sbin/sshd -T -f "+rigDir+"/sshd2_config -C user=bitc-tunnel,host=vm,addr=127.0.0.1")
	for _, want := range []string{"allowtcpforwarding remote", "permitopen none", "permitlisten 127.0.0.1:22022", "gatewayports no", "allowstreamlocalforwarding no",
		"permittunnel no", "x11forwarding no", "allowagentforwarding no", "permittty no", "permituserrc no", "forcecommand /bin/false"} {
		if !strings.Contains(got, want+"\n") {
			t.Errorf("sshd -T for bitc-tunnel lacks %q", want)
		}
	}
	// the one thing it may do: -R on 127.0.0.1:22022, and it reaches the target
	ok := tunnelSSH("-N", "-R", "127.0.0.1:22022:127.0.0.1:"+rigSSHPort)
	if err := ok.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ok.Process.Kill(); _ = ok.Wait() }()
	var b string
	for i := 0; i < 30 && !strings.HasPrefix(b, "SSH-2.0"); i++ {
		time.Sleep(200 * time.Millisecond)
		b = banner("127.0.0.1:22022")
	}
	if !strings.HasPrefix(b, "SSH-2.0") {
		t.Fatalf("the permitted -R 127.0.0.1:22022 does not carry traffic: %q", b)
	}
	// everything else is refused
	refused := func(name string, c *exec.Cmd) {
		t.Helper()
		done := make(chan error, 1)
		var out strings.Builder
		c.Stdout, c.Stderr = &out, &out
		if err := c.Start(); err != nil {
			t.Fatal(err)
		}
		go func() { done <- c.Wait() }()
		select {
		case err := <-done:
			if err == nil {
				t.Errorf("%s: ssh exited 0, want a refusal (%s)", name, out.String())
			}
		case <-time.After(8 * time.Second):
			_ = c.Process.Kill()
			t.Errorf("%s: still running after 8s, want it refused", name)
		}
	}
	refused("-R on another port", tunnelSSH("-N", "-R", "127.0.0.1:22023:127.0.0.1:"+rigSSHPort))
	refused("-R on all interfaces", tunnelSSH("-N", "-R", "0.0.0.0:22022:127.0.0.1:"+rigSSHPort))
	refused("-R to a unix socket", tunnelSSH("-N", "-R", rigDir+"/u.sock:127.0.0.1:"+rigSSHPort))
	// -L and -D start locally; the server refuses the channels
	for name, args := range map[string][]string{"-L": {"-N", "-L", "127.0.0.1:2400:127.0.0.1:" + rigSSHPort}, "-D": {"-N", "-D", "127.0.0.1:2401"}} {
		c := tunnelSSH(args...)
		if err := c.Start(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(1500 * time.Millisecond)
		switch name {
		case "-L":
			if b := banner("127.0.0.1:2400"); strings.HasPrefix(b, "SSH-2.0") {
				t.Errorf("-L carried traffic to sshd: %q", b)
			}
		case "-D":
			conn, err := net.DialTimeout("tcp", "127.0.0.1:2401", 2*time.Second)
			if err != nil {
				t.Errorf("-D: no local SOCKS listener: %v", err)
				break
			}
			_ = conn.SetDeadline(time.Now().Add(4 * time.Second))
			_, _ = conn.Write([]byte{5, 1, 0})
			buf := make([]byte, 2)
			_, _ = conn.Read(buf)
			_, _ = conn.Write([]byte{5, 1, 0, 1, 127, 0, 0, 1, byte(2222 >> 8), byte(2222 & 255)})
			reply := make([]byte, 10)
			n, _ := conn.Read(reply)
			conn.Close()
			if n >= 2 && reply[1] == 0 {
				t.Errorf("-D: the SOCKS CONNECT to sshd succeeded: %v", reply[:n])
			}
		}
		_ = c.Process.Kill()
		_ = c.Wait()
	}
	// no command, no TTY
	cmd := tunnelSSHCmd("id")
	out, err := cmd.CombinedOutput()
	if err == nil || strings.Contains(string(out), "uid=") {
		t.Errorf("a command as the tunnel user: err %v, output %q", err, out)
	}
	tty, _ := tunnelSSHCmd("true", "-tt").CombinedOutput()
	if !strings.Contains(string(tty), "PTY allocation request failed") {
		t.Errorf("a TTY request: %q, want it refused", tty)
	}
	_ = r
}
