package exit

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoBareGoStatements fails on any `go` statement outside internal/exit: a panic in a
// bare goroutine would exit 2, which the exit contract forbids.
func TestNoBareGoStatements(t *testing.T) {
	for _, root := range []string{"../../internal", "../../cmd"} {
		err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return err
			}
			if strings.Contains(filepath.ToSlash(p), "internal/exit/") {
				return nil
			}
			// the RunPod and Sprites fakes are test infrastructure and never run in the bitc binary
			if strings.Contains(filepath.ToSlash(p), "/runpodfake/") || strings.Contains(filepath.ToSlash(p), "/spritesfake/") {
				return nil
			}
			f, perr := parser.ParseFile(token.NewFileSet(), p, nil, 0)
			if perr != nil {
				return perr
			}
			ast.Inspect(f, func(n ast.Node) bool {
				if _, ok := n.(*ast.GoStmt); ok {
					t.Errorf("%s: bare go statement; use exit.Go", p)
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}
