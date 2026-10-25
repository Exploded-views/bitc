package rails

import (
	"bufio"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/Exploded-views/bitc/internal/config"
)

// Ledger is ACCOUNT/remote/<type>/ledger.jsonl (spec 8.3): launch, begin, end lines.
type Ledger struct{ Dir string }

type ledgerLine struct {
	TS      string  `json:"ts"`
	Event   string  `json:"event"` // launch | begin | end
	Name    string  `json:"name"`
	Minutes int     `json:"minutes,omitempty"`  // end: minutes since the create
	EstUSD  float64 `json:"est_usd,omitempty"`  // the job's estimate at launch
	CostUSD float64 `json:"cost_usd,omitempty"` // end: what the resource cost (0 if none was created)
	Created bool    `json:"created,omitempty"`  // end: a resource was created
	Exit    string  `json:"exit,omitempty"`     // a code, "75", "fallback" or "interrupted"
}

func (l Ledger) append(e ledgerLine) error {
	e.TS = time.Now().UTC().Format(time.RFC3339)
	b, _ := json.Marshal(e)
	fd, err := unix.Open(filepath.Join(l.Dir, "ledger.jsonl"), unix.O_WRONLY|unix.O_APPEND|unix.O_CREAT|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	_, err = unix.Write(fd, append(b, '\n'))
	return err
}

func usd(m config.Micros) float64 { return float64(m) / 1e6 }

func (l Ledger) Launch(name string, est config.Micros) error {
	return l.append(ledgerLine{Event: "launch", Name: name, EstUSD: usd(est)})
}
func (l Ledger) Begin(name string) error { return l.append(ledgerLine{Event: "begin", Name: name}) }
func (l Ledger) End(name string, minutes int, est, cost config.Micros, outcome string, created bool) error {
	return l.append(ledgerLine{Event: "end", Name: name, Minutes: minutes, EstUSD: usd(est), CostUSD: usd(cost),
		Created: created, Exit: outcome})
}

// LedgerToday is today's (UTC) spend this host saw: the cost of every ended job that created a
// resource. Open launches are left out (their resource is in L, or their reservation counts),
// so a running job is counted once; a job that created nothing (a capacity fallback) costs
// nothing (spec 8.3, spec 14 "a running job is counted once").
func LedgerToday(dir string, now time.Time) config.Micros {
	f, err := os.Open(filepath.Join(dir, "ledger.jsonl"))
	if err != nil {
		return 0
	}
	defer f.Close()
	day := now.UTC().Format("2006-01-02")
	var total float64
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var e ledgerLine
		if json.Unmarshal(sc.Bytes(), &e) != nil || !strings.HasPrefix(e.TS, day) {
			continue
		}
		if e.Event == "end" && e.Created {
			total += e.CostUSD
		}
	}
	return config.Micros(math.Round(total * 1e6))
}
