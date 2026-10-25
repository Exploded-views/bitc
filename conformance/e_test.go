package conformance

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/Exploded-views/bitc/conformance/harness"
)

func TestE1_PoolEnvSetsAndUnsets(t *testing.T) {
	h := harness.New(t)
	nodeRepo(t, h, "", nil)
	in := []string{"NODE_OPTIONS=--inherited", "NODE_HEAP_MB=1"}
	heavy := envIn(t, h, in, "heavy")
	if heavy["NODE_OPTIONS"] != "--max-old-space-size=3072" || heavy["NODE_HEAP_MB"] != "3072" {
		t.Fatalf("heavy %v", heavy)
	}
	lint := envIn(t, h, in, "lint")
	if _, ok := lint["NODE_OPTIONS"]; ok {
		t.Fatal("lint kept NODE_OPTIONS")
	}
	if _, ok := lint["NODE_HEAP_MB"]; ok {
		t.Fatal("lint kept NODE_HEAP_MB")
	}
}

func TestE2_PoolEnvOverridesDefaults(t *testing.T) {
	h := harness.New(t)
	nodeRepo(t, h, "", nil)
	cov := envIn(t, h, nil, "coverage")
	if cov["NODE_OPTIONS"] != "--max-old-space-size=2560" || cov["NODE_HEAP_MB"] != "3072" {
		t.Fatalf("coverage %v", cov)
	}
}

func TestE4_DefaultsEnvReachesEveryPool(t *testing.T) {
	h := harness.New(t)
	nodeRepo(t, h, "", nil)
	for _, p := range []string{"lint", "vitest", "db", "heavy", "git"} {
		if envIn(t, h, nil, p)["TEST_MAX_WORKERS"] != "2" {
			t.Errorf("%s lacks TEST_MAX_WORKERS=2", p)
		}
	}
}

func TestX1_TemplatesExpandPerSlot(t *testing.T) {
	h := harness.New(t)
	nodeRepo(t, h, "", nil)
	db := envIn(t, h, nil, "db")
	if db["DATABASE_URL"] != "postgresql://localhost:47411/demo" || db["TEST_DB_CONTAINER"] != "demo-db-1" {
		t.Fatalf("%v", db)
	}
}

func TestX3_VarsComeFromConfigHostWins(t *testing.T) {
	h := harness.New(t)
	nodeRepo(t, h, "vars = { db_port_base = 60000 }\n", nil)
	if got := envIn(t, h, nil, "db")["TEST_DB_PORT"]; got != "60001" {
		t.Fatal(got)
	}
}

func TestX4_InheritedValuesDoNotSurviveIntoAPoolsKeys(t *testing.T) {
	h := harness.New(t)
	nodeRepo(t, h, "", nil)
	in := []string{"DATABASE_URL=postgresql://stack/app", "TEST_DB_PORT=1", "APP_POOL_SLOT=e2e.1"}
	db := envIn(t, h, in, "db")
	if db["DATABASE_URL"] != "postgresql://localhost:47411/demo" || db["TEST_DB_PORT"] != "47411" {
		t.Fatalf("%v", db)
	}
}

func TestY1_EnvHookOutputExported(t *testing.T) {
	h := harness.New(t)
	nodeRepo(t, h, "", nil)
	e2e := envIn(t, h, nil, "e2e")
	if e2e["APP_STACK_PROJECT"] != "demo-env-31" || e2e["APP_STACK_SLOT"] != "31" ||
		e2e["DATABASE_URL"] != "postgresql://app:app@localhost:46031/app" || e2e["APP_POOL_SLOT"] != "e2e.1" {
		t.Fatalf("%v", e2e)
	}
}

func TestY2_EnvUnsetRemovesInherited(t *testing.T) {
	h := harness.New(t)
	nodeRepo(t, h, "", nil)
	e2e := envIn(t, h, []string{"TEST_DB_PORT=1", "TEST_DB_CONTAINER=x", "APP_STACK_REUSE=1"}, "e2e")
	for _, k := range []string{"TEST_DB_PORT", "TEST_DB_CONTAINER", "APP_STACK_REUSE"} {
		if _, ok := e2e[k]; ok {
			t.Errorf("%s survived", k)
		}
	}
}

func TestY3_HookRefusalIs64BeforeTheCommand(t *testing.T) {
	h := harness.New(t)
	nodeRepo(t, h, "vars = { stack_slot_base = 40 }\n", nil)
	r := h.Run("run", "e2e", "--", "echo", "ran")
	if r.Code != 64 || r.Stdout != "" || r.Stderr != "bitc: e2e: refused: stack slot 41 is outside 1..40\n" {
		t.Fatalf("%+v", r)
	}
}

func TestY4_HookThatCannotStartIs69(t *testing.T) {
	h := harness.New(t)
	nodeRepo(t, h, "", func(s string) string { return strings.Replace(s, "scripts/stack-env.sh", "scripts/missing.sh", 1) })
	r := h.Run("run", "e2e", "--", "echo", "ran")
	if r.Code != 69 || r.Stdout != "" || !strings.Contains(r.Stderr, "env hook could not start") {
		t.Fatalf("%+v", r)
	}
}

func TestY5_EnvDefaultCallerWinsHostEnvForces(t *testing.T) {
	h := harness.New(t)
	nodeRepo(t, h, "", nil)
	if got := envIn(t, h, nil, "e2e")["E2E_WORKERS"]; got != "3" {
		t.Fatal(got)
	}
	if got := envIn(t, h, []string{"E2E_WORKERS=5"}, "e2e")["E2E_WORKERS"]; got != "5" {
		t.Fatal(got)
	}
	h2 := harness.New(t)
	nodeRepo(t, h2, "", nil)
	host := string(must(readFile(h2.Config)))
	host = strings.Replace(host, `e2e = { slots = 1`, `e2e = { env = { E2E_WORKERS = "7" }, slots = 1`, 1)
	h2.Write(h2.Config, host, 0o600)
	if got := envIn(t, h2, []string{"E2E_WORKERS=5"}, "e2e")["E2E_WORKERS"]; got != "7" {
		t.Fatal(got)
	}
}

func TestY6_HoldTokenUniqueAndStateDirAbsolute(t *testing.T) {
	h := harness.New(t)
	nodeRepo(t, h, "", nil)
	a, b := envIn(t, h, nil, "e2e"), envIn(t, h, nil, "e2e")
	if a["APP_POOL_HOLD"] == "" || a["APP_POOL_HOLD"] == b["APP_POOL_HOLD"] {
		t.Fatalf("hold tokens %q %q", a["APP_POOL_HOLD"], b["APP_POOL_HOLD"])
	}
	if a["APP_STACK_TOKEN_FILE"] != filepath.Join(h.State, "e2e-stack.31") {
		t.Fatal(a["APP_STACK_TOKEN_FILE"])
	}
	if db := envIn(t, h, nil, "db"); db["APP_POOL_HOLD"] != "" || db["APP_STACK_TOKEN_FILE"] != "" {
		t.Fatal("db exported the e2e hold variables")
	}
}

func TestY8_PoolKeysDoNotLeakIntoOtherPools(t *testing.T) {
	h := harness.New(t)
	nodeRepo(t, h, "", nil)
	for _, p := range []string{"lint", "db", "heavy", "vitest"} {
		m := envIn(t, h, nil, p)
		for k := range m {
			if strings.HasPrefix(k, "APP_STACK_") || k == "APP_POOL_SLOT" || k == "E2E_WORKERS" {
				t.Errorf("%s sees %s", p, k)
			}
		}
	}
}
