package conformance

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Exploded-views/bitc/conformance/harness"
	"github.com/Exploded-views/bitc/internal/provider/runpod"
)

func install16(t *testing.T, h *harness.H) (string, []byte) {
	h.Run("run", "heavy", "--", "echo", "warm") // creates the install secret
	secret := must(readFile(filepath.Join(h.Root, "account", "install-secret")))
	return sha16(secret), secret
}

func TestRW1_OwnExpiredResourcesWithVerifiedTagOnly(t *testing.T) {
	h := harness.New(t)
	f, _ := rpRepo(t, h, "")
	inst, secret := install16(t, h)
	old := time.Now().Add(-2 * time.Hour)
	mine := runpod.EncodeName("bitc", inst, "heavy", "cpu8", "0000000a", old)
	f.AddPod("mine", mine, old.Add(-time.Hour), map[string]string{"BITC_OWNER": ownerTag(secret, mine)})
	untagged := runpod.EncodeName("bitc", inst, "heavy", "cpu8", "0000000b", old)
	f.AddPod("untagged", untagged, old.Add(-time.Hour), nil)
	other := runpod.EncodeName("bitc", "ffffffffffffffff", "heavy", "cpu8", "0000000c", old)
	f.AddPod("other", other, old.Add(-time.Hour), nil)
	live := runpod.EncodeName("bitc", inst, "heavy", "cpu8", "0000000d", time.Now().Add(time.Hour))
	f.AddPod("live", live, time.Now(), map[string]string{"BITC_OWNER": ownerTag(secret, live)})
	r := h.Run("sweep")
	if r.Code != 0 || !strings.Contains(r.Stdout, "swept resource mine (") || strings.Contains(r.Stdout, "untagged") ||
		strings.Contains(r.Stdout, "other") || strings.Contains(r.Stdout, "live") {
		t.Fatalf("%+v", r)
	}
	if r := h.Run("sweep", "--foreign"); !strings.Contains(r.Stdout, "swept foreign resource other (") {
		t.Fatalf("%+v", r)
	}
}

func TestRW2_OwnRefsOlderThan3h(t *testing.T) {
	h := harness.New(t)
	_, wt := rpRepo(t, h, "")
	inst, _ := install16(t, h)
	bare := h.Origin("app")
	head := gitIn(t, h, wt, "rev-parse", "HEAD")
	oldRef := "refs/bitc/jobs/" + inst + "/heavy/" + head[:12] + "-" + unixAgo(4*time.Hour) + "-deadbeef"
	newRef := "refs/bitc/jobs/" + inst + "/heavy/" + head[:12] + "-" + unixAgo(time.Hour) + "-cafebabe"
	otherRef := "refs/bitc/jobs/ffffffffffffffff/heavy/" + head[:12] + "-" + unixAgo(5*time.Hour) + "-0badf00d"
	for _, ref := range []string{oldRef, newRef, otherRef} {
		gitIn(t, h, bare, "update-ref", ref, head)
	}
	r := h.Run("sweep")
	if !strings.Contains(r.Stdout, "swept ref "+oldRef+" (older than 3h)") || strings.Contains(r.Stdout, newRef) || strings.Contains(r.Stdout, otherRef) {
		t.Fatalf("%+v", r)
	}
}

func TestRW3_NoCredentialsStillSweepsRefs(t *testing.T) {
	h := harness.New(t)
	rpRepo(t, h, "")
	h.HostConfig(strings.Replace(readCfg(t, h), "api_key = \"sk-test-0123456789\"\n", "", 1))
	r := h.Run("sweep")
	if r.Code != 0 || !strings.Contains(r.Stderr, "bitc: sweep: runpod: no credentials; skipped resources") {
		t.Fatalf("%+v", r)
	}
}

func TestRW4_ErrorsReportedExitZero(t *testing.T) {
	h := harness.New(t)
	f, _ := rpRepo(t, h, "")
	install16(t, h)
	f.Behave("list-500")
	r := h.Run("sweep")
	if r.Code != 0 || !strings.Contains(r.Stderr, "bitc: sweep: runpod: cannot list live resources (HTTP 500)") {
		t.Fatalf("%+v", r)
	}
}

func TestRW5_RefsOfTheRepoAroundTheCwd(t *testing.T) {
	h := harness.New(t)
	_, wt := rpRepo(t, h, "")
	inst, _ := install16(t, h)
	bare := h.Origin("app")
	head := gitIn(t, h, wt, "rev-parse", "HEAD")
	ref := "refs/bitc/jobs/" + inst + "/heavy/" + head[:12] + "-" + unixAgo(4*time.Hour) + "-deadbeef"
	gitIn(t, h, bare, "update-ref", ref, head)
	h.Cwd = filepath.Join(wt, "sub")
	if r := h.Run("sweep"); !strings.Contains(r.Stdout, "swept ref "+ref) {
		t.Fatalf("%+v", r)
	}
}

