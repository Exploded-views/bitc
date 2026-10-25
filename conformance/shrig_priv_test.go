//go:build privileged

package conformance

// The privileged rig of the SH family (providers delta H.1, C.11): this container becomes an ssh
// host exactly the way the owner's runbook does it (scripts/ssh-host-setup.sh), with a real sshd
// on 127.0.0.1, a simulated home LAN in a second network namespace (a gateway, a resolver, a
// router at 192.168.1.1, an IPv6 device and a stand-in for the internet), and the agent installed
// through the gate. Nothing leaves the container.

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/Exploded-views/bitc/conformance/harness"
	"github.com/Exploded-views/bitc/internal/agent"
)

const (
	rigDir   = "/tmp/shrig"
	rigAgent = rigDir + "/bitc-agent"
	// --allow: the one internet-side target the simulated LAN answers (NAT through vlan0); a documentation
	// range, so no test-only flag is needed. It changes only what the self-test expects to reach.
	rigSetupTail = ` --agent ` + rigAgent + ` --allow 203.0.113.7:443`
	rigSSHPort   = "2222"
	rigResolver  = "10.99.0.53"
	rigGateway   = "10.99.0.1"
	// rigController stands for the controller-facing LAN address: an ssh-like listener on :22 in the
	// simulated LAN, so the setup self-test proves the firewall blocks a port that really answers.
	rigController = "10.99.0.21"
	rigLANDevice  = "192.168.1.1"
	rigInternet   = "203.0.113.7"
	rigV6Device   = "2001:db8:1::1"
)

const rigFakeSSH = `#!/bin/sh
key=
while [ $# -gt 1 ]; do
  case $1 in -i) key=$2; shift 2 ;; -o|-p) shift 2 ;; *) shift ;; esac
done
[ -n "$key" ] && [ -s "$key" ] && : > /tmp/shrig/key-seen
exec sh -c "$1"
`

const rigLANScript = `
import socket, threading, http.server, socketserver, sys
def tcp(host, port, fam=socket.AF_INET):
    s = socket.socket(fam); s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1); s.bind((host, port)); s.listen(16)
    def loop():
        while True:
            c, _ = s.accept(); c.sendall(b"LAN\n"); c.close()
    threading.Thread(target=loop, daemon=True).start()
def dns(host):
    s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM); s.bind((host, 53))
    def loop():
        while True:
            q, a = s.recvfrom(512); s.sendto(q[:2] + b"\x81\x80" + q[4:], a)
    threading.Thread(target=loop, daemon=True).start()
tcp("192.168.1.1", 80); tcp("10.99.0.1", 80); tcp("203.0.113.7", 443); tcp("2001:db8:1::1", 443, socket.AF_INET6)
tcp("10.99.0.53", 80); dns("10.99.0.53"); tcp("10.99.0.21", 22)
open("/tmp/shrig/lan.ready", "w").write("1")
threading.Event().wait()
`

type shRigT struct {
	Sha, Bin, Key, RepoURL, RepoSha string
	SetupDry, SetupDryErr           string
	SnapBefore, SnapAfterDry        string
	Setup1, Setup2                  string
	NonRoot                         string
	LANPid                          int
}

var (
	rigOnce sync.Once
	rigVal  shRigT
	rigErr  error
	rigSeq  atomic.Int64
)

func rigRun(script string) (string, error) {
	c := exec.Command("/bin/sh", "-c", script)
	out, err := c.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%v: %s", err, out)
	}
	return string(out), nil
}

func inRigContainer(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 || os.Getenv("BITC_PRIVILEGED_TEST") != "1" {
		t.Skip("runs only as root in a throwaway container: sh scripts/privileged.sh")
	}
	_, e1 := os.Stat("/.dockerenv")
	_, e2 := os.Stat("/run/.containerenv")
	if e1 != nil && e2 != nil {
		t.Fatal("BITC_PRIVILEGED_TEST=1 outside a container: refusing to configure this machine as an ssh host")
	}
}

// rig returns the one set-up host.
func rig(t *testing.T) *shRigT {
	t.Helper()
	inRigContainer(t)
	rigOnce.Do(func() { rigErr = rigSetup(&rigVal) })
	if rigErr != nil {
		t.Fatalf("host setup: %v", rigErr)
	}
	return &rigVal
}

