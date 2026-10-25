package liveprobe

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Everything here is a pure function of recorded or hand-written output: the probes gather, these
// decide. Each has a unit test.

func ok(finding string, ev map[string]any) Outcome {
	return Outcome{Status: Answered, Finding: finding, Evidence: ev}
}

func worst(finding string, ev map[string]any) Outcome {
	return Outcome{Status: AssumeWorst, Finding: finding, Evidence: ev}
}

func failed(finding string, ev map[string]any) Outcome {
	return Outcome{Status: Error, Finding: finding, Evidence: ev}
}

// parseKV reads "key=value" lines (the first = splits).
func parseKV(out string) map[string]string {
	m := map[string]string{}
	for _, l := range strings.Split(out, "\n") {
		if k, v, has := strings.Cut(strings.TrimSpace(l), "="); has && k != "" {
			m[k] = v
		}
	}
	return m
}

// ---- 24: architecture

func parseArch(out string) Outcome {
	a := strings.TrimSpace(strings.SplitN(strings.TrimSpace(out), "\n", 2)[0])
	ev := map[string]any{"uname_m": a}
	switch a {
	case "":
		return failed("uname -m printed nothing", ev)
	case "x86_64", "amd64":
		return ok("x86_64 (amd64), as assumed", ev)
	case "aarch64", "arm64":
		return ok("aarch64: NOT amd64; an amd64 agent will not run, the agent source needs an arm64 build", ev)
	}
	return ok("architecture "+a+" is neither amd64 nor arm64", ev)
}

// ---- 15: exec user, sudo, latency

type execObs struct {
	UID                 int
	User                string
	Sudo                bool
	IDOK                bool
	LatAvg, LatMax      time.Duration
	RateLimited         bool
	MaxRunAfterAccepted bool
	MaxRunErr           string
}

// parseExecUser reads "uid=0", "user=root", "sudo=yes|no".
func parseExecUser(out string) execObs {
	kv := parseKV(out)
	var o execObs
	n, err := strconv.Atoi(kv["uid"])
	if err == nil {
		o.UID, o.IDOK = n, true
	}
	o.User = kv["user"]
	o.Sudo = kv["sudo"] == "yes"
	return o
}

func summarizeExec(o execObs) Outcome {
	ev := map[string]any{"uid": o.UID, "user": o.User, "passwordless_sudo": o.Sudo,
		"exec_latency_avg_ms": o.LatAvg.Milliseconds(), "exec_latency_max_ms": o.LatMax.Milliseconds(),
		"rate_limited_in_5_execs": o.RateLimited, "max_run_after_disconnect_30s_accepted": o.MaxRunAfterAccepted,
		"unanswered": []string{"max_run_after_disconnect maximum", "output re-attach after a drop (needs a raw session: assume none)", "idle wake reliability (SP18; see probe 19)"}}
	if o.MaxRunErr != "" {
		ev["max_run_after_disconnect_error"] = o.MaxRunErr
	}
	if !o.IDOK {
		return failed("could not read the exec user (id -u printed no number)", ev)
	}
	who := fmt.Sprintf("exec runs as %s (uid %d)", firstNonEmpty(o.User, "?"), o.UID)
	switch {
	case o.UID == 0:
		who += "; root, so sudo is not needed (set NoSudo)"
	case o.Sudo:
		who += "; passwordless sudo -n works"
	default:
		who += "; NO passwordless sudo: the agent exec cannot run as root"
	}
	return ok(fmt.Sprintf("%s; exec latency avg %d ms, max %d ms; rate limit seen: %v; max_run_after_disconnect=30s accepted: %v",
		who, o.LatAvg.Milliseconds(), o.LatMax.Milliseconds(), o.RateLimited, o.MaxRunAfterAccepted), ev)
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}

// ---- 21: /run

