//go:build linux

package agent

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// RealSlotSys runs the teardown steps of C.5 step 8 as root on the real host.
type RealSlotSys struct {
	Log io.Writer
}

const (
	stateDir = "/var/lib/bitc"
	cgRoot   = "/sys/fs/cgroup"
)

func slotPath(i int, rest ...string) string {
	return filepath.Join(append([]string{stateDir, "slots", strconv.Itoa(i)}, rest...)...)
}

func cgroupSlot(i int) string { return filepath.Join(cgRoot, "bitc", "slot-"+strconv.Itoa(i)) }

func (r RealSlotSys) logf(f string, a ...any) {
	if r.Log != nil {
		fmt.Fprintf(r.Log, f+"\n", a...)
	}
}

// UID is the uid and gid of bitc-s<slot>.
func (RealSlotSys) UID(slot int) (int, int, error) {
	u, err := user.Lookup("bitc-s" + strconv.Itoa(slot))
	if err != nil {
		return 0, 0, err
	}
	uid, gid, err := parseIDs(u.Username, u.Uid, u.Gid)
	return int(uid), int(gid), err
}

// lookupIDs is the uid and gid of the account name, through parseIDs.
func lookupIDs(name string) (uint32, uint32, error) {
	u, err := user.Lookup(name)
	if err != nil {
		return 0, 0, err
	}
	return parseIDs(name, u.Uid, u.Gid)
}

// parseIDs parses an unprivileged account's ids: decimal, 1 to 4294967294 (0 is root, and
// 4294967295 is (uid_t)-1, "no change"). A bad id is an error, never a fallback to 0.
func parseIDs(name, uid, gid string) (uint32, uint32, error) {
	u, e1 := strconv.ParseUint(uid, 10, 32)
	g, e2 := strconv.ParseUint(gid, 10, 32)
	if e1 != nil || e2 != nil || u == 0 || g == 0 || u == 1<<32-1 || g == 1<<32-1 {
		return 0, 0, fmt.Errorf("%s: bad ids %q:%q", name, uid, gid)
	}
	return uint32(u), uint32(g), nil
}

// procUIDs returns the pids with uid as real, effective, saved or fs uid (zombies excluded).
func procUIDs(uid int) (pids []int) {
	ents, _ := os.ReadDir("/proc")
	me := os.Getpid()
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == me {
			continue
		}
		b, err := os.ReadFile("/proc/" + e.Name() + "/status")
		if err != nil {
			continue
		}
		zombie, hit := false, false
		for _, l := range strings.Split(string(b), "\n") {
			if v, ok := strings.CutPrefix(l, "State:"); ok && strings.HasPrefix(strings.TrimSpace(v), "Z") {
				zombie = true
			}
			if v, ok := strings.CutPrefix(l, "Uid:"); ok {
				for _, f := range strings.Fields(v) {
					if n, err := strconv.Atoi(f); err == nil && n == uid {
						hit = true
					}
				}
			}
		}
		if hit && !zombie {
			pids = append(pids, pid)
		}
	}
	return pids
}

