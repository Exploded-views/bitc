package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Exploded-views/bitc/internal/config"
)

// The macOS client build runs the ssh provider as a client (#4): the remote agent runs on the
// Linux host. These loads are pinned to darwin so Linux CI runs the same paths; on a darwin
// host the pin is the real platform. They use the registered provider types (ssh, sprites).

const darwinSSHSection = "[providers.ssh]\nhost = \"203.0.113.7\"\nuser = \"bitc\"\nhost_key = \"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA\"\n" +
	"identity_file = \"IDENT\"\narch = \"amd64\"\nslots = 2\nresolver = \"9.9.9.9\"\n"

// darwinLoad writes host (IDENT replaced by a private key file) and loads it as a darwin build.
func darwinLoad(t *testing.T, host string, cleanup bool) (*config.Effective, error) {
	t.Helper()
	t.Cleanup(config.SetPlatformForTest("darwin"))
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(dir, "id_ed25519")
	if err := os.WriteFile(key, []byte("not read at load\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(dir, "bitc.toml")
	body := strings.ReplaceAll(host, "IDENT", key) + "[test]\naccounting_dir = \"" + filepath.Join(dir, "account") + "\"\n"
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	wt := filepath.Join(dir, "wt")
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	return config.Load(config.LoadOptions{Env: []string{"HOME=" + dir, "BITC_CONFIG=" + cfg}, Cwd: wt, Cleanup: cleanup})
}

func TestDarwinLoadsTheSSHProviderAndPoolsNamingIt(t *testing.T) {
	host := "version = 1\n[host]\nexperimental_remote = true\n[pools.heavy]\nproviders = [\"ssh\", \"local\"]\n" + darwinSSHSection
	e, err := darwinLoad(t, host, false)
	if err != nil {
		t.Fatalf("ssh must load on darwin: %v", err)
	}
	if pr := e.Providers["ssh"]; pr == nil || pr.Type != "ssh" || !pr.Fixed {
		t.Fatalf("ssh not decoded: %+v", e.Providers)
	}
	if got := e.Pools["heavy"].Providers; len(got) != 2 || got[0] != "ssh" {
		t.Fatalf("pool providers %v", got)
	}
}

// The ssh provider is still behind [host] experimental_remote on darwin, as on Linux.
func TestDarwinSSHNeedsExperimentalRemoteLikeLinux(t *testing.T) {
	_, err := darwinLoad(t, "version = 1\n"+darwinSSHSection, false)
	if err == nil || !strings.Contains(err.Error(), config.ExperimentalRemoteRefusal) {
		t.Fatalf("want the experimental_remote refusal, got %v", err)
	}
	if strings.Contains(err.Error(), "runs on Linux only") {
		t.Fatalf("ssh was refused for the platform: %v", err)
	}
}

// Sprites stays refused everywhere until #6: on darwin it gets the same refusal as on Linux, not
// the platform line. A cleanup (sweep, doctor --remote) gets it as CleanupOnly.
func TestDarwinSpritesIsRefusedForIssue6NotForThePlatform(t *testing.T) {
	host := "version = 1\n[host]\nexperimental_remote = true\n[pools.heavy]\nproviders = [\"sprites\", \"local\"]\n[providers.sprites]\norg = \"o\"\ndaily_usd = 1\n"
	_, err := darwinLoad(t, host, false)
	if err == nil || !strings.Contains(err.Error(), config.SpritesRefusal) {
		t.Fatalf("want the #6 refusal, got %v", err)
	}
	if strings.Contains(err.Error(), "runs on Linux only") || strings.Contains(err.Error(), "not local or a configured") {
		t.Fatalf("the platform or unconfigured line replaced the #6 refusal: %v", err)
	}
	// a pool naming sprites with no table is refused the same way
	_, err = darwinLoad(t, "version = 1\n[host]\nexperimental_remote = true\n[pools.heavy]\nproviders = [\"sprites\"]\n", false)
	if err == nil || !strings.Contains(err.Error(), config.SpritesRefusal) {
		t.Fatalf("pool naming sprites: %v", err)
	}
	e, err := darwinLoad(t, host, true)
	if err != nil {
		t.Fatalf("a cleanup loads sprites: %v", err)
	}
	if pr := e.Providers["sprites"]; pr == nil || !pr.CleanupOnly {
		t.Fatalf("sprites is not CleanupOnly: %+v", e.Providers["sprites"])
	}
}

// RunPod is parked on darwin: the platform refusal stays, and says so.
func TestDarwinRunPodStaysRefusedForThePlatform(t *testing.T) {
	host := "version = 1\n[host]\nexperimental_remote = true\n[providers.runpod]\ndaily_usd = 1\nimage = \"x\"\nsizes = {}\n"
	_, err := darwinLoad(t, host, false)
	if err == nil || !strings.Contains(err.Error(), "providers.runpod: RunPod runs on Linux only for now (this is a darwin build)") {
		t.Fatalf("want the RunPod platform refusal, got %v", err)
	}
	// next to ssh, only runpod is refused
	_, err = darwinLoad(t, host+darwinSSHSection, false)
	if err == nil || !strings.Contains(err.Error(), "providers.runpod: RunPod runs on Linux only") || strings.Contains(err.Error(), "providers.ssh") {
		t.Fatalf("ssh next to runpod: %v", err)
	}
}

// Memory admission stays Linux-only: ssh does not change the pool key.
func TestDarwinMemoryAdmissionStaysRefusedBesideSSH(t *testing.T) {
	host := "version = 1\n[host]\nexperimental_remote = true\n[pools.heavy]\nslots = 1\nmemory = \"8GiB\"\nproviders = [\"ssh\", \"local\"]\n" + darwinSSHSection
	_, err := darwinLoad(t, host, false)
	if err == nil || !strings.Contains(err.Error(), "pools.heavy.memory: memory admission runs on Linux only (this is a darwin build)") {
		t.Fatalf("want the memory refusal, got %v", err)
	}
	if strings.Contains(err.Error(), "providers.ssh") {
		t.Fatalf("ssh was refused too: %v", err)
	}
}