func parseRunFS(out string) Outcome {
	kv := parseKV(out)
	ev := map[string]any{"run_fstype": kv["run"], "tmp_fstype": kv["tmp"], "run_is_dir": kv["rundir"] == "yes"}
	switch {
	case kv["run"] == "tmpfs":
		return ok("/run is a tmpfs (checkpoint exclusion is probe 5)", ev)
	case kv["run"] == "" && kv["rundir"] == "yes":
		return ok("/run is a plain directory on the root filesystem, not a tmpfs: mount a tmpfs for secrets", ev)
	case kv["run"] != "":
		return ok("/run is "+kv["run"]+", not a tmpfs: mount a tmpfs for secrets", ev)
	}
	return failed("could not read /proc/mounts", ev)
}

// ---- 23: api.sock

// parseCodes reads "GET /v1/tasks 200" lines into "GET /v1/tasks" -> 200.
func parseCodes(out string) map[string]int {
	m := map[string]int{}
	for _, l := range strings.Split(out, "\n") {
		f := strings.Fields(l)
		if len(f) == 3 && (f[0] == "GET" || f[0] == "POST" || f[0] == "DELETE") {
			if n, err := strconv.Atoi(f[2]); err == nil {
				m[f[0]+" "+f[1]] = n
			}
		}
	}
	return m
}

func summarizeSock(out string) Outcome {
	codes := parseCodes(out)
	ev := map[string]any{"codes": codes, "socket_listing": firstLineWith(out, "api.sock")}
	switch {
	case strings.Contains(out, "curl=missing"):
		return worst("curl is not on the Sprite: cannot test api.sock as a non-root user; assume it is reachable", ev)
	case strings.Contains(out, "No such file") && len(codes) == 0:
		return ok("/.sprite/api.sock does not exist for a non-root user", ev)
	case len(codes) == 0:
		return failed("no api.sock answers were recorded", ev)
	}
	var reach, denied []string
	for k, c := range codes {
		if c == 0 {
			denied = append(denied, k)
		} else {
			reach = append(reach, fmt.Sprintf("%s=%d", k, c))
		}
	}
	sort.Strings(reach)
	sort.Strings(denied)
	if len(reach) == 0 {
		return ok("a non-root user cannot connect to api.sock", ev)
	}
	return ok("a non-root user reaches api.sock: "+strings.Join(reach, ", ")+" (decision 12 must assume a job can use these)", ev)
}

func firstLineWith(s, sub string) string {
	for _, l := range strings.Split(s, "\n") {
		if strings.Contains(l, sub) {
			return oneLine(l, 120)
		}
	}
	return ""
}

// ---- 20: unshare

func summarizeUnshare(out string) Outcome {
	ev := map[string]any{"tail": oneLine(lastLines(out, 3), 200)}
	switch {
	case strings.Contains(out, "mounted"):
		return ok("root inside a Sprite can unshare mount+pid namespaces and mount a tmpfs over /.sprite", ev)
	case strings.Contains(out, "not permitted") || strings.Contains(out, "Permission denied"):
		return ok("unshare or the tmpfs mount over /.sprite is refused (not permitted)", ev)
	case strings.Contains(out, "unshare: command not found") || strings.Contains(out, "unshare: not found"):
		return worst("unshare is not installed on the Sprite; assume root can do it", ev)
	}
	return failed("unexpected output from the unshare test", ev)
}

func lastLines(s string, n int) string {
	l := strings.Split(strings.TrimSpace(s), "\n")
	if len(l) > n {
		l = l[len(l)-n:]
	}
	return strings.Join(l, "\n")
}

// ---- 14: fs read and write

type fsObs struct {
	ShadowMode, ShadowOwner string
	ShadowReadErr           string // "" = fs/read returned the file
	ProcReadBytes           int64
	ProcReadErr             string
	WriteErr                string
	WriterOwner             string // "user:group mode"
	BootID                  string
}