// KillSlot kills the slot's cgroup and then every process of the uid until none is left.
func (RealSlotSys) KillSlot(slot, uid int) error {
	cg := cgroupSlot(slot)
	_ = os.WriteFile(filepath.Join(cg, "cgroup.kill"), []byte("1"), 0o200)
	deadline := time.Now().Add(10 * time.Second)
	for {
		pids := procUIDs(uid)
		if len(pids) == 0 {
			break
		}
		for _, p := range pids {
			_ = syscall.Kill(p, syscall.SIGKILL)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%d processes of uid %d survive SIGKILL", len(pids), uid)
		}
		time.Sleep(20 * time.Millisecond)
	}
	for k := 0; k < 50; k++ { // the cgroup empties a moment after its last process is reaped
		if err := unix.Rmdir(cg); err == nil || errors.Is(err, unix.ENOENT) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	return nil
}

// DropNet ends the job's namespace holder and deletes bv<slot>.
func (r RealSlotSys) DropNet(slot int) error {
	if pid, alive := recordedAlive(slotPath(slot), "holder.pid"); alive {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
	_ = os.Remove(slotPath(slot, "holder.pid"))
	_ = agentCmd("ip", "link", "del", "bv"+strconv.Itoa(slot)).Run() // absent is fine
	_ = os.RemoveAll("/run/bitc-stage-" + strconv.Itoa(slot))
	return nil
}

// ClearScheduling removes the uid's crontab and at jobs and its linger file (cron.allow and
// at.allow should make all three impossible; a doctor failure too).
func (r RealSlotSys) ClearScheduling(slot, uid int) error {
	name := "bitc-s" + strconv.Itoa(slot)
	if err := os.Remove("/var/spool/cron/crontabs/" + name); err == nil {
		r.logf("pod: removed the crontab of %s", name)
	}
	for _, d := range []string{"/var/spool/cron/atjobs", "/var/spool/at"} {
		ents, _ := os.ReadDir(d)
		for _, e := range ents {
			if fi, err := e.Info(); err == nil {
				if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) == uid && os.Remove(filepath.Join(d, e.Name())) == nil {
					r.logf("pod: removed the at job %s of %s", e.Name(), name)
				}
			}
		}
	}
	linger := "/var/lib/systemd/linger/" + name
	if _, err := os.Lstat(linger); err == nil {
		if err := os.Remove(linger); err != nil {
			return err
		}
		r.logf("pod: removed linger of %s", name)
		_ = agentCmd("loginctl", "terminate-user", name).Run()
	}
	return nil
}

// RemoveIPC removes the SysV segments, queues and semaphores the uid owns in this namespace.
func (RealSlotSys) RemoveIPC(uid int) error {
	for _, k := range []struct{ file, idcol, flag string }{
		{"/proc/sysvipc/shm", "shmid", "-m"}, {"/proc/sysvipc/msg", "msqid", "-q"}, {"/proc/sysvipc/sem", "semid", "-s"}} {
		b, err := os.ReadFile(k.file)
		if err != nil {
			continue
		}
		lines := strings.Split(string(b), "\n")
		if len(lines) < 2 {
			continue
		}
		col := map[string]int{}
		for i, h := range strings.Fields(lines[0]) {
			col[h] = i
		}
		idc, uc := col[k.idcol], col["uid"]
		for _, l := range lines[1:] {
			f := strings.Fields(l)
			if len(f) <= idc || len(f) <= uc {
				continue
			}
			if n, err := strconv.Atoi(f[uc]); err == nil && n == uid {
				_ = agentCmd("ipcrm", k.flag, f[idc]).Run()
			}
		}
	}
	return nil
}

var localFS = map[string]bool{"ext2": true, "ext3": true, "ext4": true, "xfs": true, "btrfs": true, "f2fs": true, "jfs": true,
	"reiserfs": true, "tmpfs": true, "ramfs": true, "overlay": true, "zfs": true, "vfat": true, "exfat": true}

// localMounts lists the mount points of local filesystems in this namespace.
func localMounts() (mps []string) {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return []string{"/"}
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		left, right, ok := strings.Cut(sc.Text(), " - ")
		lf, rf := strings.Fields(left), strings.Fields(right)
		if !ok || len(lf) < 5 || len(rf) < 1 || !localFS[rf[0]] {
			continue
		}
		mps = append(mps, unescapeMount(lf[4]))
	}
	return mps
}

// RemoveStray deletes every file the uid owns on any local filesystem (xdev) outside the slot
// dirs, and returns how many it removed.
func (RealSlotSys) RemoveStray(slot, uid int) (int, error) {
	n := 0
	skip := filepath.Join(stateDir, "slots")
	for _, mp := range localMounts() {
		if strings.HasPrefix(mp, "/proc") || strings.HasPrefix(mp, "/sys") {
			continue
		}
		var dev uint64
		if fi, err := os.Lstat(mp); err == nil {
			if st, ok := fi.Sys().(*syscall.Stat_t); ok {
				dev = st.Dev
			}
		}
		_ = filepath.WalkDir(mp, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if p == skip || p == "/proc" || p == "/sys" {
				return filepath.SkipDir
			}
			fi, err := d.Info()
			if err != nil {
				return nil
			}
			st, ok := fi.Sys().(*syscall.Stat_t)
			if !ok {
				return nil
			}
			if d.IsDir() && st.Dev != dev {
				return filepath.SkipDir // another filesystem: its own mount point covers it
			}
			if int(st.Uid) == uid {
				if os.RemoveAll(p) == nil { //nolint:gosec // teardown as root removes what the slot user left; the walk does not follow links
					n++
				}
				if d.IsDir() {
					return filepath.SkipDir
				}
			}
			return nil
		})
	}
	return n, nil
}

// UnmountKey unmounts the slot's deploy-key tmpfs if one is mounted.
func (RealSlotSys) UnmountKey(slot int) error {
	dir := slotPath(slot, "key")
	for i := 0; i < 3; i++ {
		if err := unix.Unmount(dir, unix.MNT_DETACH); err != nil {
			break // not a mount point (or already gone)
		}
	}
	_ = os.RemoveAll(dir)
	return nil
}

// Recreate wipes the slot's job dirs and recreates them 0700 owned by the slot user, and removes
// the per-job files the agent leaves in the slot dir.
func (RealSlotSys) Recreate(slot, uid, gid int) error {
	for _, f := range []string{"agent.log", "resolv.conf", "provisioned.json", "holder.pid"} {
		_ = os.Remove(slotPath(slot, f))
	}
	for _, d := range []string{"work", "home", "tmp", "vartmp", "runlock"} {
		p := slotPath(slot, d)
		if err := os.RemoveAll(p); err != nil {
			return err
		}
		if err := os.MkdirAll(p, 0o700); err != nil {
			return err
		}
		if err := os.Chown(p, uid, gid); err != nil {
			return err
		}
		if err := os.Chmod(p, 0o700); err != nil { //nolint:gosec // owner-only
			return err
		}
	}
	return nil
}
