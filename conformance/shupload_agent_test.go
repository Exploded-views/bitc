package conformance

// SH36 and SH37: the root side and stage 2 of the ssh upload transport, driven in-process on a
// fake slot tree with injected statfs, clock, ownership and race seams. Nothing here touches a
// real /proc, /sys, cgroup or user.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Exploded-views/bitc/internal/agent"
)

const upNonce = "0123456789abcdef0123456789abcdef"

var upNow = time.Unix(1_800_000_000, 0)

// upSys is a SlotSys that records its calls and wipes nothing (so a leftover .bitc-upload is
// removed only by the explicit cleanup under test).
type upSys struct {
	mu    sync.Mutex
	root  string
	calls []string
}

func (s *upSys) rec(f string, a ...any) {
	s.mu.Lock()
	s.calls = append(s.calls, fmt.Sprintf(f, a...))
	s.mu.Unlock()
}
func (s *upSys) UID(slot int) (int, int, error)    { return 2000 + slot, 3000 + slot, nil }
func (s *upSys) KillSlot(slot, uid int) error      { s.rec("kill %d uid %d", slot, uid); return nil }
func (s *upSys) DropNet(int) error                 { return nil }
func (s *upSys) ClearScheduling(int, int) error    { return nil }
func (s *upSys) RemoveIPC(int) error               { return nil }
func (s *upSys) RemoveStray(int, int) (int, error) { return 0, nil }
func (s *upSys) UnmountKey(int) error              { return nil }
func (s *upSys) Recreate(slot, _, _ int) error {
	return os.MkdirAll(filepath.Join(s.root, "var/lib/bitc/slots", fmt.Sprint(slot), "work"), 0o700)
}

type upRig struct {
	t       *testing.T
	root    string
	host    *agent.SlotHost
	sys     *upSys
	env     agent.UploadEnv
	work    string
	outside string
	chowns  [][3]int
	started []agent.BootPayload
	keys    []string
	startEr error
	avail   uint64
}

func newUpRig(t *testing.T) *upRig {
	t.Helper()
	root := t.TempDir()
	r := &upRig{t: t, root: root, sys: &upSys{root: root}, outside: t.TempDir(), avail: 1 << 40}
	r.host = &agent.SlotHost{Root: root, Conf: agent.HostConf{Slots: 1, Netns: true}, Now: func() time.Time { return upNow }, Sys: r.sys}
	r.work = filepath.Join(root, "var/lib/bitc/slots/1/work")
	r.env = agent.UploadEnv{
		Now:      func() time.Time { return upNow },
		Statfs:   func(string) (uint64, uint64, error) { return r.avail, 1 << 30, nil },
		Fchown:   func(_, uid, gid int) error { r.chowns = append(r.chowns, [3]int{0, uid, gid}); return nil },
		OwnerUID: os.Getuid(),
	}
	if err := r.host.Prepare([]int{1}, "job-a", upNonce, "", time.Hour); err != nil {
		t.Fatal(err)
	}
	r.sys.calls = nil
	return r
}

func (r *upRig) upload() string { return filepath.Join(r.work, ".bitc-upload") }

func (r *upRig) noUpload() {
	r.t.Helper()
	if _, err := os.Lstat(r.upload()); err == nil {
		r.t.Errorf("%s is still there after the refusal", r.upload())
	}
}

func upSpec(job string) agent.Spec {
	return agent.Spec{Job: job, Pool: "heavy", Argv: []string{"true"}, Ref: "refs/bitc/upload/" + job, Sha: strings.Repeat("a", 40),
		Timeout: agent.Duration(time.Minute), Port: -1, Nonce: "n0nce", Env: []string{}}
}

