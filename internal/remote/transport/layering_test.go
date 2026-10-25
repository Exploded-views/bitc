package transport

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// The core transport stage names no concrete provider: the upload ref prefix lives in the
// neutral provider package.
func TestTransportImportsNoConcreteProvider(t *testing.T) {
	files, _ := filepath.Glob("*.go")
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		af, err := parser.ParseFile(token.NewFileSet(), f, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range af.Imports {
			if strings.Contains(imp.Path.Value, "internal/provider/") {
				t.Errorf("%s imports %s", f, imp.Path.Value)
			}
		}
	}
}
