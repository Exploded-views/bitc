package conformance

// SH10 and the doctor and status parts of task H.3: sweep, `doctor --remote` and `status` for
// the ssh provider, against the sshfake host (providers delta C.3.6, C.8, C.11).

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Exploded-views/bitc/conformance/harness"
	"github.com/Exploded-views/bitc/internal/provider"
	"github.com/Exploded-views/bitc/internal/provider/ssh/sshfake"
	"github.com/Exploded-views/bitc/internal/state"
)

const shOtherInstall = "ffffffffffffffff"

var shOtherSecret = []byte("another-install-secret-0123456789")

func (e *shEnv) secret(t *testing.T) []byte {
	t.Helper()
	b, err := state.InstallSecret(filepath.Join(e.h.Root, "account"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// seedSlot plants a slot group of one slot: kind "own" (this install's tag), "forged" (this
// install's name, a wrong tag) or "other" (another install). exp is relative to now, age is how
// long ago the lease was taken.
func (e *shEnv) seedSlot(t *testing.T, slot int, kind, job string, exp, age time.Duration) string {
	t.Helper()
	secret, install := e.secret(t), ""
	switch kind {
	case "own", "forged":
		install = state.Install16(secret)
	case "other":
		secret, install = shOtherSecret, shOtherInstall
	default:
		t.Fatal(kind)
	}
	name := provider.ResourceName("bitc", install, "heavy", "s1", job, time.Now().Add(exp))
	tag := provider.OwnerTag(secret, name)
	if kind == "forged" {
		tag = strings.Repeat("0", 64)
	}
	o := sshfake.SlotOwner{Name: name, Nonce: "n" + job, Tag: tag, Created: time.Now().Add(-age).Unix(), LeaseUntil: time.Now().Add(exp).Unix()}
	if err := sshfake.SetOwner(e.root, slot, o); err != nil {
		t.Fatal(err)
	}
	return name
}

func (e *shEnv) held(name string) bool {
	for _, o := range sshfake.Owners(e.root) {
		if o.Name == name {
			return true
		}
	}
	return false
}

func (e *shEnv) writeReservations(t *testing.T, rs ...map[string]any) {
	t.Helper()
	b, _ := json.Marshal(rs)
	if err := os.MkdirAll(e.acct, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.acct, "reservations.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestSH10_SweepTagVerifiedRuleWithOrphans(t *testing.T) {
	h := harness.New(t)
	e := shRepo(t, h, shOpts{slots: 8})
	shRanOnSSH(t, h.Run(shEcho...)) // creates the install secret and the account directory
	const hour, old = time.Hour, 30 * time.Minute
	expired := e.seedSlot(t, 1, "own", "00000001", -hour, 3*hour)    // own, past E
	gone := e.seedSlot(t, 2, "own", "00000002", hour, old)           // own, no reservation, older than start_timeout
	young := e.seedSlot(t, 3, "own", "00000003", hour, 0)            // own, no reservation, created just now
	live := e.seedSlot(t, 4, "own", "00000004", hour, old)           // own, a reservation whose pid is alive
	orphan := e.seedSlot(t, 5, "own", "00000005", hour, 0)           // own, an orphan reservation (its pid alive, running on Sprites)
	otherExp := e.seedSlot(t, 6, "other", "00000006", -hour, 3*hour) // another install, past E
	otherLive := e.seedSlot(t, 7, "other", "00000007", hour, old)    // another install, not past E
	forged := e.seedSlot(t, 8, "forged", "00000008", -hour, 3*hour)  // this install's name, a wrong tag
	e.writeReservations(t, resv1(live, os.Getpid(), false), resv1(orphan, os.Getpid(), true))

	r := h.Run("sweep", "--dry-run")
	for _, want := range []string{"swept slot " + expired + " (past its expiry)", "swept slot " + gone + " (job process gone)", "swept slot " + orphan + " (orphaned by a pre-attach discard)"} {
		if !strings.Contains(r.Stdout, want+"\n") {
			t.Fatalf("dry run lacks %q:\n%s\n%s", want, r.Stdout, r.Stderr)
		}
	}
	if n := len(sshfake.Owners(e.root)); n != 8 {
		t.Fatalf("a dry run freed a slot: %d held", n)
	}

	r = h.Run("sweep")
	if r.Code != 0 {
		t.Fatalf("%+v", r)
	}
	for _, n := range []string{young, live, otherExp, otherLive, forged} {
		if strings.Contains(r.Stdout, n) || !e.held(n) {
			t.Fatalf("%s must be kept: %s", n, r.Stdout)
		}
	}
	for _, n := range []string{expired, gone, orphan} {
		if e.held(n) {
			t.Fatalf("%s must be freed", n)
		}
	}
	if !strings.Contains(r.Stderr, forged+" has no verified owner tag; kept") {
		t.Fatalf("stderr: %s", r.Stderr)
	}
	if rs := e.reservations(t); len(rs) != 1 || rs[0].Name != live || rs[0].Orphan {
		t.Fatalf("a confirmed teardown drops the orphan and keeps the live reservation: %v", rs)
	}

	r = h.Run("sweep", "--foreign")
	if !strings.Contains(r.Stdout, "swept slot "+otherExp+" (past its expiry; --foreign)\n") || e.held(otherExp) || !e.held(otherLive) || !e.held(forged) {
		t.Fatalf("--foreign: %s held %v", r.Stdout, sshfake.Owners(e.root))
	}

	// the next ssh launch's cap check frees an orphan at once as well
	orphan2 := e.seedSlot(t, 5, "own", "00000009", hour, 0)
	e.writeReservations(t, resv1(live, os.Getpid(), false), resv1(orphan2, os.Getpid(), true))
	shRanOnSSH(t, h.Run(shEcho...))
	if e.held(orphan2) {
		t.Fatalf("the cap check left the orphan %s", orphan2)
	}
}

// ---- doctor --remote (C.8)

func shDoctor(h *harness.H) (string, int) {
	r := h.Run("doctor", "--remote")
	return r.Stdout, r.Code
}

func TestSH30_DoctorRemoteSSHLinesFromTheHostAndTheController(t *testing.T) {
	h := harness.New(t)
	e := shRepo(t, h, shOpts{})
	out, code := shDoctor(h)
	for _, want := range []string{"ok ssh api:", "ok ssh-host-key: ssh-ed25519 SHA256:", "ok ssh-arch: amd64\n",
		"warn ssh-sweep: no sweep in 30 min\n", "warn ssh-tunnel-sshd: needs root; run runbook step \"tunnel check\"\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor lacks %q:\n%s", want, out)
		}
	}
	if code != 0 {
		t.Errorf("exit %d:\n%s", code, out)
	}
	if v := e.verbs("check-host"); len(v) == 0 || !strings.HasSuffix(v[len(v)-1], " check-host --doctor") {
		t.Errorf("doctor asks the host for the doctor items: %v", v)
	}
	// a sweep silences the advice
	h.Run("sweep")
	if out, _ := shDoctor(h); !strings.Contains(out, "ok ssh-sweep: the last sweep was 0m ago\n") {
		t.Errorf("after a sweep:\n%s", out)
	}
}

func TestSH30_DoctorRemoteSSHRelaysHostFindingsAndFails(t *testing.T) {
	h := harness.New(t)
	e := shRepo(t, h, shOpts{})
	e.script(t, sshfake.Rule{Op: "check-host", Times: -1, Exit: 69,
		Stdout: "fail ssh-host-mounts: /mnt/mac (virtiofs) visible; disable host file sharing\nwarn ssh-swap: /dev/sda2 active; per-job secrets may reach disk\nok ssh-arch: amd64\n"})
	out, code := shDoctor(h)
	for _, want := range []string{"fail ssh-host-mounts: /mnt/mac (virtiofs) visible; disable host file sharing\n", "warn ssh-swap: /dev/sda2 active; per-job secrets may reach disk\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor lacks %q:\n%s", want, out)
		}
	}
	if code != 69 {
		t.Errorf("a failing host item must exit 69, got %d", code)
	}
}