func upHash(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

// frame is the boot payload frame for code (nil: no upload) and deadline.
func (r *upRig) frame(code *agent.CodeUpload, deadline time.Time) []byte {
	r.t.Helper()
	enc, err := upSpec("j1").Encode()
	if err != nil {
		r.t.Fatal(err)
	}
	f, err := agent.BootPayload{Spec: enc, Deadline: deadline.Unix(), DeployKey: "", Code: code}.Frame()
	if err != nil {
		r.t.Fatal(err)
	}
	return f
}

func goodCode(b []byte) *agent.CodeUpload {
	return &agent.CodeUpload{Kind: "upload", Len: int64(len(b)), SHA256: upHash(b), Ref: "refs/bitc/upload/j1"}
}

// countReader counts the bytes read from r.
type countReader struct {
	r io.Reader
	n atomic.Int64
}

func (c *countReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n.Add(int64(n))
	return n, err
}

// run drives `slot-boot --run` and returns the exit code and what stdout got.
func (r *upRig) run(stdin io.Reader) (int, string) {
	r.t.Helper()
	var out bytes.Buffer
	env := agent.SlotRunEnv{Host: r.host, Upload: r.env, Start: func(p agent.BootPayload, key string) error {
		r.started = append(r.started, p)
		r.keys = append(r.keys, key)
		return r.startEr
	}}
	done := make(chan int, 1)
	go func() { done <- agent.RunSlot(env, []int{1}, "job-a", upNonce, stdin, &out) }()
	select {
	case code := <-done:
		return code, out.String()
	case <-time.After(20 * time.Second):
		r.t.Fatal("RunSlot did not return")
		return 0, ""
	}
}

func (r *upRig) refused(stdin io.Reader, want string) {
	r.t.Helper()
	code, out := r.run(stdin)
	if code != 65 || !strings.HasPrefix(out, "refused: ") || !strings.Contains(out, want) {
		r.t.Errorf("exit %d, stdout %q; want 65 and a refusal naming %q", code, out, want)
	}
	if len(r.started) != 0 {
		r.t.Errorf("stage 2 was started after the refusal")
	}
	r.noUpload()
}

func TestSH36_PayloadIsValidatedBeforeAByteIsRead(t *testing.T) {
	body := []byte("bundle-bytes")
	good := *goodCode(body)
	for _, c := range []struct {
		name string
		edit func(c *agent.CodeUpload)
		want string
	}{
		{"kind", func(c *agent.CodeUpload) { c.Kind = "git-push" }, "bad code kind"},
		{"zero length", func(c *agent.CodeUpload) { c.Len = 0 }, "bad code length"},
		{"negative length", func(c *agent.CodeUpload) { c.Len = -1 }, "bad code length"},
		{"over the cap", func(c *agent.CodeUpload) { c.Len = 1<<30 + 1 }, "bad code length"},
		{"short sha", func(c *agent.CodeUpload) { c.SHA256 = c.SHA256[:63] }, "bad code sha256"},
		{"upper case sha", func(c *agent.CodeUpload) { c.SHA256 = strings.ToUpper(c.SHA256) }, "bad code sha256"},
		{"other job's ref", func(c *agent.CodeUpload) { c.Ref = "refs/bitc/upload/j2" }, "bad code ref"},
		{"other namespace", func(c *agent.CodeUpload) { c.Ref = "refs/heads/main" }, "bad code ref"},
		{"empty ref", func(c *agent.CodeUpload) { c.Ref = "" }, "bad code ref"},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newUpRig(t)
			cu := good
			c.edit(&cu)
			frame := r.frame(&cu, upNow.Add(time.Hour))
			in := &countReader{r: io.MultiReader(bytes.NewReader(frame), bytes.NewReader(body), bytes.NewReader([]byte("MORE")))}
			r.refused(in, c.want)
			if n := in.n.Load(); n > int64(len(frame)) {
				t.Errorf("%d bytes read, the frame is %d: a byte past the frame was read before the refusal", n, len(frame))
			}
			for _, call := range r.sys.calls {
				if strings.HasPrefix(call, "kill") {
					t.Errorf("a refused payload killed the slot: %v", r.sys.calls)
				}
			}
		})
	}
}

