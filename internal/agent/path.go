//go:build linux

package agent

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// agentPath is where the agent finds its own tools (git, tar, chown, apt-get). Stage 2 runs
// with an empty environment, so the agent never relies on its own PATH.
const agentPath = "/usr/sbin:/usr/bin:/sbin:/bin"

// lookIn finds name in the colon-separated dirs; a name with a slash is returned as is.
func lookIn(dirs, name string) (string, bool) {
	if strings.Contains(name, "/") {
		return name, true
	}
	for _, d := range strings.Split(dirs, ":") {
		if d == "" || !filepath.IsAbs(d) {
			continue
		}
		p := filepath.Join(d, name)
		if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() && st.Mode().Perm()&0o111 != 0 {
			return p, true
		}
	}
	return "", false
}

// agentCmd is exec.Command for one of the agent's own tools, looked up on agentPath and run
// with it, whatever the agent's own environment is.
func agentCmd(name string, args ...string) *exec.Cmd {
	c := exec.Command(name, args...)
	if p, ok := lookIn(agentPath, name); ok {
		c.Path, c.Err = p, nil
	}
	c.Env = []string{"PATH=" + agentPath}
	return c
}

// bindPath resolves c's program against the PATH in env (the job's), not the agent's own.
func bindPath(c *exec.Cmd, argv0 string, env []string) *exec.Cmd {
	dirs := ""
	for _, e := range env {
		if v, ok := strings.CutPrefix(e, "PATH="); ok {
			dirs = v
		}
	}
	if p, ok := lookIn(dirs, argv0); ok {
		c.Path, c.Err = p, nil
	}
	return c
}
