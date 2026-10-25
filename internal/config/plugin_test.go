package config

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Exploded-views/bitc/internal/provider/plugin/pin"
)

// plug is a pinned-plugin fixture: a copy of this test binary (an ELF file) named
// bitc-provider-<typ> in a private directory.
type plug struct{ path, sum string }

func newPlug(t *testing.T, typ string) plug {
	t.Helper()
	dir := t.TempDir()
	src, err := os.Open("/proc/self/exe")
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	p := filepath.Join(dir, "bitc-provider-"+typ)
	dst, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(dst, src); err != nil {
		t.Fatal(err)
	}
	dst.Close()
	sum, err := pin.SumFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return plug{p, sum}
}

func pluginHost(typ string, pl plug, pluginExtra, extra string) string {
	return fmt.Sprintf("version = 1\n[providers.%s]\nplugin = { path = %q, sha256 = %q%s }\ndaily_usd = 5\nsizes = { cpu8 = { usd_per_hour = 0.60 } }\n%s",
		typ, pl.path, pl.sum, pluginExtra, extra)
}

func TestPluginSectionLoads(t *testing.T) {
	pl := newPlug(t, "modal")
	secret := filepath.Join(t.TempDir(), "tok")
	if err := os.WriteFile(secret, []byte("s3cret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := newFixture(t, pluginHost("modal", pl, fmt.Sprintf(", env_pass = [\"HTTPS_PROXY\"], secret_files = { token_secret = %q }", secret), "region = \"eu\"\n"), "")
	e := mustLoad(t, f)
	pr := e.Providers["modal"]
	if pr == nil || pr.Plugin == nil {
		t.Fatalf("no plugin section: %+v", pr)
	}
	if pr.Plugin.Path != pl.path || pr.Plugin.SHA256 != pl.sum || len(pr.Plugin.EnvPass) != 1 || pr.Plugin.EnvPass[0] != "HTTPS_PROXY" ||
		pr.Plugin.SecretFiles["token_secret"] != secret {
		t.Errorf("plugin pin: %+v", pr.Plugin)
	}
	if pr.Sizes["cpu8"] != 600000 {
		t.Errorf("sizes: %+v", pr.Sizes)
	}
	if _, ok := pr.Raw["region"]; !ok || pr.Raw["plugin"] != nil || pr.Raw["sizes"] != nil {
		t.Errorf("raw keys (only the non-core keys go to configure): %+v", pr.Raw)
	}
	if pr.FallbackUntil != "resource" {
		t.Errorf("a plugin defaults to the resource boundary, got %q", pr.FallbackUntil)
	}
}

func TestPluginKeyIsHostOnly(t *testing.T) {
	pl := newPlug(t, "modal")
	repo := fmt.Sprintf("version = 1\n[providers.modal]\nplugin = { path = %q, sha256 = %q }\n", pl.path, pl.sum)
	_, err := newFixture(t, pluginHost("modal", pl, "", ""), repo).load()
	expect78(t, err, "providers.modal.plugin: host-only key")
}

func TestPluginRefusesABuiltInTypeAndABadTypeName(t *testing.T) {
	for _, typ := range []string{"runpod", "sprites", "ssh", "fake", "local"} {
		pl := newPlug(t, typ)
		_, err := newFixture(t, pluginHost(typ, pl, "", ""), "").load()
		expect78(t, err, fmt.Sprintf("providers.%s.plugin: %q is a built-in provider", typ, typ))
	}
	for _, typ := range []string{"Modal", "a-b", "abcdefghijklmnopq"} {
		pl := newPlug(t, "modal")
		_, err := newFixture(t, pluginHost("\""+typ+"\"", pl, "", ""), "").load()
		expect78(t, err, fmt.Sprintf("providers.%s: invalid provider type", typ))
	}
}

func TestPluginPathAndPinChecks(t *testing.T) {
	pl := newPlug(t, "modal")
	dir := filepath.Dir(pl.path)
	_ = os.WriteFile(filepath.Join(dir, "bitc-provider-script"), []byte("#!/bin/sh\n"), 0o755)
	gw := filepath.Join(dir, "bitc-provider-gw")
	b, _ := os.ReadFile(pl.path)
	_ = os.WriteFile(gw, b, 0o775)
	_ = os.Chmod(gw, 0o775)
	suid := filepath.Join(dir, "bitc-provider-suid")
	_ = os.WriteFile(suid, b, 0o755)
	_ = os.Chmod(suid, os.ModeSetuid|0o755)
	for _, c := range []struct{ typ, path, msg string }{
		{"modal", "bitc-provider-modal", `providers.modal.plugin.path: must be an absolute path ending in bitc-provider-modal: "bitc-provider-modal"`},
		{"modal", filepath.Join(dir, "other"), `providers.modal.plugin.path: must be an absolute path ending in bitc-provider-modal: "` + filepath.Join(dir, "other") + `"`},
		{"nope", filepath.Join(dir, "bitc-provider-nope"), `providers.nope.plugin.path: missing: "` + filepath.Join(dir, "bitc-provider-nope") + `"`},
		{"script", filepath.Join(dir, "bitc-provider-script"), `providers.script.plugin.path: not an ELF file: "` + filepath.Join(dir, "bitc-provider-script") + `"`},
		{"gw", gw, "providers.gw.plugin.path: insecure: " + gw},
		{"suid", suid, `providers.suid.plugin.path: insecure: setuid/setgid/capabilities: "` + suid + `"`},
	} {
		_, err := newFixture(t, pluginHost(c.typ, plug{c.path, pl.sum}, "", ""), "").load()
		expect78(t, err, c.msg)
	}
}

