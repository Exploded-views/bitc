package status

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Exploded-views/bitc/internal/config"
	"github.com/Exploded-views/bitc/internal/redact"
	"github.com/Exploded-views/bitc/internal/state"
)

func TestSweepStaging(t *testing.T) {
	root := t.TempDir()
	wt, other := filepath.Join(root, "wt"), filepath.Join(root, "other")
	old := time.Now().Add(-StaleStaging - time.Hour)
	mk := func(p string, age time.Time) {
		if err := os.MkdirAll(filepath.Join(p, "f"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, age, age); err != nil {
			t.Fatal(err)
		}
	}
	mk(filepath.Join(wt, ".bitc", "staging", "old"), old)
	mk(filepath.Join(wt, ".bitc", "staging", "fresh"), time.Now())
	mk(filepath.Join(other, ".bitc", "staging", "old"), old) // found through a remote job's cwd
	l := state.Layout{Root: filepath.Join(root, "state")}
	if err := os.MkdirAll(l.Root, 0o700); err != nil {
		t.Fatal(err)
	}
	ev := state.Event{Event: "start", Pool: "heavy", Job: "j1", Provider: "runpod", Cwd: filepath.Join(other, "sub")}
	if err := state.AppendHistory(l.History(), ev, redact.New()); err != nil {
		t.Fatal(err)
	}
	e := &config.Effective{Worktree: wt}
	var out bytes.Buffer
	swept := func(kind, id, why string) { out.WriteString(kind + " " + id + " (" + why + ")\n") }
	sweepStaging(e, l, true, swept)
	if strings.Count(out.String(), "staging ") != 2 || strings.Contains(out.String(), "fresh") {
		t.Fatal(out.String())
	}
	if _, err := os.Stat(filepath.Join(wt, ".bitc", "staging", "old")); err != nil {
		t.Fatal("the dry run removed a dir")
	}
	sweepStaging(e, l, false, swept)
	for p, gone := range map[string]bool{
		filepath.Join(wt, ".bitc", "staging", "old"):    true,
		filepath.Join(other, ".bitc", "staging", "old"): true,
		filepath.Join(wt, ".bitc", "staging", "fresh"):  false,
	} {
		if _, err := os.Stat(p); gone != os.IsNotExist(err) {
			t.Errorf("%s: want gone=%v, stat %v", p, gone, err)
		}
	}
}
