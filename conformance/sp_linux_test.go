// Linux-only: uses spritesfake.NewJob, which runs a Linux process tree and does not build elsewhere.

package conformance

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Exploded-views/bitc/conformance/harness"
	"github.com/Exploded-views/bitc/internal/provider/sprites"
	"github.com/Exploded-views/bitc/internal/provider/sprites/spritesfake"
)

// SP8 (client part): the network and resources policies are set and read back before the first
// exec, and the job policy is set and read back before the attach exec. The call order is the
// fake's record; the client refuses an exec out of order before sending it.
func TestSP8_PoliciesBeforeExec(t *testing.T) {
	f := spritesfake.New(t, spritesfake.Options{Bin: harness.Bin, Exec: func(spritesfake.ExecContext) int { return 0 }})
	c := spClient(t, f)
	ctx := context.Background()
	name := "bitc-a"
	if _, err := c.CreateSprite(ctx, name, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Run(ctx, name, sprites.ExecRequest{Cmd: []string{"true"}}, nil); !errors.Is(err, sprites.ErrNotProvisioned) {
		t.Fatalf("exec before the policies: %v", err)
	}
	build, _ := sprites.AllowDomains("github.com", "archive.ubuntu.com")
	if err := c.ApplyBuildPolicies(ctx, name, build, sprites.Resources{LimitMB: 4096}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Run(ctx, name, sprites.ExecRequest{Cmd: []string{"true"}}, nil); err != nil {
		t.Fatal(err)
	}
	job := spritesfake.NewJob(t, "true", "")
	if _, err := c.Attach(ctx, name, sprites.AttachOptions{AgentSHA: spSHA, Sudo: true, Payload: job.Frame}); !errors.Is(err, sprites.ErrNotProvisioned) {
		t.Fatalf("attach before the job policy: %v", err)
	}
	jobPol, _ := sprites.AllowDomains("github.com")
	if err := c.ApplyJobPolicy(ctx, name, jobPol); err != nil {
		t.Fatal(err)
	}
	s, err := c.Attach(ctx, name, sprites.AttachOptions{AgentSHA: spSHA, Sudo: true, Deadline: time.Now().Add(time.Hour), Payload: job.Frame})
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()

	var order []string
	for _, call := range f.Calls() {
		order = append(order, call.Method+" "+strings.TrimPrefix(call.Path, "/v1/sprites"))
	}
	want := []string{"POST ", "POST /bitc-a/policy/network", "GET /bitc-a/policy/network", "POST /bitc-a/policy/resources", "GET /bitc-a/policy/resources",
		"GET /bitc-a/exec", // the provisioning exec
		"POST /bitc-a/policy/network", "GET /bitc-a/policy/network",
		"GET /bitc-a/exec"} // the attach exec
	if strings.Join(order, "|") != strings.Join(want, "|") {
		t.Fatalf("call order:\n%s\nwant:\n%s", strings.Join(order, "\n"), strings.Join(want, "\n"))
	}
	if n, _ := f.Policies(name); strings.Contains(n, "ubuntu") {
		t.Fatalf("the build policy is still in force at the attach: %s", n)
	}
	// a policy the API did not take blocks every exec
	f2 := spritesfake.New(t, spritesfake.Options{})
	c2 := spClient(t, f2)
	_, _ = c2.CreateSprite(ctx, name, nil)
	f2.Behave("policy-get-mismatch")
	if err := c2.ApplyBuildPolicies(ctx, name, build, sprites.Resources{LimitMB: 4096}); err == nil {
		t.Fatal("a policy that reads back differently was accepted")
	}
	if _, err := c2.Exec(ctx, name, sprites.ExecRequest{Cmd: []string{"true"}}); !errors.Is(err, sprites.ErrNotProvisioned) {
		t.Fatalf("exec after a mismatch: %v", err)
	}
	for _, call := range f2.Calls() {
		if strings.HasSuffix(call.Path, "/exec") {
			t.Fatalf("an exec reached the API: %+v", call)
		}
	}
}
