package liveprobe

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/Exploded-views/bitc/internal/provider/sprites"
)

// Groups name the Sprite a probe shares. The cheap read-only probes share one.
const (
	gNone = ""
	gRO   = "ro"
	gCkpt = "ckpt"
	gNet  = "net"
	gRes  = "res"
	gThru = "thru"
	gDNS  = "dns"
	gRet  = "ret"
	gWake = "wake"
	gTask = "tasks"
)

// Groups is how each group's Sprite is made.
var Groups = map[string]GroupSpec{
	gRO: {MemMB: 2048}, gCkpt: {Domains: []string{"registry.npmjs.org"}, MemMB: 2048}, gNet: {MemMB: 2048}, gRes: {MemMB: 1024},
	gThru: {MemMB: 2048}, gDNS: {MemMB: 1024}, gRet: {MemMB: 1024}, gWake: {MemMB: 1024}, gTask: {MemMB: 1024},
}

func secs(n int) time.Duration { return time.Duration(n) * time.Second }

// AllProbes is every item of section 9. The default plan runs the phase-1 items first, then the
// warm-reuse items (Warm).
func AllProbes() []Probe {
	return []Probe{
		{ID: 1, Title: "checkpoints over REST; api.sock", Group: gCkpt, Order: 1, Worst: secs(90), Warm: true, Run: probeCheckpointsREST},
		{ID: 2, Title: "policies survive a restore", Group: gCkpt, Order: 2, Worst: secs(90), Run: probePolicySurvive},
		{ID: 3, Title: "egress hosts for GitHub and npm", Group: gNet, Order: 1, Worst: secs(150), Run: probeEgress},
		{ID: 4, Title: "privileges policy", Group: gRO, Order: 8, Worst: secs(60), Warm: true, Run: probePrivileges},
		{ID: 5, Title: "tmpfs and auto checkpoints", Group: gCkpt, Order: 3, Worst: secs(90), Run: probeTmpfsCheckpoints},
		{ID: 6, Title: "memory and CPU limits", Group: gRes, Order: 1, Worst: secs(240), Run: probeResources},
		{ID: 7, Title: "regions", Group: gNone, Worst: secs(20), Warm: true, Run: probeRegions},
		{ID: 8, Title: "concurrency and running limits", Group: gNone, Worst: secs(20), Run: probeLimits},
		{ID: 9, Title: "Connectors (list only)", Group: gNone, Worst: secs(20), Warm: true, Run: probeConnectors},
		{ID: 10, Title: "scoped tokens", Group: gNone, Worst: secs(1), Run: assumeWorst("no API exists to check; assume there are no scoped tokens, so the org token has full power over the org")},
		{ID: 11, Title: "organization endpoint and field", Group: gRO, Order: 6, Worst: secs(20), Run: probeOrganization},
		{ID: 12, Title: "POST /exec response body", Group: gNone, Worst: secs(1), Run: assumeWorst("unused: bitc uses WSS exec only; nothing to probe")},
		{ID: 13, Title: "Sprite name length, charset and labels", Group: gNone, Own: true, Worst: secs(120), Run: probeNames},
		{ID: 14, Title: "fs/read and fs/write", Group: gRO, Order: 7, Worst: secs(60), Warm: true, Run: probeFS},
		{ID: 15, Title: "exec user, sudo, latency", Group: gRO, Order: 2, Worst: secs(60), Run: probeExecUser},
		{ID: 16, Title: "200 MiB read: WSS exec and fs/read", Group: gThru, Order: 1, Worst: secs(330), Run: probeThroughput},
		{ID: 17, Title: "Tasks API keep-alive", Group: gTask, Order: 1, Worst: secs(150), Warm: true, Run: probeTasks},
		{ID: 18, Title: "billing of a warm Sprite", Group: gNone, Worst: secs(1), Warm: true, Run: assumeWorst("no API shows billing; settled by the invoice in S.5; assume a warm Sprite is billed for hot storage")},
		{ID: 19, Title: "what wakes a Sprite", Group: gWake, Order: 1, Worst: secs(120), Warm: true, Run: probeWake},
		{ID: 20, Title: "unshare and tmpfs over /.sprite", Group: gRO, Order: 5, Worst: secs(30), Warm: true, Run: probeUnshare},
		{ID: 21, Title: "/run is a tmpfs", Group: gRO, Order: 3, Worst: secs(20), Warm: true, Run: probeRunFS},
		{ID: 22, Title: "DNS tunnelling canary", Group: gDNS, Order: 1, Worst: secs(60), Skip: skipNoCanary, Run: probeDNS},
		{ID: 23, Title: "api.sock for a non-root user", Group: gRO, Order: 4, Worst: secs(30), Run: probeSock},
		{ID: 24, Title: "Sprite CPU architecture", Group: gRO, Order: 1, Worst: secs(15), Run: probeArch},
		{ID: 25, Title: "checkpoint retention after delete", Group: gRet, Order: 1, Worst: secs(150), Run: probeRetention},
	}
}

