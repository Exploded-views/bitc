package provider

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/Exploded-views/bitc/internal/provider"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Exploded-views/bitc/conformance/harness"
	"github.com/Exploded-views/bitc/internal/provider/ssh/sshfake"
)

func sshWire(b []byte) []byte {
	out := make([]byte, 4, 4+len(b))
	binary.BigEndian.PutUint32(out, uint32(len(b)))
	return append(out, b...)
}

// sshTarget drives the ssh provider against the sshfake gate (providers delta C.11), this bitc
// being the agent the fake host installs and runs.
type sshTarget struct {
	h     *harness.H
	root  string
	slots int // the host's slots and max_live (default 2)
}

func (s *sshTarget) Name() string { return "ssh" }

func (s *sshTarget) nslots() int {
	if s.slots == 0 {
		return 2
	}
	return s.slots
}

func (s *sshTarget) Configure(t *testing.T, h *harness.H) {
	s.h, s.root = h, filepath.Join(h.Root, "sshhost")
	if err := sshfake.Init(s.root, sshfake.Config{Slots: s.nslots(), Arch: "amd64"}); err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(h.Root, "vm_ed25519")
	body := append([]byte("openssh-key-v1\x00"), sshWire([]byte("none"))...)
	body = append(body, sshWire([]byte("none"))...)
	body = append(body, sshWire(nil)...)
	body = append(body, 0, 0, 0, 1)
	body = append(body, sshWire([]byte("pub"))...)
	body = append(body, sshWire([]byte("priv"))...)
	h.Write(key, "-----BEGIN OPENSSH PRIVATE KEY-----\n"+base64.StdEncoding.EncodeToString(body)+"\n-----END OPENSSH PRIVATE KEY-----\n", 0o600)
	hostKey := "ssh-ed25519 " + base64.StdEncoding.EncodeToString(append(sshWire([]byte("ssh-ed25519")), sshWire(make([]byte, 32))...))
	h.Seam("time_scale = 0.05")
	h.Seam("agent_url = \"http://127.0.0.1:9/bitc\"")
	h.Seam("fixed_secrets = true")
	h.Seam("ssh_exec = \"local\"")
	h.Seam("ssh_gate = " + harness.Q(harness.SSHFakeGate))
	h.Seam("ssh_root = " + harness.Q(s.root))
	h.HostConfig("[remote_limits]\nworktrees = [\"**/app\"]\nforward_env_allow = [\"CANARY_VALUE\"]\n" +
		"[pools.heavy]\nslots = " + strconv.Itoa(s.nslots()) + "\nproviders = [\"ssh\", \"local\"]\nremote_limits = { size = { ssh = \"s1\" }, timeout = \"60m\" }\nwait = \"5s\"\n" +
		"[providers.ssh]\nhost = \"127.0.0.1\"\nport = 22022\nuser = \"bitc\"\nhost_key = " + harness.Q(hostKey) + "\nidentity_file = " + harness.Q(key) + "\n" +
		"arch = \"amd64\"\nslots = " + strconv.Itoa(s.nslots()) + "\nmax_live = " + strconv.Itoa(s.nslots()) + "\nresolver = \"9.9.9.9\"\nstart_timeout = \"2m\"\nsizes = { s1 = { slots = 1 } }\n")
	h.Cwd = h.Repo("app", map[string]string{".bitc.toml": "version = 1\n[remote]\nforward_env = [\"CANARY_VALUE\"]\n[pools.heavy.remote]\nallow = [[\"sh\", \"*\"], [\"echo\", \"*\"]]\n"})
}

func (s *sshTarget) Behave(t *testing.T, b string) {
	var rules []sshfake.Rule
	switch b {
	case "ok", "paginate-2":
	case "create-fails": // the host is unreachable: nothing was created
		rules = []sshfake.Rule{{Op: "probe", Times: -1, Exit: 255, Stderr: "ssh: connect to host 127.0.0.1 port 22022: Connection refused\n"}}
	case "setup-fails-after-create": // the agent refuses the boot payload: attached
		rules = []sshfake.Rule{{Op: "run", Times: -1, Read: true, Exit: 65, Stdout: "refused: truncated payload\n"}}
	case "preattach-fails": // the slot was prepared, then the prepare failed before any job input
		rules = []sshfake.Rule{{Op: "prepare", Times: -1, Exit: 69, Stderr: "bitc agent: boom\n", After: true}}
	case "setup-fails-repo-after-create": // the repo's own setup step fails by itself, on the host, after the attach
		commitRepoFile(t, s.h, "version = 1\n[remote]\nforward_env = [\"CANARY_VALUE\"]\nsetup = [{ name = \"deps\", run_as = \"job\", argv = [\"sh\", \"-c\", \"echo 'npm ERR! missing script: ci' >&2; exit 1\"] }]\n"+
			"[pools.heavy.remote]\nallow = [[\"sh\", \"*\"], [\"echo\", \"*\"]]\n")
	case "drop-log-tail": // the first tail's session drops
		rules = []sshfake.Rule{{Op: "tail", Times: 1, Exit: 255, Stderr: "Connection reset by peer\n"}}
	case "forge-result": // the host answers a result read with a document that has no valid MAC
		rules = []sshfake.Rule{{Op: "read", Times: -1, Stdout: "{\"exit\":0,\"mac\":\"00\"}\n"}}
	case "lost-after-begin": // the log never shows the EXIT marker and the host has no result
		rules = []sshfake.Rule{{Op: "tail", Times: -1, Mangle: "__BITC_EXIT__=__BITC_XXXX__"}, {Op: "read", Times: -1, Exit: 66, Stderr: "not found\n"}}
	case "restart-after-begin": // the agent log says the job was lost where BEGIN would be (same length, as the frames' offsets hold)
		rules = []sshfake.Rule{{Op: "tail", Times: -1, Mangle: "__BITC_BEGIN__ =__BITC_LOST__  "}}
	default:
		t.Fatalf("the ssh fake cannot produce %q", b)
	}
	if err := sshfake.SetScript(s.root, rules); err != nil {
		t.Fatal(err)
	}
}

