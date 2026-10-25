package liveprobe

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Defaults and hard limits (S.0: under $1 for the whole run).
const (
	DefaultMaxUSD       = 0.90
	HardMaxUSD          = 0.99
	DefaultPricePerHour = 0.7571 // the 16 GiB bound
	DefaultMaxMinutes   = 75
	MaxMaxMinutes       = 120
	MaxLive             = 1 // fixed
)

// Config is the run's settings. The unexported-by-flag fields (Now, Sleep, Log) are test seams.
type Config struct {
	Org, TokenFile, APIBase string
	Out                     string
	Probes                  []int // nil = the default plan
	DryRun                  bool
	MaxUSD, PricePerHour    float64
	MaxMinutes              int
	DNSCanary               string
	RunID                   string

	Now   func() time.Time
	Sleep func(ctx context.Context, d time.Duration) error
	Log   io.Writer
}

var (
	runIDRE  = regexp.MustCompile(`^[a-z0-9]{1,12}$`)
	domainRE = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,}$`)
)

// ErrUsage marks a flag error (exit 64).
var ErrUsage = errors.New("usage")

// errFlagHelp: -h was asked for; the usage is already printed.
var errFlagHelp = errors.New("help")

// ParseFlags reads the command line. --org and --token-file are required. Flag errors and help go to
// errOut.
func ParseFlags(args []string, errOut io.Writer) (Config, error) {
	c := Config{MaxUSD: DefaultMaxUSD, PricePerHour: DefaultPricePerHour, MaxMinutes: DefaultMaxMinutes}
	fs := flag.NewFlagSet("bitc-sprites-probe", flag.ContinueOnError)
	fs.SetOutput(errOut)
	probes := fs.String("probes", "", "comma-separated item numbers of design section 9 (default: all, phase-1 items first)")
	maxLive := fs.Int("max-live", MaxLive, "live Sprites at any time (fixed at 1)")
	fs.StringVar(&c.Org, "org", "", "the org slug the token belongs to (required)")
	fs.StringVar(&c.TokenFile, "token-file", "", "a 0600 file holding the org token (required)")
	fs.StringVar(&c.APIBase, "api-base", "", "API base URL (default the production API; http only for loopback)")
	fs.StringVar(&c.Out, "out", "", "write one JSON line per probe to this file")
	fs.BoolVar(&c.DryRun, "dry-run", false, "print the plan, the caps and the per-probe worst case; make no request")
	fs.Float64Var(&c.MaxUSD, "max-usd", DefaultMaxUSD, "spend cap in USD")
	fs.Float64Var(&c.PricePerHour, "price-per-hour", DefaultPricePerHour, "USD per Sprite-hour")
	fs.IntVar(&c.MaxMinutes, "max-minutes", DefaultMaxMinutes, "wall-clock cap in minutes")
	fs.StringVar(&c.DNSCanary, "dns-canary-domain", "", "an owner-run domain for probe 22")
	fs.StringVar(&c.RunID, "run", "", "run id (default random)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return Config{}, errFlagHelp
		}
		return Config{}, fmt.Errorf("%w: %w", ErrUsage, err)
	}
	if fs.NArg() > 0 {
		return Config{}, fmt.Errorf("%w: unexpected argument %q", ErrUsage, fs.Arg(0))
	}
	if *maxLive != MaxLive {
		return Config{}, fmt.Errorf("%w: --max-live is fixed at 1", ErrUsage)
	}
	if c.Org == "" {
		return Config{}, fmt.Errorf("%w: --org is required", ErrUsage)
	}
	if c.TokenFile == "" {
		return Config{}, fmt.Errorf("%w: --token-file is required (the token is never taken from the command line or the environment)", ErrUsage)
	}
	if c.MaxUSD <= 0 || c.MaxUSD > HardMaxUSD {
		return Config{}, fmt.Errorf("%w: --max-usd must be in (0, %.2f]", ErrUsage, HardMaxUSD)
	}
	if c.PricePerHour <= 0 {
		return Config{}, fmt.Errorf("%w: --price-per-hour must be positive", ErrUsage)
	}
	if c.MaxMinutes < 1 || c.MaxMinutes > MaxMaxMinutes {
		return Config{}, fmt.Errorf("%w: --max-minutes must be 1..%d", ErrUsage, MaxMaxMinutes)
	}
	if c.DNSCanary != "" && !domainRE.MatchString(c.DNSCanary) {
		return Config{}, fmt.Errorf("%w: --dns-canary-domain %q is not a domain", ErrUsage, c.DNSCanary)
	}
	if *probes != "" {
		seen := map[int]bool{}
		for _, f := range strings.Split(*probes, ",") {
			n, err := strconv.Atoi(strings.TrimSpace(f))
			if err != nil || n < 1 || n > 25 || seen[n] {
				return Config{}, fmt.Errorf("%w: --probes: %q is not a distinct item number 1..25", ErrUsage, f)
			}
			seen[n] = true
			c.Probes = append(c.Probes, n)
		}
	}
	if c.RunID == "" {
		var b [4]byte
		if _, err := rand.Read(b[:]); err != nil {
			return Config{}, err
		}
		c.RunID = hex.EncodeToString(b[:])
	}
	if !runIDRE.MatchString(c.RunID) {
		return Config{}, fmt.Errorf("%w: --run must be 1-12 of [a-z0-9]", ErrUsage)
	}
	return c, nil
}

// withDefaults fills the test seams.
func (c Config) withDefaults() Config {
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.Sleep == nil {
		c.Sleep = func(ctx context.Context, d time.Duration) error {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-t.C:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	if c.Log == nil {
		c.Log = io.Discard
	}
	return c
}
