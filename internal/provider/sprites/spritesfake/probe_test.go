package spritesfake_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Exploded-views/bitc/internal/provider/sprites/spritesfake"
)

func mkSprite(t *testing.T, f *spritesfake.Fake, name string) {
	t.Helper()
	if code, b := raw(t, f, "POST", "/v1/sprites", `{"name":"`+name+`"}`, nil); code != 201 {
		t.Fatalf("create %s: %d %s", name, code, b)
	}
}

func TestFakeServesTheOrganization(t *testing.T) {
	f := spritesfake.New(t, spritesfake.Options{})
	code, b := raw(t, f, "GET", "/v1/organization", "", nil)
	if code != 200 || !strings.Contains(b, `"slug":"sandbox-org"`) {
		t.Fatalf("GET /v1/organization = %d %s", code, b)
	}
}

func TestFakeManualCheckpointLifecycle(t *testing.T) {
	f := spritesfake.New(t, spritesfake.Options{})
	mkSprite(t, f, "a")
	if err := os.WriteFile(filepath.Join(f.Overlay("a"), "m"), []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, b := raw(t, f, "POST", "/v1/sprites/a/checkpoint", `{"comment":"c1"}`, nil)
	if code != 200 || !strings.Contains(b, `"id":"cp-1"`) {
		t.Fatalf("create checkpoint = %d %s", code, b)
	}
	if code, b = raw(t, f, "GET", "/v1/sprites/a/checkpoints", "", nil); code != 200 || !strings.Contains(b, `"id":"cp-1"`) || !strings.Contains(b, `"auto":false`) {
		t.Fatalf("a manual checkpoint is not listed as manual: %d %s", code, b)
	}
	if code, _ = raw(t, f, "GET", "/v1/sprites/a/checkpoints/cp-1", "", nil); code != 200 {
		t.Fatalf("get checkpoint = %d", code)
	}
	if code, _ = raw(t, f, "GET", "/v1/sprites/a/checkpoints/cp-9", "", nil); code != 404 {
		t.Fatalf("get a missing checkpoint = %d", code)
	}
	_ = os.WriteFile(filepath.Join(f.Overlay("a"), "m"), []byte("two"), 0o644)
	if code, b = raw(t, f, "POST", "/v1/sprites/a/checkpoints/cp-1/restore", "", nil); code != 200 {
		t.Fatalf("restore = %d %s", code, b)
	}
	if got, _ := os.ReadFile(filepath.Join(f.Overlay("a"), "m")); string(got) != "one" {
		t.Fatalf("restore left %q", got)
	}
	if code, _ = raw(t, f, "DELETE", "/v1/sprites/a/checkpoints/cp-1", "", nil); code != 204 {
		t.Fatalf("delete checkpoint = %d", code)
	}
	if code, _ = raw(t, f, "DELETE", "/v1/sprites/a/checkpoints/cp-1", "", nil); code != 404 {
		t.Fatalf("delete twice = %d", code)
	}
	if code, _ = raw(t, f, "POST", "/v1/sprites/zz/checkpoint", `{}`, nil); code != 404 {
		t.Fatalf("checkpoint of a missing Sprite = %d", code)
	}
}

func TestFakeDeleteTakesManualCheckpointsUnlessTheyShouldSurvive(t *testing.T) {
	f := spritesfake.New(t, spritesfake.Options{})
	mkSprite(t, f, "a")
	raw(t, f, "POST", "/v1/sprites/a/checkpoint", `{}`, nil)
	raw(t, f, "DELETE", "/v1/sprites/a", "", nil)
	if code, _ := raw(t, f, "GET", "/v1/sprites/a/checkpoints", "", nil); code != 404 {
		t.Fatalf("checkpoints outlived the Sprite: %d", code)
	}
	f.Behave("checkpoints-survive-delete")
	mkSprite(t, f, "b")
	raw(t, f, "POST", "/v1/sprites/b/checkpoint", `{}`, nil)
	raw(t, f, "DELETE", "/v1/sprites/b", "", nil)
	code, b := raw(t, f, "GET", "/v1/sprites/b/checkpoints", "", nil)
	if code != 200 || !strings.Contains(b, `"id":"cp-1"`) {
		t.Fatalf("the survive behaviour kept nothing: %d %s", code, b)
	}
	if code, _ = raw(t, f, "POST", "/v1/sprites/b/checkpoints/cp-1/restore", "", nil); code != 404 {
		t.Fatalf("restore of a deleted Sprite's checkpoint = %d", code)
	}
}

func TestFakeFileReadAndWrite(t *testing.T) {
	f := spritesfake.New(t, spritesfake.Options{})
	mkSprite(t, f, "a")
	if code, b := raw(t, f, "PUT", "/v1/sprites/a/fs/write?path=/var/tmp/x/y.txt&mkdirParents=true", "hello", nil); code != 200 {
		t.Fatalf("write = %d %s", code, b)
	}
	if got, _ := os.ReadFile(filepath.Join(f.Overlay("a"), "var/tmp/x/y.txt")); string(got) != "hello" {
		t.Fatalf("file holds %q", got)
	}
	if code, b := raw(t, f, "GET", "/v1/sprites/a/fs/read?path=/var/tmp/x/y.txt", "", nil); code != 200 || b != "hello" {
		t.Fatalf("read = %d %q", code, b)
	}
	for _, c := range []struct {
		method, path string
		want         int
	}{
		{"GET", "/v1/sprites/a/fs/read?path=/missing", 404},
		{"GET", "/v1/sprites/a/fs/read?path=rel", 400},
		{"GET", "/v1/sprites/a/fs/read?path=/x&bogus=1", 400},
		{"GET", "/v1/sprites/a/fs/read", 400},
		{"PUT", "/v1/sprites/a/fs/write?path=/q/z", 404}, // no mkdirParents, no parent
		{"GET", "/v1/sprites/zz/fs/read?path=/x", 404},
	} {
		if code, _ := raw(t, f, c.method, c.path, "", nil); code != c.want {
			t.Errorf("%s %s = %d, want %d", c.method, c.path, code, c.want)
		}
	}
	if code, _ := raw(t, f, "GET", "/v1/sprites/a/fs/read?path=/../../etc/hostname", "", nil); code == 200 {
		t.Fatal("a path escape read a host file")
	}
}
