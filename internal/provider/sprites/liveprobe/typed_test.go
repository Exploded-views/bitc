package liveprobe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"math"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Exploded-views/bitc/internal/provider"
	"github.com/Exploded-views/bitc/internal/provider/sprites"
	"github.com/Exploded-views/bitc/internal/provider/sprites/spritesfake"
)

// ---- typed writes and reads on the Sprite book

// typedCall is one typed API call on a Sprite name.
type typedCall struct {
	method string
	call   func(ctx context.Context, e *Env, name string) error
}

func closeSession(s *sprites.Session, err error) error {
	if err == nil {
		_, _ = s.Wait()
		_ = s.Close()
	}
	return err
}

func runReq(d time.Duration) sprites.ExecRequest {
	return sprites.ExecRequest{Cmd: []string{"true"}, MaxRunAfterDisconnect: d}
}

func typedWrites() []typedCall {
	return []typedCall{
		{"ApplyBuildPolicies", func(ctx context.Context, e *Env, n string) error {
			p, err := sprites.AllowDomains("registry.npmjs.org")
			if err != nil {
				return err
			}
			return e.API.ApplyBuildPolicies(ctx, n, p, sprites.Resources{LimitMB: 512})
		}},
		{"ApplyJobPolicy", func(ctx context.Context, e *Env, n string) error {
			p, err := sprites.AllowDomains("registry.npmjs.org")
			if err != nil {
				return err
			}
			return e.API.ApplyJobPolicy(ctx, n, p)
		}},
		{"Run", func(ctx context.Context, e *Env, n string) error {
			_, err := e.API.Run(ctx, n, runReq(0), nil)
			return err
		}},
		{"Exec", func(ctx context.Context, e *Env, n string) error {
			s, err := e.API.Exec(ctx, n, runReq(0))
			return closeSession(s, err)
		}},
		{"CreateCheckpoint", func(ctx context.Context, e *Env, n string) error {
			_, err := e.API.CreateCheckpoint(ctx, n, "c")
			return err
		}},
		{"RestoreCheckpoint", func(ctx context.Context, e *Env, n string) error {
			return e.API.RestoreCheckpoint(ctx, n, "cp-1")
		}},
		{"DeleteCheckpoint", func(ctx context.Context, e *Env, n string) error {
			return e.API.DeleteCheckpoint(ctx, n, "cp-1")
		}},
		{"FSWrite", func(ctx context.Context, e *Env, n string) error {
			return e.API.FSWrite(ctx, n, "/var/tmp/typed.txt", []byte("x"))
		}},
	}
}

func typedReads() []typedCall {
	return []typedCall{
		{"GetSprite", func(ctx context.Context, e *Env, n string) error {
			_, err := e.API.GetSprite(ctx, n)
			return err
		}},
		{"FSRead", func(ctx context.Context, e *Env, n string) error {
			_, err := e.API.FSRead(ctx, n, "/etc/hostname", io.Discard)
			return err
		}},
		{"ListCheckpoints", func(ctx context.Context, e *Env, n string) error {
			_, err := e.API.ListCheckpoints(ctx, n)
			return err
		}},
		{"GetCheckpoint", func(ctx context.Context, e *Env, n string) error {
			_, err := e.API.GetCheckpoint(ctx, n, "cp-1")
			return err
		}},
	}
}

// Targets of a typed call.
const (
	tLive    = "the live Sprite"
	tUnknown = "an unknown name"
	tDeleted = "a deleted known name"
	tOther   = "a Sprite of another run"
)

// typedTarget makes the name a case calls on. It runs in the probe; done cleans up what it made.
func typedTarget(ctx context.Context, e *Env, r *rig, target string) (name string, done func(), err error) {
	done = func() {}
	switch target {
	case tLive:
		return e.Sprite, done, nil
	case tUnknown:
		return notInBook, done, nil
	case tDeleted:
		name = e.NextName()
		if _, err = freeSlot(ctx, e, name, 200, 300); err != nil {
			return "", done, err
		}
		if err = e.API.DeleteSprite(ctx, name); err != nil {
			return "", done, err
		}
		return name, done, nil
	case tOther:
		name = NamePrefix + "zz-1"
		if _, err = r.c.CreateSprite(ctx, name, nil); err != nil {
			return "", done, err
		}
		return name, func() { _ = r.c.DeleteSprite(ctx, name) }, nil
	}
	return "", done, fmt.Errorf("unknown target %q", target)
}

