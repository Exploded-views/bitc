package liveprobe

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func wantOutcome(t *testing.T, o Outcome, status, finding string) {
	t.Helper()
	if o.Status != status {
		t.Fatalf("status %q, want %q (finding %q)", o.Status, status, o.Finding)
	}
	if !strings.Contains(o.Finding, finding) {
		t.Fatalf("finding %q does not contain %q", o.Finding, finding)
	}
}

func TestParseArch(t *testing.T) {
	wantOutcome(t, parseArch("x86_64\n"), Answered, "(amd64), as assumed")
	wantOutcome(t, parseArch("aarch64\n"), Answered, "NOT amd64")
	wantOutcome(t, parseArch("riscv64\n"), Answered, "neither")
	wantOutcome(t, parseArch(""), Error, "nothing")
}

func TestParseExecUserAndSummary(t *testing.T) {
	o := parseExecUser("uid=1000\nuser=sprite\nsudo=yes\n")
	if !o.IDOK || o.UID != 1000 || o.User != "sprite" || !o.Sudo {
		t.Fatalf("parsed %+v", o)
	}
	o.LatAvg, o.LatMax, o.MaxRunAfterAccepted = 120*time.Millisecond, 300*time.Millisecond, true
	wantOutcome(t, summarizeExec(o), Answered, "passwordless sudo -n works")
	r := parseExecUser("uid=0\nuser=root\nsudo=no\n")
	wantOutcome(t, summarizeExec(r), Answered, "root, so sudo is not needed")
	n := parseExecUser("uid=1000\nuser=sprite\nsudo=no\n")
	wantOutcome(t, summarizeExec(n), Answered, "NO passwordless sudo")
	wantOutcome(t, summarizeExec(parseExecUser("garbage")), Error, "could not read")
}

func TestParseRunFS(t *testing.T) {
	wantOutcome(t, parseRunFS("run=tmpfs\ntmp=overlay\nrundir=yes\n"), Answered, "/run is a tmpfs")
	wantOutcome(t, parseRunFS("tmp=overlay\nrundir=yes\n"), Answered, "plain directory")
	wantOutcome(t, parseRunFS("run=ext4\n"), Answered, "ext4, not a tmpfs")
	wantOutcome(t, parseRunFS(""), Error, "/proc/mounts")
}

func TestParseCodes(t *testing.T) {
	got := parseCodes("srwxr-xr-x 1 root root 0 api.sock\nGET /v1/tasks 200\nGET /v1/services 000\nbogus line\nGET /v1/x notanumber\n")
	if len(got) != 2 || got["GET /v1/tasks"] != 200 || got["GET /v1/services"] != 0 {
		t.Fatalf("parsed %v", got)
	}
}

func TestSummarizeSock(t *testing.T) {
	wantOutcome(t, summarizeSock("srw-rw-rw- 1 root root 0 /.sprite/api.sock\nGET /v1/tasks 200\nGET /v1/services 403\nGET /v1/sprites 000\n"), Answered, "reaches api.sock: GET /v1/services=403, GET /v1/tasks=200")
	wantOutcome(t, summarizeSock("GET /v1/tasks 000\nGET /v1/sprites 000\n"), Answered, "cannot connect")
	wantOutcome(t, summarizeSock("curl=missing\n"), AssumeWorst, "curl is not on the Sprite")
	wantOutcome(t, summarizeSock("ls: cannot access '/.sprite/api.sock': No such file or directory\n"), Answered, "does not exist")
	wantOutcome(t, summarizeSock(""), Error, "no api.sock answers")
}

func TestSummarizeUnshare(t *testing.T) {
	wantOutcome(t, summarizeUnshare("mounted\nrc=0\n"), Answered, "can unshare")
	wantOutcome(t, summarizeUnshare("unshare: unshare failed: Operation not permitted\nrc=1\n"), Answered, "refused")
	wantOutcome(t, summarizeUnshare("sh: 1: unshare: not found\nrc=127\n"), AssumeWorst, "not installed")
	wantOutcome(t, summarizeUnshare("something else\n"), Error, "unexpected")
}

