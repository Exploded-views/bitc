// Package transport is the git-push code transport stage (spec 8.4).
package transport

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/Exploded-views/bitc/internal/config"
	"github.com/Exploded-views/bitc/internal/provider"
	"github.com/Exploded-views/bitc/internal/remote"
	"github.com/Exploded-views/bitc/internal/remote/gitref"
)

func init() {
	remote.Transport = Push
	remote.CloneURLCheck = check
}

// check: a usable clone URL whose host is in transport.known_hosts (spec 8.4 preflight).
func check(j *remote.Job) error {
	if j.Core.Transport.Kind == config.TransportUpload {
		return nil // the commit travels on the provider's own session: no clone URL, host or forge
	}
	clone := ""
	if j.P.Eff.Grant != nil {
		clone = j.P.Eff.Grant.CloneURL
	}
	if clone == "" {
		out, err := exec.Command("git", "-C", j.P.Eff.Worktree, "remote", "get-url", j.Core.Transport.Remote).Output()
		if err != nil {
			return &provider.ErrIneligible{Why: "no git remote " + j.Core.Transport.Remote}
		}
		raw := strings.TrimSpace(string(out))
		if j.P.Eff.Test != nil && strings.HasPrefix(raw, "/") { // a local bare repo, tests only
			j.Spec.CloneURL, j.Spec.RepoID = raw, gitref.RepoID(raw)
			return nil
		}
		if clone, err = gitref.CloneURL(raw); err != nil {
			return &provider.ErrIneligible{Why: gitref.RedactURL(err.Error())}
		}
	}
	if !gitref.KnownHostsHas(j.Core.Transport.KnownHosts, gitref.HostOf(clone)) {
		return &provider.ErrIneligible{Why: "the clone host " + gitref.HostOf(clone) + " is not in transport.known_hosts"}
	}
	j.Spec.CloneURL, j.Spec.RepoID = clone, gitref.RepoID(clone)
	return nil
}

// Push pushes exactly the resolved HEAD sha to a fresh hidden ref and returns a cleanup that deletes it from a
// detached process (spec 8.3, 8.4).
func Push(ctx context.Context, j *remote.Job) (func(), error) {
	if j.Core.Transport.Kind == config.TransportUpload {
		return upload(j)
	}
	if j.Spec.CloneURL == "" { // preflight normally ran check already
		if err := check(j); err != nil {
			return nil, err
		}
	}
	wt := j.P.Eff.Worktree
	sha, err := exec.Command("git", "-C", wt, "rev-parse", "HEAD").Output()
	if err != nil {
		return nil, &provider.ErrIneligible{Why: "no HEAD commit"}
	}
	j.Spec.Sha = strings.TrimSpace(string(sha))
	r := make([]byte, 4)
	_, _ = rand.Read(r)
	stamp := fmt.Sprintf("%d-%s", time.Now().Unix(), hex.EncodeToString(r))
	ref := fmt.Sprintf("%s%s/%s/%s-%s", j.Core.Transport.RefPrefix, j.Spec.Install, j.Spec.Pool, j.Spec.Sha[:12], stamp)
	j.Spec.Ref = ref
	gitArgs := []string{"-C", wt, "push", "-q", j.Core.Transport.Remote, j.Spec.Sha + ":" + ref}
	refs := []string{ref}
	if b := j.Spec.AgentBuild; b != nil {
		// source = "build": the pinned agent commit rides on its own hidden ref, unique to this job so
		// a concurrent job's cleanup cannot delete it; the pod fetches that ref (spec 8.4 step 1).
		// Shaped like a job ref so crash recovery (sweep) ages it out too.
		if exec.Command("git", "-C", wt, "cat-file", "-e", b.Commit+"^{commit}").Run() != nil {
			return nil, &provider.ErrIneligible{Why: "agent commit " + b.Commit[:12] + " is not in this work tree's repository"}
		}
		b.Ref = fmt.Sprintf("%s%s/%s/%s-%s", gitref.AgentRefPrefix, j.Spec.Install, j.Spec.Pool, b.Commit[:12], stamp)
		gitArgs = append(gitArgs, b.Commit+":"+b.Ref)
		refs = append(refs, b.Ref)
	}
	var push *exec.Cmd
	if _, ok := j.P.Eff.Pools["git"]; ok {
		// The push takes turns with other git work in the git pool (spec 8.4): bitc runs itself
		// as `bitc run git -- git push ...` with the caller's environment.
		self, err := os.Executable()
		if err != nil {
			return nil, &provider.ErrTransient{Why: "could not push the commit: " + err.Error()}
		}
		push = exec.CommandContext(ctx, self, append([]string{"run", "git", "--", "git"}, gitArgs...)...)
		push.Env = append(append([]string{}, j.Inv.Env...), "GIT_TERMINAL_PROMPT=0")
		push.Dir = j.P.Eff.Cwd
	} else {
		push = exec.CommandContext(ctx, "git", gitArgs...)
		push.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	}
	if out, err := push.CombinedOutput(); err != nil {
		return nil, &provider.ErrTransient{Why: "could not push the commit: " + gitref.RedactURL(gitref.FirstLine(string(out)))}
	}
	return func() {
		for _, r := range refs {
			gitref.DeleteRefDetached(wt, j.Core.Transport.Remote, r)
		}
	}, nil
}

// upload is the transport of kind "upload" (ssh only): nothing is pushed and no ref exists on any
// remote. It only pins the commit; the provider bundles it and sends it on its own session.
func upload(j *remote.Job) (func(), error) {
	sha, err := exec.Command("git", "-C", j.P.Eff.Worktree, "rev-parse", "HEAD").Output()
	if err != nil {
		return nil, &provider.ErrIneligible{Why: "no HEAD commit"}
	}
	j.Spec.Sha = strings.TrimSpace(string(sha))
	j.Spec.Ref = provider.UploadRef(j.Spec.Job)
	return func() {}, nil
}