// runTypedCase runs one typed call on one target and checks it either reached the fake (the probe
// answers) or was refused (nothing sent, ErrIneligible, the probe ends Error naming the method).
func runTypedCase(t *testing.T, c typedCall, target string, wantSent bool) {
	t.Helper()
	r := newRig(t, nil)
	var callErr, setupErr error
	var sent int
	rep := r.runner(quick(1, gRO, time.Minute, func(ctx context.Context, e *Env) Outcome {
		name, done, err := typedTarget(ctx, e, r, target)
		defer done()
		if err != nil {
			setupErr = err
			return fine(ctx, e)
		}
		before := len(r.f.Calls())
		callErr = c.call(ctx, e, name)
		sent = len(r.f.Calls()) - before
		return fine(ctx, e)
	})).Run(context.Background())
	if setupErr != nil {
		t.Fatalf("setup: %v", setupErr)
	}
	res := result(t, rep.Report, 1)
	if wantSent {
		if sent < 1 {
			t.Errorf("%s on %s: nothing reached the fake", c.method, target)
		}
		if res.Status != Answered || rep.Exit != ExitOK {
			t.Errorf("%s on %s: probe %q %q exit %d, want Answered and ExitOK", c.method, target, res.Status, res.Finding, rep.Exit)
		}
		var inel *provider.ErrIneligible
		if errors.As(callErr, &inel) && strings.HasPrefix(inel.Why, "guard:") {
			t.Errorf("%s on %s: refused by the guard: %v", c.method, target, callErr)
		}
		return
	}
	var inel *provider.ErrIneligible
	if sent != 0 {
		t.Errorf("%s on %s: %d request(s) reached the fake, want none", c.method, target, sent)
	}
	if !errors.As(callErr, &inel) || !strings.HasPrefix(inel.Why, "guard: "+c.method) {
		t.Errorf("%s on %s: error %v, want a guard ErrIneligible naming the method", c.method, target, callErr)
	}
	if res.Status != Error || !strings.Contains(res.Finding, "guard refused "+c.method) {
		t.Errorf("%s on %s: probe %q %q, want Error with the method in the finding", c.method, target, res.Status, res.Finding)
	}
	if rep.Exit != ExitProbe {
		t.Errorf("%s on %s: exit %d, want %d", c.method, target, rep.Exit, ExitProbe)
	}
	if got := r.f.Live(); len(got) != 0 {
		t.Errorf("Sprites left: %v", got)
	}
}

func TestTypedWritesNeedALiveSpriteOfTheRun(t *testing.T) {
	for _, c := range typedWrites() {
		for _, target := range []string{tLive, tUnknown, tDeleted, tOther} {
			t.Run(c.method+" on "+target, func(t *testing.T) { runTypedCase(t, c, target, target == tLive) })
		}
	}
}

func TestTypedReadsNeedAKnownOrLiveSprite(t *testing.T) {
	// ListCheckpoints and GetCheckpoint may read a deleted name the run made (probe 25 needs it);
	// GetSprite and FSRead may wake a Sprite, so they need a live one.
	reach := map[string]map[string]bool{
		tLive:    {"GetSprite": true, "FSRead": true, "ListCheckpoints": true, "GetCheckpoint": true},
		tDeleted: {"ListCheckpoints": true, "GetCheckpoint": true},
	}
	for _, c := range typedReads() {
		for _, target := range []string{tLive, tDeleted, tUnknown, tOther} {
			t.Run(c.method+" on "+target, func(t *testing.T) { runTypedCase(t, c, target, reach[target][c.method]) })
		}
	}
}

