package conformance

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Exploded-views/bitc/conformance/harness"
)

// memHost: heavy (memory) and lint (no memory), floor 6GiB, fake meminfo with availMiB.
func memHost(t *testing.T, h *harness.H, availMiB int, extra string) string {
	t.Helper()
	return memHostFloor(t, h, availMiB, `"6GiB"`, extra)
}

// memHostFloor is memHost with the memory_floor value given as TOML text.
func memHostFloor(t *testing.T, h *harness.H, availMiB int, floor, extra string) string {
	t.Helper()
	root := filepath.Join(h.Root, "fakeroot")
	setMem(h, root, availMiB)
	h.Seam("procfs_root = " + harness.Q(root))
	h.HostConfig("groups = { bigmem = { units = 1 } }\n[host]\nmemory_floor = " + floor + "\n" + extra +
		"[pools.heavy]\nmemory = \"8GiB\"\ngroups = { bigmem = 1 }\nwait = \"4s\"\n" +
		"[pools.coverage]\nmemory = \"6GiB\"\ngroups = { bigmem = 1 }\nwait = \"4s\"\n" +
		"[pools.lint]\nwait = \"4s\"\n")
	return root
}

func setMem(h *harness.H, root string, availMiB int) {
	h.Write(filepath.Join(root, "proc", "meminfo"), "MemTotal: 33554432 kB\nMemAvailable: "+strconv.Itoa(availMiB*1024)+" kB\n", 0o600)
}

func TestF1_FloorAdmitsAtExactlyTheFloor(t *testing.T) {
	h := harness.New(t)
	root := memHost(t, h, 6144, "")
	if r := h.Run("run", "heavy", "--", "true"); r.Code != 0 {
		t.Fatalf("exactly the floor refused: %+v", r)
	}
	setMem(h, root, 6143)
	for _, p := range []string{"heavy", "coverage"} {
		if r := h.Run("run", "--wait", "0s", p, "--", "echo", "ran"); r.Code != 75 || r.Stdout != "" {
			t.Fatalf("%s under the floor: %+v", p, r)
		}
	}
}

func TestF2_WaitsHoldingNothingAndStarvesWithTheMemoryReason(t *testing.T) {
	h := harness.New(t)
	root := memHost(t, h, 4096, "")
	w := h.Start("run", "heavy", "--", "true")
	time.Sleep(600 * time.Millisecond)
	if lockHeld(filepath.Join(h.State, "pools", "heavy", "slot.1.lock")) || lockHeld(filepath.Join(h.State, "groups", "bigmem", "unit.1.lock")) {
		t.Fatal("a memory waiter holds locks")
	}
	setMem(h, root, 9000) // recovers while waiting: admitted
	if r := w.Wait(); r.Code != 0 {
		t.Fatalf("%+v", r)
	}
	setMem(h, root, 4096)
	if r := h.Run("run", "--wait", "0s", "heavy", "--", "true"); r.Stderr != "STARVED heavy (memory: 4096 MiB available, floor 6144 MiB)\n" {
		t.Fatalf("%+v", r)
	}
}

func TestF3_FloorFromHostOnlyZeroDisablesEmptyIsError(t *testing.T) {
	h := harness.New(t)
	memHost(t, h, 100, "")
	if r := h.RunWith([]string{"BITC_MEMORY_FLOOR=0GiB"}, "run", "--wait", "0s", "heavy", "--", "true"); r.Code != 75 {
		t.Fatalf("env changed the floor: %+v", r)
	}
	h2 := harness.New(t)
	memHostFloor(t, h2, 100, `"0GiB"`, "")
	if r := h2.Run("run", "heavy", "--", "true"); r.Code != 0 {
		t.Fatalf("0GiB did not disable: %+v", r)
	}
	h3 := harness.New(t)
	memHostFloor(t, h3, 100, `""`, "")
	if r := h3.Run("run", "heavy", "--", "true"); r.Code != 78 {
		t.Fatalf("empty floor: %+v", r)
	}
}

func TestF4_UnreadableMeminfoAdmitsWithOneWarning(t *testing.T) {
	h := harness.New(t)
	root := memHost(t, h, 4096, "") // slot and unit free: every poll reaches the memory step
	if r := h.Run("run", "--wait", "0s", "heavy", "--", "true"); !strings.Contains(r.Stderr, "(memory: ") {
		t.Fatalf("the memory step was not reached: %+v", r)
	}
	w := h.Start("run", "heavy", "--", "true")
	h.Until(func() bool { return len(tickets(h, "heavy")) == 1 }, 10*time.Second)
	time.Sleep(500 * time.Millisecond) // several 100 ms polls fail on memory, readable: no warning
	if w.Exited() {
		t.Fatal("admitted under the floor")
	}
	_ = removeFile(filepath.Join(root, "proc", "meminfo"))
	r := w.Wait()
	if r.Code != 0 || strings.Count(r.Stderr, "cannot read MemAvailable") != 1 {
		t.Fatalf("%+v", r)
	}
	if r := h.Run("run", "lint", "--", "true"); strings.Contains(r.Stderr, "MemAvailable") {
		t.Fatal("a pool without memory warned")
	}
}