// snapshot is a digest of the paths the setup script may touch, to prove a dry run changes none.
func rigSnapshot() string {
	out, _ := rigRun(`( ls -ld /opt/bitc /etc/bitc /var/lib/bitc /home/bitc /etc/sudoers.d/bitc /etc/ssh/sshd_config.d/bitc.conf /etc/cron.allow /etc/at.allow /etc/nftables.d /etc/sysctl.d/60-bitc.conf 2>&1; getent passwd bitc bitc-s1 bitc-s2; nft list tables 2>&1 | grep bitc ) | md5sum`)
	return strings.TrimSpace(out)
}

func rigSetup(r *shRigT) error {
	r.Bin = harness.Bin
	steps := []string{
		`echo "127.0.0.1 $(hostname)" >> /etc/hosts`,
		// cgroup v2 delegation as a host would have it: the container's own processes move to a leaf
		`mount -o remount,rw /sys/fs/cgroup && mkdir -p /sys/fs/cgroup/init && for p in $(cat /sys/fs/cgroup/cgroup.procs); do echo $p > /sys/fs/cgroup/init/cgroup.procs 2>/dev/null; done; echo '+memory +pids' > /sys/fs/cgroup/cgroup.subtree_control`,
		`mkdir -p ` + rigDir + ` /run/sshd /srv/ww && chmod 1777 /srv/ww && ssh-keygen -q -t ed25519 -N '' -f ` + rigDir + `/ck && ssh-keygen -A >/dev/null`,
		`printf 'secret' > /etc/bitc-tunnel-secret && mkdir -p /etc/bitc && install -m 0600 -o root -g root /etc/bitc-tunnel-secret /etc/bitc/tunnel_key && rm /etc/bitc-tunnel-secret`,
	}
	for _, s := range steps {
		if out, err := rigRun(s); err != nil {
			return fmt.Errorf("%s: %s", s, out)
		}
	}
	// the agent binary the setup script's firewall self-test runs: a world-executable copy, since the
	// harness's temp directory is 0700 (and the non-root run must reach the root refusal, not an --agent error)
	if out, err := rigRun(`install -m 0755 -o root -g root ` + r.Bin + ` ` + rigAgent); err != nil {
		return fmt.Errorf("agent copy: %s", out)
	}
	// the host's resolver is the simulated LAN's DNS stand-in, as a configured host's would be (the self-test
	// of --netns false probes in the container's own namespace and reads /etc/resolv.conf)
	if out, err := rigRun(`printf 'nameserver 10.99.0.53\n' > /etc/resolv.conf`); err != nil {
		return fmt.Errorf("resolv.conf: %s", out)
	}
	// the simulated LAN: a second network namespace behind the veth vlan0
	lan := `unshare -n sh -c 'echo $$ > ` + rigDir + `/lan.pid; exec sleep 100000' >/dev/null 2>&1 &
for i in 1 2 3 4 5 6 7 8 9 10; do [ -s ` + rigDir + `/lan.pid ] && break; sleep 0.2; done
P=$(cat ` + rigDir + `/lan.pid)
ip link add vlan0 type veth peer name vlan1 netns $P
ip addr add 192.168.1.2/24 dev vlan0; ip addr add 10.99.0.2/24 dev vlan0; ip -6 addr add 2001:db8:1::2/64 dev vlan0 nodad
ip link set vlan0 up; ip route add 203.0.113.0/24 via 192.168.1.1 dev vlan0
ip -6 addr show dev vlan0 | grep -q 2001:db8:1::2 || { echo 'vlan0 has no IPv6 address (container net.ipv6 disabled?)'; exit 1; }
N="nsenter -t $P -n"
$N ip link set lo up; $N ip link set vlan1 up
$N ip addr add 192.168.1.1/24 dev vlan1; $N ip addr add 10.99.0.1/24 dev vlan1; $N ip addr add 10.99.0.53/32 dev vlan1
$N ip addr add 10.99.0.21/24 dev vlan1; $N ip addr add 203.0.113.7/32 dev vlan1; $N ip -6 addr add 2001:db8:1::1/64 dev vlan1 nodad
printf '%s' '` + rigLANScript + `' > ` + rigDir + `/lan.py
$N python3 ` + rigDir + `/lan.py >` + rigDir + `/lan.log 2>&1 &
for i in $(seq 1 50); do [ -e ` + rigDir + `/lan.ready ] && break; sleep 0.1; done
[ -e ` + rigDir + `/lan.ready ]
# positive control: root (never firewalled) reaches the controller listener, so the self-test's
# "controller :22 must fail" result for slot users is not vacuous
timeout 3 bash -c 'exec 3<>/dev/tcp/` + rigController + `/22' || { echo 'the simulated controller listener is not reachable'; exit 1; }`
	if out, err := rigRun(lan); err != nil {
		return fmt.Errorf("lan: %s", out)
	}
	r.LANPid = 0
	r.Key = rigDir + "/ck"
	pub, _ := os.ReadFile(r.Key + ".pub")
	ctlKey := strings.Join(strings.Fields(string(pub))[:2], " ")
	args := `--slots 2 --resolver ` + rigResolver + ` --gateway ` + rigGateway + ` --controller ` + rigController + ` --controller-key '` + ctlKey + `' --no-systemd --memory-per-slot 512MiB --secret-path /etc/bitc/tunnel_key` + rigSetupTail
	// a dry run changes nothing; a non-root run is refused
	r.SnapBefore = rigSnapshot()
	var derr error
	r.SetupDry, derr = rigRun(`sh /src/scripts/ssh-host-setup.sh --dry-run ` + args)
	if derr != nil {
		r.SetupDryErr = derr.Error()
	}
	r.SnapAfterDry = rigSnapshot()
	r.NonRoot, _ = rigRun(`runuser -u nobody -- sh /src/scripts/ssh-host-setup.sh ` + args + ` 2>&1; echo "exit=$?"`)
	var err error
	if r.Setup1, err = rigRun(`sh /src/scripts/ssh-host-setup.sh ` + args); err != nil {
		return fmt.Errorf("setup: %v", err)
	}
	if r.Setup2, err = rigRun(`sh /src/scripts/ssh-host-setup.sh ` + args); err != nil {
		return fmt.Errorf("second setup: %v", err)
	}
	if out, err := rigRun(`/usr/sbin/sshd -o Port=` + rigSSHPort + ` -o ListenAddress=127.0.0.1 -E ` + rigDir + `/sshd.log </dev/null >/dev/null 2>&1`); err != nil {
		return fmt.Errorf("sshd: %s", out)
	}
	if err := os.WriteFile("/usr/sbin/ssh", []byte(rigFakeSSH), 0o755); err != nil { // the job's code fetch runs git over this
		return err
	}
	sum, err := fileSHA(r.Bin)
	if err != nil {
		return err
	}
	r.Sha = sum
	bin, _ := os.ReadFile(r.Bin)
	if out, _, code := r.ctl("install "+r.Sha, bin); code != 0 || !strings.HasPrefix(out, "installed ") {
		return fmt.Errorf("install: %q code %d", out, code)
	}
	r.RepoURL, r.RepoSha, err = rigRepo()
	return err
}