func TestRunAndExecCapMaxRunAfterDisconnect(t *testing.T) {
	over := runReq(31 * time.Second)
	cases := []struct {
		method string
		call   func(ctx context.Context, e *Env, req sprites.ExecRequest) error
	}{
		{"Run", func(ctx context.Context, e *Env, req sprites.ExecRequest) error {
			_, err := e.API.Run(ctx, e.Sprite, req, nil)
			return err
		}},
		{"Exec", func(ctx context.Context, e *Env, req sprites.ExecRequest) error {
			s, err := e.API.Exec(ctx, e.Sprite, req)
			return closeSession(s, err)
		}},
	}
	for _, c := range cases {
		t.Run(c.method+" at 30s", func(t *testing.T) {
			r := newRig(t, nil)
			var sent int
			rep := r.runner(quick(1, gRO, time.Minute, func(ctx context.Context, e *Env) Outcome {
				before := len(r.f.Calls())
				_ = c.call(ctx, e, runReq(30*time.Second))
				sent = len(r.f.Calls()) - before
				return fine(ctx, e)
			})).Run(context.Background())
			if res := result(t, rep.Report, 1); sent < 1 || res.Status != Answered {
				t.Errorf("sent %d, probe %+v", sent, res)
			}
		})
		t.Run(c.method+" at 31s", func(t *testing.T) {
			r := newRig(t, nil)
			var sent int
			var err error
			rep := r.runner(quick(1, gRO, time.Minute, func(ctx context.Context, e *Env) Outcome {
				before := len(r.f.Calls())
				err = c.call(ctx, e, over)
				sent = len(r.f.Calls()) - before
				return fine(ctx, e)
			})).Run(context.Background())
			var inel *provider.ErrIneligible
			if sent != 0 || !errors.As(err, &inel) {
				t.Errorf("sent %d, error %v, want nothing sent and ErrIneligible", sent, err)
			}
			if res := result(t, rep.Report, 1); res.Status != Error || !strings.Contains(res.Finding, "guard refused "+c.method) {
				t.Errorf("probe %+v", res)
			}
		})
	}
}

// ---- no silent promotion: every name-taking API method has a guarded override

// apiMethods parses types.go: the number of API methods and the names of those whose first
// parameter after ctx is called name.
func apiMethods(t *testing.T) (total int, named []string) {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), "types.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	ast.Inspect(f, func(n ast.Node) bool {
		ts, ok := n.(*ast.TypeSpec)
		if !ok || ts.Name.Name != "API" {
			return true
		}
		it, ok := ts.Type.(*ast.InterfaceType)
		if !ok {
			t.Fatal("API is not an interface")
		}
		for _, m := range it.Methods.List {
			ft, ok := m.Type.(*ast.FuncType)
			if !ok || len(m.Names) != 1 {
				t.Fatalf("API has an entry that is not a plain method: %v", m.Names)
			}
			total++
			var params []string
			for _, p := range ft.Params.List {
				if len(p.Names) == 0 {
					params = append(params, "")
				}
				for _, pn := range p.Names {
					params = append(params, pn.Name)
				}
			}
			if len(params) >= 2 && params[1] == "name" {
				named = append(named, m.Names[0].Name)
			}
		}
		return false
	})
	return total, named
}

// guardedMethods parses runner.go: the methods declared with receiver guarded.
func guardedMethods(t *testing.T) map[string]bool {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), "runner.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Recv == nil || len(fd.Recv.List) != 1 {
			continue
		}
		typ := fd.Recv.List[0].Type
		if st, ok := typ.(*ast.StarExpr); ok {
			typ = st.X
		}
		if id, ok := typ.(*ast.Ident); ok && id.Name == "guarded" {
			got[fd.Name.Name] = true
		}
	}
	return got
}

func TestEveryNameTakingAPIMethodHasAGuardedOverride(t *testing.T) {
	total, named := apiMethods(t)
	// The note says 19 methods and 15 name-taking at d5e8611e. The API interface has 21 methods
	// (Org and Secrets take no parameters and are two of them), 15 of them name-taking.
	if total != 21 || len(named) != 15 {
		t.Fatalf("the API interface has %d methods, %d of them name-taking (%v); a new method needs a look at this test", total, len(named), named)
	}
	have := guardedMethods(t)
	sort.Strings(named)
	for _, m := range named {
		if !have[m] {
			t.Errorf("API.%s takes a Sprite name and guarded has no override: it is promoted unguarded", m)
		}
	}
	for _, m := range []string{"ListSprites", "Raw"} {
		for _, n := range named {
			if n == m {
				t.Errorf("%s must not count as name-taking", m)
			}
		}
	}
}

// ---- table-write bodies

func putSprite(body any) func(e *Env, x string) rawReq {
	return func(e *Env, x string) rawReq { return rawReq{"PUT", sprite(e, ""), nil, body} }
}