func summarizeFS(o fsObs) Outcome {
	ev := map[string]any{"shadow_owner": o.ShadowOwner, "shadow_mode": o.ShadowMode, "shadow_read_error": o.ShadowReadErr,
		"proc_read_bytes": o.ProcReadBytes, "proc_read_error": o.ProcReadErr, "write_error": o.WriteErr, "written_file": o.WriterOwner,
		"boot_id_prefix": prefix(o.BootID, 8), "unanswered": []string{"boot_id change on restore and restore duration: see probe 2"}}
	var parts []string
	switch o.ShadowReadErr {
	case "":
		parts = append(parts, "fs/read reads root-only files (it runs as root)")
	default:
		parts = append(parts, "fs/read cannot read root-only /etc/shadow ("+o.ShadowReadErr+")")
	}
	switch {
	case o.ProcReadErr != "":
		parts = append(parts, "fs/read of /proc fails ("+o.ProcReadErr+")")
	case o.ProcReadBytes > 0:
		parts = append(parts, "fs/read reads /proc paths")
	default:
		parts = append(parts, "fs/read of /proc returns no bytes")
	}
	switch {
	case o.WriteErr != "":
		parts = append(parts, "fs/write failed ("+o.WriteErr+")")
	case o.WriterOwner != "":
		parts = append(parts, "fs/write creates files as "+o.WriterOwner)
	}
	return ok(strings.Join(parts, "; "), ev)
}

func prefix(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n]
	}
	return s
}

// ---- 4: privileges policy

type privObs struct {
	GetCode  int
	Attempts map[string]int // "profile=noNewPrivileges" -> status
	Applied  string         // the attempt whose answer was 2xx ("" = none)
	Sudo     string         // "yes"/"no"/"" after applying
	Sock     string         // listing of api.sock after applying
	Ckpt     string         // listing of /.sprite/checkpoints after applying
}

func summarizePrivileges(o privObs) Outcome {
	ev := map[string]any{"get_status": o.GetCode, "set_attempts": o.Attempts, "applied": o.Applied, "sudo_after": o.Sudo,
		"api_sock_after": o.Sock, "checkpoints_dir_after": o.Ckpt}
	if o.Applied == "" {
		return worst(fmt.Sprintf("no privileges policy body was accepted (GET %d, attempts %v); assume noNewPrivileges may break sudo, api.sock and the checkpoint mount", o.GetCode, o.Attempts), ev)
	}
	return ok(fmt.Sprintf("privileges policy accepted as %s; afterwards sudo -n works: %s; api.sock: %s; /.sprite/checkpoints: %s",
		o.Applied, firstNonEmpty(o.Sudo, "?"), firstNonEmpty(o.Sock, "?"), firstNonEmpty(o.Ckpt, "?")), ev)
}

// ---- 11: organization

// orgFieldPresent reports whether a Sprite body (or every Sprite of a list body) carries a
// non-empty "organization".
func orgFieldPresent(body []byte) bool {
	var one map[string]json.RawMessage
	if json.Unmarshal(body, &one) != nil {
		return false
	}
	if raw, has := one["sprites"]; has {
		var list []map[string]json.RawMessage
		if json.Unmarshal(raw, &list) != nil || len(list) == 0 {
			return false
		}
		for _, s := range list {
			if !strField(s, "organization") {
				return false
			}
		}
		return true
	}
	return strField(one, "organization")
}

func strField(m map[string]json.RawMessage, k string) bool {
	var s string
	return json.Unmarshal(m[k], &s) == nil && s != ""
}

// ---- 3: egress

type netObs struct {
	Reach map[string]bool // "https codeload.github.com" / "ssh github.com"
	Tools map[string]bool // curl, ssh-keyscan
}

var netTargets = []string{"https codeload.github.com", "https objects.githubusercontent.com", "https registry.npmjs.org", "ssh github.com"}

// parseNetOut reads "https HOST CODE", "ssh HOST LINES" and "tool NAME yes|no".
func parseNetOut(out string) netObs {
	o := netObs{Reach: map[string]bool{}, Tools: map[string]bool{}}
	for _, l := range strings.Split(out, "\n") {
		f := strings.Fields(l)
		switch {
		case len(f) == 3 && f[0] == "https":
			o.Reach["https "+f[1]] = f[2] != "000" && f[2] != ""
		case len(f) == 3 && f[0] == "ssh":
			n, _ := strconv.Atoi(f[2])
			o.Reach["ssh "+f[1]] = n > 0
		case len(f) == 3 && f[0] == "tool":
			o.Tools[f[1]] = f[2] == "yes"
		}
	}
	return o
}