func skipNoCanary(c Config) string {
	if c.DNSCanary == "" {
		return "assume-worst: no --dns-canary-domain (an owner-run domain), so tunnelling through refused names is assumed possible"
	}
	return ""
}

func assumeWorst(why string) func(context.Context, *Env) Outcome {
	return func(context.Context, *Env) Outcome { return worst(why, nil) }
}

func errText(err error) string { return oneLine(err.Error(), 120) }

func execErr(what string, err error) Outcome {
	return failed(what+": "+errText(err), map[string]any{"error": errText(err)})
}

// ---- 24

func probeArch(ctx context.Context, e *Env) Outcome {
	r, err := e.Sh(ctx, "uname -m", nil)
	if err != nil {
		return execErr("uname -m", err)
	}
	return parseArch(string(r.Stdout))
}

// ---- 15

func probeExecUser(ctx context.Context, e *Env) Outcome {
	r, err := e.Sh(ctx, `echo uid=$(id -u); echo user=$(id -un); if sudo -n true >/dev/null 2>&1; then echo sudo=yes; else echo sudo=no; fi`, nil)
	if err != nil {
		return execErr("id", err)
	}
	o := parseExecUser(string(r.Stdout))
	var total, mx time.Duration
	const n = 5
	for i := 0; i < n; i++ {
		t0 := e.Now()
		_, err := e.Sh(ctx, "true", nil)
		d := e.Now().Sub(t0)
		if err != nil && strings.Contains(err.Error(), "429") {
			o.RateLimited = true
		}
		total += d
		mx = max(mx, d)
	}
	o.LatAvg, o.LatMax = total/n, mx
	_, err = e.API.Run(ctx, e.Sprite, sprites.ExecRequest{Cmd: []string{"true"}, MaxRunAfterDisconnect: 30 * time.Second}, nil)
	o.MaxRunAfterAccepted = err == nil
	if err != nil {
		o.MaxRunErr = errText(err)
	}
	return summarizeExec(o)
}

// ---- 21

func probeRunFS(ctx context.Context, e *Env) Outcome {
	r, err := e.Sh(ctx, `awk '$2=="/run"{print "run=" $3}' /proc/mounts | tail -1; awk '$2=="/tmp"{print "tmp=" $3}' /proc/mounts | tail -1; [ -d /run ] && echo rundir=yes`, nil)
	if err != nil {
		return execErr("read /proc/mounts", err)
	}
	return parseRunFS(string(r.Stdout))
}

// ---- 23

const sockScript = `command -v curl >/dev/null 2>&1 || { echo curl=missing; exit 0; }
ls -l /.sprite/api.sock 2>&1
for p in /v1/tasks /v1/services /v1/sprites /v1/checkpoints; do
  c=$(curl -s -o /dev/null -m 5 --unix-socket /.sprite/api.sock -w '%{http_code}' "http://sprite$p" 2>/dev/null)
  echo "GET $p ${c:-000}"
done
`

func probeSock(ctx context.Context, e *Env) Outcome {
	r, err := e.Nobody(ctx, sockScript)
	if err != nil {
		return execErr("api.sock as uid 65534", err)
	}
	return summarizeSock(out(r))
}

// ---- 20

func probeUnshare(ctx context.Context, e *Env) Outcome {
	r, err := e.Root(ctx, `timeout 20 unshare -m -p -f --mount-proc sh -c 'mount -t tmpfs tmpfs /.sprite && echo mounted' 2>&1; echo rc=$?`, nil)
	if err != nil {
		return execErr("unshare", err)
	}
	return summarizeUnshare(out(r))
}

// ---- 11