func (s *sshTarget) Calls() []string {
	var out []string
	for _, c := range sshfake.Calls(s.root) {
		switch c.Op {
		case "teardown":
			out = append(out, "DELETE slot")
		case "prepare", "run", "install":
			out = append(out, "POST "+c.Op) // they make or change something on the host
		default:
			out = append(out, "GET "+c.Op)
		}
	}
	return out
}

func (s *sshTarget) TornDown() []string {
	var out []string
	for _, c := range sshfake.Calls(s.root) {
		if c.Op == "teardown" {
			f := strings.Fields(c.Verb)
			out = append(out, f[len(f)-1])
		}
	}
	return out
}

func (s *sshTarget) Supports(string) bool { return true }

func (s *sshTarget) DeclaresAttach() bool { return true }

// RecoversTail: the provider reopens a dropped tail from the offsets it has.
func (s *sshTarget) RecoversTail() {}

// APICalls are the gate calls in order, the --run call marked; Data holds the command line too.
func (s *sshTarget) APICalls(t *testing.T) []APICall {
	var out []APICall
	for _, c := range sshfake.Calls(s.root) {
		out = append(out, APICall{Op: c.Op, Attach: c.Attach, Data: c.Verb + "\n" + c.Data})
	}
	return out
}

// Canaries are B, K and M, which [test] fixed_secrets makes 0x0b.., 0x0c.. and 0x0d.. (hex).
func (s *sshTarget) Canaries(t *testing.T) []string {
	var out []string
	for _, b := range []byte{0x0b, 0x0c, 0x0d} {
		out = append(out, hex.EncodeToString(bytes.Repeat([]byte{b}, 32)))
	}
	return out
}

var _ = fmt.Sprint
var _ = os.Stat

func TestFB15_NothingCrossesBeforeAttachOnSSH(t *testing.T) {
	RunFB15(t, &sshTarget{}, harness.New)
}

func TestSH32_ProviderSuiteOnSSHFake(t *testing.T) { RunAll(t, &sshTarget{}, harness.New) }

// SetSpend: an unpriced host has no cap; pricing it and giving it a day's budget below one job's
// estimate stands for a spent day (the cap rests on the estimate and the ledger).
func (s *sshTarget) SetSpend(t *testing.T, h *harness.H, _ float64) {
	cfg, err := os.ReadFile(h.Config)
	if err != nil {
		t.Fatal(err)
	}
	h.Write(h.Config, strings.Replace(string(cfg), "[providers.ssh]\n", "[providers.ssh]\nusd_per_hour = 1\ndaily_usd = 0.01\n", 1), 0o600)
}

// Noun is the word the sweep prints for a slot group.
func (s *sshTarget) Noun() string { return "slot" }

// AddExpired plants a slot group of this install, past its expiry, with a verifying owner tag.
func (s *sshTarget) AddExpired(t *testing.T, h *harness.H) string {
	secret, err := os.ReadFile(filepath.Join(h.Root, "account", "install-secret"))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(secret)
	name := provider.ResourceName("bitc", hex.EncodeToString(sum[:])[:16], "heavy", "s1", "0000000a", time.Now().Add(-time.Hour))
	o := sshfake.SlotOwner{Name: name, Nonce: "n", Tag: provider.OwnerTag(secret, name), Created: time.Now().Add(-3 * time.Hour).Unix(), LeaseUntil: time.Now().Add(-time.Hour).Unix()}
	if err := sshfake.SetOwner(s.root, 1, o); err != nil {
		t.Fatal(err)
	}
	return name
}

// ListNames is what the provider lists: the own and foreign groups of `status`, which asks the host.
func (s *sshTarget) ListNames(t *testing.T, h *harness.H) []string {
	r := h.Run("--json", "status")
	var got struct {
		Remote map[string]struct{ Own, Foreign []struct{ Name string } }
	}
	if err := json.Unmarshal([]byte(r.Stdout), &got); err != nil {
		t.Fatalf("%v: %s", err, r.Stdout)
	}
	var out []string
	for _, x := range append(got.Remote["ssh"].Own, got.Remote["ssh"].Foreign...) {
		out = append(out, x.Name)
	}
	return out
}

// LiveNames are the slot groups the fake host holds.
func (s *sshTarget) LiveNames(*testing.T) []string {
	seen := map[string]bool{}
	var out []string
	for _, o := range sshfake.Owners(s.root) {
		if !seen[o.Name] {
			seen[o.Name] = true
			out = append(out, o.Name)
		}
	}
	return out
}
