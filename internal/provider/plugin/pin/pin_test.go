package pin

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// notBinary is the reason Check gives for a file that is not this OS's executable format.
func notBinary() string {
	if runtime.GOOS == "darwin" {
		return "not a Mach-O file"
	}
	return "not an ELF file"
}

// execCopy copies this test binary (an ELF file on Linux, a Mach-O file on darwin) to dir/name
// with mode.
func execCopy(t *testing.T, dir, name string, mode os.FileMode) string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	src, err := os.Open(self)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	p := filepath.Join(dir, name)
	dst, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o700)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(dst, src); err != nil {
		t.Fatal(err)
	}
	dst.Close()
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	return p
}

func wantKind(t *testing.T, err error, k Kind, detail string) {
	t.Helper()
	var pe *Error
	if !errors.As(err, &pe) || pe.Kind != k || (detail != "" && pe.Detail != detail) {
		t.Fatalf("got %v (%#v), want kind %d detail %q", err, pe, k, detail)
	}
}

func TestCheckAcceptsAPrivateExecutable(t *testing.T) {
	p := execCopy(t, t.TempDir(), "bitc-provider-x", 0o755)
	if err := Check(p); err != nil {
		t.Fatalf("Check: %v", err)
	}
}

func TestCheckRefusals(t *testing.T) {
	dir := t.TempDir()
	good := execCopy(t, dir, "good", 0o755)
	_ = os.Symlink(good, filepath.Join(dir, "link"))
	_ = os.WriteFile(filepath.Join(dir, "script"), []byte("#!/bin/sh\n"), 0o755)
	_ = os.Mkdir(filepath.Join(dir, "adir"), 0o700)
	plain := execCopy(t, dir, "plain", 0o644)
	gw := execCopy(t, dir, "gw", 0o775)
	ow := execCopy(t, dir, "ow", 0o757)
	suid := execCopy(t, dir, "suid", 0o755)
	sgid := execCopy(t, dir, "sgid", 0o755)
	if err := os.Chmod(suid, os.ModeSetuid|0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sgid, os.ModeSetgid|0o755); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name, path string
		kind       Kind
		detail     string
	}{
		{"missing", filepath.Join(dir, "nope"), KindReason, "missing"},
		{"symlink", filepath.Join(dir, "link"), KindReason, "symlink"},
		{"directory", filepath.Join(dir, "adir"), KindReason, "not a regular file"},
		{"not executable", plain, KindReason, "not executable"},
		{"script", filepath.Join(dir, "script"), KindReason, notBinary()},
		{"group writable", gw, KindInsecure, gw},
		{"world writable", ow, KindInsecure, ow},
		{"setuid", suid, KindPrivs, ""},
		{"setgid", sgid, KindPrivs, ""},
	} {
		t.Run(c.name, func(t *testing.T) { wantKind(t, Check(c.path), c.kind, c.detail) })
	}
}

func TestCheckRefusesAWritableParentButNotAStickyOne(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "sub")
	_ = os.Mkdir(sub, 0o700)
	p := execCopy(t, sub, "bitc-provider-x", 0o755)
	if err := os.Chmod(sub, 0o770); err != nil {
		t.Fatal(err)
	}
	wantKind(t, Check(p), KindInsecure, sub)
	if err := os.Chmod(sub, os.ModeSticky|0o777); err != nil { // world-writable but sticky, like /tmp
		t.Fatal(err)
	}
	if err := Check(p); err != nil {
		t.Fatalf("sticky parent: %v", err)
	}
	if err := os.Chmod(sub, 0o777); err != nil {
		t.Fatal(err)
	}
	wantKind(t, Check(p), KindInsecure, sub)
}

func TestOpenHashesTheOpenFdAndRefusesAChange(t *testing.T) {
	p := execCopy(t, t.TempDir(), "bitc-provider-x", 0o755)
	sum, err := SumFile(p)
	if err != nil {
		t.Fatal(err)
	}
	f, err := Open(p, sum)
	if err != nil || f == nil {
		t.Fatalf("Open with the right sum: %v %v", f, err)
	}
	f.Close()
	var ch *Changed
	_, err = Open(p, "0000000000000000000000000000000000000000000000000000000000000000")
	if !errors.As(err, &ch) || ch.Got != sum || Short(ch.Want) != "000000000000" {
		t.Fatalf("Open with a wrong sum: %v", err)
	}
	if want := "plugin " + p + " changed since load (sha256 000000000000, now " + sum[:12] + ")"; err.Error() != want {
		t.Fatalf("message %q, want %q", err.Error(), want)
	}
}

func TestOpenRefusesBitsSetAfterLoadAndSymlinks(t *testing.T) {
	dir := t.TempDir()
	p := execCopy(t, dir, "bitc-provider-x", 0o755)
	sum, _ := SumFile(p)
	if err := os.Chmod(p, os.ModeSetuid|0o755); err != nil {
		t.Fatal(err)
	}
	f, err := Open(p, sum)
	wantKind(t, err, KindPrivs, "")
	if f != nil {
		t.Fatal("Open returned a file for a setuid plugin")
	}
	if err.Error() != "insecure: setuid/setgid/capabilities" {
		t.Fatalf("message %q", err.Error())
	}
	_ = os.Chmod(p, 0o755)
	l := filepath.Join(dir, "l")
	_ = os.Symlink(p, l)
	if f, err := Open(l, sum); err == nil {
		f.Close()
		t.Fatal("Open followed a symlink")
	}
}
