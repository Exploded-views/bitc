package config

import "testing"

func TestMatch(t *testing.T) {
	cases := []struct {
		pat, name string
		want      bool
	}{
		{"**/worktrees/agent-*", "/home/u/src/app/worktrees/agent-7", true},
		{"**/worktrees/agent-*", "/home/u/src/app/worktrees/other", false},
		{"**/src/app*", "/home/u/src/app-2", true},
		{"/srv/*/repo", "/srv/a/repo", true},
		{"/srv/*/repo", "/srv/a/b/repo", false},
		{"/srv/**/repo", "/srv/a/b/repo", true},
		{"/srv/**", "/srv", true},
		{"**", "/anything/at/all", true},
		{"/x/[ab]", "/x/a", true},
	}
	for _, c := range cases {
		got, err := Match(c.pat, c.name)
		if err != nil || got != c.want {
			t.Errorf("Match(%q, %q) = %v %v, want %v", c.pat, c.name, got, err, c.want)
		}
	}
	if err := ValidGlob("/x/[a"); err == nil {
		t.Error("bad pattern accepted")
	}
}