func probeOrganization(ctx context.Context, e *Env) Outcome {
	ev := map[string]any{}
	o1, err1 := e.API.Organization(ctx)
	o2, err2 := e.API.Organization(ctx)
	if err1 != nil || err2 != nil {
		ev["organization_error"] = errText(errors.Join(err1, err2))
		return worst("GET /v1/organization is not usable ("+errText(errors.Join(err1, err2))+"); rely on the Sprite responses' organization field only", ev)
	}
	stable := string(o1.Raw) == string(o2.Raw)
	ev["slug"], ev["stable_between_two_reads"] = o1.Slug, stable
	code, b := e.API.Raw(ctx, "GET", "/v1/sprites/"+e.Sprite, nil, nil)
	getHas := code == 200 && orgFieldPresent(b)
	lcode, lb := e.API.Raw(ctx, "GET", "/v1/sprites", map[string][]string{"prefix": {NamePrefix}}, nil)
	listHas := lcode == 200 && orgFieldPresent(lb)
	ev["get_has_organization"], ev["list_has_organization"] = getHas, listHas
	return ok(fmt.Sprintf("GET /v1/organization answers (slug %q, equal on two reads: %v); organization is present on GET: %v, on every list item: %v",
		o1.Slug, stable, getHas, listHas), ev)
}

// ---- 14

func probeFS(ctx context.Context, e *Env) Outcome {
	var o fsObs
	if r, err := e.Sh(ctx, `stat -c 'owner=%U' /etc/shadow; stat -c 'mode=%a' /etc/shadow; echo bootid=$(cat /proc/sys/kernel/random/boot_id)`, nil); err == nil {
		kv := parseKV(string(r.Stdout))
		o.ShadowOwner, o.ShadowMode, o.BootID = kv["owner"], kv["mode"], kv["bootid"]
	}
	var sink bytes.Buffer
	if _, err := e.API.FSRead(ctx, e.Sprite, "/etc/shadow", &sink); err != nil {
		o.ShadowReadErr = errText(err)
	}
	sink.Reset()
	n, err := e.API.FSRead(ctx, e.Sprite, "/proc/self/status", &sink)
	o.ProcReadBytes = n
	if err != nil {
		o.ProcReadErr = errText(err)
	}
	if err := e.API.FSWrite(ctx, e.Sprite, "/var/tmp/bitcproof-14.txt", []byte("bitcproof")); err != nil {
		o.WriteErr = errText(err)
	} else if r, err := e.Sh(ctx, `stat -c '%U:%G %a' /var/tmp/bitcproof-14.txt`, nil); err == nil {
		o.WriterOwner = strings.TrimSpace(string(r.Stdout))
	}
	return summarizeFS(o)
}

// ---- 4

func probePrivileges(ctx context.Context, e *Env) Outcome {
	o := privObs{Attempts: map[string]int{}}
	o.GetCode, _ = e.rawJSON(ctx, "GET", "/v1/sprites/"+e.Sprite+"/policy/privileges", nil)
	for _, key := range []string{"profile", "mode"} {
		for _, val := range []string{"noNewPrivileges", "minimal"} {
			code, _ := e.rawJSON(ctx, "POST", "/v1/sprites/"+e.Sprite+"/policy/privileges", map[string]string{key: val})
			o.Attempts[key+"="+val] = code
			if code >= 200 && code < 300 && o.Applied == "" {
				o.Applied = key + "=" + val
			}
		}
		if o.Applied != "" {
			break
		}
	}
	if o.Applied != "" {
		if r, err := e.Sh(ctx, `if sudo -n true >/dev/null 2>&1; then echo yes; else echo no; fi`, nil); err == nil {
			o.Sudo = strings.TrimSpace(string(r.Stdout))
		}
		if r, err := e.Sh(ctx, `ls -l /.sprite/api.sock 2>&1 | head -1; echo; ls /.sprite/checkpoints 2>&1 | head -3`, nil); err == nil {
			parts := strings.SplitN(string(r.Stdout), "\n\n", 2)
			o.Sock = oneLine(parts[0], 100)
			if len(parts) > 1 {
				o.Ckpt = oneLine(parts[1], 100)
			}
		}
	}
	return summarizePrivileges(o)
}

// ---- 3

