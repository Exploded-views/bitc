// Linux-only: what the Linux spawn path guarantees through /proc and prctl. The plugin is the
// execveat'd pinned fd itself, so it is its own group leader and runs under no_new_privs.

package plugin

import "testing"

func TestSpawnRunsThePinnedFdUnderNoNewPrivsAsItsOwnGroupLeader(t *testing.T) {
	r := newRig(t)
	r.start()
	d := r.diag()
	if d.NoNewPrivs != "1" {
		t.Errorf("NoNewPrivs = %q", d.NoNewPrivs)
	}
	if d.Exe != r.bin {
		t.Errorf("exe %q, want the pinned path %q (execveat of the hashed fd)", d.Exe, r.bin)
	}
	if d.Pgid != d.Pid {
		t.Errorf("pgid %d pid %d: on Linux the plugin itself leads its process group", d.Pgid, d.Pid)
	}
}
