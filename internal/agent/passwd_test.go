package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnsurePasswdLineAddsTheJobUserOnce(t *testing.T) {
	p := filepath.Join(t.TempDir(), "passwd")
	if err := os.WriteFile(p, []byte("root:x:0:0:root:/root:/bin/sh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ { // provisioning and the attach both ensure the user
		if err := ensurePasswdLine(p, "bitcjob", "bitcjob:x:10001:10001:bitc job:/home/bitcjob:/bin/sh"); err != nil {
			t.Fatal(err)
		}
	}
	b, _ := os.ReadFile(p)
	n := 0
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(l, "bitcjob:") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d bitcjob lines:\n%s", n, b)
	}
	if !strings.HasPrefix(string(b), "root:x:0:0") {
		t.Fatalf("the existing lines changed:\n%s", b)
	}
}
