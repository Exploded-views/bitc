package provider

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/Exploded-views/bitc/conformance/harness"
	"github.com/Exploded-views/bitc/internal/provider/plugin"
)

var (
	binFlag        = flag.String("bin", "", "bitc binary under test; relative paths are from the module root (default: build ./cmd/bitc)")
	pluginFlag     = flag.String("plugin", "", "provider plugin under test for pluginTarget (default: the reference plugin)")
	pluginSumFlag  = flag.String("plugin-sha256", "", "sha256 of -plugin, checked against the file before it is pinned")
	pluginFakeFlag = flag.String("plugin-fake", "", "the plugin author's loopback fake API binary (default: the reference fake)")
)

func TestMain(m *testing.M) {
	if len(os.Args) > 2 && os.Args[1] == "__plugin-exec" { // pluginTarget's in-process adapter spawns plugins through this shim
		os.Exit(plugin.ExecShim(os.Args[2:]))
	}
	flag.Parse()
	refDir, err := harness.BuildReference()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	sshDir, err := harness.BuildSSHFake()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if *binFlag != "" {
		p := *binFlag
		if !filepath.IsAbs(p) {
			p = filepath.Join("..", "..", p)
		}
		abs, err := filepath.Abs(p)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		harness.Bin = abs
		code := m.Run()
		os.RemoveAll(refDir)
		os.RemoveAll(sshDir)
		os.Exit(code)
	}
	dir, err := os.MkdirTemp("", "bitc-conformance-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	harness.Bin = filepath.Join(dir, "bitc")
	build := exec.Command("go", "build", "-trimpath", "-o", harness.Bin, "../../cmd/bitc")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "build bitc: %v\n%s", err, out)
		os.Exit(2)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.RemoveAll(refDir)
	os.RemoveAll(sshDir)
	os.Exit(code)
}