const netScript = `for t in curl ssh-keyscan; do if command -v $t >/dev/null 2>&1; then echo "tool $t yes"; else echo "tool $t no"; fi; done
for h in codeload.github.com objects.githubusercontent.com registry.npmjs.org; do
  c=$(curl -s -o /dev/null -m 10 -w '%{http_code}' "https://$h/" 2>/dev/null)
  echo "https $h ${c:-000}"
done
n=$(ssh-keyscan -T 8 -t ed25519 github.com 2>/dev/null | wc -l)
echo "ssh github.com $n"
`

func probeEgress(ctx context.Context, e *Env) Outcome {
	var phases []netPhase
	for i, allow := range [][]string{nil, {"github.com"}, {"github.com", "codeload.github.com", "objects.githubusercontent.com", "registry.npmjs.org"}} {
		if i > 0 {
			p, err := sprites.AllowDomains(allow...)
			if err != nil {
				return execErr("policy", err)
			}
			if err := e.API.ApplyJobPolicy(ctx, e.Sprite, p); err != nil {
				return execErr("set network policy", err)
			}
		}
		r, err := e.Sh(ctx, netScript, nil)
		if err != nil {
			return execErr("egress test", err)
		}
		phases = append(phases, netPhase{Allowed: allow, Obs: parseNetOut(string(r.Stdout))})
	}
	return summarizeNet(phases)
}

// ---- 6

const resScript = `echo nproc=$(nproc); awk '/^MemTotal/{print "memtotal_kb=" $2}' /proc/meminfo
echo cgroup_max=$(cat /sys/fs/cgroup/memory.max 2>/dev/null || cat /sys/fs/cgroup/memory/memory.limit_in_bytes 2>/dev/null)`

func allocScript(mb int) string {
	return fmt.Sprintf(`mkdir -p /mnt/bitcproof-alloc && mount -t tmpfs -o size=%dm tmpfs /mnt/bitcproof-alloc && timeout 100 head -c %d /dev/zero > /mnt/bitcproof-alloc/f; echo alloc_rc=$?; rm -f /mnt/bitcproof-alloc/f`, mb, int64(mb)<<20)
}

func probeResources(ctx context.Context, e *Env) Outcome {
	o := resPolicyObs{LimitMB: 1024}
	pol := "/v1/sprites/" + e.Sprite + "/policy/resources"
	body := func(mb int) map[string]any {
		return map[string]any{"memory": map[string]any{"limit_mb": mb, "autoscale": false}}
	}
	o.SetCode, _ = e.rawJSON(ctx, "POST", pol, body(o.LimitMB))
	_, o.GetBody = e.rawJSON(ctx, "GET", pol, nil)
	if r, err := e.Sh(ctx, resScript, nil); err == nil {
		o.Before = parseResources(string(r.Stdout))
	}
	// never more than limit + 256 MiB, and never more than 17 GiB
	o.AllocMB = min(o.LimitMB+256, 17*1024)
	r, err := e.Root(ctx, allocScript(o.AllocMB), nil)
	o.Alloc = allocVerdict(string(r.Stdout), err)
	o.CPUSetCode, o.CPUSetBody = e.rawJSON(ctx, "PUT", "/v1/sprites/"+e.Sprite, map[string]any{"config": map[string]any{"ram_mb": 2048, "cpus": 2}})
	if r, err := e.Sh(ctx, resScript, nil); err == nil {
		o.After = parseResources(string(r.Stdout))
	}
	o.Big, _ = e.rawJSON(ctx, "POST", pol, body(16384))
	if r, err := e.Sh(ctx, resScript, nil); err == nil {
		o.BigMemTotalKB = parseResources(string(r.Stdout)).MemTotalKB
	}
	return summarizeResources(o)
}

// ---- 16

var throughputBytes int64 = 200 << 20

type countWriter struct{ n int64 }

func (c *countWriter) Write(p []byte) (int, error) { c.n += int64(len(p)); return len(p), nil }

