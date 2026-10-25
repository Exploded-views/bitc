// Package plugin is the plugin conformance suite (providers delta A.10.1): the protocol suite
// (rows PT) drives a plugin binary from a scripted bitc side, and the adapter rows (family PL)
// run bitc against the reference plugin in its fault modes. The provider-agnostic suite, run with
// pluginTarget, lives in conformance/provider.
package plugin

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
	binFlag    = flag.String("bin", "", "bitc binary under test; relative paths are from the module root (default: build ./cmd/bitc)")
	pluginFlag = flag.String("plugin", "", "plugin binary for the protocol suite (default: the reference plugin)")
	configFlag = flag.String("plugin-config", "", "JSON object for the plugin's configure request, with -plugin (default: the reference fake's api_base)")
)

func TestMain(m *testing.M) {
	if len(os.Args) > 2 && os.Args[1] == "__plugin-exec" { // the in-process adapter of PL10 spawns plugins through this shim
		os.Exit(plugin.ExecShim(os.Args[2:]))
	}
	flag.Parse()
	refDir, err := harness.BuildReference()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	bitcDir := ""
	if *binFlag != "" {
		p := *binFlag
		if !filepath.IsAbs(p) {
			p = filepath.Join("..", "..", p)
		}
		if harness.Bin, err = filepath.Abs(p); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
	} else {
		if bitcDir, err = os.MkdirTemp("", "bitc-conformance-"); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		harness.Bin = filepath.Join(bitcDir, "bitc")
		build := exec.Command("go", "build", "-trimpath", "-o", harness.Bin, "../../cmd/bitc")
		build.Env = append(os.Environ(), "CGO_ENABLED=0")
		if out, err := build.CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "build bitc: %v\n%s", err, out)
			os.Exit(2)
		}
	}
	code := m.Run()
	os.RemoveAll(refDir)
	if bitcDir != "" {
		os.RemoveAll(bitcDir)
	}
	os.Exit(code)
}
