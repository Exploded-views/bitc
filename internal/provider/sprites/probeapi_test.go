package sprites

import (
	"bytes"
	"context"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Exploded-views/bitc/internal/provider/sprites/spritesfake"
)

func TestRawSendsOnlyV1Paths(t *testing.T) {
	_, c := newFake(t, spritesfake.Options{})
	code, body := c.Raw(context.Background(), "GET", "/v1/sprites", url.Values{"prefix": {"x"}}, nil)
	if code != 200 || !strings.Contains(string(body), `"sprites"`) {
		t.Fatalf("Raw GET /v1/sprites = %d %q", code, body)
	}
	if code, _ := c.Raw(context.Background(), "GET", "/etc/passwd", nil, nil); code != 0 {
		t.Fatalf("Raw sent a request outside /v1/: %d", code)
	}
	if code, _ := c.Raw(context.Background(), "GET", "/v1/../x", nil, nil); code != 0 {
		t.Fatalf("Raw accepted a dot-dot path: %d", code)
	}
}

func TestOrganizationReadsTheSlug(t *testing.T) {
	_, c := newFake(t, spritesfake.Options{})
	o, err := c.Organization(context.Background())
	if err != nil || o.Slug != "sandbox-org" || !strings.Contains(string(o.Raw), "sandbox-org") {
		t.Fatalf("Organization = %+v, %v", o, err)
	}
}

func TestCheckpointCreateGetRestoreDelete(t *testing.T) {
	f, c := newFake(t, spritesfake.Options{})
	ctx := context.Background()
	if _, err := c.CreateSprite(ctx, "cp-a", nil); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.Overlay("cp-a"), "m"), []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	cp, err := c.CreateCheckpoint(ctx, "cp-a", "first")
	if err != nil || cp.ID == "" || cp.Auto {
		t.Fatalf("CreateCheckpoint = %+v, %v", cp, err)
	}
	got, err := c.GetCheckpoint(ctx, "cp-a", cp.ID)
	if err != nil || got.ID != cp.ID {
		t.Fatalf("GetCheckpoint = %+v, %v", got, err)
	}
	list, err := c.ListCheckpoints(ctx, "cp-a")
	if err != nil || len(list) != 1 || list[0].Auto {
		t.Fatalf("a manual checkpoint is listed as automatic: %+v, %v", list, err)
	}
	if err := os.WriteFile(filepath.Join(f.Overlay("cp-a"), "m"), []byte("two"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := c.RestoreCheckpoint(ctx, "cp-a", cp.ID); err != nil {
		t.Fatalf("RestoreCheckpoint: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(f.Overlay("cp-a"), "m")); string(b) != "one" {
		t.Fatalf("restore left %q, want the checkpoint's content", b)
	}
	if err := c.DeleteCheckpoint(ctx, "cp-a", cp.ID); err != nil {
		t.Fatalf("DeleteCheckpoint: %v", err)
	}
	if _, err := c.GetCheckpoint(ctx, "cp-a", cp.ID); kind(err) != "notfound" {
		t.Fatalf("a deleted checkpoint is still there: %v", err)
	}
	if err := c.RestoreCheckpoint(ctx, "cp-a", "../x"); kind(err) != "ineligible" {
		t.Fatalf("a bad checkpoint id was sent: %v", kind(err))
	}
}

func TestFSWriteThenRead(t *testing.T) {
	f, c := newFake(t, spritesfake.Options{})
	ctx := context.Background()
	if _, err := c.CreateSprite(ctx, "fs-a", nil); err != nil {
		t.Fatal(err)
	}
	if err := c.FSWrite(ctx, "fs-a", "/var/tmp/deep/x.txt", []byte("hello")); err != nil {
		t.Fatalf("FSWrite: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(f.Overlay("fs-a"), "var/tmp/deep/x.txt")); string(b) != "hello" {
		t.Fatalf("the file holds %q", b)
	}
	var w bytes.Buffer
	n, err := c.FSRead(ctx, "fs-a", "/var/tmp/deep/x.txt", &w)
	if err != nil || n != 5 || w.String() != "hello" {
		t.Fatalf("FSRead = %d %q %v", n, w.String(), err)
	}
	if _, err := c.FSRead(ctx, "fs-a", "/nope", &w); kind(err) != "notfound" {
		t.Fatalf("reading a missing file: %v", kind(err))
	}
	if _, err := c.FSRead(ctx, "fs-a", "relative", &w); kind(err) != "ineligible" {
		t.Fatalf("a relative path was sent: %v", kind(err))
	}
	if _, err := c.FSRead(ctx, "fs-a", "/../../etc/passwd", &w); err == nil || w.Len() != 5 {
		t.Fatalf("a path escape read something: %v %q", err, w.String())
	}
}
