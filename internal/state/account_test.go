package state

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/Exploded-views/bitc/internal/config"
	"github.com/Exploded-views/bitc/internal/exit"
)

func TestInstallSecretIsCreatedOnce(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "account")
	a, err := InstallSecret(dir)
	if err != nil || len(a) != 32 {
		t.Fatal(err)
	}
	b, _ := InstallSecret(dir)
	if string(a) != string(b) {
		t.Fatal("secret changed")
	}
	st, _ := os.Stat(filepath.Join(dir, "install-secret"))
	if st.Mode().Perm() != 0o600 || len(Install16(a)) != 16 {
		t.Fatal(st.Mode())
	}
}

func TestInstallSecretRejectsAShortFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "account")
	if err := MkdirPrivate(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "install-secret"), []byte("short"), 0o600); err != nil {
		t.Fatal(err)
	}
	if b, err := InstallSecret(dir); err == nil {
		t.Fatalf("a %d-byte secret was accepted", len(b))
	}
}

func nfsStatfs(t *testing.T) {
	t.Helper()
	old := statfs
	t.Cleanup(func() { statfs = old })
	statfs = func(_ string, sfs *unix.Statfs_t) error { return fakeNFS(sfs) }
}

// The accounting dir holds the launch lock, the ledger and the reservations: on a network
// filesystem flock is unreliable and the spend cap weakens, so it is refused (78) like the state dir.
func TestAccountDirRefusesANetworkFilesystem(t *testing.T) {
	nfsStatfs(t)
	dir := filepath.Join(t.TempDir(), "account")
	got, err := AccountDir(&config.Effective{Test: &config.Test{AccountingDir: dir}})
	want := "accounting dir " + dir + " is on NFS; bitc needs a local filesystem for flock"
	if exit.Code(err) != exit.Config || err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("%q: %v", got, err)
	}
}

// config check runs the same refusal on an existing accounting dir.
func TestCheckAccountDirRefusesANetworkFilesystem(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "account")
	e := &config.Effective{Test: &config.Test{AccountingDir: dir}}
	nfsStatfs(t)
	if err := MkdirPrivate(dir); err != nil {
		t.Fatal(err)
	}
	err := CheckAccountDir(e)
	if exit.Code(err) != exit.Config || err == nil || !strings.Contains(err.Error(), "accounting dir "+dir+" is on NFS") {
		t.Fatalf("%v", err)
	}
}

// A missing accounting dir on a network filesystem is refused before it is created.
func TestCheckAccountDirJudgesAMissingDirByItsParent(t *testing.T) {
	nfsStatfs(t)
	dir := filepath.Join(t.TempDir(), "home", ".local", "state", "bitc", "account")
	err := CheckAccountDir(&config.Effective{Test: &config.Test{AccountingDir: dir}})
	if exit.Code(err) != exit.Config || err == nil || !strings.Contains(err.Error(), "accounting dir "+dir+" is on NFS") {
		t.Fatalf("%v", err)
	}
	if _, err := os.Stat(dir); err == nil {
		t.Fatal("the dir was created")
	}
}

// Sweep's lenient lookup must not create the dir on a network filesystem (a write).
func TestAccountDirOnNetworkFSDoesNotCreateTheDirThere(t *testing.T) {
	nfsStatfs(t)
	dir := filepath.Join(t.TempDir(), "home", "account")
	got, netfs, err := AccountDirOnNetworkFS(&config.Effective{Test: &config.Test{AccountingDir: dir}})
	if err != nil || netfs != "NFS" || got != dir {
		t.Fatalf("%q %q %v", got, netfs, err)
	}
	if _, err := os.Stat(dir); err == nil {
		t.Fatal("the dir was created on a network filesystem")
	}
}
