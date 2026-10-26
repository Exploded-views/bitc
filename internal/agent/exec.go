//go:build linux

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// Exec delivery and the exec results path (providers delta X.1), shared by the Sprites and ssh
// providers. The host starts `bitc __agent --boot-stdin` through its provider's exec channel and
// writes one length-prefixed boot payload; later it reads results with `__agent tail`, `read`,
// `list` and `teardown`. There is no listener, no results server and no bearer: authorization is
// the exec channel itself.

// The names `read` and `tail` serve, and the job-id grammar (also enforced by the spec's names).
var (
	execFiles = map[string]bool{"agent.log": true, "out.log": true, "err.log": true, "result.json": true, "artifacts.tar.gz": true, "state.json": true}
	jobIDRE   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
)

// openJob opens the job's directory without following a link at the last component.
func openJob(root, job string) (int, error) {
	if !jobIDRE.MatchString(job) {
		return -1, ErrBadArg
	}
	fd, err := unix.Open(filepath.Join(root, job), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, ErrNotFound
	}
	return fd, nil
}

// openRegular opens name in dirfd without following links; only a regular file is returned.
func openRegular(dirfd int, name string) (*os.File, error) {
	fd, err := unix.Openat(dirfd, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, ErrNotFound
	}
	f := os.NewFile(uintptr(fd), name)
	if st, err := f.Stat(); err != nil || !st.Mode().IsRegular() {
		f.Close()
		return nil, ErrNotFound
	}
	return f, nil
}

// ReadFile writes file of job from offset off to w. Only the result files are served, never a
// .part, a link or anything else (the exec twin of the results server's rules, spec 8.4).
func ReadFile(root, job, file string, off int64, w io.Writer) error {
	if !execFiles[file] || off < 0 {
		return ErrBadArg
	}
	dirfd, err := openJob(root, job)
	if err != nil {
		return err
	}
	defer unix.Close(dirfd)
	f, err := openRegular(dirfd, file)
	if err != nil {
		return err
	}
	defer f.Close()
	if st, err := f.Stat(); err != nil || off > st.Size() {
		return ErrOffset
	}
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return err
	}
	_, err = io.Copy(w, f)
	return err
}

// TailOptions: Offsets are the host's resume points by file name (agent.log, out.log, err.log);
// Heartbeat is the interval of the alive frames, Poll how often the files are re-read.
type TailOptions struct {
	Offsets         map[string]int64
	Heartbeat, Poll time.Duration
}

// startGrace is how long a missing agent.pid still counts as alive: the host opens the tail as
// soon as the agent is started, a moment before it writes its identity.
const startGrace = 10 * time.Second