type netPhase struct {
	Allowed []string
	Obs     netObs
}

func summarizeNet(phases []netPhase) Outcome {
	ev := map[string]any{}
	var lines []string
	for i, p := range phases {
		var open []string
		for _, t := range netTargets {
			if p.Obs.Reach[t] {
				open = append(open, t)
			}
		}
		ev[fmt.Sprintf("phase%d", i)] = map[string]any{"allowed": p.Allowed, "reachable": open}
		lines = append(lines, fmt.Sprintf("allow %v -> reachable %v", p.Allowed, open))
	}
	if len(phases) == 0 {
		return failed("no network phase ran", ev)
	}
	if !phases[0].Obs.Tools["curl"] || !phases[0].Obs.Tools["ssh-keyscan"] {
		return worst("curl or ssh-keyscan is missing on the Sprite, so egress could not be measured; keep the design's wide allow list", ev)
	}
	need := map[string]string{}
	for _, t := range netTargets {
		for i, p := range phases {
			if p.Obs.Reach[t] {
				need[t] = fmt.Sprintf("first reachable in phase %d (allowed %v)", i, p.Allowed)
				break
			}
		}
		if _, has := need[t]; !has {
			need[t] = "never reachable"
		}
	}
	ev["needed"] = need
	if phases[0].Obs.Reach["https registry.npmjs.org"] {
		return failed("control failed: registry.npmjs.org was reachable under an empty allow policy", ev)
	}
	return ok(strings.Join(lines, "; "), ev)
}

// ---- 6: resources

type resObs struct {
	Nproc         int
	MemTotalKB    int64
	CgroupMax     string
	CgroupMaxByte int64
}

func parseResources(out string) resObs {
	kv := parseKV(out)
	var o resObs
	o.Nproc, _ = strconv.Atoi(kv["nproc"])
	o.MemTotalKB, _ = strconv.ParseInt(kv["memtotal_kb"], 10, 64)
	o.CgroupMax = kv["cgroup_max"]
	o.CgroupMaxByte, _ = strconv.ParseInt(kv["cgroup_max"], 10, 64)
	return o
}

// allocVerdict: "killed" (the exec ended or the shell was killed), "failed" (a nonzero exit),
// "completed".
func allocVerdict(out string, execErr error) string {
	kv := parseKV(out)
	switch {
	case execErr != nil:
		return "killed"
	case kv["alloc_rc"] == "0":
		return "completed"
	case kv["alloc_rc"] == "":
		return "killed"
	}
	return "failed"
}

type resPolicyObs struct {
	LimitMB       int
	SetCode       int
	GetBody       string
	Before, After resObs
	CPUSetCode    int
	CPUSetBody    string
	Alloc         string // allocVerdict at limit+256 MiB
	AllocMB       int
	Big           int   // 16384 policy status
	BigMemTotalKB int64 // MemTotal after the 16 GiB policy
}

