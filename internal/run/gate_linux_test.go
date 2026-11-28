// Linux-only: reads the signal dispositions from /proc/<pid>/status.

package run

import (
	"context"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// sigFields reads the SigIgn, SigBlk and SigCgt masks written by the test command.
func sigFields(t *testing.T, path string) map[string]uint64 {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]uint64{}
	for _, l := range strings.Split(string(b), "\n") {
		name, val, ok := strings.Cut(l, ":")
		if !ok {
			continue
		}
		if v, err := strconv.ParseUint(strings.TrimSpace(val), 16, 64); err == nil {
			out[name] = v
		}
	}
	if len(out) != 3 {
		t.Fatalf("%s: %q", path, b)
	}
	return out
}

// T7: the command's signal dispositions and mask are those it has when started by Exec at base,
// that is, a direct start while Exec's four signals are caught. SigIgn and SigCgt must be equal.
// The blocked mask a forked child gets is the forking thread's, which the Go runtime sometimes
// leaves with every signal blocked, at base and with the gate alike, so it varies from start to
// start: the mask via Exec may have no bit that no direct start has.
func TestDispositionsMatchADirectStart(t *testing.T) {
	dir := t.TempDir()
	script := `grep -E '^Sig(Ign|Blk|Cgt):' /proc/$$/status > "$1"`
	s := spec(t, "sh", "-c", script, "sh", filepath.Join(dir, "via"))
	if r, err := Exec(context.Background(), s); err != nil || r.Code != 0 {
		t.Fatalf("%+v %v", r, err)
	}
	via := sigFields(t, filepath.Join(dir, "via"))
	c := make(chan os.Signal, 4)
	signal.Notify(c, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT)
	defer signal.Stop(c)
	var blocked uint64
	for i := 0; i < 30; i++ {
		out := filepath.Join(dir, "direct"+strconv.Itoa(i))
		d := exec.Command("sh", "-c", script, "sh", out)
		d.Env, d.Dir, d.Stdin = s.Env, s.Dir, s.Stdin
		d.SysProcAttr = childAttr(true)
		if err := d.Run(); err != nil {
			t.Fatal(err)
		}
		direct := sigFields(t, out)
		for _, name := range []string{"SigIgn", "SigCgt"} {
			if direct[name] != via[name] {
				t.Errorf("%s: %016x via Exec, %016x for a direct start", name, via[name], direct[name])
			}
		}
		blocked |= direct["SigBlk"]
	}
	if via["SigBlk"]&^blocked != 0 {
		t.Errorf("SigBlk %016x via Exec has bits no direct start blocked (%016x)", via["SigBlk"], blocked)
	}
}