// Tail streams agent.log, out.log and err.log of job from the given offsets, with heartbeats,
// until result.json exists, the agent has exited (it does so right after its EXIT marker) and everything is sent (then Done), or the agent is gone without a
// result (a heartbeat with alive=false, then Done).
func Tail(ctx context.Context, root, job string, o TailOptions, w io.Writer) error {
	dirfd, err := openJob(root, job)
	if err != nil {
		return err
	}
	defer unix.Close(dirfd)
	if o.Poll <= 0 {
		o.Poll = 200 * time.Millisecond
	}
	if o.Heartbeat <= 0 {
		o.Heartbeat = 15 * time.Second
	}
	offs := map[string]int64{"agent.log": 0, "out.log": 0, "err.log": 0}
	for k, v := range o.Offsets {
		if _, ok := offs[k]; !ok || v < 0 {
			return ErrBadArg
		}
		offs[k] = v
	}
	enc := json.NewEncoder(w)
	started, lastHB := time.Now(), time.Time{}
	for {
		// The agent commits result.json, then writes state and its EXIT marker, then exits. Done
		// needs the result and the agent gone, and the snapshot comes before the drain: whatever
		// a dead agent wrote is then in the files the drain reads.
		_, resErr := os.Stat(filepath.Join(root, job, "result.json"))
		finished := resErr == nil
		agentUp := agentAlive(root, job)
		sent := false
		for _, name := range []string{"agent.log", "out.log", "err.log"} {
			n, err := tailFile(dirfd, name, offs[name], enc)
			if err != nil {
				return err
			}
			offs[name] += n
			sent = sent || n > 0
		}
		if finished && !agentUp && !sent {
			return enc.Encode(Frame{Done: true})
		}
		alive := agentUp || (!pidRecorded(root, job) && time.Since(started) < startGrace)
		if !alive && !finished && !sent {
			f := false
			if err := enc.Encode(Frame{HB: true, Alive: &f}); err != nil {
				return err
			}
			return enc.Encode(Frame{Done: true})
		}
		if time.Since(lastHB) >= o.Heartbeat {
			lastHB = time.Now()
			if err := enc.Encode(Frame{HB: true, Alive: &alive}); err != nil {
				return err
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(o.Poll):
		}
	}
}

// tailFile sends what name holds past off (the committed file, or its .part while the job runs)
// in frames of at most 64 KiB and returns the byte count.
func tailFile(dirfd int, name string, off int64, enc *json.Encoder) (int64, error) {
	f, err := openRegular(dirfd, name)
	if err != nil && name != "agent.log" {
		f, err = openRegular(dirfd, name+".part")
	}
	if err != nil {
		return 0, nil
	}
	defer f.Close()
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return 0, err
	}
	var sent int64
	buf := make([]byte, 64<<10)
	for {
		n, err := f.Read(buf)
		if n > 0 {
			if e := enc.Encode(Frame{File: name, Off: off + sent, Data: buf[:n]}); e != nil {
				return sent, e
			}
			sent += int64(n)
		}
		if err != nil || n == 0 {
			return sent, nil
		}
	}
}

// procStart is the start time (clock ticks since boot) of pid, field 22 of /proc/<pid>/stat; with
// the pid it identifies a process even if the pid is later reused.
func procStart(pid int) (string, bool) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return "", false
	}
	i := strings.LastIndexByte(string(b), ')')
	if i < 0 {
		return "", false
	}
	f := strings.Fields(string(b[i+1:]))
	if len(f) < 20 {
		return "", false
	}
	return f[19], true
}

// identity formats "<pid> <start time>", what agent.pid and job.pid hold.
func identity(pid int) string {
	s, _ := procStart(pid)
	return fmt.Sprintf("%d %s\n", pid, s)
}

// recordedAlive reports whether the process named in file (inside dir) is still that process.
func recordedAlive(dir, file string) (pid int, alive bool) {
	b, err := os.ReadFile(filepath.Join(dir, file))
	if err != nil {
		return 0, false
	}
	f := strings.Fields(string(b))
	if len(f) != 2 {
		return 0, false
	}
	pid, err = strconv.Atoi(f[0])
	if err != nil || pid <= 1 {
		return 0, false
	}
	start, ok := procStart(pid)
	return pid, ok && start == f[1]
}

func agentAlive(root, job string) bool {
	_, alive := recordedAlive(filepath.Join(root, job), "agent.pid")
	return alive
}

func pidRecorded(root, job string) bool {
	_, err := os.Stat(filepath.Join(root, job, "agent.pid"))
	return err == nil
}

// List prints one JSON line per job directory: its job id, phase (from state.json), whether the
// agent is alive and the exit code once there is one.
func List(root string, w io.Writer) error {
	ents, err := os.ReadDir(root)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	sort.Slice(ents, func(i, j int) bool { return ents[i].Name() < ents[j].Name() })
	enc := json.NewEncoder(w)
	for _, e := range ents {
		if !jobIDRE.MatchString(e.Name()) || !e.IsDir() {
			continue
		}
		line := struct {
			Job   string `json:"job"`
			Phase string `json:"phase"`
			Alive bool   `json:"alive"`
			Exit  *int   `json:"exit,omitempty"`
		}{Job: e.Name(), Alive: agentAlive(root, e.Name())}
		var st struct {
			Phase string `json:"phase"`
			Exit  *int   `json:"exit"`
		}
		var b strings.Builder
		if ReadFile(root, e.Name(), "state.json", 0, &b) == nil && json.Unmarshal([]byte(b.String()), &st) == nil {
			line.Phase, line.Exit = st.Phase, st.Exit
		}
		if err := enc.Encode(line); err != nil {
			return err
		}
	}
	return nil
}

