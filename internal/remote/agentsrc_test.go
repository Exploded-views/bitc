package remote

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Exploded-views/bitc/internal/config"
	"github.com/Exploded-views/bitc/internal/provider"
	"github.com/Exploded-views/bitc/internal/version"
)

// standIn makes this test a release build of 1.2.3 running on goos, with no network.
func standIn(t *testing.T, goos string, checks string, agent []byte) {
	t.Helper()
	oldG, oldR, oldV, oldC, oldA := hostGOOS, version.Release, version.Version, FetchChecksums, FetchAgent
	t.Cleanup(func() {
		hostGOOS, version.Release, version.Version, FetchChecksums, FetchAgent = oldG, oldR, oldV, oldC, oldA
	})
	hostGOOS, version.Release, version.Version = goos, "true", "1.2.3"
	FetchChecksums = func(string) ([]byte, error) { return []byte(checks), nil }
	FetchAgent = func(string) ([]byte, error) { return agent, nil }
}

// A macOS client is not its own agent: the release source is the bare linux asset for the
// remote's architecture, named by checksums.txt, whatever the client's own architecture is (#4).
func TestReleaseAgentOnADarwinClientIsTheLinuxAsset(t *testing.T) {
	sum := sha256.Sum256([]byte("linux agent"))
	want := hex.EncodeToString(sum[:])
	standIn(t, "darwin", want+"  bitc_1.2.3_linux_amd64\n"+strings.Repeat("2", 64)+"  bitc_1.2.3_darwin_amd64\n", []byte("linux agent"))
	core := &config.Provider{Type: "ssh", Agent: config.Agent{Source: "release"}}
	e := &config.Effective{Test: &config.Test{AccountingDir: filepath.Join(t.TempDir(), "acct")}}

	src, err := ResolveAgentSourceFor(e, core, "amd64")
	if err != nil {
		t.Fatal(err)
	}
	if src.URL != releaseBase+"/v1.2.3/bitc_1.2.3_linux_amd64" || src.SHA256 != want || src.Path != "" {
		t.Fatalf("the darwin client must name the linux asset, not itself: %+v", src)
	}

	// the bytes an ssh host is sent: downloaded once into the account's agents dir, hash-checked
	sha, path, err := ResolveAgentFile(e, core, "amd64")
	if err != nil || sha != want {
		t.Fatalf("sha %q err %v", sha, err)
	}
	if self, _ := os.Executable(); path == self {
		t.Fatal("the darwin client offered its own Mach-O as the agent")
	}
	if b, err := os.ReadFile(path); err != nil || string(b) != "linux agent" {
		t.Fatalf("cached agent: %q %v", b, err)
	}
	FetchAgent = func(string) ([]byte, error) { t.Fatal("a cached agent is not fetched again"); return nil, nil }
	if _, again, err := ResolveAgentFile(e, core, "amd64"); err != nil || again != path {
		t.Fatalf("second resolve: %q %v", again, err)
	}
}

// A download that does not match checksums.txt is never sent to the host.
func TestReleaseAgentOnADarwinClientIsHashChecked(t *testing.T) {
	standIn(t, "darwin", strings.Repeat("1", 64)+"  bitc_1.2.3_linux_arm64\n", []byte("tampered"))
	core := &config.Provider{Type: "ssh", Agent: config.Agent{Source: "release"}}
	e := &config.Effective{Test: &config.Test{AccountingDir: filepath.Join(t.TempDir(), "acct")}}
	_, _, err := ResolveAgentFile(e, core, "arm64")
	var inel *provider.ErrIneligible
	if !errors.As(err, &inel) || !strings.Contains(err.Error(), "does not match checksums.txt") {
		t.Fatalf("got %v", err)
	}
}

// A url source is delivered as given, on any client OS: paths[arch] are the linux binaries.
func TestURLAgentIsTheOperatorsFileOnADarwinClient(t *testing.T) {
	standIn(t, "darwin", "", nil)
	f := t.TempDir() + "/agent-amd64"
	if err := os.WriteFile(f, []byte("operator agent"), 0o600); err != nil {
		t.Fatal(err)
	}
	core := &config.Provider{Type: "ssh", Agent: config.Agent{Source: "url", URL: "https://example.test/bitc_{arch}", Paths: map[string]string{"amd64": f}}}
	e := &config.Effective{Test: &config.Test{AccountingDir: filepath.Join(t.TempDir(), "acct")}}
	src, err := ResolveAgentSourceFor(e, core, "amd64")
	sum := sha256.Sum256([]byte("operator agent"))
	if err != nil || src.URL != "https://example.test/bitc_amd64" || src.Path != f || src.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("%+v %v", src, err)
	}
}
