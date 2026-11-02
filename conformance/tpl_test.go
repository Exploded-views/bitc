package conformance

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The TPL rows: the GitHub issue forms in .github/ISSUE_TEMPLATE. go.mod has no YAML parser, so
// like wflint_test.go this reads the forms line by line; the forms keep to the simple layout
// GitHub documents (top-level name, description, labels, body; one "- type:" per body item).

var (
	tplTypes   = map[string]bool{"markdown": true, "textarea": true, "input": true, "dropdown": true, "checkboxes": true}
	tplLabels  = map[string]bool{"bug": true, "friction": true, "docs": true, "dogfood": true, "pool": true}
	tplItemRE  = regexp.MustCompile(`(?m)^  - type:\s*(\S+)\s*$`)
	tplIDRE    = regexp.MustCompile(`(?m)^    id:\s*(\S+)\s*$`)
	tplRendRE  = regexp.MustCompile(`(?m)^      render:\s*(\S+)\s*$`)
	tplReqRE   = regexp.MustCompile(`required:\s*true`)
	tplFlowRE  = regexp.MustCompile(`(?m)^labels:\s*\[(.*)\]\s*$`)
	tplBlockRE = regexp.MustCompile(`(?m)^labels:\s*$((?:\n\s+-\s+\S+)+)`)
)

type tplItem struct{ typ, id, render, text string }

type tplForm struct {
	file, text string
	labels     []string
	items      []tplItem
}

func tplLoad(t *testing.T, name string) tplForm {
	t.Helper()
	f := filepath.Join("..", ".github", "ISSUE_TEMPLATE", name)
	b, err := os.ReadFile(f)
	if err != nil {
		t.Errorf("%s: %v", name, err)
		return tplForm{file: name}
	}
	form := tplForm{file: name, text: string(b)}
	if m := tplFlowRE.FindStringSubmatch(form.text); m != nil {
		for _, l := range strings.Split(m[1], ",") {
			form.labels = append(form.labels, strings.Trim(strings.TrimSpace(l), `"'`))
		}
	} else if m := tplBlockRE.FindStringSubmatch(form.text); m != nil {
		for _, l := range strings.Split(m[1], "\n") {
			if l = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(l), "-")); l != "" {
				form.labels = append(form.labels, strings.Trim(l, `"'`))
			}
		}
	}
	idx := tplItemRE.FindAllStringSubmatchIndex(form.text, -1)
	for i, ix := range idx {
		end := len(form.text)
		if i+1 < len(idx) {
			end = idx[i+1][0]
		}
		it := tplItem{typ: form.text[ix[2]:ix[3]], text: form.text[ix[0]:end]}
		if m := tplIDRE.FindStringSubmatch(it.text); m != nil {
			it.id = m[1]
		}
		if m := tplRendRE.FindStringSubmatch(it.text); m != nil {
			it.render = m[1]
		}
		form.items = append(form.items, it)
	}
	return form
}

func (f tplForm) item(id string) (tplItem, bool) {
	for _, it := range f.items {
		if it.id == id {
			return it, true
		}
	}
	return tplItem{}, false
}

func tplForms(t *testing.T) []tplForm {
	t.Helper()
	files, err := filepath.Glob("../.github/ISSUE_TEMPLATE/*.yml")
	if err != nil || len(files) == 0 {
		t.Fatalf("no issue templates found: %v", err)
	}
	var out []tplForm
	for _, f := range files {
		if n := filepath.Base(f); n != "config.yml" {
			out = append(out, tplLoad(t, n))
		}
	}
	return out
}

func TestTPL1_EveryTemplateParsesAsAnIssueForm(t *testing.T) {
	for _, f := range tplForms(t) {
		for _, key := range []string{"name", "description", "body"} {
			if !regexp.MustCompile(`(?m)^` + key + `:`).MatchString(f.text) {
				t.Errorf("%s: no top-level %s:", f.file, key)
			}
		}
		if len(f.items) == 0 {
			t.Errorf("%s: body has no items", f.file)
		}
		ids := map[string]bool{}
		inputs := 0
		for _, it := range f.items {
			if !tplTypes[it.typ] {
				t.Errorf("%s: unknown body type %q", f.file, it.typ)
			}
			if it.typ == "markdown" {
				continue
			}
			inputs++
			if it.id == "" {
				t.Errorf("%s: a %s item has no id", f.file, it.typ)
			} else if ids[it.id] {
				t.Errorf("%s: duplicate id %q", f.file, it.id)
			}
			ids[it.id] = true
			if !strings.Contains(it.text, "label:") {
				t.Errorf("%s: item %q has no label", f.file, it.id)
			}
		}
		if inputs == 0 {
			t.Errorf("%s: no input fields", f.file)
		}
	}
}

func TestTPL2_LabelsAreOnlyTheFiveThatExist(t *testing.T) {
	for _, f := range tplForms(t) {
		if len(f.labels) == 0 {
			t.Errorf("%s: no labels", f.file)
		}
		for _, l := range f.labels {
			if !tplLabels[l] {
				t.Errorf("%s: label %q is not one of bug, friction, docs, dogfood, pool", f.file, l)
			}
		}
	}
}

func tplNeed(t *testing.T, f tplForm, ids ...string) {
	t.Helper()
	for _, id := range ids {
		it, ok := f.item(id)
		if !ok {
			t.Errorf("%s: no field with id %q", f.file, id)
		} else if !tplReqRE.MatchString(it.text) {
			t.Errorf("%s: field %q is not required", f.file, id)
		}
	}
}

func TestTPL3_BugAndFrictionFormsRequireTheirFields(t *testing.T) {
	bug := tplLoad(t, "bug.yml")
	tplNeed(t, bug, "version", "os", "command", "happened")
	if len(bug.labels) != 1 || bug.labels[0] != "bug" {
		t.Errorf("bug.yml labels = %q, want [bug]", bug.labels)
	}
	if it, ok := bug.item("command"); ok && it.render != "shell" {
		t.Errorf("bug.yml command renders as %q, want shell", it.render)
	}
	fr := tplLoad(t, "friction.yml")
	tplNeed(t, fr, "goal", "slowed")
	if len(fr.labels) != 1 || fr.labels[0] != "friction" {
		t.Errorf("friction.yml labels = %q, want [friction]", fr.labels)
	}
}

func TestTPL4_ConfigKeepsBlankIssuesOffAndTheSecurityAndDocsLinks(t *testing.T) {
	b := string(must(os.ReadFile("../.github/ISSUE_TEMPLATE/config.yml")))
	for _, want := range []string{
		"blank_issues_enabled: false",
		"https://github.com/Exploded-views/bitc/security/advisories/new",
		"https://github.com/Exploded-views/bitc/blob/main/",
	} {
		if !strings.Contains(b, want) {
			t.Errorf("config.yml lacks %q", want)
		}
	}
}

func TestTPL5_OutputAndConfigFieldsCarryARedactNote(t *testing.T) {
	n := 0
	for _, f := range tplForms(t) {
		for _, it := range f.items {
			if it.typ != "textarea" || (it.render != "text" && it.render != "toml") {
				continue
			}
			n++
			if !strings.Contains(strings.ToLower(it.text), "redact") {
				t.Errorf("%s: field %q collects output or config but has no redact note", f.file, it.id)
			}
		}
	}
	if n < 2 {
		t.Errorf("found %d output/config fields, want at least 2 (bug.yml output and config)", n)
	}
}