func probeThroughput(ctx context.Context, e *Env) Outcome {
	r, err := e.Sh(ctx, fmt.Sprintf(`timeout 100 head -c %d /dev/urandom > /var/tmp/bitcproof-200m && stat -c %%s /var/tmp/bitcproof-200m`, throughputBytes), nil)
	if err != nil || strings.TrimSpace(string(r.Stdout)) != fmt.Sprint(throughputBytes) {
		return failed("could not create the 200 MiB file", map[string]any{"stdout": oneLine(string(r.Stdout), 60), "error": fmt.Sprint(err)})
	}
	var viaExec, viaFS transfer
	{
		cctx, cancel := context.WithTimeout(ctx, secs(100))
		t0 := e.Now()
		cw := &countWriter{}
		s, err := e.API.Exec(cctx, e.Sprite, sprites.ExecRequest{Cmd: []string{"cat", "/var/tmp/bitcproof-200m"}})
		if err == nil {
			_, cerr := io.Copy(cw, s.Stdout())
			_, werr := s.Wait()
			_ = s.Close()
			err = errors.Join(cerr, werr)
		}
		viaExec = transfer{Bytes: cw.n, Dur: e.Now().Sub(t0)}
		if err != nil {
			viaExec.Err = errText(err)
		}
		cancel()
	}
	{
		cctx, cancel := context.WithTimeout(ctx, secs(100))
		t0 := e.Now()
		cw := &countWriter{}
		_, err := e.API.FSRead(cctx, e.Sprite, "/var/tmp/bitcproof-200m", cw)
		viaFS = transfer{Bytes: cw.n, Dur: e.Now().Sub(t0)}
		if err != nil {
			viaFS.Err = errText(err)
		}
		cancel()
	}
	return summarizeTransfer(throughputBytes, viaExec, viaFS)
}

// ---- 13

func probeNames(ctx context.Context, e *Env) Outcome {
	base := func() string { return e.NextName() }
	pad := func(n int) string {
		s := base() + "-"
		for len(s) < n {
			s += "x"
		}
		return s[:n]
	}
	type try struct {
		label  string
		name   string
		labels map[string]string
		want   bool
	}
	good := map[string]string{"bitc": "1"}
	tries := []try{
		{"uppercase", base() + "-Bad", nil, false},
		{"underscore", base() + "-a_b", nil, false},
		{"dot", base() + "-a.b", nil, false},
		{"64 chars", pad(64), nil, false},
		{"label value of 300 chars", pad(30), map[string]string{"bitc": strings.Repeat("v", 300)}, false},
		{"63 chars", pad(63), good, true},
	}
	var res []nameTry
	labelRead := map[string]string{}
	updates := map[string]int{}
	for _, t := range tries {
		code, _ := e.TryCreate(ctx, t.name, t.labels)
		created := code >= 200 && code < 300
		res = append(res, nameTry{Label: t.label, Len: len(t.name), WantOK: t.want, Code: code, Created: created})
		if created || code == 0 || code >= 500 {
			if t.want && created {
				if s, err := e.API.GetSprite(ctx, t.name); err == nil {
					for k, v := range s.Labels {
						labelRead[k] = v
					}
				}
				for _, m := range []string{"PUT", "PATCH"} {
					c, _ := e.rawJSON(ctx, m, "/v1/sprites/"+t.name, map[string]any{"labels": map[string]string{"bitc": "2"}})
					updates[m] = c
				}
			}
			if err := e.API.DeleteSprite(ctx, t.name); err != nil {
				return failed("a Sprite made by a name try could not be deleted: "+errText(err), nil)
			}
		}
	}
	return summarizeNames(res, labelRead, updates)
}

// ---- 22

func probeDNS(ctx context.Context, e *Env) Outcome {
	sub := fmt.Sprintf("bp-%s-%s.%s", e.Cfg.RunID, e.ShortMarker(), e.Cfg.DNSCanary)
	r, err := e.Sh(ctx, fmt.Sprintf(`getent hosts %s; echo getent_rc=$?`, sub), nil)
	if err != nil {
		return execErr("dns query", err)
	}
	fmt.Fprintf(e.Cfg.Log, "probe 22: queried %s; check the authoritative server's log for it\n", sub)
	return worst("queried "+sub+" from inside a Sprite under an empty allow policy; OWNER CHECK PENDING: if that name appears in the authoritative server's log, DNS tunnelling is possible (assume it is until checked)",
		map[string]any{"queried": sub, "result": oneLine(out(r), 120)})
}

// ---- 1, 2, 5

const markerSetup = `read -r O; read -r T
mkdir -p /run/bitcproof /var/tmp/bitcproof
if [ "$(awk '$2=="/run"{print $3}' /proc/mounts | tail -1)" != tmpfs ]; then mount -t tmpfs tmpfs /run/bitcproof || { echo setup=mount-failed; exit 0; }; fi
printf %s "$O" > /var/tmp/bitcproof/o; printf %s "$T" > /run/bitcproof/t; sync
echo setup=ok`

