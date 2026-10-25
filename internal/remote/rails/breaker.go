package rails

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"

	"github.com/Exploded-views/bitc/internal/config"
)

type failure struct {
	TS    int64  `json:"ts"`
	Kind  string `json:"kind"` // failure | pause
	Until int64  `json:"until,omitempty"`
	Why   string `json:"why,omitempty"`
}

func appendFailure(dir string, f failure) error {
	b, _ := json.Marshal(f)
	fd, err := unix.Open(filepath.Join(dir, "failures.jsonl"), unix.O_WRONLY|unix.O_APPEND|unix.O_CREAT|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	_, err = unix.Write(fd, append(b, '\n'))
	return err
}

// RecordFailure: one record per 75 after BEGIN (spec 8.3 breaker).
func RecordFailure(dir string) error {
	return appendFailure(dir, failure{TS: time.Now().Unix(), Kind: "failure"})
}

// Pause: one record per ErrUnavailable; launches are refused until now + window.
func Pause(dir, why string, window time.Duration) error {
	return appendFailure(dir, failure{TS: time.Now().Unix(), Kind: "pause", Until: time.Now().Add(window).Unix(), Why: why})
}

// Paused reports whether launches are refused now, and why.
func Paused(dir string, b config.Breaker, now time.Time) (string, bool) {
	f, err := os.Open(filepath.Join(dir, "failures.jsonl"))
	if err != nil {
		return "", false
	}
	defer f.Close()
	n := 0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var r failure
		if json.Unmarshal(sc.Bytes(), &r) != nil {
			continue
		}
		switch {
		case r.Kind == "pause" && now.Unix() < r.Until:
			return r.Why + " (until " + time.Unix(r.Until, 0).UTC().Format(time.RFC3339) + ")", true
		case r.Kind == "failure" && now.Sub(time.Unix(r.TS, 0)) < b.Window:
			n++
		}
	}
	if n >= b.Failures {
		return fmt.Sprintf("%d failures after start in the last %s", n, b.Window), true
	}
	return "", false
}