func TestSH36_HostConfMaxBundleIsOptionalAndBoundsTheCap(t *testing.T) {
	base := "slots=1\nresolver=9.9.9.9\ngateway=10.0.0.1\n"
	c, err := agent.ParseHostConf([]byte(base))
	if err != nil || c.BundleCap() != 1<<30 {
		t.Errorf("without max_bundle: cap %d, err %v; want 1 GiB and no error", c.BundleCap(), err)
	}
	c, err = agent.ParseHostConf([]byte(base + "max_bundle=2GiB\n"))
	if err != nil || c.BundleCap() != 2<<30 {
		t.Errorf("max_bundle=2GiB: cap %d, err %v; want 2 GiB", c.BundleCap(), err)
	}
	for _, bad := range []string{"max_bundle=9GiB", "max_bundle=0", "max_bundle=lots"} {
		if _, err := agent.ParseHostConf([]byte(base + bad + "\n")); err == nil || !strings.Contains(err.Error(), "max_bundle") {
			t.Errorf("%s: error %v, want one naming max_bundle", bad, err)
		}
	}
	// the cap the payload check uses is the host's
	r := newUpRig(t)
	r.host.Conf.MaxBundle = 10
	body := []byte("0123456789A")
	r.refused(io.MultiReader(bytes.NewReader(r.frame(goodCode(body), upNow.Add(time.Hour))), bytes.NewReader(body)), "bad code length")
}

func TestSH36_InsufficientSpaceIsRefusedBeforeAnythingIsWritten(t *testing.T) {
	body := bytes.Repeat([]byte("x"), 1000)
	r := newUpRig(t)
	r.avail = 3*1000 + 1<<30 - 1
	var asked string
	r.env.Statfs = func(p string) (uint64, uint64, error) { asked = p; return r.avail, 1 << 30, nil }
	r.refused(io.MultiReader(bytes.NewReader(r.frame(goodCode(body), upNow.Add(time.Hour))), bytes.NewReader(body)), "insufficient disk for bundle")
	if asked != r.work {
		t.Errorf("statfs asked about %q, want the slot's work dir %q", asked, r.work)
	}
	// exactly 3 x len + 1 GiB is enough
	r2 := newUpRig(t)
	r2.avail = 3*1000 + 1<<30
	if code, out := r2.run(io.MultiReader(bytes.NewReader(r2.frame(goodCode(body), upNow.Add(time.Hour))), bytes.NewReader(body))); code != 0 {
		t.Errorf("exact space: exit %d, stdout %q; want 0", code, out)
	}
}

func TestSH36_StallsHitTheIdleTimeoutAndTheOverallDeadline(t *testing.T) {
	body := bytes.Repeat([]byte("y"), 64)
	type tc struct {
		name     string
		idle     time.Duration
		overall  time.Duration
		deadline time.Duration // payload deadline, from the upload clock
		drip     time.Duration // 0: write half and stall
	}
	for _, c := range []tc{
		{"idle", 150 * time.Millisecond, time.Hour, time.Hour, 0},
		{"overall", time.Hour, 250 * time.Millisecond, time.Hour, 20 * time.Millisecond},
		{"payload deadline", time.Hour, time.Hour, 300 * time.Millisecond, 20 * time.Millisecond},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newUpRig(t)
			r.env.Idle, r.env.Overall = c.idle, c.overall
			pr, pw := io.Pipe()
			defer pw.Close()
			go func() {
				if c.drip == 0 {
					_, _ = pw.Write(body[:32])
					return
				}
				for _, b := range body[:63] { // never the last byte
					if _, err := pw.Write([]byte{b}); err != nil {
						return
					}
					time.Sleep(c.drip)
				}
			}()
			// the payload deadline has whole seconds: put the upload clock just before it
			r.env.Now = func() time.Time { return upNow.Add(time.Hour - c.deadline) }
			start := time.Now()
			r.refused(io.MultiReader(bytes.NewReader(r.frame(goodCode(body), upNow.Add(time.Hour))), pr), "truncated payload")
			if time.Since(start) > 5*time.Second {
				t.Errorf("the refusal took %v", time.Since(start))
			}
		})
	}
}