func patchSprite(body any) func(e *Env, x string) rawReq {
	return func(e *Env, x string) rawReq { return rawReq{"PATCH", sprite(e, ""), nil, body} }
}

func postPriv(body any) func(e *Env, x string) rawReq {
	return func(e *Env, x string) rawReq { return rawReq{"POST", sprite(e, "/policy/privileges"), nil, body} }
}

func postRes(body any) func(e *Env, x string) rawReq {
	return func(e *Env, x string) rawReq { return rawReq{"POST", sprite(e, "/policy/resources"), nil, body} }
}

func mem(m map[string]any) map[string]any {
	return map[string]any{"memory": m}
}

func cfg(c map[string]any) map[string]any {
	return map[string]any{"config": c}
}

func lbl(l map[string]any) map[string]any {
	return map[string]any{"labels": l}
}

func kv(k string, v any) map[string]any {
	return map[string]any{k: v}
}

func two(a, b string, x, y any) map[string]any {
	return map[string]any{a: x, b: y}
}

func TestTableBodiesTheProbesSendAreAccepted(t *testing.T) {
	cases := []struct {
		name string
		req  func(e *Env, x string) rawReq
	}{
		{"PUT labels", putSprite(lbl(kv("bitc", "2")))},
		{"PATCH labels", patchSprite(lbl(kv("bitc", "2")))},
		{"PUT labels as a string map", putSprite(map[string]any{"labels": map[string]string{"bitc": "2"}})},
		{"PUT config", putSprite(cfg(two("ram_mb", "cpus", 2048, 2)))},
		{"PUT config below the caps", putSprite(cfg(two("ram_mb", "cpus", 1, 1)))},
		{"PUT labels and config", putSprite(two("labels", "config", kv("bitc", "1"), kv("cpus", 1)))},
		{"privileges profile noNewPrivileges", postPriv(kv("profile", "noNewPrivileges"))},
		{"privileges profile minimal", postPriv(kv("profile", "minimal"))},
		{"privileges mode noNewPrivileges", postPriv(kv("mode", "noNewPrivileges"))},
		{"privileges mode minimal", postPriv(kv("mode", "minimal"))},
		{"privileges as a string map", postPriv(map[string]string{"profile": "minimal"})},
		{"resources 1024", postRes(mem(two("limit_mb", "autoscale", 1024, false)))},
		{"resources 16384", postRes(mem(two("limit_mb", "autoscale", 16384, false)))},
		{"resources 1", postRes(mem(two("limit_mb", "autoscale", 1, false)))},
		{"PUT a 16-character label", putSprite(lbl(kv("bitc", strings.Repeat("v", 16))))},
		{"PUT a 1-character label", putSprite(lbl(kv("bitc", "1")))},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newRig(t, nil)
			var sent int
			var q rawReq
			rep := r.runner(quick(1, gRO, time.Minute, func(ctx context.Context, e *Env) Outcome {
				q = c.req(e, "")
				before := len(r.f.Calls())
				q.send(ctx, e)
				sent = len(r.f.Calls()) - before
				return fine(ctx, e)
			})).Run(context.Background())
			if sent != 1 {
				t.Errorf("%s %s: %d request(s) reached the fake, want 1", q.method, q.path, sent)
			}
			if res := result(t, rep.Report, 1); res.Status != Answered || rep.Exit != ExitOK {
				t.Errorf("probe %+v exit %d", res, rep.Exit)
			}
		})
	}
}

