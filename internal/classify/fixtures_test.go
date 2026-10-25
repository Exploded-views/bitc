package classify

import (
	"path/filepath"
	"testing"

	ct "github.com/Exploded-views/bitc/internal/classify/classifytest"
)

func TestFixtures(t *testing.T) {
	root := ct.FixtureTree(t)
	rules := ct.NodeMonorepoRules()
	atRoot := Context{Cwd: root, LogicalCwd: root, Worktree: root}
	for _, a := range ct.RefusedFromRoot {
		if _, refused := Check(rules, a, atRoot); !refused {
			t.Errorf("not refused: %q", a)
		}
	}
	for _, a := range ct.AllowedFromRoot {
		if reason, refused := Check(rules, a, atRoot); refused {
			t.Errorf("refused (%s): %q", reason, a)
		}
	}
	sub := filepath.Join(root, "src", "app")
	inSub := Context{Cwd: sub, LogicalCwd: sub, Worktree: root}
	for _, a := range ct.RefusedFromSubdir {
		if _, refused := Check(rules, append([]string{"npx", "eslint"}, a...), inSub); !refused {
			t.Errorf("from src/app not refused: npx eslint %q", a)
		}
	}
	for _, a := range ct.AllowedFromSubdir {
		if _, refused := Check(rules, append([]string{"npx", "eslint"}, a...), inSub); refused {
			t.Errorf("from src/app refused: npx eslint %q", a)
		}
	}
	pre := filepath.Join(root, "src", "lib", "auth-utils")
	inPre := Context{Cwd: pre, LogicalCwd: pre, Worktree: root}
	for _, a := range ct.AllowedFromPrefixSibling {
		if reason, refused := Check(rules, append([]string{"npx", "eslint"}, a...), inPre); refused {
			t.Errorf("from src/lib/auth-utils refused (%s): npx eslint %q", reason, a)
		}
	}
	for _, a := range ct.RefusedFromPrefixSibling {
		if _, refused := Check(rules, append([]string{"npx", "eslint"}, a...), inPre); !refused {
			t.Errorf("from src/lib/auth-utils not refused: npx eslint %q", a)
		}
	}
}
