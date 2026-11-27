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

func TestRawAllowsOnlyPlainSegments(t *testing.T) {
	f, c := newFake(t, spritesfake.Options{})
	ctx := context.Background()
	refused := []string{
		"/v1/sprites%00", "/v1/%73prites", "/v1/sprites\x00", "/v1/sprites\x7f", "/v1/spr ites",
		"/v1/sprites,x", "/v1/sprites=x", "/v1/sprites&x", "/v1/sprites+x", "/v1/sprites$x",
		"/v1/sprites@x", "/v1/sprites:create", "/v1/sprites!", "/v1/sprites'x", "/v1/sprites(x)",
		"/v1/sprites*", "/v1/spr" + string(rune(0x00e9)) + "tes", "/v1//sprites", "/v1/sprites/",
		"/v1/./sprites", "/v1/sprites/.",
	}
	for _, m := range []string{"GET", "POST"} {
		for _, p := range refused {
			before := len(f.Calls())
			code, _ := c.Raw(ctx, m, p, nil, nil)
			if code != 0 {
				t.Errorf("Raw %s %q answered %d, want 0", m, p, code)
			}
			if n := len(f.Calls()); n != before {
				t.Errorf("Raw %s %q sent %d request(s)", m, p, n-before)
			}
		}
	}
	for _, p := range []string{
		"/v1/sprites", "/v1/regions", "/v1/sprites/bitc-a",
		"/v1/sprites/bitc-a/checkpoints/ck-1.v2_x~y", "/v1/sprites/Bad_name.x",
	} {
		before := len(f.Calls())
		code, _ := c.Raw(ctx, "GET", p, nil, nil)
		if code == 0 {
			t.Errorf("Raw GET %q answered 0, want a status", p)
		}
		if n := len(f.Calls()); n != before+1 {
			t.Errorf("Raw GET %q sent %d request(s), want 1", p, n-before)
		}
	}
}

func TestRawRefusesSemicolonPaths(t *testing.T) {
	f, c := newFake(t, spritesfake.Options{})
	ctx := context.Background()
	for _, tc := range []struct{ method, path string }{
		{"POST", "/v1/sprites;x"},
		{"POST", "/v1/sprites;"},
		{"GET", "/v1/sprites;x"},
		{"GET", "/v1/;/sprites"},
		{"DELETE", "/v1/sprites/bitc-a;x"},
	} {
		before := len(f.Calls())
		code, _ := c.Raw(ctx, tc.method, tc.path, nil, nil)
		if code != 0 {
			t.Errorf("Raw %s %s answered %d, want 0", tc.method, tc.path, code)
		}
		if n := len(f.Calls()); n != before {
			t.Errorf("Raw %s %s sent %d request(s)", tc.method, tc.path, n-before)
		}
	}
	if code, _ := c.Raw(ctx, "GET", "/v1/sprites", nil, nil); code != 200 {
		t.Errorf("Raw GET /v1/sprites = %d, want 200", code)
	}
}

func TestRawRefusesNetworkPolicyWrites(t *testing.T) {
	f, c := newFake(t, spritesfake.Options{})
	ctx := context.Background()
	if _, err := c.CreateSprite(ctx, "bitc-a", nil); err != nil {
		t.Fatal(err)
	}
	body := map[string]any{"rules": []NetRule{{Domain: "github.com", Action: "allow"}}}
	for _, m := range []string{"POST", "PUT", "PATCH", "post", "Put"} {
		for _, p := range []string{"/v1/sprites/bitc-a/policy/network", "/v1/sprites/bitc-a/policy/network/"} {
			if code, _ := c.Raw(ctx, m, p, nil, body); code != 0 {
				t.Errorf("Raw %s %s was sent: %d", m, p, code)
			}
		}
	}
	if net, _ := f.Policies("bitc-a"); strings.Contains(net, "github.com") {
		t.Fatalf("a Raw write set a network policy: %s", net)
	}
	// a read of the policy and a write of another policy are still fine
	if code, _ := c.Raw(ctx, "GET", "/v1/sprites/bitc-a/policy/network", nil, nil); code != 200 {
		t.Errorf("Raw GET of the network policy = %d", code)
	}
	if code, _ := c.Raw(ctx, "POST", "/v1/sprites/bitc-a/policy/resources", nil, map[string]any{"memory": map[string]any{"limit_mb": 1024, "autoscale": false}}); code != 200 {
		t.Errorf("Raw POST of the resources policy = %d", code)
	}
}

func TestRawRefusesEveryNonReadOfTheNetworkPolicyAfterNormalising(t *testing.T) {
	f, c := newFake(t, spritesfake.Options{})
	ctx := context.Background()
	if _, err := c.CreateSprite(ctx, "bitc-a", nil); err != nil {
		t.Fatal(err)
	}
	before, _ := f.Policies("bitc-a")
	paths := []string{
		"/v1/sprites/bitc-a/policy/network",
		"/v1/sprites/bitc-a/policy//network",
		"/v1/sprites/bitc-a/policy/./network",
		"/v1/sprites/bitc-a/policy/network/.",
		"/v1/sprites/bitc-a/policy/NETWORK",
		"/v1/sprites/bitc-a/Policy/Network/",
		"/v1/sprites/bitc-a/policy/network?x=1",
		"/v1/sprites/bitc-a/policy/network#x",
	}
	for _, m := range []string{"DELETE", "delete", "POST", "post", "PUT", "PATCH", "patch", "OPTIONS", "FOO"} {
		for _, p := range paths {
			if code, _ := c.Raw(ctx, m, p, nil, map[string]any{"rules": []NetRule{}}); code != 0 {
				t.Errorf("Raw %s %s was sent: %d", m, p, code)
			}
		}
	}
	if after, _ := f.Policies("bitc-a"); after != before {
		t.Fatalf("a Raw call changed the network policy: %s -> %s", before, after)
	}
	for _, m := range []string{"GET", "get", "HEAD"} {
		if code, _ := c.Raw(ctx, m, "/v1/sprites/bitc-a/policy/network", nil, nil); code == 0 {
			t.Errorf("Raw %s of the network policy was refused", m)
		}
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