// ensureMarkers writes the overlay marker and the tmpfs marker (once per Sprite).
func ensureMarkers(ctx context.Context, e *Env) bool {
	if v, has := e.State["markers"].(bool); has {
		return v
	}
	r, err := e.Root(ctx, markerSetup, []byte(e.Marker+"-o\n"+e.Marker+"-t\n"))
	good := err == nil && strings.Contains(string(r.Stdout), "setup=ok")
	e.State["markers"] = good
	return good
}

// ensureCheckpoint takes the group's first manual checkpoint (once per Sprite).
func ensureCheckpoint(ctx context.Context, e *Env) (string, error) {
	if id, has := e.State["ckpt"].(string); has {
		return id, nil
	}
	if e.State["ckpt_err"] != nil {
		return "", e.State["ckpt_err"].(error)
	}
	ensureMarkers(ctx, e)
	cp, err := e.API.CreateCheckpoint(ctx, e.Sprite, NamePrefix+e.Cfg.RunID+"-ck")
	if err != nil {
		e.State["ckpt_err"] = err
		return "", err
	}
	e.State["ckpt"] = cp.ID
	return cp.ID, nil
}

func countAutos(cps []sprites.Checkpoint) int {
	n := 0
	for _, c := range cps {
		if c.Auto {
			n++
		}
	}
	return n
}

func probeTmpfsCheckpoints(ctx context.Context, e *Env) Outcome {
	setup := ensureMarkers(ctx, e)
	_, cerr := ensureCheckpoint(ctx, e)
	cps, _ := e.API.ListCheckpoints(ctx, e.Sprite)
	var s ckptSearch
	if cerr == nil {
		r, err := e.Root(ctx, `read -r O; read -r T
d=/.sprite/checkpoints
[ -d "$d" ] || { echo ckptdir=missing; exit 0; }
echo "overlay_hits=$(timeout 60 grep -rlF -- "$O" "$d" 2>/dev/null | wc -l)"
echo "tmpfs_hits=$(timeout 60 grep -rlF -- "$T" "$d" 2>/dev/null | wc -l)"`, []byte(e.Marker+"-o\n"+e.Marker+"-t\n"))
		if err == nil {
			s = parseCkptSearch(string(r.Stdout))
		}
	} else {
		return worst("no checkpoint could be taken ("+errText(cerr)+"); assume tmpfs content is checkpointed", map[string]any{"error": errText(cerr)})
	}
	return summarizeCkptSearch(s, len(cps), countAutos(cps), setup)
}

func probePolicySurvive(ctx context.Context, e *Env) Outcome {
	var o surviveObs
	id, err := ensureCheckpoint(ctx, e)
	if err != nil {
		o.CreateErr = errText(err)
		return summarizePolicySurvive(o)
	}
	net, res := "/v1/sprites/"+e.Sprite+"/policy/network", "/v1/sprites/"+e.Sprite+"/policy/resources"
	bootID := func() string {
		if r, err := e.Sh(ctx, "cat /proc/sys/kernel/random/boot_id", nil); err == nil {
			return strings.TrimSpace(string(r.Stdout))
		}
		return ""
	}
	_, o.NetBefore = e.rawJSONFull(ctx, net)
	_, o.ResBefore = e.rawJSONFull(ctx, res)
	o.BootBefore = bootID()
	t0 := e.Now()
	if err := e.API.RestoreCheckpoint(ctx, e.Sprite, id); err != nil {
		o.RestoreErr = errText(err)
		return summarizePolicySurvive(o)
	}
	o.RestoreDur = e.Now().Sub(t0)
	_, o.NetAfter = e.rawJSONFull(ctx, net)
	_, o.ResAfter = e.rawJSONFull(ctx, res)
	o.BootAfter = bootID()
	return summarizePolicySurvive(o)
}

// rawJSONFull is a GET whose whole body is kept (a policy is small).
func (e *Env) rawJSONFull(ctx context.Context, path string) (int, string) {
	code, b := e.API.Raw(ctx, "GET", path, nil, nil)
	return code, string(b)
}

