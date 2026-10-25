package config

import (
	"strings"
	"testing"
)

func TestExpand(t *testing.T) {
	v := Vars{"slot": "2", "db_port_base": "47410", "pool": "db", "name": "x", "two": "2"}
	cases := map[string]string{
		"postgresql://localhost:{slot+db_port_base}/demo": "postgresql://localhost:47412/demo",
		"{pool}.{slot}":      "db.2",
		"{slot-1}":           "1",
		"{slot+10}":          "12",
		"{{literal}} {pool}": "{literal} db",
		"plain":              "plain",
		"":                   "",
		"{two+slot}":         "4",
	}
	for raw, want := range cases {
		got, err := Template{Raw: raw}.Expand(v)
		if err != nil || got != want {
			t.Errorf("%q: %q %v, want %q", raw, got, err, want)
		}
	}
}

func TestTemplateErrors(t *testing.T) {
	v := Vars{"slot": "1", "name": "x"}
	for raw, frag := range map[string]string{
		"{nope}":         "unknown template variable nope",
		"{name+1}":       "not an integer",
		"{slot+x1}":      "unknown template variable x1",
		"{slot":          "unclosed",
		"x}":             "unmatched }",
		"{}":             "empty",
		"{slot*2}":       "malformed",
		"{slot+01}":      "malformed",
		"{sha256:a.txt}": "remote-only",
	} {
		_, err := Template{Raw: raw}.Expand(v)
		if err == nil || !strings.Contains(err.Error(), frag) {
			t.Errorf("%q: %v, want %q", raw, err, frag)
		}
	}
}

func TestTemplateNames(t *testing.T) {
	names, err := Template{Raw: "{slot+db_port_base}/{pool}/{sha256:package-lock.json}"}.Names()
	if err != nil || strings.Join(names, ",") != "slot,db_port_base,pool,sha256" {
		t.Fatalf("%v %v", names, err)
	}
}

func FuzzTemplate(f *testing.F) {
	for _, s := range []string{"{a}", "{a+1}", "{{", "}}", "{sha256:x}", "{a-b}", "x{y}z"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		parts, err := ParseTemplate(raw)
		if err != nil {
			return
		}
		vars := Vars{}
		for _, p := range parts {
			if p.Var != "" {
				vars[p.Var] = "1"
			}
			if p.Arg != "" && !intRE.MatchString(p.Arg) {
				vars[p.Arg] = "1"
			}
		}
		_, _ = Template{Raw: raw}.Expand(vars) // must not panic
	})
}

func FuzzParseDuration(f *testing.F) {
	for _, s := range []string{"1h30m", "0s", "05s", "1.5h"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		d, err := ParseDuration(s)
		if err == nil && d < 0 {
			t.Fatalf("%q gave negative %v", s, d)
		}
	})
}
