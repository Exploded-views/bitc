package spritesfake_test

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/Exploded-views/bitc/internal/provider/sprites/spritesfake"
)

// A Sprite's directory is named by the fake-made id with a "_" prefix, never by the request name:
// the name is request data, and under lax-names it can be anything.
func TestFakeNamesSpriteDirsByID(t *testing.T) {
	t.Run("lax names make no directory from the request", func(t *testing.T) {
		f := spritesfake.New(t, spritesfake.Options{})
		root := f.Root()
		// a name that, joined to the root, would leave it; unique per fake so the clean-up of a
		// stray directory cannot touch anything else
		escape := "../escape-" + filepath.Base(root)
		stray := filepath.Join(filepath.Dir(root), "escape-"+filepath.Base(root))
		t.Cleanup(func() { _ = os.RemoveAll(stray) })
		f.BehaveN("lax-names", -1)
		for _, name := range []string{escape, "a/b"} {
			body := `{"name":"` + name + `"}`
			if code, _ := raw(t, f, "POST", "/v1/sprites", body, nil); code != 201 && code != 200 {
				t.Fatalf("lax create of %q = %d, want 2xx", name, code)
			}
			ov := f.Overlay(name)
			if ov == "" {
				t.Fatalf("no overlay for %q", name)
			}
			rel, err := filepath.Rel(root, ov)
			if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				t.Errorf("the overlay of %q is %s, outside the root %s", name, ov, root)
			}
		}
		if _, err := os.Stat(stray); err == nil {
			t.Errorf("a directory was made outside the root: %s", stray)
		}
		ents, err := os.ReadDir(root)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, e := range ents {
			got = append(got, e.Name())
		}
		sort.Strings(got)
		if want := []string{"_sp1", "_sp2"}; strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("the root holds %v, want %v (one directory per fake-made id)", got, want)
		}
	})

	t.Run("a Sprite named like another Sprite's id does not share its directory", func(t *testing.T) {
		f := spritesfake.New(t, spritesfake.Options{})
		c := client(t, f)
		ctx := context.Background()
		// "b" is sp1 and is deleted with a surviving checkpoint; "sp3" is sp2; restoring "b" then
		// recreates it as sp3, the id that the valid name "sp3" would also map to
		id := deletedWithCheckpoint(t, f, c, "b")
		if _, err := c.CreateSprite(ctx, "sp3", nil); err != nil {
			t.Fatal(err)
		}
		f.BehaveN("restore-recreates", 1)
		if err := c.RestoreCheckpoint(ctx, "b", id); err != nil {
			t.Fatalf("restore: %v", err)
		}
		again, err := c.GetSprite(ctx, "b")
		if err != nil || again.ID != "sp3" {
			t.Fatalf("the recreated Sprite %+v %v, want id sp3", again, err)
		}
		other := f.Overlay("sp3")
		if other == "" || f.Overlay("b") == "" {
			t.Fatalf("overlays: b %q, sp3 %q", f.Overlay("b"), other)
		}
		if f.Overlay("b") == other || filepath.Dir(f.Overlay("b")) == filepath.Dir(other) {
			t.Errorf("the Sprites named b (id sp3) and sp3 (id sp2) share a directory: %s and %s", f.Overlay("b"), other)
		}
		marker := filepath.Join(other, "marker")
		if err := os.WriteFile(marker, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := c.DeleteSprite(ctx, "b"); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(marker); err != nil {
			t.Errorf("deleting b removed the overlay of sp3: %v", err)
		}
		if err := c.DeleteSprite(ctx, "sp3"); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Dir(other)); err == nil {
			t.Errorf("deleting sp3 left its directory %s", filepath.Dir(other))
		}
	})
}
