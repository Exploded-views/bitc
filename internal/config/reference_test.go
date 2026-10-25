package config

import (
	"bufio"
	"os"
	"strings"
	"testing"
)

//go:generate sh -c "go run ./gen > ../../docs/config-reference.md"

func TestReferenceIsGenerated(t *testing.T) {
	b, err := os.ReadFile("../../docs/config-reference.md")
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != GenerateReference() {
		t.Fatal("docs/config-reference.md is stale: run go generate ./internal/config")
	}
}

// TestDefaultsMatchSpec compares tag defaults with the transcription of spec 5.4 in
// testdata/spec-defaults.txt ("key = default" per line). Update both when the spec changes.
func TestDefaultsMatchSpec(t *testing.T) {
	ref := GenerateReference()
	f, err := os.Open("testdata/spec-defaults.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, _ := strings.Cut(line, " = ")
		want := "| `" + k + "` |"
		i := strings.Index(ref, want)
		if i < 0 {
			t.Errorf("%s: not in the generated reference", k)
			continue
		}
		row := ref[i : i+strings.Index(ref[i:], "\n")]
		if !strings.Contains(row, "`"+v+"`") {
			t.Errorf("%s: reference row %q, spec 5.4 says %s", k, row, v)
		}
	}
}
