//go:build linux

package agent

import (
	"os"
	"testing"
)

// A probe that means to run as an unprivileged account never falls back to uid or gid 0, and an
// id that does not fit a uint32 is refused, not wrapped. 4294967295 is (uid_t)-1, "no change".
func TestParseIDsRefusesBadOrRootIDs(t *testing.T) {
	for _, c := range [][2]string{{"", "1001"}, {"x", "1001"}, {"-1", "1001"}, {"4294967296", "1001"},
		{"1001", "4294967296"}, {"0", "1001"}, {"1001", "0"}, {"1001", " 1001"}, {"1001", "4294967295"}} {
		if _, _, err := parseIDs("bitc-s1", c[0], c[1]); err == nil {
			t.Errorf("%q:%q: no error", c[0], c[1])
		}
	}
	if uid, gid, err := parseIDs("bitc-s1", "1001", "1002"); err != nil || uid != 1001 || gid != 1002 {
		t.Errorf("1001:1002 -> %d:%d %v", uid, gid, err)
	}
}

// The doctor's ssh-reach-key probe opens the key as each slot account: an account whose passwd
// ids are root, bad or past a uint32 is never opened as, since a cast would wrap it to uid 0.
func TestKeyProbeNeverOpensAsRootOrBadIDs(t *testing.T) {
	for _, ids := range []string{"0:0", "996:0", "0:996", "x:996", "996:x", "-1:996", "996:-1", "4294967296:996", "996:4294967296", "4294967295:996"} {
		h := newFakeHost(t)
		h.write("etc/passwd", "root:x:0:0:root:/root:/bin/bash\nbitc:x:995:995::/home/bitc:/bin/sh\nbitc-s1:x:"+ids+"::/var/lib/bitc/slots/1/home:/usr/sbin/nologin\nbitc-s2:x:997:997::/var/lib/bitc/slots/2/home:/usr/sbin/nologin\n", 0o644)
		env := h.env(false)
		env.OpenAs = func(uid, gid uint32, path string) error {
			if uid < 1 || gid < 1 || uid == 1<<32-1 || gid == 1<<32-1 {
				t.Errorf("ids %s: opened %s as %d:%d", ids, path, uid, gid)
			}
			return os.ErrPermission
		}
		CheckHost(env)
	}
}

// job-exec's last root step sets the ids it was given: a uid or gid that is root, bad or past a
// uint32 is refused, since Setuid would wrap it to 0.
func TestJobExecRefusesRootOrOutOfRangeIDs(t *testing.T) {
	ok := func(uid, gid string) bool {
		_, _, _, _, _, _, ok := parseJobExec([]string{"--uid", uid, "--gid", gid, "--prog", "/bin/true", "--", "true"})
		return ok
	}
	if !ok("1001", "1002") {
		t.Error("1001:1002 refused")
	}
	for _, c := range [][2]string{{"0", "1002"}, {"1001", "0"}, {"x", "1002"}, {"-1", "1002"}, {"4294967296", "1002"},
		{"1001", "4294967296"}, {"4294967295", "1002"}, {"1001", "4294967295"}} {
		if ok(c[0], c[1]) {
			t.Errorf("%q:%q accepted", c[0], c[1])
		}
	}
}