func summarizeResources(o resPolicyObs) Outcome {
	ev := map[string]any{"limit_mb": o.LimitMB, "set_status": o.SetCode, "policy_read_back": o.GetBody, "nproc": o.Before.Nproc,
		"memtotal_kb": o.Before.MemTotalKB, "cgroup_max": o.Before.CgroupMax, "alloc_mb": o.AllocMB, "alloc": o.Alloc,
		"config_set_status": o.CPUSetCode, "config_set_body": o.CPUSetBody, "nproc_after_config": o.After.Nproc,
		"memtotal_kb_after_config": o.After.MemTotalKB, "limit_16g_status": o.Big, "memtotal_kb_after_16g": o.BigMemTotalKB}
	var p []string
	switch o.Alloc {
	case "completed":
		p = append(p, fmt.Sprintf("limit_mb=%d did NOT stop a %d MiB tmpfs allocation (not enforced against tmpfs)", o.LimitMB, o.AllocMB))
	case "killed", "failed":
		p = append(p, fmt.Sprintf("limit_mb=%d enforced: a %d MiB tmpfs allocation was %s", o.LimitMB, o.AllocMB, o.Alloc))
	default:
		p = append(p, "allocation not run")
	}
	switch {
	case o.CPUSetCode >= 200 && o.CPUSetCode < 300:
		p = append(p, fmt.Sprintf("config ram_mb/cpus accepted; nproc %d -> %d, MemTotal %d -> %d kB", o.Before.Nproc, o.After.Nproc, o.Before.MemTotalKB, o.After.MemTotalKB))
	default:
		p = append(p, fmt.Sprintf("config ram_mb/cpus not accepted over PUT (HTTP %d)", o.CPUSetCode))
	}
	switch {
	case o.Big >= 200 && o.Big < 300 && o.BigMemTotalKB >= 15*1024*1024:
		p = append(p, fmt.Sprintf("16 GiB available (policy accepted, MemTotal %d kB)", o.BigMemTotalKB))
	case o.Big >= 200 && o.Big < 300:
		p = append(p, fmt.Sprintf("16 GiB policy accepted but MemTotal is %d kB", o.BigMemTotalKB))
	default:
		p = append(p, fmt.Sprintf("16 GiB policy refused (HTTP %d)", o.Big))
	}
	if o.Before.Nproc == 0 && o.Before.MemTotalKB == 0 {
		return failed("could not read nproc or MemTotal", ev)
	}
	return ok(strings.Join(p, "; "), ev)
}

// ---- 16: throughput

type transfer struct {
	Bytes int64
	Dur   time.Duration
	Err   string
}

func (t transfer) mibps() float64 {
	if t.Dur <= 0 {
		return 0
	}
	return float64(t.Bytes) / (1 << 20) / t.Dur.Seconds()
}

func summarizeTransfer(want int64, viaExec, viaFS transfer) Outcome {
	ev := map[string]any{"want_bytes": want, "exec_bytes": viaExec.Bytes, "exec_seconds": viaExec.Dur.Seconds(), "exec_mib_per_s": round1(viaExec.mibps()), "exec_error": viaExec.Err,
		"fs_bytes": viaFS.Bytes, "fs_seconds": viaFS.Dur.Seconds(), "fs_mib_per_s": round1(viaFS.mibps()), "fs_error": viaFS.Err}
	desc := func(name string, t transfer) string {
		switch {
		case t.Err != "":
			return fmt.Sprintf("%s failed after %d bytes (%s)", name, t.Bytes, t.Err)
		case t.Bytes != want:
			return fmt.Sprintf("%s returned %d of %d bytes", name, t.Bytes, want)
		}
		return fmt.Sprintf("%s %.1f MiB/s (%.1f s)", name, t.mibps(), t.Dur.Seconds())
	}
	if viaExec.Bytes == 0 && viaFS.Bytes == 0 && viaExec.Err != "" && viaFS.Err != "" {
		return failed(desc("WSS exec", viaExec)+"; "+desc("fs/read", viaFS), ev)
	}
	return ok(desc("WSS exec", viaExec)+"; "+desc("fs/read", viaFS), ev)
}

func round1(f float64) float64 { return float64(int64(f*10+0.5)) / 10 }

// ---- 13: names

type nameTry struct {
	Label   string
	Len     int
	WantOK  bool
	Code    int
	Created bool
}

func summarizeNames(tries []nameTry, labelRead map[string]string, updateCodes map[string]int) Outcome {
	ev := map[string]any{"labels_read_back": labelRead, "label_update_status": updateCodes}
	var lines, bad []string
	var rows []map[string]any
	for _, t := range tries {
		okTry := (t.WantOK && t.Created) || (!t.WantOK && !t.Created && t.Code >= 400 && t.Code < 500)
		lines = append(lines, fmt.Sprintf("%s (%d chars): HTTP %d", t.Label, t.Len, t.Code))
		rows = append(rows, map[string]any{"try": t.Label, "len": t.Len, "status": t.Code, "created": t.Created, "as_expected": okTry})
		if !okTry {
			bad = append(bad, t.Label)
		}
	}
	ev["tries"] = rows
	f := strings.Join(lines, "; ")
	if len(bad) > 0 {
		f += "; UNEXPECTED: " + strings.Join(bad, ", ")
	}
	if len(tries) == 0 {
		return failed("no name was tried", ev)
	}
	return ok(f, ev)
}