func TestF5_NoMemoryPoolsAndNestedCallsSkipTheFloor(t *testing.T) {
	h := harness.New(t)
	root := memHost(t, h, 100, "")
	if r := h.Run("run", "lint", "--", "true"); r.Code != 0 {
		t.Fatalf("%+v", r)
	}
	setMem(h, root, 9000)
	script := `echo "MemAvailable: 1024 kB" > "$2/proc/meminfo"; "$1" run coverage -- echo nested-ok`
	r := h.Run("run", "heavy", "--", "sh", "-c", script, "_", harness.Bin, root)
	if r.Code != 0 || !strings.Contains(r.Stdout, "nested-ok") {
		t.Fatalf("%+v", r)
	}
}

// The waiter first fails on memory (its last step); then memory recovers while a pool without
// memory takes the shared unit, so later attempts fail earlier, on the group. The final reason
// must be the last attempt's, never the stale memory one.
func TestT2_ReasonRecomputedEveryAttempt(t *testing.T) {
	h := harness.New(t)
	root := filepath.Join(h.Root, "fakeroot")
	setMem(h, root, 100)
	h.Seam("procfs_root = " + harness.Q(root))
	h.HostConfig("groups = { bigmem = { units = 1 } }\n[host]\nmemory_floor = \"6GiB\"\n" +
		"[pools.heavy]\nslots = 2\nwait = \"4s\"\nmemory = \"1GiB\"\ngroups = { bigmem = 1 }\n" +
		"[pools.side]\ngroups = { bigmem = 1 }\nwait = \"0s\"\n")
	w := h.StartWith([]string{"BITC_POLL=1s"}, "run", "--wait", "4s", "heavy", "--", "true")
	h.Until(func() bool { return len(tickets(h, "heavy")) == 1 }, 10*time.Second)
	time.Sleep(300 * time.Millisecond) // the first attempt (memory: ...) is done; the next is at 1s
	if w.Exited() {
		t.Fatal("admitted under the floor")
	}
	side := h.Hold(nil, "side") // takes bigmem: the waiter's group_wait is empty after a memory failure
	setMem(h, root, 9000)
	r := w.Wait()
	if r.Code != 75 || r.Stderr != "STARVED heavy (group bigmem busy)\n" {
		t.Fatalf("a stale memory reason survived: %+v", r)
	}
	side.Release()
}
func TestMR1_CgroupHeadroomAndReservations(t *testing.T) {
	h := harness.New(t)
	root := filepath.Join(h.Root, "fakeroot")
	setMem(h, root, 30000)
	h.Write(filepath.Join(root, "proc", "self", "cgroup"), "0::/a\n", 0o600)
	h.Write(filepath.Join(root, "sys", "fs", "cgroup", "a", "memory.max"), strconv.Itoa(16<<30)+"\n", 0o600)
	h.Write(filepath.Join(root, "sys", "fs", "cgroup", "a", "memory.current"), strconv.Itoa(2<<30)+"\n", 0o600)
	h.Seam("procfs_root = " + harness.Q(root))
	h.HostConfig("[host]\nmemory_floor = \"6GiB\"\nreservation_window = \"30s\"\n" +
		"[pools.a]\nmemory = \"8GiB\"\nwait = \"0s\"\n[pools.b]\nmemory = \"4GiB\"\nwait = \"0s\"\n[pools.c]\nmemory = \"1GiB\"\nwait = \"0s\"\n")
	// cgroup headroom 14 GiB is tighter than MemAvailable (29 GiB)
	a := h.Hold(nil, "a") // 14 >= 6: admitted, reserves 8 GiB while it runs
	b := h.Hold(nil, "b") // 14 - 8 = 6: exactly the floor, reserves 4 GiB
	r := h.Run("run", "c", "--", "true")
	if r.Code != 75 || r.Stderr != "STARVED c (memory: 2048 MiB available, floor 6144 MiB)\n" {
		t.Fatalf("%+v", r)
	}
	b.Release()
	a.Release()
	if r := h.Run("run", "c", "--", "true"); r.Code != 0 {
		t.Fatalf("reservations not dropped at the end of their holds: %+v", r)
	}
}

func removeFile(p string) error { return os.Remove(p) }
