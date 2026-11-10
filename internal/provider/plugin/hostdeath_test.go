package plugin

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Exploded-views/bitc/internal/config"
	"github.com/Exploded-views/bitc/internal/provider"
	"github.com/Exploded-views/bitc/internal/provider/plugin/pin"
	"github.com/Exploded-views/bitc/internal/redact"
)

// hostMain is a throwaway "bitc" for TestAPluginDiesWithItsSigkilledBitc: it starts the stub
// plugin through the adapter, says "ready" and waits to be killed. args: bin sum dir.
func hostMain(args []string) int {
	if len(args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: __host bin sum dir")
		return 64
	}
	bin, sum, dir := args[0], args[1], args[2]
	core := &config.Provider{Type: "stub", NamePrefix: "bitc", APIKey: "sk-test-key-1", DailyUSD: 5_000_000, MaxLive: 2,
		LaunchWait: time.Minute, StartTimeout: 2 * time.Minute, Breaker: config.Breaker{Failures: 3, Window: time.Hour},
		FallbackUntil: "resource", FallbackCap: "attach",
		Plugin:   &config.PluginPin{Path: bin, SHA256: sum, SecretFiles: map[string]string{}},
		Sizes:    map[string]config.Micros{"cpu8": 600_000},
		SizesRaw: map[string]any{"cpu8": map[string]any{"usd_per_hour": 0.6}},
		Raw:      map[string]any{"region": "eu"}}
	eff := &config.Effective{Host: config.Host{StateDir: filepath.Join(dir, "state")},
		Test: &config.Test{AccountingDir: filepath.Join(dir, "account"), TimeScale: 1}}
	p, err := New(core, eff)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if err := p.Start(context.Background(), provider.Runtime{Red: redact.New(), Stderr: os.Stderr}); err != nil {
		fmt.Fprintln(os.Stderr, "start:", err)
		return 1
	}
	fmt.Println("ready")
	select {}
}

// The plugin must not outlive bitc, even when bitc is SIGKILLed and runs no cleanup: Pdeathsig on
// Linux, the shim's lifeline (or kqueue on the bitc pid) on darwin. The whole group goes, shim
// included.
func TestAPluginDiesWithItsSigkilledBitc(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "bitc-provider-stub")
	b, err := os.ReadFile(stubBin)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, b, 0o755); err != nil {
		t.Fatal(err)
	}
	sum, _ := pin.SumFile(bin)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	host := exec.Command(self, "__host", bin, sum, dir)
	errs := &syncBuf{}
	host.Stderr = errs
	out, err := host.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := host.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = host.Process.Kill(); _ = host.Wait() })
	line, _ := bufio.NewReader(out).ReadString('\n')
	if strings.TrimSpace(line) != "ready" {
		t.Fatalf("the stand-in bitc did not start the plugin: %q stderr %q", line, errs.String())
	}
	r := &rig{t: t, own: filepath.Join(dir, "state", "plugins", "stub")}
	d := r.diag()
	t.Cleanup(func() { _ = syscall.Kill(-d.Pgid, syscall.SIGKILL) }) // never leave a plugin behind
	if d.Pgid == syscall.Getpgrp() {
		t.Fatalf("the plugin shares the test's group %d", d.Pgid)
	}
	if err := host.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	_ = host.Wait()
	deadline := time.Now().Add(30 * time.Second)
	for {
		pluginUp := syscall.Kill(d.Pid, 0) == nil
		groupUp := syscall.Kill(-d.Pgid, 0) == nil
		if !pluginUp && !groupUp {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("after SIGKILL of bitc the plugin %d (alive %v) / its group %d (alive %v) still runs", d.Pid, pluginUp, d.Pgid, groupUp)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