// ---- 5, 1, 2: checkpoints

type ckptSearch struct {
	DirMissing  bool
	OverlayHits int
	TmpfsHits   int
	HaveOverlay bool
	HaveTmpfs   bool
}

func parseCkptSearch(out string) ckptSearch {
	kv := parseKV(out)
	var s ckptSearch
	s.DirMissing = kv["ckptdir"] == "missing"
	var err error
	_, err = strconv.Atoi(kv["overlay_hits"])
	s.HaveOverlay = err == nil
	s.OverlayHits, _ = strconv.Atoi(kv["overlay_hits"])
	_, err = strconv.Atoi(kv["tmpfs_hits"])
	s.HaveTmpfs = err == nil
	s.TmpfsHits, _ = strconv.Atoi(kv["tmpfs_hits"])
	return s
}

func summarizeCkptSearch(s ckptSearch, listed, autos int, setupOK bool) Outcome {
	ev := map[string]any{"checkpoints_listed": listed, "auto_checkpoints_listed": autos, "overlay_marker_files_in_checkpoints": s.OverlayHits,
		"tmpfs_marker_files_in_checkpoints": s.TmpfsHits, "unanswered": []string{"turning auto checkpoints off or excluding tmpfs by option (no API known): assume impossible"}}
	switch {
	case !setupOK:
		return worst("could not place the markers (no tmpfs mount, or the exec failed); assume tmpfs content is checkpointed", ev)
	case s.DirMissing:
		return worst("/.sprite/checkpoints is not mounted inside the Sprite, so checkpoint contents cannot be searched; assume tmpfs content is checkpointed", ev)
	case !s.HaveOverlay || !s.HaveTmpfs:
		return failed("the checkpoint search printed no counts", ev)
	case s.OverlayHits == 0:
		return worst("positive control failed: the overlay marker file is not in the checkpoint; assume tmpfs content is checkpointed", ev)
	case s.TmpfsHits == 0:
		return ok("a root-mounted tmpfs is allowed and its marker file is NOT in the checkpoint (overlay control found)", ev)
	}
	return ok("the tmpfs marker file IS in the checkpoint: tmpfs is not excluded (worst case holds)", ev)
}

type ckptREST struct {
	CreateErr     string
	ID            string
	Listed, Autos int
	DeleteErr     string
	GoneAfter     bool
	SockRoot      map[string]int
	SockNobody    map[string]int
}

func summarizeCheckpointsREST(o ckptREST) Outcome {
	ev := map[string]any{"create_error": o.CreateErr, "listed": o.Listed, "auto_listed": o.Autos, "delete_error": o.DeleteErr,
		"gone_after_delete": o.GoneAfter, "api_sock_root": o.SockRoot, "api_sock_nobody": o.SockNobody}
	if o.CreateErr != "" {
		return worst("a checkpoint could not be created over REST ("+o.CreateErr+"); assume checkpoints are CLI-only and cannot be deleted", ev)
	}
	del := "REST DELETE of a checkpoint works"
	switch {
	case o.DeleteErr != "":
		del = "REST DELETE of a checkpoint fails (" + o.DeleteErr + ")"
	case !o.GoneAfter:
		del = "REST DELETE answered but the checkpoint is still listed"
	}
	autos := "the list holds no automatic checkpoints"
	if o.Autos > 0 {
		autos = fmt.Sprintf("the list includes %d automatic checkpoints", o.Autos)
	}
	sock := fmt.Sprintf("api.sock checkpoint paths from root %v, from a non-root user %v", o.SockRoot, o.SockNobody)
	return ok(del+"; "+autos+"; "+sock, ev)
}