func TestSummarizeFS(t *testing.T) {
	o := summarizeFS(fsObs{ShadowOwner: "root", ShadowMode: "640", ProcReadBytes: 900, WriterOwner: "sprite:sprite 644", BootID: "0123456789abcdef"})
	wantOutcome(t, o, Answered, "reads root-only files")
	if !strings.Contains(o.Finding, "reads /proc paths") || !strings.Contains(o.Finding, "creates files as sprite:sprite 644") {
		t.Fatalf("finding %q", o.Finding)
	}
	o = summarizeFS(fsObs{ShadowReadErr: "HTTP 403", ProcReadErr: "HTTP 404", WriteErr: "HTTP 500"})
	if !strings.Contains(o.Finding, "cannot read root-only /etc/shadow (HTTP 403)") || !strings.Contains(o.Finding, "of /proc fails") || !strings.Contains(o.Finding, "fs/write failed") {
		t.Fatalf("finding %q", o.Finding)
	}
}

func TestSummarizePrivileges(t *testing.T) {
	wantOutcome(t, summarizePrivileges(privObs{GetCode: 404, Attempts: map[string]int{"profile=minimal": 400}}), AssumeWorst, "no privileges policy body was accepted")
	o := summarizePrivileges(privObs{GetCode: 200, Applied: "profile=noNewPrivileges", Sudo: "no", Sock: "srw 1 root", Ckpt: "ls: cannot open"})
	wantOutcome(t, o, Answered, "accepted as profile=noNewPrivileges; afterwards sudo -n works: no")
}

func TestOrgFieldPresent(t *testing.T) {
	cases := map[string]bool{
		`{"name":"a","organization":"sandbox-org"}`: true,
		`{"name":"a"}`:                   false,
		`{"name":"a","organization":""}`: false,
		`{"sprites":[{"organization":"o"},{"organization":"o"}]}`: true,
		`{"sprites":[{"organization":"o"},{"name":"b"}]}`:         false,
		`{"sprites":[]}`: false,
		`not json`:       false,
	}
	for body, want := range cases {
		if got := orgFieldPresent([]byte(body)); got != want {
			t.Errorf("orgFieldPresent(%s) = %v, want %v", body, got, want)
		}
	}
}

const netAllClosed = "tool curl yes\ntool ssh-keyscan yes\nhttps codeload.github.com 000\nhttps objects.githubusercontent.com 000\nhttps registry.npmjs.org 000\nssh github.com 0\n"
const netGithubOnly = "tool curl yes\ntool ssh-keyscan yes\nhttps codeload.github.com 000\nhttps objects.githubusercontent.com 000\nhttps registry.npmjs.org 000\nssh github.com 1\n"
const netAllOpen = "tool curl yes\ntool ssh-keyscan yes\nhttps codeload.github.com 404\nhttps objects.githubusercontent.com 400\nhttps registry.npmjs.org 200\nssh github.com 1\n"

func TestParseNetOut(t *testing.T) {
	o := parseNetOut(netGithubOnly)
	if !o.Tools["curl"] || !o.Tools["ssh-keyscan"] || !o.Reach["ssh github.com"] || o.Reach["https registry.npmjs.org"] {
		t.Fatalf("parsed %+v", o)
	}
	if o := parseNetOut("tool curl no\n"); o.Tools["curl"] {
		t.Fatalf("a missing tool parsed as present: %+v", o)
	}
}

func TestSummarizeNet(t *testing.T) {
	phases := []netPhase{
		{Obs: parseNetOut(netAllClosed)},
		{Allowed: []string{"github.com"}, Obs: parseNetOut(netGithubOnly)},
		{Allowed: []string{"github.com", "codeload.github.com"}, Obs: parseNetOut(netAllOpen)},
	}
	o := summarizeNet(phases)
	wantOutcome(t, o, Answered, "reachable [ssh github.com]")
	need := o.Evidence["needed"].(map[string]string)
	if !strings.Contains(need["ssh github.com"], "phase 1") || !strings.Contains(need["https codeload.github.com"], "phase 2") {
		t.Fatalf("needed: %v", need)
	}
	wantOutcome(t, summarizeNet([]netPhase{{Obs: parseNetOut("tool curl no\ntool ssh-keyscan yes\n")}}), AssumeWorst, "missing")
	wantOutcome(t, summarizeNet([]netPhase{{Obs: parseNetOut(netAllOpen)}}), Error, "control failed")
	wantOutcome(t, summarizeNet(nil), Error, "no network phase")
}