func TestTableBodiesAreRefused(t *testing.T) {
	big := 1 << 62
	cases := []refusalCase{
		// PUT and PATCH /v1/sprites/{s}
		{name: "PUT an extra top-level key", req: putSprite(two("labels", "extra", kv("bitc", "1"), 1))},
		{name: "PUT only an extra key", req: putSprite(kv("extra", 1))},
		{name: "PUT name", req: putSprite(kv("name", notInBook))},
		{name: "PUT name beside config", req: putSprite(two("config", "name", kv("cpus", 1), notInBook))},
		{name: "PUT organization", req: putSprite(kv("organization", "other-org"))},
		{name: "PATCH id", req: patchSprite(kv("id", "sp1"))},
		{name: "PUT a label key other than bitc", req: putSprite(lbl(kv("owner", "x")))},
		{name: "PUT two label keys", req: putSprite(lbl(two("bitc", "owner", "1", "x")))},
		{name: "PUT a label value of 17 characters", req: putSprite(lbl(kv("bitc", strings.Repeat("v", 17))))},
		{name: "PUT a label value that is a number", req: putSprite(lbl(kv("bitc", 1)))},
		{name: "PUT empty labels", req: putSprite(lbl(map[string]any{}))},
		{name: "PUT empty config", req: putSprite(cfg(map[string]any{}))},
		{name: "PUT an extra config key", req: putSprite(cfg(two("cpus", "disk_gb", 1, 10)))},
		{name: "PUT ram_mb plus one", req: putSprite(cfg(kv("ram_mb", 2049)))},
		{name: "PUT cpus plus one", req: putSprite(cfg(kv("cpus", 3)))},
		{name: "PUT ram_mb 0", req: putSprite(cfg(kv("ram_mb", 0)))},
		{name: "PUT cpus 0", req: putSprite(cfg(kv("cpus", 0)))},
		{name: "PUT ram_mb -1", req: putSprite(cfg(kv("ram_mb", -1)))},
		{name: "PUT cpus -1", req: putSprite(cfg(kv("cpus", -1)))},
		{name: "PUT ram_mb 1.5", req: putSprite(cfg(kv("ram_mb", 1.5)))},
		{name: "PUT cpus 1.5", req: putSprite(cfg(kv("cpus", 1.5)))},
		{name: "PUT ram_mb as a string", req: putSprite(cfg(kv("ram_mb", "2048")))},
		{name: "PUT ram_mb NaN", req: putSprite(cfg(kv("ram_mb", math.NaN())))},
		{name: "PUT ram_mb infinite", req: putSprite(cfg(kv("ram_mb", math.Inf(1))))},
		{name: "PUT ram_mb huge", req: putSprite(cfg(kv("ram_mb", big)))},
		{name: "PUT {}", req: putSprite(map[string]any{})},
		{name: "PUT nil body", req: putSprite(nil)},
		{name: "PUT a string body", req: putSprite("x")},
		{name: "PUT an array body", req: putSprite([]any{kv("labels", 1)})},
		{name: "PATCH {}", req: patchSprite(map[string]any{})},
		{name: "PUT labels that is not an object", req: putSprite(kv("labels", "bitc"))},
		{name: "PUT config that is not an object", req: putSprite(kv("config", 1))},
		// POST .../policy/privileges
		{name: "privileges value not on the list", req: postPriv(kv("profile", "root"))},
		{name: "privileges mode not on the list", req: postPriv(kv("mode", "full"))},
		{name: "privileges value in the wrong case", req: postPriv(kv("profile", "Minimal"))},
		{name: "privileges two keys", req: postPriv(two("profile", "mode", "minimal", "minimal"))},
		{name: "privileges an extra key", req: postPriv(two("profile", "name", "minimal", notInBook))},
		{name: "privileges another key", req: postPriv(kv("level", "minimal"))},
		{name: "privileges a non-string value", req: postPriv(kv("profile", true))},
		{name: "privileges {}", req: postPriv(map[string]any{})},
		{name: "privileges nil body", req: postPriv(nil)},
		{name: "privileges a string body", req: postPriv("minimal")},
		// POST .../policy/resources
		{name: "resources an extra top-level key", req: postRes(two("memory", "name", two("limit_mb", "autoscale", 1024, false), notInBook))},
		{name: "resources only an extra key", req: postRes(kv("cpu", kv("limit", 1)))},
		{name: "resources an extra memory key", req: postRes(mem(map[string]any{"limit_mb": 1024, "autoscale": false, "swap": 1}))},
		{name: "resources limit plus one", req: postRes(mem(two("limit_mb", "autoscale", 16385, false)))},
		{name: "resources limit 0", req: postRes(mem(two("limit_mb", "autoscale", 0, false)))},
		{name: "resources limit -1", req: postRes(mem(two("limit_mb", "autoscale", -1, false)))},
		{name: "resources limit 1.5", req: postRes(mem(two("limit_mb", "autoscale", 1.5, false)))},
		{name: "resources limit as a string", req: postRes(mem(two("limit_mb", "autoscale", "1024", false)))},
		{name: "resources limit NaN", req: postRes(mem(two("limit_mb", "autoscale", math.NaN(), false)))},
		{name: "resources autoscale true", req: postRes(mem(two("limit_mb", "autoscale", 1024, true)))},
		{name: "resources autoscale true alone", req: postRes(mem(kv("autoscale", true)))},
		{name: "resources autoscale as a string", req: postRes(mem(two("limit_mb", "autoscale", 1024, "false")))},
		{name: "resources no autoscale", req: postRes(mem(kv("limit_mb", 1024)))},
		{name: "resources no limit_mb", req: postRes(mem(kv("autoscale", false)))},
		{name: "PUT an empty label value", req: putSprite(lbl(kv("bitc", "")))},
		{name: "resources empty memory", req: postRes(mem(map[string]any{}))},
		{name: "resources {}", req: postRes(map[string]any{})},
		{name: "resources a string body", req: postRes("memory")},
		{name: "resources memory that is not an object", req: postRes(kv("memory", 1024))},
		{name: "resources nil body", req: postRes(nil)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { runRefusal(t, c) })
	}
}

