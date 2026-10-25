package run

import (
	"reflect"
	"testing"

	"github.com/Exploded-views/bitc/internal/exit"
)

func TestParseRunArgs(t *testing.T) {
	ra, err := ParseRunArgs([]string{"--wait", "30s", "--name", "j1", "lint", "--", "npm", "run", "--", "-x"})
	if err != nil {
		t.Fatal(err)
	}
	if ra.Pool != "lint" || ra.Wait != "30s" || ra.Name != "j1" ||
		!reflect.DeepEqual(ra.Argv, []string{"npm", "run", "--", "-x"}) {
		t.Fatalf("%+v", ra)
	}
	ra, err = ParseRunArgs([]string{"db", "psql", "-c", "select 1"})
	if err != nil || ra.Pool != "db" || !reflect.DeepEqual(ra.Argv, []string{"psql", "-c", "select 1"}) {
		t.Fatalf("%+v %v", ra, err)
	}
	ra, err = ParseRunArgs([]string{"--artifacts", "a,b/", "--provider", "runpod", "heavy", "--", "x"})
	if err != nil || ra.Provider != "runpod" || !reflect.DeepEqual(*ra.Artifacts, []string{"a", "b"}) {
		t.Fatalf("%+v %v", ra, err)
	}
}

func TestParseRunArgsUsage(t *testing.T) {
	for _, args := range [][]string{
		{}, {"lint"}, {"lint", "--"}, {"--wait"}, {"--bogus", "lint", "true"},
		{"--local", "--provider", "runpod", "heavy", "true"},
	} {
		if _, err := ParseRunArgs(args); exit.Code(err) != exit.Usage {
			t.Errorf("%q: got %v, want 64", args, err)
		}
	}
}
