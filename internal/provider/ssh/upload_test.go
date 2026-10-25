package ssh

import "testing"

func TestGitFloorLine(t *testing.T) {
	const cve = "CVE-2024-32002"
	cases := []struct {
		name, version, changelog, level, detail string
	}{
		{"exactly the floor", "git version 2.45.1", "", "ok", "git 2.45.1"},
		{"newer minor", "git version 2.46.0", "", "ok", "git 2.46.0"},
		{"newer major", "git version 3.0.0", "", "ok", "git 3.0.0"},
		{"with a vendor suffix", "git version 2.47.1 (Apple Git-155)", "", "ok", "git 2.47.1"},
		{"a fourth component", "git version 2.45.1.windows.1", "", "ok", "git 2.45.1"},
		{"one patch below", "git version 2.45.0", "", "fail", "git 2.45.0 is older than 2.45.1 and its package changelog does not name " + cve},
		{"no patch number", "git version 2.45", "", "fail", "git 2.45 is older than 2.45.1 and its package changelog does not name " + cve},
		{"a distribution backport", "git version 2.39.5", "git (1:2.39.5-0+deb12u1) fixes " + cve, "ok", "git 2.39.5 (the package changelog names " + cve + ")"},
		{"a changelog without the CVE", "git version 2.39.5", "CVE-2024-32004", "fail", "git 2.39.5 is older than 2.45.1 and its package changelog does not name " + cve},
		{"the changelog never lowers a newer git", "git version 2.46.0", "nothing", "ok", "git 2.46.0"},
		{"no version", "", cve, "fail", "the host did not report its git version; upload needs git 2.45.1 or later"},
		{"unparsable version", "git: command not found", cve, "fail", "the host did not report its git version; upload needs git 2.45.1 or later"},
	}
	for _, c := range cases {
		got := GitFloorLine(c.version, c.changelog)
		if got.Level != c.level || got.Check != "ssh-git" || got.Detail != c.detail {
			t.Errorf("%s: got %+v, want %s ssh-git %q", c.name, got, c.level, c.detail)
		}
	}
}