func TestPluginShaChecks(t *testing.T) {
	pl := newPlug(t, "modal")
	_, err := newFixture(t, pluginHost("modal", plug{pl.path, "ABC"}, "", ""), "").load()
	expect78(t, err, `providers.modal.plugin.sha256: invalid: "ABC"`)
	zero := strings.Repeat("0", 64)
	_, err = newFixture(t, pluginHost("modal", plug{pl.path, zero}, "", ""), "").load()
	expect78(t, err, "providers.modal.plugin: sha256 mismatch (want 000000000000, file "+pl.sum[:12]+")")
}

func TestPluginEnvPassAndSecretFiles(t *testing.T) {
	pl := newPlug(t, "modal")
	for _, v := range []string{"bad name", "BITC_X", "MY_TOKEN", "AWS_SECRET_ACCESS_KEY"} {
		_, err := newFixture(t, pluginHost("modal", pl, fmt.Sprintf(", env_pass = [%q]", v), ""), "").load()
		expect78(t, err, fmt.Sprintf("providers.modal.plugin.env_pass: %q not allowed", v))
	}
	loose := filepath.Join(t.TempDir(), "tok")
	_ = os.WriteFile(loose, []byte("x\n"), 0o644)
	_ = os.Chmod(loose, 0o644)
	_, err := newFixture(t, pluginHost("modal", pl, fmt.Sprintf(", secret_files = { token_secret = %q }", loose), ""), "").load()
	expect78(t, err, "providers.modal.plugin.secret_files.token_secret: ", "mode 600")
	_, err = newFixture(t, pluginHost("modal", pl, ", secret_files = { api_key = \"/x\" }", ""), "").load()
	expect78(t, err, "providers.modal.plugin.secret_files.api_key: ")
	_, err = newFixture(t, pluginHost("modal", pl, ", secret_files = { Tok = \"/x\" }", ""), "").load()
	expect78(t, err, "providers.modal.plugin.secret_files.Tok: ")
	_, err = newFixture(t, pluginHost("modal", pl, ", colour = 1", ""), "").load()
	expect78(t, err, "unknown key providers.modal.plugin.colour")
}

func TestPluginNeedsAHostPricePerSize(t *testing.T) {
	pl := newPlug(t, "modal")
	msg := "providers.modal.sizes: a usd_per_hour per size is required for a plugin"
	base := fmt.Sprintf("version = 1\n[providers.modal]\nplugin = { path = %q, sha256 = %q }\ndaily_usd = 5\n", pl.path, pl.sum)
	for _, sizes := range []string{"", "sizes = {}\n", "sizes = { cpu8 = {} }\n", "sizes = { cpu8 = { usd_per_hour = 0 } }\n", "sizes = { cpu8 = { usd_per_hour = \"1\" } }\n"} {
		_, err := newFixture(t, base+sizes, "").load()
		expect78(t, err, msg)
	}
	host := pluginHost("modal", pl, "", "") + "[pools.heavy]\nproviders = [\"modal\"]\nremote_limits = { size = { modal = \"gpu1\" } }\n"
	_, err := newFixture(t, host, "").load()
	expect78(t, err, msg)
}

func TestPluginInlineSecretNamedKeyIs78(t *testing.T) {
	pl := newPlug(t, "modal")
	_, err := newFixture(t, pluginHost("modal", pl, "", "token_secret = \"abc\"\n"), "").load()
	expect78(t, err, "providers.modal.token_secret: secret; set it in plugin.secret_files")
}

func TestPluginExplainNamesTheFilesAndNeverTheirContents(t *testing.T) {
	pl := newPlug(t, "modal")
	secret := filepath.Join(t.TempDir(), "tok")
	_ = os.WriteFile(secret, []byte("hunter2-secret-value\n"), 0o600)
	e := mustLoad(t, newFixture(t, pluginHost("modal", pl, fmt.Sprintf(", secret_files = { token_secret = %q }", secret), ""), ""))
	out := strings.Join(Explain(e, "providers.modal"), "\n")
	for _, want := range []string{"providers.modal.plugin.path = " + pl.path, "providers.modal.plugin.sha256 = " + pl.sum,
		"providers.modal.plugin.secret_files.token_secret = " + secret, "providers.modal.sizes.cpu8.usd_per_hour = $0.60"} {
		if !strings.Contains(out, want) {
			t.Errorf("explain lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "hunter2") {
		t.Errorf("explain printed a secret value:\n%s", out)
	}
}
