package conformance

// Every verb the ssh provider sends is replayed through the real scripts/ssh-gate.sh grammar
// (the SH11 gate rig), so a verb the gate would answer with 64 cannot pass an SH test just
// because sshfake, a Go re-implementation, accepts it.

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Exploded-views/bitc/conformance/harness"
	"github.com/Exploded-views/bitc/internal/provider"
)

var shShaRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

// gateAnswers replays verbs through a copy of the gate whose host.conf has slots slots and
// returns the verbs it answered with 64 (a grammar violation). The agent hash is swapped for the
// rig's own installed agent after it has been checked as 64 hex: an uninstalled agent is 66,
// which would hide every argument after it.
func gateAnswers(t *testing.T, slots int, verbs []string) (rejected []string) {
	t.Helper()
	g := newGateRig(t)
	if err := os.WriteFile(g.conf, []byte("slots="+strconv.Itoa(slots)+"\nresolver=9.9.9.9\ngateway=192.168.64.1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= slots; i++ {
		if err := os.MkdirAll(filepath.Join(g.state, "slots", strconv.Itoa(i)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, v := range verbs {
		w := strings.Split(v, " ")
		if len(w) >= 2 && (w[0] == "agent" || w[0] == "install") {
			if !shShaRE.MatchString(w[1]) {
				rejected = append(rejected, v)
				continue
			}
			w[1] = g.sha
		}
		if _, code := g.run("", strings.Join(w, " ")); code == 64 {
			rejected = append(rejected, v)
		}
	}
	return rejected
}

// checkTheRealGate runs at the end of every SH test that built a fake host.
func (e *shEnv) checkTheRealGate(t *testing.T) {
	t.Helper()
	var verbs []string
	for _, c := range e.calls() {
		verbs = append(verbs, c.Verb)
	}
	if bad := gateAnswers(t, e.slots, verbs); len(bad) > 0 {
		t.Errorf("the real gate answers 64 to what the provider sent: %q", bad)
	}
}

func TestSH11_EveryVerbTheProviderSendsPassesTheRealGate(t *testing.T) {
	// a pool name long enough that the resource name is cut to its 63-byte limit
	h := harness.New(t)
	const pool = "a-pool-with-a-very-long-name-for-the-cap"
	e := shRepo(t, h, shOpts{pool: pool})
	r := h.Run("run", pool, "--", "echo", "on-ssh")
	shRanOnSSH(t, r)
	prep := e.verbs("prepare")
	if len(prep) != 1 {
		t.Fatalf("prepare verbs %q", prep)
	}
	f := strings.Fields(prep[0])
	if len(f[5]) != 63 {
		t.Errorf("the job's resource name %q is %d bytes, want the 63-byte limit", f[5], len(f[5]))
	}
	if !strings.HasPrefix(f[7], "tag=") || len(f[7]) != len("tag=")+64 {
		t.Errorf("the owner tag %q is not 64 hex", f[7])
	}
	if len(e.verbs("tail")) == 0 {
		t.Fatal("no tail call: the job id the provider follows was not exercised")
	}
	if bad := gateAnswers(t, 2, e.verbsOf("tail", "read", "teardown", "prepare", "run")); len(bad) > 0 {
		t.Errorf("rejected by the real gate: %q", bad)
	}
	// the limit itself: ResourceNameMax at 63 passes, one more byte does not
	exp := time.Now().Add(time.Hour)
	ok := provider.ResourceNameMax("bitc", strings.Repeat("a", 16), strings.Repeat("p", 60), "s1", "job12345", exp, 63)
	if len(ok) != 63 {
		t.Fatalf("ResourceNameMax gave %d bytes", len(ok))
	}
	if bad := gateAnswers(t, 2, []string{"agent " + strings.Repeat("a", 64) + " teardown 1 " + ok}); len(bad) != 0 {
		t.Errorf("a 63-byte name is rejected: %q", bad)
	}
	if bad := gateAnswers(t, 2, []string{"agent " + strings.Repeat("a", 64) + " teardown 1 " + strings.Repeat("a", 65)}); len(bad) != 1 {
		t.Errorf("a 65-byte name is accepted by the gate: %q", bad)
	}
}

func (e *shEnv) verbsOf(ops ...string) []string {
	var out []string
	for _, c := range e.calls() {
		for _, op := range ops {
			if c.Op == op {
				out = append(out, c.Verb)
			}
		}
	}
	return out
}
