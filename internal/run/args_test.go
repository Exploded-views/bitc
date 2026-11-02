package run

import (
	"reflect"
	"strings"
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

func TestParseRunArgsUnits(t *testing.T) {
	ra, err := ParseRunArgs([]string{"--units", "cpu=3", "--units", "big_mem=2", "--wait", "5s", "work", "--", "true"})
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]int{"cpu": 3, "big-mem": 2}; !reflect.DeepEqual(ra.Units, want) {
		t.Fatalf("Units = %v, want %v (group names are canonical)", ra.Units, want)
	}
	if ra.Pool != "work" || !reflect.DeepEqual(ra.Argv, []string{"true"}) {
		t.Fatalf("%+v", ra)
	}
	ra, err = ParseRunArgs([]string{"work", "--units", "cpu=3"}) // after the pool it is the command's argv
	if err != nil || ra.Units != nil || !reflect.DeepEqual(ra.Argv, []string{"--units", "cpu=3"}) {
		t.Fatalf("%+v %v", ra, err)
	}
}

func TestParseRunArgsUnitsUsage(t *testing.T) {
	for _, v := range []string{"", "cpu", "cpu=", "=3", "cpu=x", "cpu=-1", "cpu=1.5", "cpu=03", "cpu=1=2", "cpu 1=2", "cpu=99999999999999999999"} {
		if _, err := ParseRunArgs([]string{"--units", v, "work", "--", "true"}); exit.Code(err) != exit.Usage {
			t.Errorf("--units %q: got %v, want 64", v, err)
		}
	}
	if _, err := ParseRunArgs([]string{"--units"}); exit.Code(err) != exit.Usage {
		t.Errorf("--units without a value: got %v, want 64", err)
	}
	if _, err := ParseRunArgs([]string{"--units", "cpu=1", "--units", "cpu=2", "work", "--", "true"}); exit.Code(err) != exit.Usage {
		t.Errorf("a group named twice: got %v, want 64", err)
	}
	if _, err := ParseRunArgs([]string{"--units", "cpu=1", "--units", "cpu_x=2", "--units", "cpu-x=3", "work", "true"}); exit.Code(err) != exit.Usage {
		t.Errorf("one group spelled two ways: got %v, want 64", err)
	}
}

// k = 0 parses; the config layer rejects it knowing the group's units (1..N).
func TestParseRunArgsUnitsZeroIsLeftToConfig(t *testing.T) {
	ra, err := ParseRunArgs([]string{"--units", "cpu=0", "work", "--", "true"})
	if err != nil || ra.Units["cpu"] != 0 || len(ra.Units) != 1 {
		t.Fatalf("%+v %v", ra, err)
	}
}

// A bad --units value is one line: no usage text after it (spec 3.1).
func TestParseRunArgsUnitsMessageIsOneLine(t *testing.T) {
	for _, v := range []string{"cpu", "cpu=", "=3", "cpu=x", "cpu=-1", "cpu=03", "cpu=99999999999999999999"} {
		_, err := ParseRunArgs([]string{"--units", v, "work", "--", "true"})
		if err == nil || strings.Contains(err.Error(), "\n") || strings.Contains(err.Error(), "usage:") || !strings.Contains(err.Error(), "--units") {
			t.Errorf("--units %q: %v", v, err)
		}
	}
}