func TestParseResourcesAndAllocVerdict(t *testing.T) {
	o := parseResources("nproc=2\nmemtotal_kb=2097152\ncgroup_max=1073741824\n")
	if o.Nproc != 2 || o.MemTotalKB != 2097152 || o.CgroupMaxByte != 1<<30 {
		t.Fatalf("parsed %+v", o)
	}
	if v := allocVerdict("alloc_rc=0\n", nil); v != "completed" {
		t.Fatalf("clean allocation: %q", v)
	}
	if v := allocVerdict("alloc_rc=1\n", nil); v != "failed" {
		t.Fatalf("failed allocation: %q", v)
	}
	if v := allocVerdict("", nil); v != "killed" {
		t.Fatalf("no output: %q", v)
	}
	if v := allocVerdict("alloc_rc=0\n", errors.New("stream ended")); v != "killed" {
		t.Fatalf("exec error: %q", v)
	}
}

func TestSummarizeResources(t *testing.T) {
	base := resPolicyObs{LimitMB: 1024, SetCode: 200, AllocMB: 1280, Before: resObs{Nproc: 1, MemTotalKB: 1000000}, After: resObs{Nproc: 2, MemTotalKB: 2000000},
		CPUSetCode: 200, Big: 200, BigMemTotalKB: 16 * 1024 * 1024}
	enforced := base
	enforced.Alloc = "killed"
	o := summarizeResources(enforced)
	wantOutcome(t, o, Answered, "limit_mb=1024 enforced: a 1280 MiB tmpfs allocation was killed")
	if !strings.Contains(o.Finding, "nproc 1 -> 2") || !strings.Contains(o.Finding, "16 GiB available") {
		t.Fatalf("finding %q", o.Finding)
	}
	loose := base
	loose.Alloc, loose.CPUSetCode, loose.Big = "completed", 404, 400
	o = summarizeResources(loose)
	wantOutcome(t, o, Answered, "did NOT stop")
	if !strings.Contains(o.Finding, "not accepted over PUT (HTTP 404)") || !strings.Contains(o.Finding, "16 GiB policy refused (HTTP 400)") {
		t.Fatalf("finding %q", o.Finding)
	}
	wantOutcome(t, summarizeResources(resPolicyObs{}), Error, "could not read")
}

func TestSummarizeTransfer(t *testing.T) {
	want := int64(200 << 20)
	o := summarizeTransfer(want, transfer{Bytes: want, Dur: 20 * time.Second}, transfer{Bytes: want, Dur: 10 * time.Second})
	wantOutcome(t, o, Answered, "WSS exec 10.0 MiB/s (20.0 s); fs/read 20.0 MiB/s (10.0 s)")
	o = summarizeTransfer(want, transfer{Bytes: 5, Err: "stream broke"}, transfer{Bytes: want - 1, Dur: time.Second})
	wantOutcome(t, o, Answered, "WSS exec failed after 5 bytes (stream broke)")
	if !strings.Contains(o.Finding, "fs/read returned") {
		t.Fatalf("finding %q", o.Finding)
	}
	wantOutcome(t, summarizeTransfer(want, transfer{Err: "x"}, transfer{Err: "y"}), Error, "failed")
}

func TestSummarizeNames(t *testing.T) {
	tries := []nameTry{
		{Label: "uppercase", Len: 30, Code: 400},
		{Label: "64 chars", Len: 64, Code: 422},
		{Label: "63 chars", Len: 63, WantOK: true, Code: 201, Created: true},
	}
	o := summarizeNames(tries, map[string]string{"bitc": "1"}, map[string]int{"PUT": 404})
	wantOutcome(t, o, Answered, "63 chars (63 chars): HTTP 201")
	if strings.Contains(o.Finding, "UNEXPECTED") {
		t.Fatalf("finding %q", o.Finding)
	}
	tries[1] = nameTry{Label: "64 chars", Len: 64, Code: 201, Created: true}
	tries[2] = nameTry{Label: "63 chars", Len: 63, WantOK: true, Code: 400}
	o = summarizeNames(tries, nil, nil)
	wantOutcome(t, o, Answered, "UNEXPECTED: 64 chars, 63 chars")
	wantOutcome(t, summarizeNames(nil, nil, nil), Error, "no name")
}