func TestSH30_DoctorRemoteSSHUnreachableAndArchMismatch(t *testing.T) {
	h := harness.New(t)
	e := shRepo(t, h, shOpts{})
	e.script(t, sshfake.Rule{Op: "probe", Times: -1, Exit: 255, Stderr: "ssh: connect to host 127.0.0.1 port 22022: Connection refused\n"},
		sshfake.Rule{Op: "list", Times: -1, Exit: 255, Stderr: "ssh: connect to host 127.0.0.1 port 22022: Connection refused\n"})
	if out, code := shDoctor(h); code != 69 || !strings.Contains(out, "fail ssh-host: ssh 127.0.0.1: connection refused") {
		t.Errorf("unreachable: exit %d:\n%s", code, out)
	}
	h2 := harness.New(t)
	shRepo(t, h2, shOpts{arch: "arm64", hostArch: "amd64"})
	if out, code := shDoctor(h2); code != 69 || !strings.Contains(out, "fail ssh-arch: host arch amd64, configured arm64") {
		t.Errorf("arch: exit %d:\n%s", code, out)
	}
}

func TestSH30_DoctorRemoteSSHTunnelSshd(t *testing.T) {
	want := map[string]string{"allowtcpforwarding": "remote", "permitopen": "none", "permitlisten": "127.0.0.1:22022", "gatewayports": "no",
		"allowstreamlocalforwarding": "no", "permittunnel": "no", "x11forwarding": "no", "allowagentforwarding": "no", "permittty": "no",
		"permituserrc": "no", "forcecommand": "/bin/false"}
	sshd := func(t *testing.T, h *harness.H, override map[string]string) {
		var b strings.Builder
		b.WriteString("#!/bin/sh\ncat <<'EOF'\nport 22\n")
		for k, v := range want {
			if o, ok := override[k]; ok {
				v = o
			}
			b.WriteString(k + " " + v + "\n")
		}
		b.WriteString("EOF\n")
		p := filepath.Join(h.Root, "fake-sshd")
		h.Write(p, b.String(), 0o700)
		h.Seam("ssh_sshd = " + harness.Q(p))
	}
	h := harness.New(t)
	sshd(t, h, nil)
	shRepo(t, h, shOpts{})
	if out, _ := shDoctor(h); !strings.Contains(out, "ok ssh-tunnel-sshd: bitc-tunnel is limited to listening on 127.0.0.1:22022\n") {
		t.Errorf("a locked-down account:\n%s", out)
	}
	h2 := harness.New(t)
	sshd(t, h2, map[string]string{"gatewayports": "yes", "forcecommand": "none"})
	shRepo(t, h2, shOpts{})
	out, code := shDoctor(h2)
	if code != 69 || !strings.Contains(out, "fail ssh-tunnel-sshd: gatewayports is yes, want no\n") || !strings.Contains(out, "fail ssh-tunnel-sshd: forcecommand is none, want /bin/false\n") {
		t.Errorf("a loose account: exit %d:\n%s", code, out)
	}
}

