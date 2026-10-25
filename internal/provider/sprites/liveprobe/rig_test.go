package liveprobe

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Exploded-views/bitc/internal/provider/sprites"
	"github.com/Exploded-views/bitc/internal/provider/sprites/spritesfake"
)

// vclock is a virtual clock: only Sleep moves it, so Sprite-seconds are exact.
type vclock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *vclock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *vclock) Sleep(ctx context.Context, d time.Duration) error {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
	return ctx.Err()
}

type rig struct {
	f     *spritesfake.Fake
	c     *sprites.Client
	cfg   Config
	clock *vclock
	log   *bytes.Buffer
}

func newRig(t *testing.T, exec func(spritesfake.ExecContext) int) *rig {
	t.Helper()
	r := &rig{clock: &vclock{t: time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)}, log: &bytes.Buffer{}}
	r.f = spritesfake.New(t, spritesfake.Options{Exec: exec})
	c, err := sprites.New(sprites.Options{APIBase: r.f.URL(), Org: r.f.Org(), Token: r.f.Token(), Timeout: 5 * time.Second, HandshakeTimeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	r.c = c
	r.cfg = Config{Org: r.f.Org(), RunID: "t1", MaxUSD: DefaultMaxUSD, PricePerHour: DefaultPricePerHour, MaxMinutes: DefaultMaxMinutes,
		Now: r.clock.Now, Sleep: r.clock.Sleep, Log: r.log}
	return r
}

func (r *rig) runner(probes ...Probe) *Runner { return NewRunner(r.cfg, r.c, probes) }

func (r *rig) creates() int {
	n := 0
	for _, c := range r.f.Calls() {
		if c.Method == "POST" && c.Path == "/v1/sprites" {
			n++
		}
	}
	return n
}

func result(t *testing.T, rep Report, id int) Result {
	t.Helper()
	for _, r := range rep.Results {
		if r.ID == id {
			return r
		}
	}
	t.Fatalf("no result for item %d in %+v", id, rep.Results)
	return Result{}
}

func quick(id int, group string, worst time.Duration, run func(context.Context, *Env) Outcome) Probe {
	return Probe{ID: id, Title: "t", Group: group, Worst: worst, Run: run}
}

func fine(context.Context, *Env) Outcome { return ok("fine", nil) }

func tokenFile(t *testing.T, mode os.FileMode, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(p, []byte(content+"\n"), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	return p
}

// scripted answers the probes' shell commands the way a healthy Sprite would.
func scripted(fp **spritesfake.Fake) func(spritesfake.ExecContext) int {
	return func(x spritesfake.ExecContext) int {
		script := x.Cmd[len(x.Cmd)-1]
		say := func(s string) int { _, _ = io.WriteString(x.Stdout, s); return 0 }
		drain := func() { _, _ = io.Copy(io.Discard, x.Stdin) }
		has := func(subs ...string) bool {
			for _, s := range subs {
				if !strings.Contains(script, s) {
					return false
				}
			}
			return true
		}
		switch {
		case x.Cmd[0] == "cat":
			b, err := os.ReadFile(filepath.Join(x.Overlay, "var/tmp/bitcproof-200m"))
			if err != nil {
				return 1
			}
			_, _ = x.Stdout.Write(b)
			return 0
		case script == "uname -m":
			return say("x86_64\n")
		case has("echo uid=$(id -u)"):
			return say("uid=1000\nuser=sprite\nsudo=yes\n")
		case has(`print "run=`):
			return say("run=tmpfs\ntmp=overlay\nrundir=yes\n")
		case has("cat > /tmp/bitcproof-nobody.sh"):
			drain()
			return 0
		case has("setpriv --reuid"):
			return say("srw-rw-rw- 1 root root 0 /.sprite/api.sock\nGET /v1/tasks 200\nGET /v1/services 403\nGET /v1/checkpoints 000\n")
		case has("for p in /v1/checkpoints /v1/checkpoint"):
			return say("GET /v1/checkpoints 200\nGET /v1/checkpoint 404\n")
		case has("unshare -m"):
			return say("mounted\nrc=0\n")
		case has("owner=%U"):
			return say("owner=root\nmode=640\nbootid=aaaaaaaa-bbbb\n")
		case has("%U:%G %a"):
			return say("sprite:sprite 644\n")
		case has("kernel/random/boot_id"):
			return say("aaaaaaaa-bbbb\n")
		case has("ssh-keyscan -T"):
			f := *fp
			net, _ := f.Policies(x.Sprite)
			open := func(d string) bool { return strings.Contains(net, `"`+d+`"`) }
			code := func(d string) string {
				if open(d) {
					return "404"
				}
				return "000"
			}
			ssh := "0"
			if open("github.com") {
				ssh = "1"
			}
			return say("tool curl yes\ntool ssh-keyscan yes\nhttps codeload.github.com " + code("codeload.github.com") + "\nhttps objects.githubusercontent.com " + code("objects.githubusercontent.com") +
				"\nhttps registry.npmjs.org " + code("registry.npmjs.org") + "\nssh github.com " + ssh + "\n")
		case has("alloc_rc"):
			return 137
		case has("nproc="):
			return say("nproc=2\nmemtotal_kb=16777216\ncgroup_max=max\n")
		case has("mkdir -p /run/bitcproof"):
			drain()
			return say("setup=ok\n")
		case has("overlay_hits"):
			drain()
			return say("overlay_hits=1\ntmpfs_hits=0\n")
		case has("bitcproof-200m", "head -c"):
			if err := os.MkdirAll(filepath.Join(x.Overlay, "var/tmp"), 0o755); err != nil {
				return 1
			}
			if err := os.WriteFile(filepath.Join(x.Overlay, "var/tmp/bitcproof-200m"), bytes.Repeat([]byte("z"), int(throughputBytes)), 0o644); err != nil {
				return 1
			}
			return say(strconv.FormatInt(throughputBytes, 10) + "\n")
		case has("-X DELETE"):
			return say("204")
		case has(`"expire":"5m"`):
			return say("{\"id\":\"t1\"}\ncode=201\n")
		case has("http://sprite/v1/tasks"):
			return say("200")
		case has("getent hosts"):
			return say("getent_rc=2\n")
		}
		return 0
	}
}
