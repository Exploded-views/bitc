package gitref

import "testing"

func TestCloneURL(t *testing.T) {
	cases := map[string]string{
		"https://github.com/o/r":              "git@github.com:o/r.git",
		"https://github.com/o/r.git":          "git@github.com:o/r.git",
		"https://user:tok@github.com/o/r.git": "git@github.com:o/r.git",
		"git@github.com:o/r.git":              "git@github.com:o/r.git",
		"ssh://git:pw@github.com/o/r.git":     "ssh://git@github.com/o/r.git",
	}
	for in, want := range cases {
		got, err := CloneURL(in)
		if err != nil || got != want {
			t.Errorf("%s: %q %v", in, got, err)
		}
	}
	if _, err := CloneURL("https://git.example.com:8443/o/r"); err == nil {
		t.Error("a port must be ineligible (set clone_url)")
	}
	if RepoID("git@github.com:o/r.git") == RepoID("git@github.com:o/s.git") || len(RepoID("x")) != 16 {
		t.Error("repo id")
	}
	if !KnownHostsHas("github.com ssh-ed25519 AAAA\n", "github.com") || KnownHostsHas("github.com ssh-ed25519 AAAA\n", "gitlab.com") {
		t.Error("known hosts")
	}
}