func TestSH36_ShortStreamAndHashMismatchAreRefused(t *testing.T) {
	body := []byte("a perfectly fine bundle, honest")
	r := newUpRig(t)
	r.refused(io.MultiReader(bytes.NewReader(r.frame(goodCode(body), upNow.Add(time.Hour))), bytes.NewReader(body[:len(body)-3])), "truncated payload")
	r = newUpRig(t)
	bad := append([]byte(nil), body...)
	bad[0] ^= 1
	r.refused(io.MultiReader(bytes.NewReader(r.frame(goodCode(body), upNow.Add(time.Hour))), bytes.NewReader(bad)), "bundle hash mismatch")
}

func TestSH36_StaleAndHostileEntriesNeverRedirectTheWrite(t *testing.T) {
	body := []byte("fresh bundle bytes")
	victim := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(victim, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, plant := range map[string]func(r *upRig){
		"symlink at the dir": func(r *upRig) {
			if err := os.Symlink(r.outside, r.upload()); err != nil {
				t.Fatal(err)
			}
		},
		"regular file at the dir": func(r *upRig) {
			if err := os.WriteFile(r.upload(), []byte("junk"), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"stale dir with a bundle symlink": func(r *upRig) {
			if err := os.MkdirAll(filepath.Join(r.upload(), "deep", "er"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(victim, filepath.Join(r.upload(), "code.bundle")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(r.outside, filepath.Join(r.upload(), "deep", "er", "link")); err != nil {
				t.Fatal(err)
			}
		},
		"stale bundle file": func(r *upRig) {
			if err := os.MkdirAll(r.upload(), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(r.upload(), "code.bundle"), []byte("old"), 0o600); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := newUpRig(t)
			if err := os.WriteFile(filepath.Join(r.outside, "precious"), []byte("keep"), 0o600); err != nil {
				t.Fatal(err)
			}
			plant(r)
			code, out := r.run(io.MultiReader(bytes.NewReader(r.frame(goodCode(body), upNow.Add(time.Hour))), bytes.NewReader(body)))
			if code != 0 {
				t.Fatalf("exit %d, stdout %q", code, out)
			}
			if got, _ := os.ReadFile(filepath.Join(r.upload(), "code.bundle")); !bytes.Equal(got, body) {
				t.Errorf("code.bundle = %q, want the streamed bytes", got)
			}
			if st, err := os.Lstat(r.upload()); err != nil || !st.IsDir() {
				t.Errorf(".bitc-upload is not a real directory: %v", err)
			}
			if got, _ := os.ReadFile(victim); string(got) != "keep" {
				t.Errorf("the file behind a planted link was written: %q", got)
			}
			ents, _ := os.ReadDir(r.outside)
			if len(ents) != 1 || ents[0].Name() != "precious" {
				t.Errorf("the dir behind a planted link changed: %v", ents)
			}
		})
	}
}

func TestSH36_DirSwappedBetweenMkdirAndOpenIsRaced(t *testing.T) {
	body := []byte("bundle")
	r := newUpRig(t)
	r.env.Raced = func() {
		_ = os.Remove(r.upload())
		if err := os.Symlink(r.outside, r.upload()); err != nil {
			t.Error(err)
		}
	}
	r.refused(io.MultiReader(bytes.NewReader(r.frame(goodCode(body), upNow.Add(time.Hour))), bytes.NewReader(body)), "upload dir raced")
	if ents, _ := os.ReadDir(r.outside); len(ents) != 0 {
		t.Errorf("something was written through the swapped link: %v", ents)
	}
	// swapped for a real directory of someone else: the inode check catches it. The made dir is
	// renamed aside, not removed: a removed inode can be reused at once (tmpfs, overlayfs), and a
	// new dir with the same uid, dev and ino is indistinguishable by design.
	r = newUpRig(t)
	r.env.Raced = func() {
		if err := os.Rename(r.upload(), r.upload()+".aside"); err != nil {
			t.Error(err)
		}
		if err := os.Mkdir(r.upload(), 0o750); err != nil {
			t.Error(err)
		}
	}
	r.refused(io.MultiReader(bytes.NewReader(r.frame(goodCode(body), upNow.Add(time.Hour))), bytes.NewReader(body)), "upload dir raced")
}

func TestSH36_HappyPathModesOwnersAndNoBytesPastLen(t *testing.T) {
	body := bytes.Repeat([]byte("z"), 100_000)
	r := newUpRig(t)
	in := &countReader{r: io.MultiReader(bytes.NewReader(r.frame(goodCode(body), upNow.Add(time.Hour))), bytes.NewReader(body), bytes.NewReader([]byte("EXTRA")))}
	frame := len(r.frame(goodCode(body), upNow.Add(time.Hour)))
	code, out := r.run(in)
	if code != 0 || out != "started job-a\n" {
		t.Fatalf("exit %d, stdout %q; want 0 and the started line", code, out)
	}
	if n := in.n.Load(); n != int64(frame+len(body)) {
		t.Errorf("%d bytes read, want the frame and exactly len (%d)", n, frame+len(body))
	}
	for p, want := range map[string]os.FileMode{r.upload(): 0o750, filepath.Join(r.upload(), "code.bundle"): 0o640} {
		st, err := os.Lstat(p)
		if err != nil || st.Mode().Perm() != want {
			t.Errorf("%s: mode %v, err %v; want %v", p, st, err, want)
		}
	}
	if got, _ := os.ReadFile(filepath.Join(r.upload(), "code.bundle")); !bytes.Equal(got, body) {
		t.Errorf("code.bundle differs from the streamed bytes")
	}
	if len(r.chowns) != 2 || r.chowns[0][1] != r.env.OwnerUID || r.chowns[0][2] != 3001 || r.chowns[1][1] != r.env.OwnerUID || r.chowns[1][2] != 3001 {
		t.Errorf("fchown calls %v, want the dir and the bundle to root:bitc-s1 (gid 3001)", r.chowns)
	}
	if len(r.sys.calls) == 0 || r.sys.calls[0] != "kill 1 uid 2001" {
		t.Errorf("slot calls %v, want the slot killed before the stream", r.sys.calls)
	}
	if len(r.started) != 1 || r.started[0].Code == nil || r.started[0].DeployKey != "" || r.keys[0] != "" {
		t.Errorf("stage 2 start: %+v keys %q", r.started, r.keys)
	}
	if _, err := os.Stat(filepath.Join(r.root, "var/lib/bitc/upload.lock")); err != nil {
		t.Errorf("the host-wide upload.lock was never taken: %v", err)
	}
	if o, _ := r.host.Owners(); o[1] == nil || o[1].Job != "j1" || o[1].LeaseUntil != upNow.Add(time.Hour).Unix() {
		t.Errorf("owner after the handoff: %+v", o[1])
	}
}

func TestSH36_Stage2StartFailureRemovesTheUpload(t *testing.T) {
	body := []byte("bundle")
	r := newUpRig(t)
	r.startEr = fmt.Errorf("fork failed")
	code, out := r.run(io.MultiReader(bytes.NewReader(r.frame(goodCode(body), upNow.Add(time.Hour))), bytes.NewReader(body)))
	if code != 69 || strings.Contains(out, "started") {
		t.Errorf("exit %d, stdout %q; want 69 and no started line", code, out)
	}
	r.noUpload()
}

func TestSH36_SlotCleanAndTeardownRemoveAStaleUpload(t *testing.T) {
	r := newUpRig(t)
	plant := func() {
		if err := os.MkdirAll(filepath.Join(r.upload(), "sub"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(r.upload(), "code.bundle"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	r.host.Now = func() time.Time { return upNow.Add(2 * time.Hour) }
	plant()
	if err := r.host.Prepare([]int{1}, "job-b", strings.Repeat("f", 32), "", time.Hour); err != nil {
		t.Fatal(err)
	}
	r.noUpload()
	plant()
	if got, err := r.host.Teardown([]int{1}, "job-b"); err != nil || got != "torn" {
		t.Fatalf("teardown: %q, %v", got, err)
	}
	r.noUpload()
}
