package remote

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Exploded-views/bitc/internal/config"
	"github.com/Exploded-views/bitc/internal/provider"
	_ "github.com/Exploded-views/bitc/internal/provider/sprites"
	"github.com/Exploded-views/bitc/internal/provider/sprites/spritesfake"
)

// A CleanupOnly core gets the cleanup handle (never a provider); doctor --remote says jobs are
// refused, lists and checks the sweep timer.
func TestCleanupOnlySpritesHandleAndDoctor(t *testing.T) {
	f := spritesfake.New(t, spritesfake.Options{})
	acct := t.TempDir()
	if err := os.Chmod(acct, 0o700); err != nil { // a private accounting dir, as state requires
		t.Fatal(err)
	}
	e := &config.Effective{Test: &config.Test{TimeScale: 0.05, AccountingDir: acct}}
	raw := map[string]any{"org": f.Org(), "api_base": f.URL(), "sizes": map[string]any{"cpu8": map[string]any{"memory": "16GiB"}}}
	core := &config.Provider{Type: "sprites", NamePrefix: "bitc", APIKey: f.Token(), Raw: raw, StartTimeout: time.Minute, CleanupOnly: true}
	c, err := sweepHandle(context.Background(), "sprites", core, e)
	if err != nil {
		t.Fatal(err)
	}
	if _, full := c.(provider.Provider); full {
		t.Fatal("a CleanupOnly core got a whole provider")
	}
	var lines []string
	doctorCleanup(context.Background(), "sprites", core, e, func(level, check, detail string) {
		lines = append(lines, level+" "+check+": "+detail)
	})
	got := strings.Join(lines, "\n")
	for _, want := range []string{"warn sprites jobs: refused in this release", "ok sprites api: 0 live bitc- resources", "fail sprites sweep timer"} {
		if !strings.Contains(got, want) {
			t.Errorf("doctor lacks %q:\n%s", want, got)
		}
	}
	stampSweep(acct+"/remote/sprites", time.Now())
	lines = nil
	doctorCleanup(context.Background(), "sprites", core, e, func(level, check, detail string) { lines = append(lines, level+" "+check) })
	if !strings.Contains(strings.Join(lines, "\n"), "ok sprites sweep timer") {
		t.Errorf("after a sweep: %v", lines)
	}
	// a type without a cleanup handle, and a broken section, are errors
	if _, err := sweepHandle(context.Background(), "fake", &config.Provider{Type: "fake", CleanupOnly: true}, e); err == nil {
		t.Fatal("fake has no cleanup handle")
	}
	lines = nil
	doctorCleanup(context.Background(), "sprites", &config.Provider{Type: "sprites", NamePrefix: "bitc", APIKey: f.Token(), Raw: map[string]any{}, CleanupOnly: true}, e,
		func(level, check, detail string) { lines = append(lines, level+" "+check) })
	if !strings.Contains(strings.Join(lines, "\n"), "fail sprites config") {
		t.Errorf("broken section: %v", lines)
	}
}