type surviveObs struct {
	RestoreErr            string
	RestoreDur            time.Duration
	NetBefore, NetAfter   string
	ResBefore, ResAfter   string
	BootBefore, BootAfter string
	CreateErr             string
}

// canonJSON re-marshals JSON text so key order and spacing do not matter.
func canonJSON(s string) string {
	var v any
	if json.Unmarshal([]byte(s), &v) != nil {
		return strings.TrimSpace(s)
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func summarizePolicySurvive(o surviveObs) Outcome {
	ev := map[string]any{"restore_error": o.RestoreErr, "restore_seconds": o.RestoreDur.Seconds(), "boot_id_changed": o.BootBefore != o.BootAfter && o.BootBefore != "",
		"boot_before": prefix(o.BootBefore, 8), "boot_after": prefix(o.BootAfter, 8),
		"unanswered": []string{"setting a policy before first boot (the Sprite is already booted when created): keep create, policy, verify, exec"}}
	if o.CreateErr != "" {
		return worst("no checkpoint to restore ("+o.CreateErr+"); assume policies do not survive a restore", ev)
	}
	if o.RestoreErr != "" {
		return worst("restore over REST failed ("+o.RestoreErr+"); assume policies do not survive a restore", ev)
	}
	netOK := canonJSON(o.NetBefore) == canonJSON(o.NetAfter)
	resOK := canonJSON(o.ResBefore) == canonJSON(o.ResAfter)
	ev["network_survived"], ev["resources_survived"] = netOK, resOK
	return ok(fmt.Sprintf("after a restore (%.1f s): network policy survived: %v; resources policy survived: %v; boot_id changed: %v",
		o.RestoreDur.Seconds(), netOK, resOK, ev["boot_id_changed"]), ev)
}

// ---- 25: retention after delete

type retObs struct {
	IDs       []string
	DeleteErr string
	ListCode  int
	ListIDs   []string
	GetCodes  map[string]int
	Restore   map[string]int
	Recreated bool
}

func summarizeRetention(o retObs) Outcome {
	ev := map[string]any{"checkpoint_ids_before_delete": o.IDs, "delete_error": o.DeleteErr, "list_status": o.ListCode, "listed_after_delete": o.ListIDs,
		"get_status": o.GetCodes, "restore_status": o.Restore, "restore_recreated_the_sprite": o.Recreated}
	if o.DeleteErr != "" {
		return failed("the Sprite delete was not confirmed ("+o.DeleteErr+"): retention could not be tested", ev)
	}
	if len(o.IDs) == 0 {
		return worst("no checkpoint was created, so retention could not be tested; assume checkpoints survive a delete", ev)
	}
	surv := len(o.ListIDs) > 0 || (o.ListCode >= 200 && o.ListCode < 300)
	for _, c := range o.GetCodes {
		surv = surv || (c >= 200 && c < 300)
	}
	for _, c := range o.Restore {
		surv = surv || (c >= 200 && c < 300)
	}
	if surv {
		return ok(fmt.Sprintf("checkpoints SURVIVE a Sprite delete (list HTTP %d with %v, get %v, restore %v): decision 10's send_env default becomes \"forwarded\"", o.ListCode, o.ListIDs, o.GetCodes, o.Restore), ev)
	}
	return ok(fmt.Sprintf("no checkpoint survives a Sprite delete (list HTTP %d, get %v, restore %v)", o.ListCode, o.GetCodes, o.Restore), ev)
}

// ---- 19: wake

type wakeObs struct {
	Idle, GetAgain, AfterList, AfterFS string
}

func summarizeWake(w wakeObs) Outcome {
	ev := map[string]any{"status_after_idle": w.Idle, "status_get_again": w.GetAgain, "status_after_list": w.AfterList, "status_after_fs_read": w.AfterFS}
	switch {
	case w.Idle == "":
		return failed("no status was read", ev)
	case w.Idle == "running":
		return worst("inconclusive: the Sprite still read running after the idle wait (a GET wakes it, or it did not idle); assume GET, list and fs/read wake a Sprite", ev)
	case w.GetAgain == "running":
		return ok("GET /v1/sprites/{name} WAKES a Sprite (warm on the first read, running on the second); assume list and fs/read do too", ev)
	}
	f := "GET does not wake a Sprite"
	if w.AfterList == "running" {
		return ok(f+"; a list call WAKES it (assume fs/read does too)", ev)
	}
	f += "; list does not wake it"
	if w.AfterFS == "running" {
		return ok(f+"; fs/read WAKES it", ev)
	}
	return ok(f+"; fs/read does not wake it", ev)
}

// ---- 17: tasks

type taskObs struct {
	PostCode int
	ID       string
	ListCode int
	Status   string
	DeleteOK bool
}

// parseTaskID reads the id out of the Tasks API answer to a POST.
func parseTaskID(body string) string {
	var m map[string]any
	if json.Unmarshal([]byte(strings.TrimSpace(body)), &m) != nil {
		return ""
	}
	for _, k := range []string{"id", "task_id", "name"} {
		if s, isStr := m[k].(string); isStr && s != "" {
			return s
		}
	}
	return ""
}

func summarizeTasks(o taskObs) Outcome {
	ev := map[string]any{"post_status": o.PostCode, "task_id_found": o.ID != "", "list_status": o.ListCode, "status_after_idle_with_task": o.Status, "deleted": o.DeleteOK,
		"unanswered": []string{"whether a Task outlives a restore: assume it does not"}}
	switch {
	case o.PostCode < 200 || o.PostCode > 299 || o.ID == "":
		return worst(fmt.Sprintf("a root process could not create a Task over api.sock (HTTP %d); the keep-alive falls back to the 20 s heartbeat exec", o.PostCode), ev)
	case o.Status == "running":
		return ok("a Task works from a root exec process and the Sprite stayed running through the idle wait (read by GET; see probe 19 for whether GET wakes)", ev)
	case o.Status == "":
		return failed("no status was read after the idle wait", ev)
	}
	return ok(fmt.Sprintf("a Task was created but the Sprite went %s during the idle wait: a Task does NOT hold running; use the 20 s heartbeat exec", o.Status), ev)
}

// ---- 8, 7: API-exposed facts

// parseLimits finds every number under a key containing "limit" or "quota" anywhere in a JSON body.
func parseLimits(raw []byte) map[string]any {
	var v any
	out := map[string]any{}
	if json.Unmarshal(raw, &v) != nil {
		return out
	}
	var walk func(path string, v any)
	walk = func(path string, v any) {
		switch t := v.(type) {
		case map[string]any:
			for k, x := range t {
				p := k
				if path != "" {
					p = path + "." + k
				}
				lk := strings.ToLower(k)
				if _, isNum := x.(float64); isNum && (strings.Contains(lk, "limit") || strings.Contains(lk, "quota")) {
					out[p] = x
				}
				walk(p, x)
			}
		case []any:
			for i, x := range t {
				walk(fmt.Sprintf("%s[%d]", path, i), x)
			}
		}
	}
	walk("", v)
	return out
}

// parseRegions reads a list of region names from a JSON body (an array, or an object holding one
// under "regions", "items" or "data"; elements are strings or objects with a name/id/code).
func parseRegions(body []byte) []string {
	var list []json.RawMessage
	if json.Unmarshal(body, &list) != nil {
		var obj map[string]json.RawMessage
		if json.Unmarshal(body, &obj) != nil {
			return nil
		}
		for _, k := range []string{"regions", "items", "data"} {
			if raw, has := obj[k]; has && json.Unmarshal(raw, &list) == nil {
				break
			}
		}
	}
	var names []string
	for _, raw := range list {
		var s string
		var o struct{ Name, ID, Code string }
		switch {
		case json.Unmarshal(raw, &s) == nil:
		case json.Unmarshal(raw, &o) == nil:
			s = firstNonEmpty(o.Code, o.Name, o.ID)
		}
		if s != "" {
			names = append(names, s)
		}
	}
	sort.Strings(names)
	return names
}
