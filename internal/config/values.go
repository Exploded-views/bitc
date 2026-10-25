package config

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var (
	intRE      = regexp.MustCompile(`^(0|[1-9][0-9]*)$`)
	durationRE = regexp.MustCompile(`^((0|[1-9][0-9]*)(ms|s|m|h|d))+$`)
	durPartRE  = regexp.MustCompile(`(0|[1-9][0-9]*)(ms|s|m|h|d)`)
	sizeRE     = regexp.MustCompile(`^(0|[1-9][0-9]*)(KiB|MiB|GiB|TiB)$`)
	nameRE     = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	sizeKeyRE  = regexp.MustCompile(`^[a-z0-9]{1,10}$`) // ten characters keep resource names within 63 (ruling 19)
	envNameRE  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// ParseInt accepts 0 or [1-9][0-9]* (spec 5.2; no leading zero, sign, unit or fraction).
func ParseInt(s string) (int64, error) {
	if !intRE.MatchString(s) {
		return 0, errors.New("must be a whole number written 0 or 1-9 then digits (no leading zero, sign, unit or fraction)")
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, errors.New("is out of range")
	}
	return n, nil
}

var durUnits = map[string]time.Duration{"ms": time.Millisecond, "s": time.Second, "m": time.Minute, "h": time.Hour, "d": 24 * time.Hour}

// ParseDuration accepts <int><unit>[<int><unit>...] with units ms s m h d (spec 5.2).
func ParseDuration(s string) (time.Duration, error) {
	if !durationRE.MatchString(s) {
		return 0, errors.New("must be a duration like 30s, 10m or 1h30m (units ms s m h d; no bare number, sign, fraction or leading zero)")
	}
	var total time.Duration
	for _, m := range durPartRE.FindAllStringSubmatch(s, -1) {
		n, err := strconv.ParseInt(m[1], 10, 64)
		unit := durUnits[m[2]]
		if err != nil || n > int64(math.MaxInt64/unit) || total > time.Duration(math.MaxInt64)-time.Duration(n)*unit {
			return 0, errors.New("is out of range")
		}
		total += time.Duration(n) * unit
	}
	return total, nil
}

var sizeUnits = map[string]int64{"KiB": 1 << 10, "MiB": 1 << 20, "GiB": 1 << 30, "TiB": 1 << 40}

// ParseSize accepts <int><KiB|MiB|GiB|TiB> (spec 5.2).
func ParseSize(s string) (Size, error) {
	m := sizeRE.FindStringSubmatch(s)
	if m == nil {
		return 0, errors.New("must be a size like 512MiB or 6GiB (units KiB MiB GiB TiB)")
	}
	n, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil || n > math.MaxInt64/sizeUnits[m[2]] {
		return 0, errors.New("is out of range")
	}
	return Size(n * sizeUnits[m[2]]), nil
}

// String prints the size in the largest unit that divides it exactly.
func (s Size) String() string {
	for _, u := range []string{"TiB", "GiB", "MiB", "KiB"} {
		if int64(s)%sizeUnits[u] == 0 && s != 0 {
			return fmt.Sprintf("%d%s", int64(s)/sizeUnits[u], u)
		}
	}
	if s == 0 {
		return "0GiB"
	}
	return fmt.Sprintf("%dB", int64(s))
}

// ParseMoney accepts a TOML number >= 0 with at most 4 decimals (spec 5.2).
func ParseMoney(v any) (Micros, error) {
	switch n := v.(type) {
	case int64:
		if n < 0 || n > math.MaxInt64/1_000_000 {
			return 0, errors.New("must be a number of dollars >= 0")
		}
		return Micros(n * 1_000_000), nil
	case float64:
		if math.IsNaN(n) || math.IsInf(n, 0) || n < 0 || n > 1e12 {
			return 0, errors.New("must be a number of dollars >= 0")
		}
		txt := strconv.FormatFloat(n, 'f', -1, 64)
		if i := strings.IndexByte(txt, '.'); i >= 0 && len(txt)-i-1 > 4 {
			return 0, errors.New("must have at most 4 decimals")
		}
		return Micros(math.Round(n * 1_000_000)), nil
	default:
		return 0, errors.New("must be a number of dollars (not a string)")
	}
}

// Dollars prints the amount with two decimals, as the cap note does (spec 8.3).
func (m Micros) Dollars() string {
	cents := (int64(m) + 5_000) / 10_000
	return fmt.Sprintf("$%d.%02d", cents/100, cents%100)
}

func ValidPoolName(s string) bool { return nameRE.MatchString(s) }
func CanonName(s string) string   { return strings.ReplaceAll(s, "_", "-") }
func ValidSizeKey(s string) bool  { return sizeKeyRE.MatchString(s) }

// ValidEnvName accepts ^[A-Za-z_][A-Za-z0-9_]*$ not starting BITC_ (spec 5.2).
func ValidEnvName(s string) bool {
	return envNameRE.MatchString(s) && !strings.HasPrefix(s, "BITC_")
}