// ---- status

func TestSH31_StatusSlotsSectionForSSH(t *testing.T) {
	h := harness.New(t)
	e := shRepo(t, h, shOpts{slots: 3})
	shRanOnSSH(t, h.Run(shEcho...))
	name := e.seedSlot(t, 2, "own", "0000000a", time.Hour, time.Minute)
	other := e.seedSlot(t, 3, "other", "0000000b", time.Hour, time.Minute)
	r := h.Run("--json", "status")
	var got map[string]any
	if err := json.Unmarshal([]byte(r.Stdout), &got); err != nil {
		t.Fatalf("%v: %s", err, r.Stdout)
	}
	var schema map[string]any
	if err := json.Unmarshal(must(os.ReadFile("../docs/status.schema.json")), &schema); err != nil {
		t.Fatal(err)
	}
	checkSchema(t, "$", schema, got)
	sec, _ := got["remote"].(map[string]any)["ssh"].(map[string]any)
	slots, _ := sec["slots"].(map[string]any)
	if sec == nil || slots == nil || slots["total"] != float64(3) || slots["busy"] != float64(2) || slots["free"] != float64(1) {
		t.Fatalf("slots: %s", r.Stdout)
	}
	held, _ := slots["held"].([]any)
	if len(held) != 2 {
		t.Fatalf("held: %s", r.Stdout)
	}
	first, _ := held[0].(map[string]any)
	if first["name"] != name || first["slots"].([]any)[0] != float64(2) || first["own"] != true {
		t.Errorf("first held group: %v", first)
	}
	if second, _ := held[1].(map[string]any); second["name"] != other || second["own"] != false {
		t.Errorf("second held group: %v", second)
	}
	if len(sec["own"].([]any)) != 1 || len(sec["foreign"].([]any)) != 1 {
		t.Errorf("own and foreign: %s", r.Stdout)
	}
}