func probeCheckpointsREST(ctx context.Context, e *Env) Outcome {
	var o ckptREST
	ensureMarkers(ctx, e)
	cp, err := e.API.CreateCheckpoint(ctx, e.Sprite, NamePrefix+e.Cfg.RunID+"-c1")
	if err != nil {
		o.CreateErr = errText(err)
		return summarizeCheckpointsREST(o)
	}
	o.ID = cp.ID
	cps, _ := e.API.ListCheckpoints(ctx, e.Sprite)
	o.Listed, o.Autos = len(cps), countAutos(cps)
	if err := e.API.DeleteCheckpoint(ctx, e.Sprite, cp.ID); err != nil {
		o.DeleteErr = errText(err)
	}
	after, _ := e.API.ListCheckpoints(ctx, e.Sprite)
	o.GoneAfter = true
	for _, c := range after {
		if c.ID == cp.ID {
			o.GoneAfter = false
		}
	}
	// from inside: can code reach checkpoints through api.sock? GET only, as root and as uid 65534
	script := `command -v curl >/dev/null 2>&1 || { echo curl=missing; exit 0; }
for p in /v1/checkpoints /v1/checkpoint; do
  c=$(curl -s -o /dev/null -m 5 --unix-socket /.sprite/api.sock -w '%{http_code}' "http://sprite$p" 2>/dev/null)
  echo "GET $p ${c:-000}"
done`
	if r, err := e.Root(ctx, script, nil); err == nil {
		o.SockRoot = parseCodes(string(r.Stdout))
	}
	if r, err := e.Nobody(ctx, script); err == nil {
		o.SockNobody = parseCodes(string(r.Stdout))
	}
	return summarizeCheckpointsREST(o)
}

// ---- 25

func probeRetention(ctx context.Context, e *Env) Outcome {
	var o retObs
	if !ensureMarkers(ctx, e) {
		return worst("markers could not be placed; assume checkpoints survive a delete", nil)
	}
	name := e.Sprite
	cname := NamePrefix + e.Cfg.RunID + "-keep"
	cp, err := e.API.CreateCheckpoint(ctx, name, cname)
	if err != nil {
		return worst("a checkpoint could not be created ("+errText(err)+"); assume checkpoints survive a delete", map[string]any{"error": errText(err)})
	}
	o.IDs = []string{cp.ID}
	if cps, err := e.API.ListCheckpoints(ctx, name); err == nil {
		for _, c := range cps {
			if c.ID != cp.ID {
				o.IDs = append(o.IDs, c.ID)
			}
		}
	}
	if err := e.DeleteSelf(ctx); err != nil {
		o.DeleteErr = errText(err)
		return summarizeRetention(o)
	}
	o.GetCodes, o.Restore = map[string]int{}, map[string]int{}
	list, lerr := e.API.ListCheckpoints(ctx, name)
	o.ListCode = 404
	if lerr == nil {
		o.ListCode = 200
		for _, c := range list {
			o.ListIDs = append(o.ListIDs, c.ID)
		}
	} else if !errors.Is(lerr, sprites.ErrNotFound) {
		o.ListCode = 0
	}
	for _, id := range append(append([]string{}, o.IDs...), cname) {
		_, gerr := e.API.GetCheckpoint(ctx, name, id)
		o.GetCodes[id] = codeOf(gerr)
		rerr := e.API.RestoreCheckpoint(ctx, name, id)
		o.Restore[id] = codeOf(rerr)
	}
	if left, err := e.API.ListSprites(ctx, name); err == nil {
		for _, s := range left {
			if s.Name == name {
				o.Recreated = true
				_ = e.API.DeleteSprite(ctx, name)
			}
		}
	}
	return summarizeRetention(o)
}

// codeOf is an HTTP-like code for an API error: 200 for nil, 404 for not found, 0 otherwise.
func codeOf(err error) int {
	switch {
	case err == nil:
		return 200
	case errors.Is(err, sprites.ErrNotFound):
		return 404
	}
	return 0
}

// ---- 19

func probeWake(ctx context.Context, e *Env) Outcome {
	var w wakeObs
	if err := e.Sleep(ctx, secs(45)); err != nil {
		return failed("interrupted", nil)
	}
	status := func() string {
		s, err := e.API.GetSprite(ctx, e.Sprite)
		if err != nil {
			return ""
		}
		return s.Status
	}
	w.Idle = status()
	if w.Idle == "" || w.Idle == "running" {
		return summarizeWake(w)
	}
	if w.GetAgain = status(); w.GetAgain == "running" {
		return summarizeWake(w)
	}
	_, _ = e.API.ListSprites(ctx, NamePrefix)
	w.AfterList = status()
	if w.AfterList == "running" {
		return summarizeWake(w)
	}
	_, _ = e.API.FSRead(ctx, e.Sprite, "/etc/hostname", io.Discard)
	w.AfterFS = status()
	return summarizeWake(w)
}