// Teardown kills the job's process group and the agent, each only if its recorded identity
// (pid and start time) still matches, then removes the job's directory. It prints `torn <job>`,
// or `gone` when there was no such job.
func Teardown(root, job string, w io.Writer) error {
	if !jobIDRE.MatchString(job) {
		return ErrBadArg
	}
	dir := filepath.Join(root, job)
	if st, err := os.Lstat(dir); err != nil || !st.IsDir() {
		_, err := fmt.Fprintln(w, "gone")
		return err
	}
	if pid, alive := recordedAlive(dir, "job.pid"); alive && pid != os.Getpid() {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
	if pid, alive := recordedAlive(dir, "agent.pid"); alive && pid != os.Getpid() {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
	var err error
	for i := 0; i < 5; i++ { // a dying process may still be writing
		if err = os.RemoveAll(dir); err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "torn %s\n", job)
	return err
}

var offsetRE = regexp.MustCompile(`^[0-9]{1,18}$`)

func parseOffset(s string) (int64, bool) {
	if !offsetRE.MatchString(s) {
		return 0, false
	}
	n, err := strconv.ParseInt(s, 10, 64)
	return n, err == nil
}

// isExecVerb reports whether args name the exec results path.
func isExecVerb(args []string) bool {
	if len(args) == 0 {
		return false
	}
	switch args[0] {
	case "--root", "read", "tail", "list", "teardown":
		return true
	}
	return false
}

// ExecVerb runs `__agent [--root DIR] read|tail|list|teardown ...` and returns its exit code:
// 64 for a bad argument (nothing on stdout), 65 for an offset past the end, 66 when the job or
// file is not there, 69 for an error of the agent's own.
func ExecVerb(args []string, stdout io.Writer, stderr io.Writer) int {
	root := "/run/bitc"
	if len(args) >= 2 && args[0] == "--root" {
		root, args = args[1], args[2:]
		if !filepath.IsAbs(root) {
			fmt.Fprintln(stderr, "bitc agent: --root must be an absolute path")
			return 64
		}
	}
	bad := func(why string) int { fmt.Fprintln(stderr, "bitc agent: "+why); return 64 }
	if len(args) == 0 {
		return bad("no verb")
	}
	var err error
	switch args[0] {
	case "read":
		if len(args) != 4 {
			return bad("usage: read <job> <file> <offset>")
		}
		off, ok := parseOffset(args[3])
		if !ok {
			return bad("offset must be a decimal number")
		}
		err = ReadFile(root, args[1], args[2], off, stdout)
	case "tail":
		if len(args) != 5 {
			return bad("usage: tail <job> <off_agent> <off_out> <off_err>")
		}
		o := TailOptions{Offsets: map[string]int64{}}
		for i, name := range []string{"agent.log", "out.log", "err.log"} {
			off, ok := parseOffset(args[2+i])
			if !ok {
				return bad("offsets must be decimal numbers")
			}
			o.Offsets[name] = off
		}
		err = Tail(context.Background(), root, args[1], o, stdout)
	case "list":
		if len(args) != 1 {
			return bad("usage: list")
		}
		err = List(root, stdout)
	case "teardown":
		if len(args) != 2 {
			return bad("usage: teardown <job>")
		}
		err = Teardown(root, args[1], stdout)
	default:
		return bad("unknown verb " + args[0])
	}
	switch {
	case err == nil:
		return 0
	case errors.Is(err, ErrBadArg):
		return bad("bad job id or file name")
	case errors.Is(err, ErrOffset):
		fmt.Fprintln(stderr, "bitc agent: offset beyond the end of the file")
		return 65
	case errors.Is(err, ErrNotFound):
		fmt.Fprintln(stderr, "bitc agent: not found")
		return 66
	}
	fmt.Fprintln(stderr, "bitc agent: "+err.Error())
	return 69
}