func fileSHA(p string) (string, error) {
	out, err := exec.Command("sha256sum", p).Output()
	if err != nil {
		return "", err
	}
	return strings.Fields(string(out))[0], nil
}

func rigRepo() (url, sha string, err error) {
	script := `set -e
rm -rf /srv/bitc-rig; mkdir -p /srv/bitc-rig/src && cd /srv/bitc-rig/src && git init -q && echo hi > file && git add -A && git -c user.name=t -c user.email=t@example.invalid commit -q -m i
git rev-parse HEAD > /srv/bitc-rig/sha
git init -q --bare /srv/bitc-rig/r.git && git push -q /srv/bitc-rig/r.git HEAD:refs/bitc/jobs/x`
	if out, e := rigRun(script); e != nil {
		return "", "", fmt.Errorf("repo: %s", out)
	}
	b, _ := os.ReadFile("/srv/bitc-rig/sha")
	return "git@localhost:/srv/bitc-rig/r.git", strings.TrimSpace(string(b)), nil
}

// ctl runs one gate command the way the controller does: through ssh to the forced command.
func (r *shRigT) ctl(cmd string, stdin []byte) (stdout, stderr string, code int) {
	c := exec.Command("/usr/bin/ssh", "-F", "none", "-o", "BatchMode=yes", "-o", "IdentitiesOnly=yes", "-i", r.Key,
		"-o", "UserKnownHostsFile=/dev/null", "-o", "StrictHostKeyChecking=no", "-o", "LogLevel=ERROR",
		"-o", "ForwardAgent=no", "-p", rigSSHPort, "-l", "bitc", "--", "127.0.0.1", cmd)
	c.Stdin = bytes.NewReader(stdin)
	var o, e bytes.Buffer
	c.Stdout, c.Stderr = &o, &e
	err := c.Run()
	code = 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		code = -1
	}
	return o.String(), e.String(), code
}