// ---- 17

func probeTasks(ctx context.Context, e *Env) Outcome {
	var o taskObs
	create := `command -v curl >/dev/null 2>&1 || { echo curl=missing; exit 0; }
curl -s -m 5 --unix-socket /.sprite/api.sock -H 'Content-Type: application/json' -d '{"expire":"5m"}' -w '\ncode=%{http_code}\n' http://sprite/v1/tasks`
	r, err := e.Root(ctx, create, nil)
	if err != nil {
		return execErr("create a Task", err)
	}
	body, _, _ := strings.Cut(string(r.Stdout), "\ncode=")
	o.PostCode = int(parseKVInt(parseKV(string(r.Stdout))["code"]))
	o.ID = parseTaskID(body)
	if o.ID == "" {
		return summarizeTasks(o)
	}
	if lr, err := e.Root(ctx, `curl -s -o /dev/null -m 5 --unix-socket /.sprite/api.sock -w '%{http_code}' http://sprite/v1/tasks`, nil); err == nil {
		o.ListCode = int(parseKVInt(strings.TrimSpace(string(lr.Stdout))))
	}
	_ = e.Sleep(ctx, secs(50))
	if s, err := e.API.GetSprite(ctx, e.Sprite); err == nil {
		o.Status = s.Status
	}
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), secs(20))
	defer cancel()
	if dr, err := e.Root(dctx, fmt.Sprintf(`curl -s -o /dev/null -m 5 -X DELETE --unix-socket /.sprite/api.sock -w '%%{http_code}' http://sprite/v1/tasks/%s`, shellSafe(o.ID)), nil); err == nil {
		o.DeleteOK = strings.HasPrefix(strings.TrimSpace(string(dr.Stdout)), "2")
	}
	return summarizeTasks(o)
}

func parseKVInt(s string) int64 {
	var n int64
	for _, r := range strings.TrimSpace(s) {
		if r < '0' || r > '9' {
			return 0
		}
		n = n*10 + int64(r-'0')
	}
	return n
}

// shellSafe keeps only [A-Za-z0-9._-] of an id the Sprite sent back, so it cannot become syntax.
func shellSafe(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-' {
			return r
		}
		return -1
	}, s)
}

// ---- 7, 8, 9

func probeRegions(ctx context.Context, e *Env) Outcome {
	code, b := e.API.Raw(ctx, "GET", "/v1/regions", nil, nil)
	names := parseRegions(b)
	ev := map[string]any{"status": code, "regions": names}
	if code < 200 || code > 299 || len(names) == 0 {
		return worst(fmt.Sprintf("no region list from GET /v1/regions (HTTP %d); assume the placement is not pinnable", code), ev)
	}
	return ok(fmt.Sprintf("GET /v1/regions lists %d regions: %s", len(names), strings.Join(names, ", ")), ev)
}

func probeLimits(ctx context.Context, e *Env) Outcome {
	o, err := e.API.Organization(ctx)
	if err != nil {
		return worst("GET /v1/organization is not usable ("+errText(err)+"); the limits are unknown", map[string]any{"error": errText(err)})
	}
	lim := parseLimits(o.Raw)
	if len(lim) == 0 {
		return worst("the organization answer exposes no limit fields; assume a low running_limit and warm_limit, and that concurrent_sprite_limit_exceeded is a 403 or 429", map[string]any{"limits": lim})
	}
	return ok(fmt.Sprintf("the organization answer exposes limits: %v (the HTTP status of concurrent_sprite_limit_exceeded is not shown by it)", lim), map[string]any{"limits": lim})
}

func probeConnectors(ctx context.Context, e *Env) Outcome {
	names, err := e.API.ListConnections(ctx)
	if err != nil {
		return worst("connectors could not be listed ("+errText(err)+"); whether a Connector can serve git fetch stays unknown (list only, none is ever created)", map[string]any{"error": errText(err)})
	}
	return ok(fmt.Sprintf("%d Connectors in the org %v (list only; none created, so git fetch through one is not tested)", len(names), names), map[string]any{"connectors": names})
}
