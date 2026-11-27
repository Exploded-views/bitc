package spritesfake_test

import (
	"context"
	"errors"
	"testing"

	"github.com/Exploded-views/bitc/internal/provider"
	"github.com/Exploded-views/bitc/internal/provider/sprites"
	"github.com/Exploded-views/bitc/internal/provider/sprites/spritesfake"
)

// deletedWithCheckpoint makes a Sprite with one checkpoint and deletes it, the checkpoint
// surviving.
func deletedWithCheckpoint(t *testing.T, f *spritesfake.Fake, c *sprites.Client, name string) string {
	t.Helper()
	ctx := context.Background()
	if _, err := c.CreateSprite(ctx, name, nil); err != nil {
		t.Fatal(err)
	}
	cp, err := c.CreateCheckpoint(ctx, name, "c")
	if err != nil {
		t.Fatal(err)
	}
	f.BehaveN("checkpoints-survive-delete", 1)
	if err := c.DeleteSprite(ctx, name); err != nil {
		t.Fatal(err)
	}
	return cp.ID
}

func TestRestoreRecreatesADeletedSpriteWithItsSurvivingCheckpoints(t *testing.T) {
	f := spritesfake.New(t, spritesfake.Options{})
	c := client(t, f)
	ctx := context.Background()
	id := deletedWithCheckpoint(t, f, c, "bitcproof-rr-1")
	before, err := c.CreateSprite(ctx, "bitcproof-rr-other", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteSprite(ctx, "bitcproof-rr-other"); err != nil {
		t.Fatal(err)
	}
	if err := c.RestoreCheckpoint(ctx, "bitcproof-rr-1", id); !errors.Is(err, sprites.ErrNotFound) {
		t.Fatalf("restore without the behaviour: %v, want 404", err)
	}
	f.BehaveN("restore-recreates", 1)
	if err := c.RestoreCheckpoint(ctx, "bitcproof-rr-1", "cp-9"); !errors.Is(err, sprites.ErrNotFound) {
		t.Fatalf("restore of a checkpoint that did not survive: %v, want 404", err)
	}
	if err := c.RestoreCheckpoint(ctx, "bitcproof-rr-1", id); err != nil {
		t.Fatalf("restore with the behaviour: %v", err)
	}
	if live := f.Live(); len(live) != 1 || live[0] != "bitcproof-rr-1" {
		t.Fatalf("live %v, want the recreated Sprite", live)
	}
	s, err := c.GetSprite(ctx, "bitcproof-rr-1")
	if err != nil || s.ID == "" || s.ID == before.ID {
		t.Fatalf("the recreated Sprite %+v %v", s, err)
	}
	cps, err := c.ListCheckpoints(ctx, "bitcproof-rr-1")
	if err != nil || len(cps) != 1 || cps[0].ID != id {
		t.Fatalf("the recreated Sprite lists %+v %v, want the surviving checkpoint again", cps, err)
	}
	// the behaviour was armed once; a second restore on the live Sprite is a plain restore
	if err := c.RestoreCheckpoint(ctx, "bitcproof-rr-1", id); err != nil {
		t.Fatalf("restore on the recreated Sprite: %v", err)
	}
	if err := c.DeleteSprite(ctx, "bitcproof-rr-1"); err != nil {
		t.Fatal(err)
	}
}

func TestRestore500AnswersAnErrorAndDoesNothing(t *testing.T) {
	f := spritesfake.New(t, spritesfake.Options{})
	c := client(t, f)
	ctx := context.Background()
	if _, err := c.CreateSprite(ctx, "bitcproof-r5-1", nil); err != nil {
		t.Fatal(err)
	}
	cp, err := c.CreateCheckpoint(ctx, "bitcproof-r5-1", "c")
	if err != nil {
		t.Fatal(err)
	}
	f.BehaveN("restore-500", 1)
	var tr *provider.ErrTransient
	if err := c.RestoreCheckpoint(ctx, "bitcproof-r5-1", cp.ID); !errors.As(err, &tr) {
		t.Fatalf("restore: %v, want a transient (5xx) error", err)
	}
	if err := c.RestoreCheckpoint(ctx, "bitcproof-r5-1", cp.ID); err != nil {
		t.Fatalf("the behaviour was armed once: %v", err)
	}
}