func TestSummarizeCkptSearch(t *testing.T) {
	both := parseCkptSearch("overlay_hits=1\ntmpfs_hits=1\n")
	wantOutcome(t, summarizeCkptSearch(both, 2, 1, true), Answered, "tmpfs marker file IS in the checkpoint")
	excl := parseCkptSearch("overlay_hits=2\ntmpfs_hits=0\n")
	wantOutcome(t, summarizeCkptSearch(excl, 2, 1, true), Answered, "NOT in the checkpoint")
	wantOutcome(t, summarizeCkptSearch(parseCkptSearch("ckptdir=missing\n"), 0, 0, true), AssumeWorst, "not mounted")
	wantOutcome(t, summarizeCkptSearch(parseCkptSearch("overlay_hits=0\ntmpfs_hits=0\n"), 1, 0, true), AssumeWorst, "positive control failed")
	wantOutcome(t, summarizeCkptSearch(excl, 0, 0, false), AssumeWorst, "could not place the markers")
	wantOutcome(t, summarizeCkptSearch(parseCkptSearch(""), 0, 0, true), Error, "no counts")
}

func TestSummarizeCheckpointsREST(t *testing.T) {
	good := ckptREST{ID: "cp1", Listed: 3, Autos: 2, GoneAfter: true, SockRoot: map[string]int{"GET /v1/checkpoints": 200}, SockNobody: map[string]int{"GET /v1/checkpoints": 403}}
	o := summarizeCheckpointsREST(good)
	wantOutcome(t, o, Answered, "REST DELETE of a checkpoint works; the list includes 2 automatic checkpoints")
	bad := good
	bad.DeleteErr, bad.Autos = "HTTP 405", 0
	wantOutcome(t, summarizeCheckpointsREST(bad), Answered, "REST DELETE of a checkpoint fails (HTTP 405); the list holds no automatic")
	still := good
	still.GoneAfter = false
	wantOutcome(t, summarizeCheckpointsREST(still), Answered, "still listed")
	wantOutcome(t, summarizeCheckpointsREST(ckptREST{CreateErr: "HTTP 404"}), AssumeWorst, "CLI-only")
}

func TestSummarizePolicySurvive(t *testing.T) {
	o := surviveObs{NetBefore: `{"rules":[{"domain":"a.com","action":"allow"}]}`, NetAfter: `{ "rules": [ {"action":"allow","domain":"a.com"} ] }`,
		ResBefore: `{"memory":{"limit_mb":2048}}`, ResAfter: `{"memory":{"limit_mb":2048}}`, RestoreDur: 3 * time.Second, BootBefore: "aaaaaaaa-1", BootAfter: "bbbbbbbb-2"}
	got := summarizePolicySurvive(o)
	wantOutcome(t, got, Answered, "network policy survived: true; resources policy survived: true; boot_id changed: true")
	o.ResAfter = `{}`
	wantOutcome(t, summarizePolicySurvive(o), Answered, "resources policy survived: false")
	wantOutcome(t, summarizePolicySurvive(surviveObs{RestoreErr: "HTTP 404"}), AssumeWorst, "restore over REST failed")
	wantOutcome(t, summarizePolicySurvive(surviveObs{CreateErr: "HTTP 500"}), AssumeWorst, "no checkpoint to restore")
}

