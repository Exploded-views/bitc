package liveprobe

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/Exploded-views/bitc/internal/provider/sprites"
)

// NewClient builds the Sprites client from the flags: the token comes from the 0600 token file
// only, and the client refuses a token whose org slug is not --org (the org guard, before any
// create). It makes no request.
func NewClient(c Config) (*sprites.Client, error) {
	return sprites.New(sprites.Options{APIBase: c.APIBase, Org: c.Org, TokenFile: c.TokenFile})
}

// Main is the tool: flags, the dry run or the real run, the report. It returns the exit code.
func Main(args []string, stdout, stderr io.Writer) int {
	cfg, err := ParseFlags(args, stderr)
	if err != nil {
		if !errors.Is(err, errFlagHelp) {
			fmt.Fprintf(stderr, "bitc-sprites-probe: %v\n", err)
		}
		return ExitUsage
	}
	cfg.Log = stderr
	client, err := NewClient(cfg)
	if err != nil {
		fmt.Fprintf(stderr, "bitc-sprites-probe: %v\n", err)
		return ExitUsage
	}
	all := AllProbes()
	plan, err := BuildPlan(all, cfg.Probes)
	if err != nil {
		fmt.Fprintf(stderr, "bitc-sprites-probe: %v\n", err)
		return ExitUsage
	}
	if cfg.DryRun {
		plan.Print(stdout, cfg)
		return ExitOK
	}
	plan.Print(stderr, cfg)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	res := NewRunner(cfg, client, all).Run(ctx)
	if cfg.Out != "" {
		if err := writeOut(cfg.Out, res.Report.Results); err != nil {
			fmt.Fprintf(stderr, "bitc-sprites-probe: --out: %v\n", err)
			if res.Exit == ExitOK {
				res.Exit = ExitProbe
			}
		}
	}
	WriteMarkdown(stdout, res.Report)
	return res.Exit
}

func writeOut(path string, results []Result) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if err := WriteJSONL(f, results); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}