// agentCtl is `agent <sha> <verb...>` through the gate.
func (r *shRigT) agentCtl(verb string, stdin []byte) (string, string, int) {
	return r.ctl("agent "+r.Sha+" "+verb, stdin)
}

// ---- jobs

type rigJob struct {
	r                      *shRigT
	Name, Nonce, Slots, ID string
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// prepare leases slots to a fresh name and returns the job handle and the raw reply.
func (r *shRigT) prepare(slots string, extra string) (*rigJob, string, string, int) {
	n := rigSeq.Add(1)
	j := &rigJob{r: r, Name: fmt.Sprintf("bitc-rig-%d", n), Nonce: randHex(16), Slots: slots, ID: fmt.Sprintf("job%d", n)}
	out, errOut, code := r.agentCtl("slot-boot --prepare "+slots+" "+j.Name+" "+j.Nonce+extra, nil)
	return j, out, errOut, code
}

// spec builds the job's boot frame. The script runs under /bin/sh in the checked-out tree.
func (j *rigJob) frame(script string, timeout time.Duration, deadline time.Duration) []byte {
	spec := agent.Spec{Job: j.ID, Pool: "heavy", Argv: []string{"/bin/sh", "-c", script}, CloneURL: j.r.RepoURL,
		Ref: "refs/bitc/jobs/x", Sha: j.r.RepoSha, Timeout: agent.Duration(timeout), Port: -1,
		Bearer: strings.Repeat("04", 32), MacKey: strings.Repeat("05", 32), Nonce: "n0nce"}
	enc, err := spec.Encode()
	if err != nil {
		panic(err)
	}
	f, err := agent.BootPayload{Spec: enc, DeployKey: "test-deploy-key", Deadline: time.Now().Add(deadline).Unix()}.Frame()
	if err != nil {
		panic(err)
	}
	return f
}

// run sends the payload; the reply is `started <name>` on success.
func (j *rigJob) run(script string) (string, string, int) {
	return j.r.agentCtl("slot-boot --run "+j.Slots+" "+j.Name+" "+j.Nonce, j.frame(script, 2*time.Minute, 10*time.Minute))
}

type rigResult struct {
	Out, Err string
	Exit     int
	Done     bool
}

// wait follows the job with `tail` until it is done and returns its output and exit code.
func (j *rigJob) wait(t *testing.T) rigResult {
	t.Helper()
	var res rigResult
	deadline := time.Now().Add(150 * time.Second)
	offs := map[string]int{"agent.log": 0, "out.log": 0, "err.log": 0}
	var log strings.Builder
	for time.Now().Before(deadline) {
		stdout, _, _ := j.r.agentCtl(fmt.Sprintf("tail %s %d %d %d", j.ID, offs["agent.log"], offs["out.log"], offs["err.log"]), nil)
		for _, l := range strings.Split(stdout, "\n") {
			var f agent.Frame
			if json.Unmarshal([]byte(l), &f) != nil {
				continue
			}
			switch f.File {
			case "out.log":
				res.Out += string(f.Data)
			case "err.log":
				res.Err += string(f.Data)
			case "agent.log":
				log.Write(f.Data)
			}
			if f.File != "" {
				offs[f.File] += len(f.Data)
			}
			if f.Done {
				res.Done = true
			}
		}
		if res.Done {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	if !res.Done {
		t.Fatalf("job %s did not finish\nagent log: %s", j.ID, log.String())
	}
	rj, _, _ := j.r.agentCtl("read "+j.ID+" result.json 0", nil)
	var rf agent.ResultFile
	if json.Unmarshal([]byte(rj), &rf) != nil {
		t.Fatalf("no result.json for %s: %q\nagent log: %s", j.ID, rj, log.String())
	}
	res.Exit = rf.Exit
	return res
}

func (j *rigJob) teardown() (string, string, int) {
	return j.r.agentCtl("teardown "+j.Slots+" "+j.Name, nil)
}

// go starts a job and waits for it; the job is torn down by the caller (some tests look first).
func (r *shRigT) job(t *testing.T, slots, script string) (*rigJob, rigResult) {
	t.Helper()
	j, out, errOut, code := r.prepare(slots, "")
	if code != 0 || !strings.HasPrefix(out, "prepared ") {
		t.Fatalf("prepare %s: %q %q code %d", slots, out, errOut, code)
	}
	out, errOut, code = j.run(script)
	if code != 0 || out != "started "+j.Name+"\n" {
		t.Fatalf("run %s: %q %q code %d", slots, out, errOut, code)
	}
	return j, j.wait(t)
}

// ---- small helpers

func sh(t *testing.T, script string) string {
	t.Helper()
	out, err := rigRun(script)
	if err != nil {
		t.Fatalf("%s: %v", script, err)
	}
	return out
}

// uidOf is the uid of a user on this host.
func uidOf(t *testing.T, user string) int {
	t.Helper()
	var n int
	if _, err := fmt.Sscan(strings.TrimSpace(sh(t, "id -u "+user)), &n); err != nil {
		t.Fatal(err)
	}
	return n
}

// gidOf is the primary gid of a user on this host.
func gidOf(t *testing.T, user string) int {
	t.Helper()
	var n int
	if _, err := fmt.Sscan(strings.TrimSpace(sh(t, "id -g "+user)), &n); err != nil {
		t.Fatal(err)
	}
	return n
}

// procsOfUser counts the live processes of a user.
func procsOfUser(user string) int {
	out, _ := rigRun("ps -u " + user + " -o pid= 2>/dev/null | wc -l")
	var n int
	fmt.Sscan(strings.TrimSpace(out), &n)
	return n
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

// holdFile writes content under dir as a helper for tests that plant things.
func holdFile(t *testing.T, p, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

var _ = syscall.SIGKILL

// The test binary doubles as the agent for the in-process probes (agent.NewHostEnvFor starts it).
func init() {
	if len(os.Args) >= 3 && os.Args[1] == "__agent" {
		os.Exit(agent.Main(os.Args[2:], os.Stdout))
	}
}

// checkHost runs the installed agent's check-host as root, directly (not through ssh).
func (r *shRigT) checkHost(args ...string) (string, int) {
	c := exec.Command("/opt/bitc/agent/"+r.Sha+"/bitc", append([]string{"__agent", "check-host"}, args...)...)
	out, err := c.Output()
	if ee, ok := err.(*exec.ExitError); ok {
		return string(out), ee.ExitCode()
	}
	return string(out), 0
}

// fails returns the fail lines of a check-host output.
func fails(out string) (f []string) {
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "fail ") {
			f = append(f, l)
		}
	}
	return f
}

// reconfigure runs the setup script again with other flags (the idempotent runbook re-run).
func (r *shRigT) reconfigure(t *testing.T, flags string) {
	t.Helper()
	pub, _ := os.ReadFile(r.Key + ".pub")
	key := strings.Join(strings.Fields(string(pub))[:2], " ")
	sh(t, `sh /src/scripts/ssh-host-setup.sh --resolver `+rigResolver+` --gateway `+rigGateway+` --controller `+rigController+` --controller-key '`+key+`' --no-systemd --secret-path /etc/bitc/tunnel_key`+rigSetupTail+` `+flags)
}

func (r *shRigT) defaultConfig(t *testing.T) { r.reconfigure(t, "--slots 2 --memory-per-slot 512MiB") }

// probePy is a job-side connect test: it prints `<host> <port> open|refused|blocked <error>`.
const probePy = `
import socket, sys
def c(h, p, fam=socket.AF_INET):
    s = socket.socket(fam); s.settimeout(3)
    try:
        s.connect((h, p)); print(h, p, "open"); s.close()
    except ConnectionRefusedError: print(h, p, "refused")
    except Exception as e: print(h, p, "blocked", type(e).__name__)
`
