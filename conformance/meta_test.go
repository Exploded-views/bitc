package conformance

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"
)

type row struct {
	ID      string `toml:"id"`
	Task    string `toml:"task"`
	Desc    string `toml:"desc"`
	Todo    bool   `toml:"todo"`
	Ported  string `toml:"ported"`
	Changed string `toml:"changed"`
	Test    string `toml:"test"`
	Dropped string `toml:"dropped"`
}

var idRE = regexp.MustCompile(`^[A-Z]+[0-9]+$`)
var testRE = regexp.MustCompile(`^Test([A-Z]+[0-9]+)_`)

func loadRows(t *testing.T) []row {
	t.Helper()
	b, err := os.ReadFile("behaviours.toml")
	if err != nil {
		t.Fatal(err)
	}
	var reg struct {
		Rows []row `toml:"rows"`
	}
	dec := toml.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&reg); err != nil {
		t.Fatal(err)
	}
	return reg.Rows
}

// spec121 returns spec section 12.1 with backticks dropped and whitespace collapsed.
func spec121(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("../docs/spec.md")
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	i := strings.Index(s, "### 12.1 ")
	if i < 0 {
		t.Fatal("docs/spec.md has no section 12.1")
	}
	s = s[i:]
	if j := strings.Index(s[3:], "\n## "); j >= 0 {
		s = s[:j+3]
	}
	return strings.Join(strings.Fields(strings.ReplaceAll(s, "`", "")), " ")
}

func changeKeys(t *testing.T) map[string]bool {
	t.Helper()
	b, err := os.ReadFile("changes.toml")
	if err != nil {
		t.Fatal(err)
	}
	var c struct {
		Changes []struct{ Key, Text, Anchor, Unlisted string } `toml:"changes"`
	}
	if err := toml.Unmarshal(b, &c); err != nil {
		t.Fatal(err)
	}
	keys := map[string]bool{}
	spec := spec121(t)
	for _, ch := range c.Changes {
		keys[ch.Key] = true
		switch {
		case ch.Anchor != "" && ch.Unlisted != "":
			t.Errorf("change %s: anchor and unlisted are exclusive", ch.Key)
		case ch.Anchor != "":
			if !strings.Contains(spec, ch.Anchor) {
				t.Errorf("change %s: anchor %q is not in spec 12.1", ch.Key, ch.Anchor)
			}
		case ch.Unlisted != "":
			t.Logf("change %s is not listed in spec 12.1: %s", ch.Key, ch.Unlisted)
		default:
			t.Errorf("change %s: needs an anchor in spec 12.1 (or an unlisted marker)", ch.Key)
		}
	}
	return keys
}

func testFuncs(t *testing.T) map[string]bool {
	t.Helper()
	names := map[string]bool{}
	files, _ := filepath.Glob("*_test.go")
	sub, _ := filepath.Glob("*/*_test.go")
	for _, f := range append(files, sub...) {
		af, err := parser.ParseFile(token.NewFileSet(), f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range af.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && fd.Recv == nil && strings.HasPrefix(fd.Name.Name, "Test") {
				names[fd.Name.Name] = true
			}
		}
	}
	return names
}

func TestMetaRegister(t *testing.T) {
	rows, keys, tests := loadRows(t), changeKeys(t), testFuncs(t)
	seen := map[string]bool{}
	for _, r := range rows {
		if !idRE.MatchString(r.ID) || seen[r.ID] {
			t.Errorf("%q: bad or duplicate id", r.ID)
		}
		seen[r.ID] = true
		if r.Desc == "" || r.Task == "" {
			t.Errorf("%s: desc and task are required", r.ID)
		}
		n := 0
		for _, set := range []bool{r.Todo, r.Ported != "", r.Changed != "", r.Dropped != ""} {
			if set {
				n++
			}
		}
		if n != 1 {
			t.Errorf("%s: needs exactly one of todo, ported, changed, dropped", r.ID)
		}
		name := r.Ported
		if r.Changed != "" {
			name = r.Test
			if !keys[r.Changed] {
				t.Errorf("%s: change key %q is not in changes.toml (spec 12.1)", r.ID, r.Changed)
			}
		}
		if name != "" {
			if !tests[name] {
				t.Errorf("%s: test %s does not exist", r.ID, name)
			}
			if m := testRE.FindStringSubmatch(name); m == nil || m[1] != r.ID {
				t.Errorf("%s: test %s must be named Test%s_...", r.ID, name, r.ID)
			}
		}
	}
	for name := range tests {
		if strings.HasPrefix(name, "TestMeta") || name == "TestMain" {
			continue
		}
		m := testRE.FindStringSubmatch(name)
		if m == nil {
			t.Errorf("%s: conformance tests are named Test<ID>_...", name)
		} else if !seen[m[1]] {
			t.Errorf("%s: id %s is not in behaviours.toml", name, m[1])
		}
	}
}

// TestMetaNoTodo fails on todo rows of the milestone given with -milestone, or earlier.
func TestMetaNoTodo(t *testing.T) {
	if *milestone == "" {
		t.Skip("run with -milestone=M1|M2|M3")
	}
	limit := strings.TrimPrefix(*milestone, "M")
	for _, r := range loadRows(t) {
		if r.Todo && strings.Split(r.Task, ".")[0] <= limit {
			t.Errorf("%s (task %s) is still todo", r.ID, r.Task)
		}
	}
}