// A body that decodes to the same value in every reader is sent as the re-marshal of what passed
// the check, never as the caller's value: a duplicate key cannot send what the check did not see,
// and the key order is the checked one.
func TestTableBodySentIsTheCheckedRemarshal(t *testing.T) {
	cases := []struct {
		name, method, tail, body, want string
	}{
		{"key order", "POST", "/policy/resources", `{"memory": {"limit_mb":  1024, "autoscale": false}}`, `{"memory":{"autoscale":false,"limit_mb":1024}}`},
		{"duplicate label key", "PUT", "", `{"labels":{"bitc":"1"},"labels":{"bitc":"2"}}`, `{"labels":{"bitc":"2"}}`},
		{"duplicate inner key", "PATCH", "", `{"labels":{"bitc":"1","bitc":"2"}}`, `{"labels":{"bitc":"2"}}`},
		{"config order", "PUT", "", `{"config":{"ram_mb":2048,"cpus":2}}`, `{"config":{"cpus":2,"ram_mb":2048}}`},
		{"number spelling", "PUT", "", `{"config":{"cpus":2.0}}`, `{"config":{"cpus":2}}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newRig(t, nil)
			var body []byte
			rep := r.runner(quick(1, gRO, time.Minute, func(ctx context.Context, e *Env) Outcome {
				before := len(r.f.Calls())
				code, _ := e.API.Raw(ctx, c.method, sprite(e, c.tail), nil, json.RawMessage(c.body))
				calls := r.f.Calls()
				if len(calls)-before != 1 {
					return failed(fmt.Sprintf("code %d, %d request(s) sent", code, len(calls)-before), nil)
				}
				body = calls[len(calls)-1].Body
				return fine(ctx, e)
			})).Run(context.Background())
			if res := result(t, rep.Report, 1); res.Status != Answered {
				t.Fatalf("%+v", res)
			}
			if string(body) != c.want {
				t.Errorf("the fake recorded %s, want the checked re-marshal %s", body, c.want)
			}
		})
	}
}

// ---- probe 2, probe 25 and the regression run

func restoreCalls(f *spritesfake.Fake) int {
	n := 0
	for _, c := range f.Calls() {
		if c.Method == "POST" && strings.HasSuffix(c.Path, "/restore") {
			n++
		}
	}
	return n
}

// Probe 2 restores a checkpoint of its live Sprite: the restore reaches the fake and the book
// never re-registers.
func TestProbe2RestoreOfALiveSpriteReachesTheFakeWithoutReRegistering(t *testing.T) {
	r := scriptedRig(t)
	rep := r.runner(probeByID(t, 2)).Run(context.Background())
	res := result(t, rep.Report, 2)
	if res.Status == Error || rep.Exit != ExitOK {
		t.Fatalf("probe 2 %+v exit %d", res, rep.Exit)
	}
	if n := restoreCalls(r.f); n != 1 {
		t.Errorf("%d restore(s) reached the fake, want 1", n)
	}
	if len(rep.Report.Sprites) != 1 {
		t.Errorf("the book has %d records, want 1: %+v", len(rep.Report.Sprites), rep.Report.Sprites)
	}
}

// retCase runs probe 25 alone against the fake under one behaviour.
type retCase struct {
	name string
	arm  func(f *spritesfake.Fake)
	// the restore codes of the checkpoint and of the made-up id, and whether the probe saw the
	// Sprite recreated
	restore   string
	recreated bool
	// records in the book after the run: a recreated or kept-live Sprite is a second record
	records int
}

func retCases() []retCase {
	return []retCase{
		{"no survival", func(f *spritesfake.Fake) {}, "map[bitcproof-s0-t1-keep:404 cp-1:404]", false, 1},
		{"checkpoints survive, restore 404", func(f *spritesfake.Fake) { f.BehaveN("checkpoints-survive-delete", 1) }, "map[bitcproof-s0-t1-keep:404 cp-1:404]", false, 1},
		{"restore recreates", func(f *spritesfake.Fake) {
			f.BehaveN("checkpoints-survive-delete", 1)
			f.BehaveN("restore-recreates", 1)
		}, "map[bitcproof-s0-t1-keep:404 cp-1:200]", true, 2},
		{"restore 500", func(f *spritesfake.Fake) {
			f.BehaveN("checkpoints-survive-delete", 1)
			f.BehaveN("restore-500", -1)
		}, "map[bitcproof-s0-t1-keep:0 cp-1:0]", false, 2},
	}
}

func TestProbe25EndToEndUnderEachRestoreBehaviour(t *testing.T) {
	for _, c := range retCases() {
		t.Run(c.name, func(t *testing.T) {
			r := scriptedRig(t)
			c.arm(r.f)
			rep := r.runner(probeByID(t, 25)).Run(context.Background())
			res := result(t, rep.Report, 25)
			if res.Status != Answered || rep.Exit != ExitOK {
				t.Fatalf("probe 25 %q %q exit %d", res.Status, res.Finding, rep.Exit)
			}
			if _, has := res.Evidence["guard_refusals"]; has || rep.Report.GuardRefusals != 0 {
				t.Errorf("guard refusals: %v (%d)", res.Evidence["guard_refusals"], rep.Report.GuardRefusals)
			}
			if got := fmt.Sprint(res.Evidence["restore_status"]); got != c.restore {
				t.Errorf("restore_status %s, want %s", got, c.restore)
			}
			if got := res.Evidence["restore_recreated_the_sprite"]; got != c.recreated {
				t.Errorf("restore_recreated_the_sprite %v, want %v", got, c.recreated)
			}
			if len(rep.Report.Leftovers) != 0 || len(r.f.Live()) != 0 {
				t.Errorf("leftovers %v, live %v", rep.Report.Leftovers, r.f.Live())
			}
			// the book counts a Sprite the restore made, and closes it
			recs := rep.Report.Sprites
			if len(recs) != c.records {
				t.Fatalf("the book has %d records, want %d: %+v", len(recs), c.records, recs)
			}
			for _, rec := range recs {
				if !rec.DeleteOK {
					t.Errorf("record not closed: %+v", rec)
				}
			}
		})
	}
}

// The default plan: no refusals, and the fake records the probes' typed writes.
func TestTypedWritesOfTheDefaultPlanReachTheFake(t *testing.T) {
	r := scriptedRig(t)
	r.cfg.DNSCanary = "canary.example.org"
	rep := r.runner(AllProbes()...).Run(context.Background())
	if rep.Exit != ExitOK || rep.Report.GuardRefusals != 0 {
		t.Fatalf("exit %d, %d refusals\n%s", rep.Exit, rep.Report.GuardRefusals, Markdown(rep.Report))
	}
	var fsWrite, restore, applyJob, ckpt int
	for _, c := range r.f.Calls() {
		switch {
		case c.Method == "PUT" && strings.HasSuffix(c.Path, "/fs/write"):
			fsWrite++
		case c.Method == "POST" && strings.HasSuffix(c.Path, "/restore"):
			restore++
		case c.Method == "POST" && strings.HasSuffix(c.Path, "/policy/network"):
			applyJob++
		case c.Method == "POST" && strings.HasSuffix(c.Path, "/checkpoint"):
			ckpt++
		}
	}
	if fsWrite == 0 || restore == 0 || applyJob == 0 || ckpt == 0 {
		t.Fatalf("the fake did not record the probes' typed writes: fs/write %d, restore %d, network policy %d, checkpoint %d", fsWrite, restore, applyJob, ckpt)
	}
}