func TestSummarizeRetention(t *testing.T) {
	gone := retObs{IDs: []string{"cp1"}, ListCode: 404, GetCodes: map[string]int{"cp1": 404}, Restore: map[string]int{"cp1": 404}}
	wantOutcome(t, summarizeRetention(gone), Answered, "no checkpoint survives")
	kept := retObs{IDs: []string{"cp1"}, ListCode: 200, ListIDs: []string{"cp1"}, GetCodes: map[string]int{"cp1": 200}, Restore: map[string]int{"cp1": 404}}
	wantOutcome(t, summarizeRetention(kept), Answered, `SURVIVE a Sprite delete`)
	restoreOnly := retObs{IDs: []string{"cp1"}, ListCode: 404, GetCodes: map[string]int{"cp1": 404}, Restore: map[string]int{"cp1": 200}}
	wantOutcome(t, summarizeRetention(restoreOnly), Answered, `"forwarded"`)
	wantOutcome(t, summarizeRetention(retObs{IDs: []string{"cp1"}, DeleteErr: "HTTP 500"}), Error, "not confirmed")
	wantOutcome(t, summarizeRetention(retObs{}), AssumeWorst, "no checkpoint was created")
}

func TestSummarizeWake(t *testing.T) {
	wantOutcome(t, summarizeWake(wakeObs{Idle: "warm", GetAgain: "warm", AfterList: "warm", AfterFS: "warm"}), Answered, "GET does not wake a Sprite; list does not wake it; fs/read does not wake it")
	wantOutcome(t, summarizeWake(wakeObs{Idle: "warm", GetAgain: "running"}), Answered, "GET /v1/sprites/{name} WAKES")
	wantOutcome(t, summarizeWake(wakeObs{Idle: "warm", GetAgain: "warm", AfterList: "running"}), Answered, "a list call WAKES")
	wantOutcome(t, summarizeWake(wakeObs{Idle: "cold", GetAgain: "cold", AfterList: "cold", AfterFS: "running"}), Answered, "fs/read WAKES it")
	wantOutcome(t, summarizeWake(wakeObs{Idle: "running"}), AssumeWorst, "inconclusive")
	wantOutcome(t, summarizeWake(wakeObs{}), Error, "no status")
}

func TestParseTaskIDAndSummarizeTasks(t *testing.T) {
	if id := parseTaskID(`{"id":"t-1","expire":"5m"}`); id != "t-1" {
		t.Fatalf("id %q", id)
	}
	if id := parseTaskID(`{"task_id":"t-2"}`); id != "t-2" {
		t.Fatalf("id %q", id)
	}
	if id := parseTaskID(`<html>`); id != "" {
		t.Fatalf("id %q", id)
	}
	wantOutcome(t, summarizeTasks(taskObs{PostCode: 201, ID: "t", Status: "running"}), Answered, "stayed running")
	wantOutcome(t, summarizeTasks(taskObs{PostCode: 201, ID: "t", Status: "warm"}), Answered, "does NOT hold running")
	wantOutcome(t, summarizeTasks(taskObs{PostCode: 403}), AssumeWorst, "20 s heartbeat")
	wantOutcome(t, summarizeTasks(taskObs{PostCode: 201, ID: "t"}), Error, "no status")
}

func TestParseLimitsAndRegions(t *testing.T) {
	lim := parseLimits([]byte(`{"slug":"o","plan":{"running_limit":10,"warm_limit":50,"name":"payg"},"limits":[{"concurrent_limit":3}]}`))
	if lim["plan.running_limit"] != 10.0 || lim["plan.warm_limit"] != 50.0 || lim["limits[0].concurrent_limit"] != 3.0 || len(lim) != 3 {
		t.Fatalf("limits %v", lim)
	}
	if len(parseLimits([]byte(`{"slug":"o"}`))) != 0 || len(parseLimits([]byte(`nope`))) != 0 {
		t.Fatal("limits found where there are none")
	}
	got := parseRegions([]byte(`{"regions":[{"code":"ord"},{"name":"iad"},"ams",{"x":1}]}`))
	if strings.Join(got, ",") != "ams,iad,ord" {
		t.Fatalf("regions %v", got)
	}
	if got := parseRegions([]byte(`["b","a"]`)); strings.Join(got, ",") != "a,b" {
		t.Fatalf("regions %v", got)
	}
	if parseRegions([]byte(`{"error":"x"}`)) != nil {
		t.Fatal("regions from an error body")
	}
}