func sha16(secret []byte) string {
	sum := sha256.Sum256(secret)
	return hex.EncodeToString(sum[:])[:16]
}

func unixAgo(d time.Duration) string { return strconv.FormatInt(time.Now().Add(-d).Unix(), 10) }

func TestSW1_DoctorRemoteChecks(t *testing.T) {
	h := harness.New(t)
	f, _ := rpRepo(t, h, "")
	if r := h.Run("doctor", "--remote"); r.Code != 0 || !strings.Contains(r.Stdout, "ok runpod api:") || strings.Contains(r.Stdout, "sk-test") {
		t.Fatalf("%+v", r)
	}
	inst, _ := install16(t, h)
	exp := time.Now().Add(-2 * time.Hour)
	other := runpod.EncodeName("bitc", "ffffffffffffffff", "heavy", "cpu8", "0000000c", exp)
	f.AddPod("other", other, exp.Add(-time.Hour), nil)
	own := runpod.EncodeName("bitc", inst, "heavy", "cpu8", "0000000e", exp)
	f.AddPod("mine", own, exp.Add(-time.Hour), nil)
	if r := h.Run("doctor", "--remote"); !strings.Contains(r.Stdout, "warn runpod foreign: other ("+other+", past its expiry; sweep --foreign tears it down)") || strings.Contains(r.Stdout, "foreign: mine") {
		t.Fatalf("%+v", r)
	}
	h.HostConfig(strings.Replace(readCfg(t, h), "api_key = \"sk-test-0123456789\"\n", "", 1))
	if r := h.Run("doctor", "--remote"); r.Code != 69 || !strings.Contains(r.Stdout, "fail runpod credentials:") {
		t.Fatalf("%+v", r)
	}
}

func TestSW2_StatusShowsOwnAndForeignResourcesAndSpend(t *testing.T) {
	h := harness.New(t)
	f, _ := rpRepo(t, h, "")
	f.SetSpend(1.25)
	p := h.Start("run", "heavy", "--", "sh", "-c", "sleep 3")
	h.Until(func() bool { return calls(f, "POST /v2/pods") == 1 }, 20*time.Second)
	r := h.Run("--json", "status")
	p.Wait()
	if !strings.Contains(r.Stdout, `"own": [`) || !strings.Contains(r.Stdout, `"spend_usd": "$1.25"`) || !strings.Contains(r.Stdout, `"pool": "heavy"`) {
		t.Fatalf("%s", r.Stdout)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(r.Stdout), &got); err != nil {
		t.Fatal(err)
	}
	var schema map[string]any
	if err := json.Unmarshal(must(os.ReadFile("../docs/status.schema.json")), &schema); err != nil {
		t.Fatal(err)
	}
	checkSchema(t, "$", schema, got)
	if rem, _ := got["remote"].(map[string]any); rem["runpod"] == nil {
		t.Fatalf("no runpod section: %s", r.Stdout)
	}
}

func TestRW6_SweepPrunesDeadReservations(t *testing.T) {
	h := harness.New(t)
	rpRepo(t, h, "")
	install16(t, h)
	path := filepath.Join(h.Root, "account", "remote", "runpod", "reservations.json")
	body := fmt.Sprintf(`[{"pid":2147483000,"start":1,"name":"bitc-dead","estimate":1000,"expiry":%d}]`, time.Now().Add(time.Hour).Unix())
	write := func() {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write()
	if r := h.Run("sweep", "--dry-run"); !strings.Contains(r.Stdout, "swept reservation bitc-dead (its process is gone)") || string(must(os.ReadFile(path))) != body {
		t.Fatalf("%+v", r)
	}
	if r := h.Run("sweep"); !strings.Contains(r.Stdout, "swept reservation bitc-dead (its process is gone)") || strings.Contains(string(must(os.ReadFile(path))), "bitc-dead") {
		t.Fatalf("%+v", r)
	}
	write()
	h.HostConfig(strings.Replace(readCfg(t, h), "api_key = \"sk-test-0123456789\"\n", "", 1))
	if r := h.Run("sweep"); !strings.Contains(r.Stdout, "swept reservation bitc-dead") {
		t.Fatalf("no credentials: %+v", r)
	}
}
